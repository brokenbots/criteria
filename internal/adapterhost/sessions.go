package adapterhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
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

	mu       sync.Mutex
	sessions map[string]*Session
}

// SetGraph provides the compiled workflow graph so the session manager
// can look up per-adapter environment policies (e.g. sandbox) at open time.
func (m *SessionManager) SetGraph(g *workflow.FSMGraph) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.graph = g
}

// SetLockfile provides the parsed lockfile so the session manager can
// resolve container images for adapters bound to container environments.
func (m *SessionManager) SetLockfile(lf *lockfile.Lockfile) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lockfile = lf
}

// PermissionState holds runtime permission audit data for a session.
// Populated by WS16; present as a stub field in this workstream.
type PermissionState struct{}

type Session struct {
	Name            string
	Adapter         string
	Config          map[string]string
	Secrets         map[string]string // resolved secret values (WS13)
	OnCrash         string
	Capabilities    []string // cached from plug.Info() at Open time
	PermissionState PermissionState
	handle          Handle
	respawned       bool
	closing         atomic.Bool
	SandboxCleanup  func() // removes transient cgroup dirs, etc.
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

func (m *SessionManager) Open(ctx context.Context, name, adapterName, onCrash string, config, secrets map[string]string) error {
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

	customizer, cleanup, sandboxErr := m.buildSandboxCustomizer(name)
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

	return m.registerSession(ctx, name, adapterName, onCrash, config, secrets, caps, plug, cleanup)
}

func (m *SessionManager) resolveAdapterHandle(ctx context.Context, name, adapterName string, customizer func(string, *exec.Cmd)) (Handle, error) {
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

func (m *SessionManager) registerSession(ctx context.Context, name, adapterName, onCrash string, config, secrets map[string]string, caps []string, plug Handle, cleanup func()) error {
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
	m.sessions[name] = &Session{
		Name:           name,
		Adapter:        adapterName,
		Config:         cloneConfig(config),
		Secrets:        cloneConfig(secrets),
		OnCrash:        normalizeOnCrash(onCrash),
		Capabilities:   caps,
		handle:         plug,
		SandboxCleanup: cleanup,
	}
	return nil
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
	if sess.SandboxCleanup != nil {
		sess.SandboxCleanup()
	}
	err := sess.handle.CloseSession(ctx, name)
	sess.handle.Kill()
	return err
}

func (m *SessionManager) wrapSink(sink adapter.EventSink) adapter.EventSink {
	if m.RedactionRegistry == nil {
		return sink
	}
	return &secrets.RedactingEventSink{Registry: m.RedactionRegistry, Inner: sink}
}

func (m *SessionManager) Execute(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	sess, err := m.lookup(name)
	if err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}

	sink = m.wrapSink(sink)

	result, execErr := sess.handle.Execute(ctx, name, step, sink)
	if execErr == nil {
		if m.RedactionRegistry != nil {
			for outName, outVal := range result.Outputs {
				if f, ok := step.OutputSchema[outName]; ok && f.Sensitive {
					m.RedactionRegistry.Register(outVal)
				}
			}
		}
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
		result, retryErr := sess.handle.Execute(ctx, name, step, sink)
		if retryErr == nil {
			if m.RedactionRegistry != nil {
				for outName, outVal := range result.Outputs {
					if f, ok := step.OutputSchema[outName]; ok && f.Sensitive {
						m.RedactionRegistry.Register(outVal)
					}
				}
			}
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
	customizer, cleanup, sandboxErr := m.buildSandboxCustomizer(sess.Name)
	if sandboxErr != nil {
		return fmt.Errorf("session %q respawn: %w", sess.Name, sandboxErr)
	}

	var plug Handle
	var err error
	if dl, ok := m.loader.(*DefaultLoader); ok {
		runnerFunc, containerErr := adapter.BuildContainerRunner(m.graph, m.lockfile, sess.Name)
		if containerErr != nil {
			if cleanup != nil {
				cleanup()
			}
			return containerErr
		}
		if runnerFunc != nil {
			plug, err = dl.ResolveWithRunnerFunc(ctx, sess.Adapter, runnerFunc)
		} else {
			plug, err = dl.ResolveWithCustomizer(ctx, sess.Adapter, customizer)
		}
	} else {
		plug, err = m.loader.Resolve(ctx, sess.Adapter)
	}
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
	return nil
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
