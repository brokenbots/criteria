package adapterhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/internal/log"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

const (
	OnCrashFail     = "fail"
	OnCrashRespawn  = "respawn"
	OnCrashAbortRun = "abort_run"
)

var (
	ErrSessionAlreadyOpen = errors.New("session already open")
	ErrUnknownSession     = errors.New("unknown session")
)

// FatalRunError signals a non-recoverable adapter failure that should abort
// the workflow run immediately (without applying failure-outcome fallback).
type FatalRunError struct {
	Err error
}

func (e *FatalRunError) Error() string {
	if e == nil || e.Err == nil {
		return "fatal run error"
	}
	return e.Err.Error()
}

func (e *FatalRunError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type SessionManager struct {
	loader Loader
	graph  *workflow.FSMGraph

	// sandboxProbeOverride is a test hook that replaces sandbox.Probe().
	// When nil the real Probe() is used.
	sandboxProbeOverride func() sandbox.Capabilities

	// lockfile holds the parsed lockfile for container-mode lookups.
	lockfile *lockfile.Lockfile

	// RedactionRegistry masks secret values from all host output streams.
	RedactionRegistry *secrets.Registry

	// Audit receives structured DecisionLogEntry records for every permission
	// decision. If nil, audit logging is a no-op.
	Audit AuditWriter

	// remoteShim is set when the workflow references a remote environment.
	// It provides phone-home adapter handles instead of local binaries.
	remoteShim RemoteShim

	// LifecycleSink receives adapter provisioning events. When a verified-only
	// adapter is promoted to a bound session, "opened" is emitted through this
	// sink so lifecycle observers see the event at the correct phase-2 moment.
	LifecycleSink LifecycleSink

	// HeartbeatStallThreshold is the duration after which a log-stream heartbeat
	// is considered stalled. If zero, the default 90s is used. This is primarily
	// a test hook so conformance and regression tests can use a short threshold.
	HeartbeatStallThreshold time.Duration

	// RespawnLogStreamDrainTimeout is the maximum time restartLogStream waits for
	// the previous log-stream watcher to finish after cancellation. If zero, a
	// bound derived from HeartbeatStallThreshold is used (1/3 of the threshold,
	// clamped between 5s and 30s). This is a test hook so the bounded-wait
	// regression test can use a short value.
	RespawnLogStreamDrainTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]*Session
	// verified holds adapters that have passed eager verification (phase 1)
	// but have not yet been bound to their working directory (phase 2).
	// Binding happens automatically on the first Execute call.
	verified map[string]*verifiedRecord
	// bindMu serializes the one-time promotion of a verified adapter to a
	// bound session. This prevents concurrent Execute callers from racing to
	// bind the same adapter and seeing ErrSessionAlreadyOpen from the winner.
	bindMu sync.Mutex

	// allowedRoots restricts environment working_directory values. Empty means
	// no additional root checks; paths containing ".." are always rejected.
	allowedRoots []string

	// graphAdapters caches every adapter node in the compiled graph tree, keyed
	// by instance ID. It is populated by VerifyGraph and used at resolution time
	// to know whether an adapter is OCI-backed without re-reading workflow files.
	graphAdapters map[string]*workflow.AdapterNode
	// adapterDirs records the workflow directory each adapter was declared in,
	// keyed by instance ID. Populated by VerifyGraph.
	adapterDirs map[string]string
}

// verifiedRecord stores the host-visible state of an adapter that has been
// verified but not yet bound. It contains everything needed to start the
// long-lived session later.
type verifiedRecord struct {
	name             string
	adapter          string
	onCrash          string
	config           map[string]string
	secrets          map[string]string
	secretOriginRefs map[string]secrets.OriginRef
	capabilities     []string
	workingDir       string
	adapterDigest    digest.Digest
	// scopeName is the engine scope that verified this adapter. It is used to
	// attribute the phase-2 "opened" lifecycle event to the same scope as the
	// "verified" event.
	scopeName string
}

func (m *SessionManager) heartbeatStallThreshold() time.Duration {
	if m.HeartbeatStallThreshold > 0 {
		return m.HeartbeatStallThreshold
	}
	return 90 * time.Second
}

func (m *SessionManager) respawnLogStreamDrainTimeout() time.Duration {
	if m.RespawnLogStreamDrainTimeout > 0 {
		return m.RespawnLogStreamDrainTimeout
	}
	third := m.heartbeatStallThreshold() / 3
	if third < 5*time.Second {
		return 5 * time.Second
	}
	if third > 30*time.Second {
		return 30 * time.Second
	}
	return third
}

// RemoteShim is the interface the session manager uses to wait for remote
// adapter connections.
type RemoteShim interface {
	WaitForHandle(ctx context.Context, adapterType string) (Handle, error)
	// WaitForFreshHandle waits for a connection whose handle is not `stale`,
	// used on crash-respawn so the dead handle is never handed back.
	WaitForFreshHandle(ctx context.Context, adapterType string, stale Handle) (Handle, error)
}

// LifecycleSink receives adapter lifecycle events from the session manager.
type LifecycleSink interface {
	OnAdapterLifecycle(runID, adapter, status, detail string)
}

// SetGraph provides the compiled workflow graph so the session manager
// can look up per-adapter environment policies (e.g. sandbox) at open time.
func (m *SessionManager) SetGraph(g *workflow.FSMGraph) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.graph = g
}

// SetRemoteShim provides the remote shim so the session manager can dispatch
// adapters bound to remote environments to the phone-home listener.
func (m *SessionManager) SetRemoteShim(shim RemoteShim) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remoteShim = shim
}

// SetLockfile provides the parsed lockfile so the session manager can
// resolve container images for adapters bound to container environments.
func (m *SessionManager) SetLockfile(lf *lockfile.Lockfile) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lockfile = lf
}

// GetLockfile returns the current lockfile (may be nil).
func (m *SessionManager) GetLockfile() *lockfile.Lockfile {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lockfile
}

// graphAdapterRef identifies a single adapter node in the compiled graph tree
// so VerifyGraph can walk the whole tree and attribute errors to a workflow
// directory.
type graphAdapterRef struct {
	instanceID string
	node       *workflow.AdapterNode
	graph      *workflow.FSMGraph
}

// collectGraphAdapters appends every adapter declared in g (and recursively in
// its subworkflows) to refs. Parent adapters appear before child adapters so the
// cached metadata reflects the first declaration when an instance is
// re-declared in a subworkflow.
func collectGraphAdapters(g *workflow.FSMGraph, refs *[]graphAdapterRef) {
	if g == nil {
		return
	}
	for _, id := range g.AdapterOrder {
		*refs = append(*refs, graphAdapterRef{
			instanceID: id,
			node:       g.Adapters[id],
			graph:      g,
		})
	}
	for _, name := range g.SubworkflowOrder {
		collectGraphAdapters(g.Subworkflows[name].Body, refs)
	}
}

// VerifyGraph eagerly verifies every adapter declared anywhere in the compiled
// graph tree before any step runs. It performs the same handshake and policy
// checks as per-scope Verify but does not store verified records, so session
// binding remains lazy at scope entry. Missing lockfile entries for OCI-backed
// adapters are reported as fatal errors naming the workflow directory, the
// adapter instance, and the remediation command.
func (m *SessionManager) VerifyGraph(ctx context.Context, graph *workflow.FSMGraph, vars map[string]cty.Value) error {
	if graph == nil {
		return nil
	}
	var refs []graphAdapterRef
	collectGraphAdapters(graph, &refs)

	for _, ref := range refs {
		if err := m.verifyGraphAdapter(ctx, graph, ref, vars); err != nil {
			return err
		}
	}
	return nil
}

func (m *SessionManager) verifyGraphAdapter(ctx context.Context, root *workflow.FSMGraph, ref graphAdapterRef, vars map[string]cty.Value) error {
	evalVars := vars
	if ref.graph != root {
		// For subworkflows, use the callee's declared variable defaults at
		// startup. Runtime input bindings are evaluated later at scope entry.
		evalVars = workflow.SeedVarsFromGraph(ref.graph)
	}

	// Re-evaluate adapter config so var.* values are available, but file()
	// content is served from the compile-time cache. This matches what
	// prepareScopeAdapter does at scope entry.
	config := ref.node.Config
	if len(ref.node.ConfigExprs) > 0 {
		opts := workflow.DefaultFunctionOptions(ref.graph.WorkflowDir)
		opts.FileCache = ref.graph.FileCache
		runtimeConfig, err := workflow.ResolveInputExprsWithOpts(ref.node.ConfigExprs, evalVars, opts)
		if err != nil {
			return fmt.Errorf("verify adapter %q in %q: evaluate config: %w", ref.instanceID, ref.graph.WorkflowDir, err)
		}
		config = runtimeConfig
	}

	secretMap, err := m.verifyGraphSecrets(ref, evalVars)
	if err != nil {
		return err
	}

	if _, err := m.verifyAdapterInfo(ctx, ref.instanceID, ref.node.Type, config, secretMap); err != nil {
		return fmt.Errorf("verify adapter %q in %q: %w; run 'criteria adapter lock %s'", ref.instanceID, ref.graph.WorkflowDir, err, ref.graph.WorkflowDir)
	}

	m.cacheGraphAdapterRef(ref)
	return nil
}

func (m *SessionManager) cacheGraphAdapterRef(ref graphAdapterRef) {
	if m.graphAdapters == nil {
		m.graphAdapters = make(map[string]*workflow.AdapterNode)
	}
	if _, ok := m.graphAdapters[ref.instanceID]; !ok {
		m.graphAdapters[ref.instanceID] = ref.node
	}
	if m.adapterDirs == nil {
		m.adapterDirs = make(map[string]string)
	}
	if _, ok := m.adapterDirs[ref.instanceID]; !ok {
		m.adapterDirs[ref.instanceID] = ref.graph.WorkflowDir
	}
}

// verifyGraphSecrets evaluates an adapter's secret expressions for VerifyGraph,
// using the compile-time file cache. Returns nil when the adapter declares no
// secrets.
func (m *SessionManager) verifyGraphSecrets(ref graphAdapterRef, vars map[string]cty.Value) (map[string]string, error) {
	if len(ref.node.Secrets) == 0 {
		return nil, nil
	}
	opts := workflow.DefaultFunctionOptions(ref.graph.WorkflowDir)
	opts.FileCache = ref.graph.FileCache
	out, err := workflow.ResolveInputExprsWithOpts(ref.node.Secrets, vars, opts)
	if err != nil {
		return nil, fmt.Errorf("verify adapter %q in %q: evaluate secrets: %w", ref.instanceID, ref.graph.WorkflowDir, err)
	}
	return out, nil
}

// SetAllowedWorkingDirRoots restricts the directories an environment may bind
// to. Empty (the default) disables the additional root check; paths containing
// ".." are always rejected.
func (m *SessionManager) SetAllowedWorkingDirRoots(roots []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allowedRoots = append([]string(nil), roots...)
}

// ValidateWorkingDirFaceValue rejects a resolved working directory that is
// structurally invalid before the run starts. Paths containing ".." are always
// rejected. When allowed roots are configured, the directory must lie under one
// of them. A missing directory is *not* an error here: it will be deferred to
// session binding, allowing a workflow step to create it first.
func (m *SessionManager) ValidateWorkingDirFaceValue(dir string) error {
	if dir == "" {
		return nil
	}

	for _, part := range strings.Split(filepath.ToSlash(dir), "/") {
		if part == ".." {
			return fmt.Errorf("working directory %q contains \"..\"", dir)
		}
	}

	m.mu.Lock()
	roots := m.allowedRoots
	m.mu.Unlock()
	if len(roots) == 0 {
		return nil
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve working directory %q: %w", dir, err)
	}
	absDir = filepath.Clean(absDir)

	for _, root := range roots {
		root = filepath.Clean(root)
		if root == "" {
			continue
		}
		if root == string(filepath.Separator) {
			return nil
		}
		if absDir == root || strings.HasPrefix(absDir, root+string(filepath.Separator)) {
			return nil
		}
	}

	return fmt.Errorf("working directory %q is outside allowed roots", dir)
}

type Session struct {
	Name             string
	Adapter          string
	Config           map[string]string
	Secrets          map[string]string            // resolved secret values (WS13)
	SecretOriginRefs map[string]secrets.OriginRef // unevaluated origin refs for snapshot/restore (WS18)
	OnCrash          string
	Capabilities     []string // cached from plug.Info() at Open time
	PermissionState  *permissionState
	handle           Handle
	respawned        bool
	closing          atomic.Bool
	SandboxCleanup   func() // removes transient cgroup dirs, etc.
	// WorkingDir is the resolved environment working_directory (the adapter
	// process launch cwd), evaluated at adapter init. Persisted so respawn and
	// snapshot/restore relaunch the adapter in the same directory.
	WorkingDir string
	// AdapterDigest is the lockfile digest at the time the session was opened.
	AdapterDigest digest.Digest

	// WS15: session-level log stream lifecycle and heartbeat tracking.
	logMu          sync.Mutex
	cancelLog      func()
	logDone        <-chan error
	logHostCancel  chan struct{} // closed by the host before cancelLog(); per-stream
	logStreamAlive atomic.Bool
	hbMonitor      adapter.HeartbeatMonitor
	currentSink    adapter.EventSink
	currentSinkMu  sync.Mutex

	// WS15: MergeBuffer interleaves log and adapter events by timestamp.
	mergeBuf *log.MergeBuffer

	// WS17: session-level pause state for idempotency.
	paused  bool
	pauseMu sync.Mutex
}

// Pause halts work on the session without losing state.
// It calls the adapter handle first, then pauses the permission state.
// Calling Pause on an already-paused session is a no-op.
func (s *Session) Pause(ctx context.Context) error {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	if s.paused {
		return nil
	}
	if err := s.handle.Pause(ctx, s.Name); err != nil {
		return err
	}
	if s.PermissionState != nil {
		s.PermissionState.Pause()
	}
	s.paused = true
	return nil
}

// Resume continues the session from where it was paused.
// It resumes the permission state first, then calls the adapter handle.
// Calling Resume on an already-active session is a no-op.
func (s *Session) Resume(ctx context.Context) error {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	if !s.paused {
		return nil
	}
	if s.PermissionState != nil {
		s.PermissionState.Resume()
	}
	if err := s.handle.Resume(ctx, s.Name); err != nil {
		return err
	}
	s.paused = false
	return nil
}

// Inspect returns a structured read-only view of the session's state.
func (s *Session) Inspect(ctx context.Context) (*v2.InspectResponse, error) {
	return s.handle.Inspect(ctx, s.Name)
}

func NewSessionManager(loader Loader) *SessionManager {
	return &SessionManager{
		loader:   loader,
		sessions: map[string]*Session{},
		verified: map[string]*verifiedRecord{},
	}
}

// SetSandboxProbeOverride sets the test hook that replaces sandbox.Probe()
// when evaluating sandbox requirements. It is intended for tests that need to
// simulate a host with missing sandbox primitives.
func (m *SessionManager) SetSandboxProbeOverride(fn func() sandbox.Capabilities) {
	m.sandboxProbeOverride = fn
}

// validateSandboxPrimitivesEagerly runs the host-side sandbox validation for
// instanceID without creating side effects. It resolves the sandbox policy,
// probes the host's sandbox primitives, and calls sandbox.Prepare with
// ValidateOnly set so no transient cgroup directories or other bind-time state
// is allocated. If the adapter is not bound to a sandbox environment or no
// policy exists, it returns nil. In strict mode a missing primitive (or any
// other Prepare validation failure) returns an error so the run fails at
// startup; in permissive mode the failure is logged and the function returns
// nil, matching the bind-time behavior.
func (m *SessionManager) validateSandboxPrimitivesEagerly(instanceID string) error {
	envNode, rp, ok := m.sandboxEnvAndPolicy(instanceID)
	if !ok {
		return nil
	}

	var adapterBinary string
	if m.graph != nil {
		if adapterNode, ok := m.graph.Adapters[instanceID]; ok {
			path, discoverErr := DiscoverBinary(adapterNode.Type)
			if discoverErr == nil {
				adapterBinary = path
			} else {
				var notFound *ErrAdapterNotFound
				if !errors.As(discoverErr, &notFound) {
					return fmt.Errorf("discover adapter binary: %w", discoverErr)
				}
				// Built-in adapter or missing plugin: leave adapterBinary empty.
				// The primitive-availability check does not depend on it.
			}
		}
	}

	caps := sandbox.Probe()
	if m.sandboxProbeOverride != nil {
		caps = m.sandboxProbeOverride()
	}
	if missing := caps.Missing(); len(missing) > 0 {
		slog.Info("sandbox primitives missing", "missing", missing, "instance", instanceID)
	}

	ctx := sandbox.PrepareContext{
		Policy:        rp,
		Env:           envNode,
		Caps:          caps,
		AdapterBinary: adapterBinary,
		ValidateOnly:  true,
	}
	_, err := sandbox.Handler{}.Prepare(ctx)
	if err != nil {
		if rp.PolicyMode == "strict" {
			return fmt.Errorf("sandbox strict mode: %w", err)
		}
		slog.Info("sandbox permissive degradation", "instance", instanceID, "error", err)
	}
	return nil
}

// buildSandboxCustomizer returns a function that applies the sandbox
// configuration to an exec.Cmd, or nil if the adapter is not bound to a
// sandbox environment. The second return value is a cleanup function
// that removes transient resources (e.g. cgroup directories); it may be
// nil. The third return value is an error that is only non-nil when the
// sandbox policy is strict and a required primitive is unavailable; in
// that case the caller must abort the session before Resolve.
func (m *SessionManager) buildSandboxCustomizer(instanceID, workingDir string) (customizer func(name string, cmd *exec.Cmd), cleanup func(), err error) {
	envNode, rp, ok := m.sandboxEnvAndPolicy(instanceID)
	if !ok {
		return nil, nil, nil
	}

	// Resolve the adapter binary path so the sandbox profile can
	// pre-allowlist it at prepare time (avoids mutating the profile
	// inside ApplyToCmd).
	var adapterBinary string
	if m.graph != nil {
		if adapterNode, ok := m.graph.Adapters[instanceID]; ok {
			path, discoverErr := DiscoverBinary(adapterNode.Type)
			if discoverErr == nil {
				adapterBinary = path
			} else {
				var notFound *ErrAdapterNotFound
				if !errors.As(discoverErr, &notFound) {
					return nil, nil, fmt.Errorf("discover adapter binary: %w", discoverErr)
				}
				// Built-in adapter or missing plugin: leave adapterBinary empty.
				// For built-ins the customizer is never invoked; for missing
				// plugins ResolveWithCustomizer will fail later anyway.
			}
		}
	}

	caps := sandbox.Probe()
	if m.sandboxProbeOverride != nil {
		caps = m.sandboxProbeOverride()
	}
	if missing := caps.Missing(); len(missing) > 0 {
		slog.Info("sandbox primitives missing", "missing", missing, "instance", instanceID)
	}

	ctx := sandbox.PrepareContext{
		Policy:        rp,
		Env:           envNode,
		Caps:          caps,
		AdapterBinary: adapterBinary,
	}
	prep, err := sandbox.Handler{}.Prepare(ctx)
	if err != nil {
		if rp.PolicyMode == "strict" {
			return nil, nil, fmt.Errorf("sandbox strict mode: %w", err)
		}
		slog.Info("sandbox permissive degradation", "instance", instanceID, "error", err)
		return nil, nil, nil
	}

	customizer, cleanup = makeSandboxCustomizer(&prep, envNode, workingDir)
	return customizer, cleanup, nil
}

// sandboxEnvAndPolicy resolves the sandbox environment node and resolved
// policy for the given adapter instance. It returns false if the adapter
// is not bound to a sandbox environment or no policy is available.
func (m *SessionManager) sandboxEnvAndPolicy(instanceID string) (envNode *workflow.EnvironmentNode, rp *workflow.ResolvedPolicy, ok bool) {
	if m.graph == nil {
		return nil, nil, false
	}
	adapterNode, ok := m.graph.Adapters[instanceID]
	if !ok {
		return nil, nil, false
	}
	envKey := adapterNode.Environment
	if envKey == "" {
		envKey = m.graph.DefaultEnvironment
	}
	if envKey == "" {
		return nil, nil, false
	}
	envNode, ok = m.graph.Environments[envKey]
	if !ok {
		return nil, nil, false
	}
	if envNode.Type != "sandbox" {
		return nil, nil, false
	}
	cacheKey := instanceID + ":" + envKey
	rp, ok = m.graph.ResolvedPolicies[cacheKey]
	if !ok {
		return nil, nil, false
	}
	return envNode, rp, true
}

// buildCommandCustomizer composes the sandbox command customizer (if any) with a
// working-directory customizer derived from the bound environment. The
// environment's working_directory becomes the adapter process launch cwd, which
// shell/copilot adapters inherit as the default directory for their work.
//
// Container environments never reach this path (they launch via a container
// runner) and never carry a working_directory; remote environments apply their
// working_directory on the remote host. So this only adjusts cwd for locally
// launched shell and sandbox adapters.
func (m *SessionManager) buildCommandCustomizer(instanceID, workingDir string) (customizer func(name string, cmd *exec.Cmd), cleanup func(), err error) {
	sandboxCust, cleanup, err := m.buildSandboxCustomizer(instanceID, workingDir)
	if err != nil {
		return nil, nil, err
	}

	if workingDir == "" {
		return sandboxCust, cleanup, nil
	}

	customizer = func(name string, cmd *exec.Cmd) {
		if sandboxCust != nil {
			// Sandbox customizers set cmd.Env (scrubbed) and may set cmd.Dir.
			// Run it first, then override the launch cwd so it wins over the
			// sandbox default (ApplyToCmd only sets Dir when empty; the bwrap
			// path manages the inner cwd via --chdir).
			sandboxCust(name, cmd)
		} else {
			// No sandbox customizer, but providing any customizer flips
			// go-plugin's SkipHostEnv to true (see loader.go). Preserve the host
			// environment ourselves so the adapter keeps PATH and friends;
			// go-plugin still appends its handshake vars.
			cmd.Env = os.Environ()
		}
		cmd.Dir = workingDir
	}
	return customizer, cleanup, nil
}

func (m *SessionManager) Open(ctx context.Context, name, adapterName, onCrash string, config, secrets map[string]string) error {
	return m.OpenWithOriginRefs(ctx, name, adapterName, onCrash, config, secrets, nil, "")
}

// OpenWithOriginRefs opens an adapter session. workingDir is the resolved
// environment working_directory (evaluated at adapter init); it becomes the
// adapter process launch cwd. Pass "" for no working-directory override.
func (m *SessionManager) OpenWithOriginRefs(ctx context.Context, name, adapterName, onCrash string, config, secrets map[string]string, originRefs map[string]secrets.OriginRef, workingDir string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("session name is required")
	}
	if strings.TrimSpace(adapterName) == "" {
		return fmt.Errorf("session %q: adapter name is required", name)
	}

	m.mu.Lock()
	if _, exists := m.sessions[name]; exists {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	m.mu.Unlock()

	customizer, cleanup, sandboxErr := m.buildCommandCustomizer(name, workingDir)
	if sandboxErr != nil {
		return fmt.Errorf("session %q: %w", name, sandboxErr)
	}

	var plug Handle
	var err error
	plug, err = m.resolveAdapterHandle(ctx, name, adapterName, customizer)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return err
	}

	// Cache capabilities so HasCapability can be called without a separate Info RPC.
	// On error, capabilities default to nil — the runtime gate rejects parallel use.
	var caps []string
	if info, infoErr := plug.Info(ctx); infoErr == nil {
		caps = append([]string(nil), info.Capabilities...)
	}

	if err := plug.OpenSession(ctx, name, config, secrets); err != nil {
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return err
	}

	return m.registerSession(ctx, name, adapterName, onCrash, config, secrets, originRefs, caps, plug, cleanup, workingDir)
}

// Verify performs phase-1 adapter verification without binding the adapter to
// its working directory. It resolves the adapter binary (or container/remote
// equivalent), validates the protocol handshake via Info, checks the runtime
// config against the adapter's manifest schema, and ensures required secrets
// are present. The spawned handle is then killed; no long-lived session exists
// after this call returns. Verification runs eagerly at scope start so broken
// adapters fail before any step executes.
//
// If a verified or bound record already exists for name (e.g. a parent-scope
// adapter re-declared in a subworkflow), Verify returns ErrSessionAlreadyOpen.
// The caller should treat that as a no-op for re-declared adapters.
func (m *SessionManager) Verify(ctx context.Context, name, adapterName, onCrash string, config, secrets map[string]string, originRefs map[string]secrets.OriginRef, workingDir, scopeName string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("session name is required")
	}
	if strings.TrimSpace(adapterName) == "" {
		return fmt.Errorf("session %q: adapter name is required", name)
	}

	if err := m.checkDuplicateLocked(name); err != nil {
		return err
	}

	caps, err := m.verifyAdapterInfo(ctx, name, adapterName, config, secrets)
	if err != nil {
		return err
	}

	return m.storeVerifiedRecord(name, adapterName, onCrash, config, secrets, originRefs, workingDir, caps, scopeName)
}

// checkDuplicateLocked returns ErrSessionAlreadyOpen if the named session is
// already bound or verified. It is the caller's responsibility to hold no lock.
func (m *SessionManager) checkDuplicateLocked(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[name]; exists {
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	if _, exists := m.verified[name]; exists {
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	return nil
}

// verifyAdapterInfo performs the phase-1 adapter handshake in a neutral launch
// directory: it resolves the binary, calls Info, validates config and secrets,
// validates sandbox primitive availability for strict sandbox adapters, and
// kills the temporary handle. It returns the adapter capabilities on success.
func (m *SessionManager) verifyAdapterInfo(ctx context.Context, name, adapterName string, config, secrets map[string]string) ([]string, error) {
	// Phase 1 uses a neutral launch directory. The working-directory-dependent,
	// side-effecting parts of sandbox setup (cgroup directory creation, the bwrap
	// --chdir to the resolved working directory) remain a bind-time concern. The
	// host-side primitive-availability and strict-mode validation can and do run
	// eagerly here so that a strict-sandbox adapter with missing primitives fails
	// before any step executes.
	plug, err := m.resolveAdapterHandle(ctx, name, adapterName, nil)
	if err != nil {
		return nil, err
	}
	defer plug.Kill()

	info, infoErr := plug.Info(ctx)
	if infoErr != nil {
		return nil, fmt.Errorf("adapter %q handshake: %w", adapterName, infoErr)
	}

	if schemaErr := validateConfigAgainstSchema(config, info.AdapterInfo.ConfigSchema); schemaErr != nil {
		return nil, fmt.Errorf("adapter %q config: %w", adapterName, schemaErr)
	}

	if secretErr := validateRequiredSecrets(secrets, info.AdapterInfo.ConfigSchema); secretErr != nil {
		return nil, fmt.Errorf("adapter %q secrets: %w", adapterName, secretErr)
	}

	if sandboxErr := m.validateSandboxPrimitivesEagerly(name); sandboxErr != nil {
		return nil, fmt.Errorf("adapter %q sandbox validation: %w", adapterName, sandboxErr)
	}

	return info.Capabilities, nil
}

// storeVerifiedRecord stores a verified adapter record, guarding against races.
func (m *SessionManager) storeVerifiedRecord(name, adapterName, onCrash string, config, secrets map[string]string, originRefs map[string]secrets.OriginRef, workingDir string, capabilities []string, scopeName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[name]; exists {
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	if _, exists := m.verified[name]; exists {
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}

	rec := &verifiedRecord{
		name:             name,
		adapter:          adapterName,
		onCrash:          normalizeOnCrash(onCrash),
		config:           cloneConfig(config),
		secrets:          cloneConfig(secrets),
		secretOriginRefs: cloneOriginRefs(originRefs),
		capabilities:     append([]string(nil), capabilities...),
		workingDir:       workingDir,
		scopeName:        scopeName,
	}
	if a := m.lockedAdapterFor(name); a != nil {
		rec.adapterDigest = digest.Digest(a.ResolvedDigest)
	}
	m.verified[name] = rec
	return nil
}

func (m *SessionManager) resolveAdapterHandle(ctx context.Context, name, adapterName string, customizer func(string, *exec.Cmd)) (Handle, error) {
	// Remote-mode dispatch: if the adapter is bound to a remote environment,
	// wait for the adapter to phone home via the shim.
	if m.remoteShim != nil && m.isRemoteAdapter(name) {
		return m.remoteShim.WaitForHandle(ctx, adapterName)
	}

	if dl, ok := m.loader.(*DefaultLoader); ok {
		// Dev-mode dispatch: adapters registered with `criteria adapter dev`
		// are keyed by "<type>.<name>" and take precedence over lockfile pins
		// so local development builds are always used when explicitly bound.
		if devPath, ok := dl.DevBinding(name); ok {
			discover := func(_ string) (string, error) { return devPath, nil }
			return dl.ResolveWithDiscovery(ctx, adapterName, discover, customizer)
		}

		// Container-mode dispatch: if the adapter is bound to a container
		// environment, use a docker/podman runner instead of a local binary.
		runnerFunc, containerErr := adapter.BuildContainerRunner(m.graph, m.lockfile, name)
		if containerErr != nil {
			return nil, containerErr
		}
		if runnerFunc != nil {
			return dl.ResolveWithRunnerFunc(ctx, adapterName, runnerFunc)
		}
		// Digest-addressed dispatch: when the instance is pinned in the
		// lockfile, resolve the exact binary by digest so that two instances of
		// the same adapter type at different versions launch distinct binaries.
		if a := m.lockedAdapterFor(name); a != nil && a.ResolvedDigest != "" {
			enc := EncodeDigest(digest.Digest(a.ResolvedDigest))
			discover := func(t string) (string, error) { return DiscoverBinaryAt(t, enc) }
			h, err := dl.ResolveWithDiscovery(ctx, adapterName, discover, customizer)
			if err != nil {
				dir := m.adapterDir(name)
				return nil, fmt.Errorf("adapter %q in %q (digest %s) could not be resolved: %w; run 'criteria adapter lock %s'", name, dir, a.ResolvedDigest, err, dir)
			}
			return h, nil
		}
		// An OCI-backed adapter without a matching lockfile entry must never
		// fall back to by-name discovery. Surface a clear error that names the
		// workflow directory, the adapter instance, and the remediation command.
		if m.isOCIAdapter(name) {
			dir := m.adapterDir(name)
			return nil, fmt.Errorf("adapter %q in %q is not pinned in the lockfile; run 'criteria adapter lock %s'", name, dir, dir)
		}
		return dl.ResolveWithCustomizer(ctx, adapterName, customizer)
	}
	return m.loader.Resolve(ctx, adapterName)
}

// lockedAdapterFor returns the lockfile entry for the adapter instance keyed by
// the session instanceID ("<type>.<name>"), matching BOTH type and name so that
// multiple versions of the same adapter type resolve to distinct entries.
func (m *SessionManager) lockedAdapterFor(instanceID string) *lockfile.LockedAdapter {
	if m.lockfile == nil {
		return nil
	}
	typ, nm, ok := strings.Cut(instanceID, ".")
	if !ok {
		return nil
	}
	for i := range m.lockfile.Adapters {
		a := &m.lockfile.Adapters[i]
		if a.Type == typ && a.Name == nm {
			return a
		}
	}
	return nil
}

// isRemoteAdapter returns true when the adapter declaration is bound to a
// remote environment (or the default environment is remote).
func (m *SessionManager) isRemoteAdapter(instanceID string) bool {
	if m.graph == nil {
		return false
	}
	adapterNode, ok := m.graph.Adapters[instanceID]
	if !ok {
		return false
	}
	envKey := adapterNode.Environment
	if envKey == "" {
		envKey = m.graph.DefaultEnvironment
	}
	if envKey == "" {
		return false
	}
	envNode, ok := m.graph.Environments[envKey]
	if !ok {
		return false
	}
	return envNode.Type == "remote"
}

// isOCIAdapter reports whether the named adapter instance declares an OCI
// source. OCI adapters require a matching lockfile entry and may not fall back
// to by-name discovery. The lookup uses the graph tree cached by VerifyGraph so
// subworkflow adapters are covered even though the root graph does not contain
// them.
func (m *SessionManager) isOCIAdapter(instanceID string) bool {
	if m.graphAdapters != nil {
		if node, ok := m.graphAdapters[instanceID]; ok {
			return node.Source != ""
		}
	}
	if m.graph != nil {
		if node, ok := m.graph.Adapters[instanceID]; ok {
			return node.Source != ""
		}
	}
	return false
}

// adapterDir returns the workflow directory associated with instanceID,
// defaulting to the graph's root directory when no cached directory exists.
func (m *SessionManager) adapterDir(instanceID string) string {
	if m.adapterDirs != nil {
		if dir, ok := m.adapterDirs[instanceID]; ok {
			return dir
		}
	}
	if m.graph != nil {
		return m.graph.WorkflowDir
	}
	return ""
}

// makeSandboxCustomizer builds the exec.Cmd customizer and cleanup from
// a prepared LinuxPrepared config. It handles both the bubblewrap and
// in-process shim paths.
func makeSandboxCustomizer(prep *sandbox.LinuxPrepared, envNode *workflow.EnvironmentNode, workingDir string) (customizer func(name string, cmd *exec.Cmd), cleanup func()) {
	cleanup = func() { _ = prep.Cleanup() }
	if bwrapCmd := sandbox.MaybeUseBubblewrap(prep, envNode, workingDir); bwrapCmd != nil {
		return func(_ string, cmd *exec.Cmd) {
			cmd.Path = bwrapCmd.Path
			cmd.Args = bwrapCmd.Args
			cmd.Env = bwrapCmd.Env
			cmd.Dir = bwrapCmd.Dir
			cmd.SysProcAttr = nil
		}, cleanup
	}
	return func(_ string, cmd *exec.Cmd) {
		_ = prep.ApplyToCmd(cmd, os.Args[0])
	}, cleanup
}

func (m *SessionManager) registerSession(ctx context.Context, name, adapterName, onCrash string, config, secrets map[string]string, originRefs map[string]secrets.OriginRef, caps []string, plug Handle, cleanup func(), workingDir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[name]; exists {
		_ = plug.CloseSession(ctx, name)
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	sess := &Session{
		Name:             name,
		Adapter:          adapterName,
		Config:           cloneConfig(config),
		Secrets:          cloneConfig(secrets),
		SecretOriginRefs: cloneOriginRefs(originRefs),
		OnCrash:          normalizeOnCrash(onCrash),
		Capabilities:     caps,
		handle:           plug,
		SandboxCleanup:   cleanup,
		WorkingDir:       workingDir,
	}
	if a := m.lockedAdapterFor(name); a != nil {
		sess.AdapterDigest = digest.Digest(a.ResolvedDigest)
	}
	m.sessions[name] = sess

	m.startPermissionStream(ctx, sess, plug)
	m.startLogStream(ctx, sess, plug)
	return nil
}

// bindVerifiedRecord performs phase-2 session binding for a previously verified
// adapter. It launches the adapter in its resolved working directory, opens the
// long-lived session, and starts the permission/log streams. On success the
// verified record is promoted to a bound session.
func (m *SessionManager) bindVerifiedRecord(ctx context.Context, rec *verifiedRecord, stepName string) error {
	customizer, cleanup, sandboxErr := m.buildCommandCustomizer(rec.name, rec.workingDir)
	if sandboxErr != nil {
		return fmt.Errorf("session %q: %w", rec.name, sandboxErr)
	}

	plug, err := m.resolveAdapterHandle(ctx, rec.name, rec.adapter, customizer)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return err
	}

	var caps []string
	if info, infoErr := plug.Info(ctx); infoErr == nil {
		caps = append([]string(nil), info.Capabilities...)
	}

	if err := plug.OpenSession(ctx, rec.name, rec.config, rec.secrets); err != nil {
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return err
	}

	if err := m.registerSession(ctx, rec.name, rec.adapter, rec.onCrash, rec.config, rec.secrets, rec.secretOriginRefs, caps, plug, cleanup, rec.workingDir); err != nil {
		return err
	}

	if m.LifecycleSink != nil {
		m.LifecycleSink.OnAdapterLifecycle(rec.scopeName, rec.name, "opened", stepName)
	}
	return nil
}

// startPermissionStream starts the session-scoped Permissions stream if the
// adapter supports it.
func (m *SessionManager) startPermissionStream(ctx context.Context, sess *Session, plug Handle) {
	sess.PermissionState = NewPermissionState(sess.Name, m.Audit)
	if streamer, ok := plug.(PermissionStreamer); ok {
		cancel, err := streamer.StartPermissionStream(ctx, sess.Name, sess.PermissionState.Requests())
		if err != nil {
			slog.Warn("adapter permission stream start failed", "session", sess.Name, "err", err)
		} else {
			sess.PermissionState.SetStreamCancel(cancel)
		}
	}
}

// startLogStream starts the dedicated per-session Log stream if the adapter
// supports it.
func (m *SessionManager) startLogStream(ctx context.Context, sess *Session, plug Handle) {
	if starter, ok := plug.(LogStreamStarter); ok {
		m.beginLogStream(ctx, sess, starter)
	}
}

// beginLogStream starts the adapter Log stream for an open session, wires the
// heartbeat monitor, and launches the watcher. On error all stream state is
// cleared.
func (m *SessionManager) beginLogStream(ctx context.Context, sess *Session, starter LogStreamStarter) {
	logAdapterSink := &sessionLogAdapterSink{sess: sess}
	redactedLogSink := m.wrapSink(logAdapterSink)
	sess.mergeBuf = log.NewMergeBuffer(redactedLogSink, 500*time.Millisecond)
	logSink := &logForwardSink{
		sink: sess.mergeBuf,
		onHeartbeat: func() {
			sess.hbMonitor.Record()
		},
	}
	cancel, done, err := starter.StartLogStream(ctx, sess.Name, logSink)
	if err != nil {
		slog.Warn("adapter log stream start failed", "session", sess.Name, "err", err)
		sess.mergeBuf.Close()
		sess.mergeBuf = nil
		sess.resetLogStreamState()
		return
	}

	// Each stream generation gets its own host-cancel channel. The host closes
	// it *before* calling cancel() so the watcher can distinguish host-initiated
	// teardown from an adapter that returned early from Log.
	hostCancel := make(chan struct{})
	var closed atomic.Bool
	wrappedCancel := func() {
		if closed.CompareAndSwap(false, true) {
			close(hostCancel)
		}
		if cancel != nil {
			cancel()
		}
	}

	sess.logMu.Lock()
	sess.cancelLog = wrappedCancel
	sess.logDone = done
	sess.logHostCancel = hostCancel
	sess.logMu.Unlock()
	sess.logStreamAlive.Store(true)
	sess.hbMonitor.Record()
	go m.watchLogStream(sess, done, hostCancel)
}

// watchLogStream waits for the adapter's Log stream to end. If it ends while
// the session is still open and the host did not cancel it, the adapter broke
// the contract that the log stream must remain open for the lifetime of the
// session; we log a clear diagnostic and disarm the heartbeat stall detector
// so the session is not falsely declared crashed later.
func (m *SessionManager) watchLogStream(sess *Session, done <-chan error, hostCancel <-chan struct{}) {
	select {
	case <-hostCancel:
		// Host-initiated teardown for this specific stream generation. Drain
		// any terminal error and return without touching logStreamAlive.
		select {
		case <-done:
		default:
		}
		return
	case err := <-done:
		// The stream ended without a host cancellation signal. Re-check
		// hostCancel in case both channels became ready at the same time and
		// the select chose done.
		select {
		case <-hostCancel:
			return
		default:
		}
		if sess.closing.Load() {
			return
		}
		sess.logStreamAlive.Store(false)
		if err != nil {
			slog.Warn("adapter log stream ended unexpectedly; heartbeat stall detector disarmed", "session", sess.Name, "adapter", sess.Adapter, "error", err)
		} else {
			slog.Warn("adapter log stream ended while session was open; heartbeat stall detector disarmed (adapter broke the log-stream contract)", "session", sess.Name, "adapter", sess.Adapter)
		}
	}
}

// Close is intentionally idempotent: closing an unknown session is a no-op.
func (m *SessionManager) Close(ctx context.Context, name string) error {
	m.mu.Lock()
	sess, exists := m.sessions[name]
	if exists {
		delete(m.sessions, name)
	}
	// Verified-but-never-bound adapters are tracked for per-scope teardown via
	// initScopeAdapters/tearDownScopeAdapters. Remove the verified record so a
	// later scope reusing the same instance ID can verify and bind afresh.
	delete(m.verified, name)
	m.mu.Unlock()

	if !exists {
		return nil
	}
	sess.closing.Store(true)
	sess.logStreamAlive.Store(false)
	if sess.PermissionState != nil {
		sess.PermissionState.Stop()
	}
	sess.logMu.Lock()
	cancelLog := sess.cancelLog
	sess.logMu.Unlock()
	if cancelLog != nil {
		cancelLog()
	}
	if sess.mergeBuf != nil {
		sess.mergeBuf.Close()
	}
	if sess.SandboxCleanup != nil {
		sess.SandboxCleanup()
	}
	err := sess.handle.CloseSession(ctx, name)
	sess.handle.Kill()
	return err
}

// PauseAll iterates over every open session and calls Pause on each.
// It is reentrant and idempotent: pausing an already-paused session is a no-op.
func (m *SessionManager) PauseAll(ctx context.Context) error {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	var firstErr error
	for _, s := range sessions {
		if err := s.Pause(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ResumeAll iterates over every open session and calls Resume on each.
// It is reentrant and idempotent: resuming an already-active session is a no-op.
func (m *SessionManager) ResumeAll(ctx context.Context) error {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	var firstErr error
	for _, s := range sessions {
		if err := s.Resume(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// InspectSession returns the inspect response for a single session by name.
func (m *SessionManager) InspectSession(ctx context.Context, name string) (*v2.InspectResponse, error) {
	sess, err := m.lookup(name)
	if err != nil {
		return nil, err
	}
	return sess.Inspect(ctx)
}

func (m *SessionManager) wrapSink(sink adapter.EventSink) adapter.EventSink {
	if m.RedactionRegistry == nil {
		return sink
	}
	return &secrets.RedactingEventSink{Registry: m.RedactionRegistry, Inner: sink}
}

func (m *SessionManager) registerSensitiveOutputs(result adapter.Result, step *workflow.StepNode) {
	if m.RedactionRegistry == nil {
		return
	}
	for outName, outVal := range result.Outputs {
		if f, ok := step.OutputSchema[outName]; ok && f.Sensitive {
			rendered, err := workflow.RenderOutputValue(outVal)
			if err != nil {
				continue
			}
			m.RedactionRegistry.Register(rendered)
		}
	}
}

func (m *SessionManager) handleCrash(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink, sess *Session, execErr error) (adapter.Result, error) {
	slog.Warn("adapter session crashed", "session", sess.Name, "adapter", sess.Adapter, "error", execErr)

	// The effective crash policy is the step's (the compiler resolves it as
	// step-overrides-adapter), falling back to the session's adapter-level policy.
	// Without this, an on_crash declared only on the step would be ignored at
	// runtime (sess.OnCrash is captured from the adapter block at open time).
	onCrash := sess.OnCrash
	if step != nil && step.OnCrash != "" {
		onCrash = normalizeOnCrash(step.OnCrash)
	}

	switch onCrash {
	case OnCrashRespawn:
		sink.Adapter("session.respawned", map[string]any{
			"session": sess.Name,
			"adapter": sess.Adapter,
			"error":   execErr.Error(),
		})
		if respawnErr := m.respawn(ctx, sess); respawnErr != nil {
			return m.failResult(sink, sess, fmt.Errorf("respawn after crash failed: %w (original crash: %w)", respawnErr, execErr))
		}
		retrySink := sink
		if sess.mergeBuf != nil {
			retrySink = sess.mergeBuf
		}
		result, retryErr := sess.handle.Execute(ctx, name, step, retrySink)
		if retryErr == nil {
			m.registerSensitiveOutputs(result, step)
			return result, nil
		}
		return m.failResult(sink, sess, retryErr)
	case OnCrashAbortRun:
		sink.Adapter("session.crash", map[string]any{
			"session": sess.Name,
			"adapter": sess.Adapter,
			"policy":  onCrash,
			"error":   execErr.Error(),
		})
		return adapter.Result{Outcome: "failure"}, &FatalRunError{Err: fmt.Errorf("session %q crashed and on_crash=abort_run", name)}
	default:
		return m.failResult(sink, sess, execErr)
	}
}

// bindVerifiedAndLookup promotes a verified-only adapter to a bound session.
// It serializes concurrent attempts so only one caller performs the bind and
// the rest wait and then use the bound session. A binding error is wrapped
// with the adapter name, step name, and working directory so failures at first
// use are actionable.
func (m *SessionManager) bindVerifiedAndLookup(ctx context.Context, name string, step *workflow.StepNode) (*Session, error) {
	m.bindMu.Lock()
	defer m.bindMu.Unlock()

	// Another goroutine may have bound the session while we were waiting.
	if sess, err := m.lookup(name); err == nil {
		return sess, nil
	}

	m.mu.Lock()
	rec := m.verified[name]
	m.mu.Unlock()
	if rec == nil {
		return nil, ErrUnknownSession
	}

	stepName := ""
	if step != nil {
		stepName = step.Name
	}
	if bindErr := m.bindVerifiedRecord(ctx, rec, stepName); bindErr != nil {
		return nil, fmt.Errorf("bind adapter %q for step %q in working directory %q: %w", rec.name, stepName, rec.workingDir, bindErr)
	}

	m.mu.Lock()
	delete(m.verified, name)
	m.mu.Unlock()

	return m.lookup(name)
}

func (m *SessionManager) Execute(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	sess, err := m.lookup(name)
	if err != nil {
		if !errors.Is(err, ErrUnknownSession) {
			return adapter.Result{Outcome: "failure"}, err
		}

		sess, err = m.bindVerifiedAndLookup(ctx, name, step)
		if err != nil {
			return adapter.Result{Outcome: "failure"}, err
		}
	}

	sink = m.wrapSink(sink)

	// WS15: heartbeat-stall detection. If no heartbeat has been received for
	// longer than the stall threshold while the log stream is still alive,
	// treat the session as crashed before attempting the step. If the log stream
	// ended early (contract violation) watchLogStream has already disarmed the
	// detector, so we never declare a crash on a heartbeat the adapter was not
	// sending.
	if sess.logStreamAlive.Load() && sess.hbMonitor.Stalled(m.heartbeatStallThreshold()) {
		return m.handleCrash(ctx, name, step, sink, sess, fmt.Errorf("heartbeat stall (>%s)", m.heartbeatStallThreshold()))
	}

	m.setStepPolicy(sess, step)
	m.bindCurrentSink(sess, sink)
	defer m.unbindCurrentSink(sess)

	execSink := m.execSinkForSession(sess, sink)
	permSink := newPermissionInterceptSink(execSink, sess)

	result, execErr := sess.handle.Execute(ctx, name, step, permSink)

	m.maybeOverrideOutcome(permSink, &result)

	if execErr == nil {
		m.registerSensitiveOutputs(result, step)
		return result, nil
	}

	// An explicit Close/Shutdown (closing flag) or a host-canceled context
	// (run timeout, user abort) both cause the gRPC stream to produce
	// EOF/broken-pipe errors. Check this before the string heuristic so
	// neither case is misclassified as a crash.
	if sess.closing.Load() || ctx.Err() != nil {
		slog.Debug("adapter stream closed (expected)", "session", sess.Name, "adapter", sess.Adapter)
		return result, execErr
	}

	if !isLikelySessionCrash(sess, execErr) {
		return result, execErr
	}

	return m.handleCrash(ctx, name, step, sink, sess, execErr)
}

// setStepPolicy builds the CombinedPolicy for this step and wires it into the
// session's PermissionState.
func (m *SessionManager) setStepPolicy(sess *Session, step *workflow.StepNode) {
	var envPolicy *workflow.ResolvedPolicy
	if m.graph != nil && step != nil && step.AdapterRef != "" {
		adapterNode := m.graph.Adapters[step.AdapterRef]
		if adapterNode != nil {
			var envKey string
			if step.Environment != "" {
				envKey = step.Environment
			} else if adapterNode.Environment != "" {
				envKey = adapterNode.Environment
			}
			if envKey != "" && m.graph.ResolvedPolicies != nil {
				policyKey := step.AdapterRef + ":" + envKey
				envPolicy = m.graph.ResolvedPolicies[policyKey]
			}
		}
	}
	policy := NewCombinedPolicy(sess.Adapter, step.AllowTools, envPolicy)
	if sess.PermissionState != nil {
		sess.PermissionState.SetPolicy(policy)
	}
}

func (m *SessionManager) bindCurrentSink(sess *Session, sink adapter.EventSink) {
	sess.currentSinkMu.Lock()
	sess.currentSink = sink
	sess.currentSinkMu.Unlock()
}

func (m *SessionManager) unbindCurrentSink(sess *Session) {
	if sess.mergeBuf != nil {
		sess.mergeBuf.Flush()
	}
	sess.currentSinkMu.Lock()
	sess.currentSink = nil
	sess.currentSinkMu.Unlock()
}

func (m *SessionManager) execSinkForSession(sess *Session, sink adapter.EventSink) adapter.EventSink {
	if sess.mergeBuf != nil {
		return sess.mergeBuf
	}
	return sink
}

func newPermissionInterceptSink(inner adapter.EventSink, sess *Session) *permissionInterceptSink {
	return &permissionInterceptSink{
		inner:     inner,
		permState: sess.PermissionState,
		session:   sess,
	}
}

func (m *SessionManager) maybeOverrideOutcome(permSink *permissionInterceptSink, result *adapter.Result) {
	if permSink != nil && permSink.anyDenied && result.Outcome == "success" {
		result.Outcome = "needs_review"
	}
}

// HasCapability reports whether the session identified by name has capName in
// its cached capabilities slice. Returns false if the session is unknown or
// has no capabilities cached. Thread-safe.
func (m *SessionManager) HasCapability(name, capName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[name]
	if ok {
		for _, c := range sess.Capabilities {
			if c == capName {
				return true
			}
		}
		return false
	}
	rec, ok := m.verified[name]
	if !ok {
		return false
	}
	for _, c := range rec.capabilities {
		if c == capName {
			return true
		}
	}
	return false
}

// AdapterHandle returns the underlying adapter handle for the named session.
// It is used by conformance tests to inspect handle capabilities without
// spawning a throwaway probe process. Returns (nil, false) if the session is
// not found. Thread-safe.
func (m *SessionManager) AdapterHandle(name string) (Handle, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[name]
	if !ok {
		return nil, false
	}
	return sess.handle, true
}

func (m *SessionManager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for name, sess := range m.sessions {
		sessions = append(sessions, sess)
		delete(m.sessions, name)
	}
	for name := range m.verified {
		delete(m.verified, name)
	}
	m.mu.Unlock()

	var errs []error
	for _, sess := range sessions {
		sess.closing.Store(true)
		sess.logStreamAlive.Store(false)
		if sess.PermissionState != nil {
			sess.PermissionState.Stop()
		}
		sess.logMu.Lock()
		cancelLog := sess.cancelLog
		sess.logMu.Unlock()
		if cancelLog != nil {
			cancelLog()
		}
		if sess.mergeBuf != nil {
			sess.mergeBuf.Close()
		}
		if sess.SandboxCleanup != nil {
			sess.SandboxCleanup()
		}
		if err := sess.handle.CloseSession(ctx, sess.Name); err != nil {
			errs = append(errs, err)
		}
		sess.handle.Kill()
	}
	if err := m.loader.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (m *SessionManager) lookup(name string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownSession, name)
	}
	return sess, nil
}

func (m *SessionManager) respawn(ctx context.Context, sess *Session) error {
	sess.handle.Kill()
	if sess.SandboxCleanup != nil {
		sess.SandboxCleanup()
	}
	customizer, cleanup, sandboxErr := m.buildCommandCustomizer(sess.Name, sess.WorkingDir)
	if sandboxErr != nil {
		return fmt.Errorf("session %q respawn: %w", sess.Name, sandboxErr)
	}

	plug, err := m.resolveAdapterForRespawn(ctx, sess, customizer)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return err
	}
	if err := plug.OpenSession(ctx, sess.Name, sess.Config, sess.Secrets); err != nil {
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return err
	}
	sess.handle = plug
	sess.SandboxCleanup = cleanup
	sess.respawned = true

	m.restartPermissionStream(ctx, sess, plug)
	m.restartLogStream(ctx, sess, plug)

	return nil
}

func (m *SessionManager) resolveAdapterForRespawn(ctx context.Context, sess *Session, customizer func(string, *exec.Cmd)) (Handle, error) {
	// Remote-mode dispatch: if the adapter is bound to a remote environment,
	// wait for the adapter to phone home via the shim (respawn = reconnect).
	if m.remoteShim != nil && m.isRemoteAdapter(sess.Name) {
		// Exclude the just-crashed handle so we wait for the replacement
		// connection rather than the dead session still in the shim's map.
		return m.remoteShim.WaitForFreshHandle(ctx, sess.Adapter, sess.handle)
	}

	if dl, ok := m.loader.(*DefaultLoader); ok {
		runnerFunc, containerErr := adapter.BuildContainerRunner(m.graph, m.lockfile, sess.Name)
		if containerErr != nil {
			return nil, containerErr
		}
		if runnerFunc != nil {
			return dl.ResolveWithRunnerFunc(ctx, sess.Adapter, runnerFunc)
		}
		return dl.ResolveWithCustomizer(ctx, sess.Adapter, customizer)
	}
	return m.loader.Resolve(ctx, sess.Adapter)
}

func (m *SessionManager) restartPermissionStream(ctx context.Context, sess *Session, plug Handle) {
	if sess.PermissionState == nil {
		return
	}
	sess.PermissionState.Stop()
	sess.PermissionState = NewPermissionState(sess.Name, m.Audit)
	if streamer, ok := plug.(PermissionStreamer); ok {
		cancel, err := streamer.StartPermissionStream(ctx, sess.Name, sess.PermissionState.Requests())
		if err != nil {
			slog.Warn("adapter permission stream restart failed after respawn", "session", sess.Name, "err", err)
		} else {
			sess.PermissionState.SetStreamCancel(cancel)
		}
	}
}

// restartLogStream cancels the old log stream, closes the old merge buffer, and
// starts a new one for the given handle.
func (m *SessionManager) restartLogStream(ctx context.Context, sess *Session, plug Handle) {
	sess.logMu.Lock()
	oldCancel := sess.cancelLog
	oldDone := sess.logDone
	sess.cancelLog = nil
	sess.logDone = nil
	sess.logHostCancel = nil
	sess.logMu.Unlock()

	if oldCancel != nil {
		// The wrapped cancel closes the stream's host-cancel channel *before*
		// invoking the adapter cancel, so the old watcher can deterministically
		// identify this as host-initiated teardown.
		oldCancel()
	}
	if oldDone != nil {
		// Wait for the previous watcher to finish before reusing the merge buffer
		// and heartbeat state, avoiding races and goroutine leaks. Bound the wait
		// so a broken adapter that never closes its done channel cannot wedge the
		// respawn path and prevent the step retry from starting.
		drainTimeout := m.respawnLogStreamDrainTimeout()
		select {
		case <-oldDone:
		case <-ctx.Done():
		case <-time.After(drainTimeout):
			slog.Warn("adapter log stream did not drain after cancel; continuing respawn to avoid wedge",
				"session", sess.Name, "adapter", sess.Adapter, "drain_timeout", drainTimeout)
		}
	}
	if sess.mergeBuf != nil {
		sess.mergeBuf.Close()
	}

	if starter, ok := plug.(LogStreamStarter); ok {
		m.beginLogStream(ctx, sess, starter)
	} else {
		sess.resetLogStreamState()
		sess.mergeBuf = nil
	}
}

// resetLogStreamState clears the log-stream handles and marks the stream dead.
func (s *Session) resetLogStreamState() {
	s.logMu.Lock()
	s.cancelLog = nil
	s.logDone = nil
	s.logHostCancel = nil
	s.logMu.Unlock()
	s.logStreamAlive.Store(false)
}

func (m *SessionManager) failResult(sink adapter.EventSink, sess *Session, err error) (adapter.Result, error) {
	sink.Adapter("session.crash", map[string]any{
		"session": sess.Name,
		"adapter": sess.Adapter,
		"policy":  sess.OnCrash,
		"error":   err.Error(),
	})
	return adapter.Result{Outcome: "failure"}, fmt.Errorf("session %q crashed: %w", sess.Name, err)
}

func normalizeOnCrash(v string) string {
	switch strings.TrimSpace(v) {
	case OnCrashRespawn:
		return OnCrashRespawn
	case OnCrashAbortRun:
		return OnCrashAbortRun
	default:
		return OnCrashFail
	}
}

func isLikelySessionCrash(sess *Session, err error) bool {
	if err == nil {
		return false
	}
	if sess.closing.Load() {
		// Expected: caller initiated close; any subsequent EOF /
		// transport-closing / broken-pipe is the normal teardown.
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection") ||
		strings.Contains(msg, "transport is closing") ||
		strings.Contains(msg, "unavailable") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "terminated")
}

// sessionLogAdapterSink routes session-level log lines to the active step's
// EventSink when a step is executing, or to structured logs otherwise.
type sessionLogAdapterSink struct {
	sess *Session
}

func (s *sessionLogAdapterSink) Log(stream string, chunk []byte) {
	s.sess.currentSinkMu.Lock()
	sink := s.sess.currentSink
	s.sess.currentSinkMu.Unlock()
	if sink != nil {
		sink.Log(stream, chunk)
	} else {
		slog.Info("adapter log", "session", s.sess.Name, "stream", stream, "line", string(chunk))
	}
}

func (s *sessionLogAdapterSink) Adapter(kind string, data any) {
	s.sess.currentSinkMu.Lock()
	sink := s.sess.currentSink
	s.sess.currentSinkMu.Unlock()
	if sink != nil {
		sink.Adapter(kind, data)
	} else {
		slog.Info("adapter event", "session", s.sess.Name, "kind", kind, "data", data)
	}
}

func cloneOriginRefs(m map[string]secrets.OriginRef) map[string]secrets.OriginRef {
	if m == nil {
		return nil
	}
	out := make(map[string]secrets.OriginRef, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// validateConfigAgainstSchema performs a runtime check of the resolved config
// against the adapter's declared manifest schema. It only enforces presence of
// required non-sensitive fields; sensitive required values are resolved
// separately and validated by validateRequiredSecrets. The compiler already
// rejects unknown keys and incompatible types at workflow compile time, and the
// manifest schema is intentionally a subset of the HCL type system.
func validateConfigAgainstSchema(config map[string]string, schema map[string]workflow.ConfigField) error {
	if len(schema) == 0 {
		return nil
	}
	var missing []string
	for name, field := range schema {
		if !field.Required || field.Sensitive {
			continue
		}
		val, ok := config[name]
		if !ok || strings.TrimSpace(val) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("missing required config field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// validateRequiredSecrets checks that every config field marked Sensitive and
// Required has a non-empty resolved secret value. Non-sensitive required values
// are covered by validateConfigAgainstSchema; this path exists because secrets
// are resolved separately from static config.
func validateRequiredSecrets(secrets map[string]string, schema map[string]workflow.ConfigField) error {
	if len(schema) == 0 {
		return nil
	}
	var missing []string
	for name, field := range schema {
		if !(field.Required && field.Sensitive) {
			continue
		}
		val, ok := secrets[name]
		if !ok || strings.TrimSpace(val) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("missing required secret(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// SessionSnapshot records the complete host-visible state of a session at a
// point in time, sufficient to restore it on the same or a restarted host.
type SessionSnapshot struct {
	AdapterState     []byte                       `json:"-"` // opaque to host (from adapter via Snapshot RPC)
	SchemaVersion    uint32                       `json:"schema_version"`
	PermissionState  []byte                       `json:"permission_state"`   // from PermissionState.MarshalState() (WS16)
	SecretOriginRefs map[string]secrets.OriginRef `json:"secret_origin_refs"` // from sessions config; values not included
	AdapterDigest    digest.Digest                `json:"adapter_digest"`     // adapter manifest digest at snapshot time
	HostArch         string                       `json:"host_arch"`          // GOOS/GOARCH at snapshot
	WorkingDir       string                       `json:"working_dir"`        // resolved environment working_directory at snapshot
	CreatedAt        time.Time                    `json:"created_at"`
}

const currentSnapshotSchemaVersion uint32 = 1

// Snapshot pauses the session, captures adapter state, permission state, and
// host metadata, and returns a SessionSnapshot. The session remains paused after
// the call; the caller must Resume to continue execution.
func (s *Session) Snapshot(ctx context.Context) (*SessionSnapshot, error) {
	if err := s.Pause(ctx); err != nil {
		return nil, fmt.Errorf("pause before snapshot: %w", err)
	}

	resp, err := s.handle.Snapshot(ctx, s.Name)
	if err != nil {
		return nil, fmt.Errorf("adapter snapshot: %w", err)
	}

	var permState []byte
	if s.PermissionState != nil {
		permState, err = s.PermissionState.MarshalState()
		if err != nil {
			return nil, fmt.Errorf("marshal permission state: %w", err)
		}
	}

	return &SessionSnapshot{
		AdapterState:     resp.State,
		SchemaVersion:    currentSnapshotSchemaVersion,
		PermissionState:  permState,
		SecretOriginRefs: cloneOriginRefs(s.SecretOriginRefs),
		AdapterDigest:    s.AdapterDigest,
		HostArch:         runtime.GOOS + "/" + runtime.GOARCH,
		WorkingDir:       s.WorkingDir,
		CreatedAt:        time.Now(),
	}, nil
}

// SnapshotAll iterates over every open session and calls Snapshot on each.
// Errors from individual sessions are collected and returned joined.
func (m *SessionManager) SnapshotAll(ctx context.Context) (map[string]*SessionSnapshot, error) {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	out := make(map[string]*SessionSnapshot, len(sessions))
	var errs []error
	for _, s := range sessions {
		snap, err := s.Snapshot(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("session %q: %w", s.Name, err))
			continue
		}
		out[s.Name] = snap
	}
	return out, errors.Join(errs...)
}

// openAndRestoreAdapter resolves the adapter handle, opens a fresh session, and replays
// the saved adapter state. On error it kills the plug and runs the cleanup func.
func (m *SessionManager) openAndRestoreAdapter(ctx context.Context, name, adapterName string, config, resolvedSecrets map[string]string, snap *SessionSnapshot) (Handle, func(), error) {
	customizer, cleanup, sandboxErr := m.buildCommandCustomizer(name, snap.WorkingDir)
	if sandboxErr != nil {
		return nil, nil, fmt.Errorf("session %q: %w", name, sandboxErr)
	}

	plug, err := m.resolveAdapterHandle(ctx, name, adapterName, customizer)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, err
	}

	if err := plug.OpenSession(ctx, name, config, resolvedSecrets); err != nil {
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, fmt.Errorf("open restored session: %w", err)
	}

	if err := plug.Restore(ctx, name, snap.AdapterState, snap.SchemaVersion); err != nil {
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, fmt.Errorf("adapter restore: %w", err)
	}

	return plug, cleanup, nil
}

func buildRestoredSession(name, adapterName, onCrash string, config, resolvedSecrets map[string]string, originRefs map[string]secrets.OriginRef, caps []string, plug Handle, cleanup func(), permState *permissionState, workingDir string) *Session {
	return &Session{
		Name:             name,
		Adapter:          adapterName,
		Config:           cloneConfig(config),
		Secrets:          resolvedSecrets,
		SecretOriginRefs: cloneOriginRefs(originRefs),
		OnCrash:          normalizeOnCrash(onCrash),
		Capabilities:     caps,
		handle:           plug,
		SandboxCleanup:   cleanup,
		PermissionState:  permState,
		WorkingDir:       workingDir,
	}
}

func (m *SessionManager) registerRestoredSession(ctx context.Context, name string, plug Handle, cleanup func(), sess *Session) error {
	m.mu.Lock()
	if _, exists := m.sessions[name]; exists {
		m.mu.Unlock()
		_ = plug.CloseSession(ctx, name)
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	m.sessions[name] = sess
	m.mu.Unlock()
	return nil
}

// Restore validates a snapshot against cross-host compatibility rules,
// re-resolves secrets, opens a fresh adapter session, replays adapter state,
// restores permission state, and registers the reconstructed session.
func (m *SessionManager) Restore(ctx context.Context, name, adapterName, onCrash string, config map[string]string, envNode *workflow.EnvironmentNode, snap *SessionSnapshot) (*Session, error) {
	if err := m.validateSnapshotCompatibility(name, snap); err != nil {
		return nil, err
	}

	resolvedSecrets, err := m.resolveSnapshotSecrets(ctx, envNode, snap.SecretOriginRefs)
	if err != nil {
		return nil, err
	}

	plug, cleanup, err := m.openAndRestoreAdapter(ctx, name, adapterName, config, resolvedSecrets, snap)
	if err != nil {
		return nil, err
	}

	var caps []string
	if info, infoErr := plug.Info(ctx); infoErr == nil {
		caps = append([]string(nil), info.Capabilities...)
	}

	permState, err := m.restorePermissionState(name, snap.PermissionState)
	if err != nil {
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}

	sess := buildRestoredSession(name, adapterName, onCrash, config, resolvedSecrets, snap.SecretOriginRefs, caps, plug, cleanup, permState, snap.WorkingDir)
	if err := m.registerRestoredSession(ctx, name, plug, cleanup, sess); err != nil {
		return nil, err
	}

	if permState != nil {
		m.startPermissionStream(ctx, sess, plug)
	}
	m.startLogStream(ctx, sess, plug)
	return sess, nil
}

func (m *SessionManager) restorePermissionState(name string, blob []byte) (*permissionState, error) {
	if len(blob) == 0 {
		return nil, nil
	}
	permState := NewPermissionState(name, m.Audit)
	if err := permState.RestoreState(blob, nil, m.Audit); err != nil {
		return nil, fmt.Errorf("restore permission state: %w", err)
	}
	return permState, nil
}

func (m *SessionManager) validateSnapshotCompatibility(adapterKey string, snap *SessionSnapshot) error {
	if snap.SchemaVersion != currentSnapshotSchemaVersion {
		return fmt.Errorf("snapshot schema version %d is not supported (expected %d)", snap.SchemaVersion, currentSnapshotSchemaVersion)
	}

	wantArch := runtime.GOOS + "/" + runtime.GOARCH
	if snap.HostArch != "" && snap.HostArch != wantArch {
		return fmt.Errorf("snapshot host arch %q does not match current host %q", snap.HostArch, wantArch)
	}

	if m.lockfile == nil {
		return nil // nothing to compare against
	}

	var currentDigest string
	for i := range m.lockfile.Adapters {
		a := &m.lockfile.Adapters[i]
		if a.Type+"."+a.Name == adapterKey {
			currentDigest = a.ResolvedDigest
			break
		}
	}
	if currentDigest == "" {
		return nil // adapter not in lockfile; skip digest check
	}
	if snap.AdapterDigest != "" && snap.AdapterDigest.String() != currentDigest {
		return fmt.Errorf("snapshot was taken against adapter %q@%s; current lockfile pins %q@%s. Resume requires the same adapter version", adapterKey, snap.AdapterDigest, adapterKey, currentDigest)
	}
	return nil
}

func (m *SessionManager) resolveSnapshotSecrets(ctx context.Context, envNode *workflow.EnvironmentNode, originRefs map[string]secrets.OriginRef) (map[string]string, error) {
	if len(originRefs) == 0 {
		return nil, nil
	}

	stack, err := secrets.StackFromEnvironment(envNode)
	if err != nil {
		return nil, fmt.Errorf("build secret stack for restore: %w", err)
	}

	resolved := make(map[string]string, len(originRefs))
	for name, ref := range originRefs {
		if ref.Kind == "literal" {
			resolved[name] = ref.Ref
			if m.RedactionRegistry != nil {
				m.RedactionRegistry.Register(ref.Ref)
			}
			continue
		}
		val, err := stack.Resolve(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("missing secret %q: %w", name, err)
		}
		resolved[name] = val
		if m.RedactionRegistry != nil {
			m.RedactionRegistry.Register(val)
		}
	}
	return resolved, nil
}
