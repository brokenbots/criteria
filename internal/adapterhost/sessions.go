package adapterhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/internal/log"
	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
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

	mu       sync.Mutex
	sessions map[string]*Session
}

// RemoteShim is the interface the session manager uses to wait for remote
// adapter connections.
type RemoteShim interface {
	WaitForHandle(ctx context.Context, adapterType string) (Handle, error)
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
	// AdapterDigest is the lockfile digest at the time the session was opened.
	AdapterDigest digest.Digest

	// WS15: session-level log stream lifecycle and heartbeat tracking.
	cancelLog     func()
	hbMonitor     adapter.HeartbeatMonitor
	currentSink   adapter.EventSink
	currentSinkMu sync.Mutex

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
	}
}

// buildSandboxCustomizer returns a function that applies the sandbox
// configuration to an exec.Cmd, or nil if the adapter is not bound to a
// sandbox environment. The second return value is a cleanup function
// that removes transient resources (e.g. cgroup directories); it may be
// nil. The third return value is an error that is only non-nil when the
// sandbox policy is strict and a required primitive is unavailable; in
// that case the caller must abort the session before Resolve.
func (m *SessionManager) buildSandboxCustomizer(instanceID string) (customizer func(name string, cmd *exec.Cmd), cleanup func(), err error) {
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

	customizer, cleanup = makeSandboxCustomizer(&prep, envNode)
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

// boundEnvironment resolves the environment node bound to the given adapter
// instance (its declared environment, or the workflow default). It returns nil
// when no environment is bound or the graph is unavailable.
func (m *SessionManager) boundEnvironment(instanceID string) *workflow.EnvironmentNode {
	if m.graph == nil {
		return nil
	}
	adapterNode, ok := m.graph.Adapters[instanceID]
	if !ok {
		return nil
	}
	envKey := adapterNode.Environment
	if envKey == "" {
		envKey = m.graph.DefaultEnvironment
	}
	if envKey == "" {
		return nil
	}
	return m.graph.Environments[envKey]
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
func (m *SessionManager) buildCommandCustomizer(instanceID string) (func(string, *exec.Cmd), func(), error) {
	sandboxCust, cleanup, err := m.buildSandboxCustomizer(instanceID)
	if err != nil {
		return nil, nil, err
	}

	var workingDir string
	if env := m.boundEnvironment(instanceID); env != nil {
		workingDir = env.WorkingDirectory
	}
	if workingDir == "" {
		return sandboxCust, cleanup, nil
	}

	customizer := func(name string, cmd *exec.Cmd) {
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
	return m.OpenWithOriginRefs(ctx, name, adapterName, onCrash, config, secrets, nil)
}

func (m *SessionManager) OpenWithOriginRefs(ctx context.Context, name, adapterName, onCrash string, config, secrets map[string]string, originRefs map[string]secrets.OriginRef) error {
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

	customizer, cleanup, sandboxErr := m.buildCommandCustomizer(name)
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

	return m.registerSession(ctx, name, adapterName, onCrash, config, secrets, originRefs, caps, plug, cleanup)
}

func (m *SessionManager) resolveAdapterHandle(ctx context.Context, name, adapterName string, customizer func(string, *exec.Cmd)) (Handle, error) {
	// Remote-mode dispatch: if the adapter is bound to a remote environment,
	// wait for the adapter to phone home via the shim.
	if m.remoteShim != nil && m.isRemoteAdapter(name) {
		return m.remoteShim.WaitForHandle(ctx, adapterName)
	}

	if dl, ok := m.loader.(*DefaultLoader); ok {
		// Container-mode dispatch: if the adapter is bound to a container
		// environment, use a docker/podman runner instead of a local binary.
		runnerFunc, containerErr := adapter.BuildContainerRunner(m.graph, m.lockfile, name)
		if containerErr != nil {
			return nil, containerErr
		}
		if runnerFunc != nil {
			return dl.ResolveWithRunnerFunc(ctx, adapterName, runnerFunc)
		}
		return dl.ResolveWithCustomizer(ctx, adapterName, customizer)
	}
	return m.loader.Resolve(ctx, adapterName)
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

// makeSandboxCustomizer builds the exec.Cmd customizer and cleanup from
// a prepared LinuxPrepared config. It handles both the bubblewrap and
// in-process shim paths.
func makeSandboxCustomizer(prep *sandbox.LinuxPrepared, envNode *workflow.EnvironmentNode) (customizer func(name string, cmd *exec.Cmd), cleanup func()) {
	cleanup = func() { _ = prep.Cleanup() }
	if bwrapCmd := sandbox.MaybeUseBubblewrap(prep, envNode); bwrapCmd != nil {
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

func (m *SessionManager) registerSession(ctx context.Context, name, adapterName, onCrash string, config, secrets map[string]string, originRefs map[string]secrets.OriginRef, caps []string, plug Handle, cleanup func()) error {
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
	}
	if m.lockfile != nil {
		for i := range m.lockfile.Adapters {
			a := &m.lockfile.Adapters[i]
			if a.Type == adapterName {
				sess.AdapterDigest = digest.Digest(a.ResolvedDigest)
				break
			}
		}
	}
	m.sessions[name] = sess

	m.startPermissionStream(ctx, sess, plug)
	m.startLogStream(ctx, sess, plug)
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
		logAdapterSink := &sessionLogAdapterSink{sess: sess}
		redactedLogSink := m.wrapSink(logAdapterSink)
		sess.mergeBuf = log.NewMergeBuffer(redactedLogSink, 500*time.Millisecond)
		logSink := &logForwardSink{
			sink: sess.mergeBuf,
			onHeartbeat: func() {
				sess.hbMonitor.Record()
			},
		}
		cancel, err := starter.StartLogStream(ctx, sess.Name, logSink)
		if err != nil {
			slog.Warn("adapter log stream start failed", "session", sess.Name, "err", err)
		} else {
			sess.cancelLog = cancel
			sess.hbMonitor.Record()
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
	m.mu.Unlock()

	if !exists {
		return nil
	}
	sess.closing.Store(true)
	if sess.PermissionState != nil {
		sess.PermissionState.Stop()
	}
	if sess.cancelLog != nil {
		sess.cancelLog()
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

	switch sess.OnCrash {
	case OnCrashRespawn:
		sink.Adapter("session.respawned", map[string]any{
			"session": sess.Name,
			"adapter": sess.Adapter,
			"error":   execErr.Error(),
		})
		if respawnErr := m.respawn(ctx, sess); respawnErr != nil {
			return m.failResult(sink, sess, execErr)
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
			"policy":  sess.OnCrash,
			"error":   execErr.Error(),
		})
		return adapter.Result{Outcome: "failure"}, &FatalRunError{Err: fmt.Errorf("session %q crashed and on_crash=abort_run", name)}
	default:
		return m.failResult(sink, sess, execErr)
	}
}

func (m *SessionManager) Execute(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	sess, err := m.lookup(name)
	if err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}

	sink = m.wrapSink(sink)

	// WS15: heartbeat-stall detection. If no heartbeat has been received for
	// >90s, treat the session as crashed before attempting the step.
	if sess.cancelLog != nil && sess.hbMonitor.Stalled(90*time.Second) {
		return m.handleCrash(ctx, name, step, sink, sess, errors.New("heartbeat stall (>90s)"))
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
	if !ok {
		return false
	}
	for _, c := range sess.Capabilities {
		if c == capName {
			return true
		}
	}
	return false
}

func (m *SessionManager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for name, sess := range m.sessions {
		sessions = append(sessions, sess)
		delete(m.sessions, name)
	}
	m.mu.Unlock()

	var errs []error
	for _, sess := range sessions {
		sess.closing.Store(true)
		if sess.PermissionState != nil {
			sess.PermissionState.Stop()
		}
		if sess.cancelLog != nil {
			sess.cancelLog()
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
	customizer, cleanup, sandboxErr := m.buildCommandCustomizer(sess.Name)
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
		return m.remoteShim.WaitForHandle(ctx, sess.Adapter)
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
	if sess.cancelLog != nil {
		sess.cancelLog()
	}
	if sess.mergeBuf != nil {
		sess.mergeBuf.Close()
	}
	if starter, ok := plug.(LogStreamStarter); ok {
		logAdapterSink := &sessionLogAdapterSink{sess: sess}
		redactedLogSink := m.wrapSink(logAdapterSink)
		sess.mergeBuf = log.NewMergeBuffer(redactedLogSink, 500*time.Millisecond)
		logSink := &logForwardSink{
			sink: sess.mergeBuf,
			onHeartbeat: func() {
				sess.hbMonitor.Record()
			},
		}
		cancel, err := starter.StartLogStream(ctx, sess.Name, logSink)
		if err != nil {
			slog.Warn("adapter log stream restart failed after respawn", "session", sess.Name, "err", err)
			sess.cancelLog = nil
		} else {
			sess.cancelLog = cancel
			sess.hbMonitor.Record()
		}
	} else {
		sess.cancelLog = nil
		sess.mergeBuf = nil
	}
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

// SessionSnapshot records the complete host-visible state of a session at a
// point in time, sufficient to restore it on the same or a restarted host.
type SessionSnapshot struct {
	AdapterState     []byte                       `json:"-"` // opaque to host (from adapter via Snapshot RPC)
	SchemaVersion    uint32                       `json:"schema_version"`
	PermissionState  []byte                       `json:"permission_state"`   // from PermissionState.MarshalState() (WS16)
	SecretOriginRefs map[string]secrets.OriginRef `json:"secret_origin_refs"` // from sessions config; values not included
	AdapterDigest    digest.Digest                `json:"adapter_digest"`     // adapter manifest digest at snapshot time
	HostArch         string                       `json:"host_arch"`          // GOOS/GOARCH at snapshot
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
	customizer, cleanup, sandboxErr := m.buildCommandCustomizer(name)
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

func buildRestoredSession(name, adapterName, onCrash string, config, resolvedSecrets map[string]string, originRefs map[string]secrets.OriginRef, caps []string, plug Handle, cleanup func(), permState *permissionState) *Session {
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

	sess := buildRestoredSession(name, adapterName, onCrash, config, resolvedSecrets, snap.SecretOriginRefs, caps, plug, cleanup, permState)
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
