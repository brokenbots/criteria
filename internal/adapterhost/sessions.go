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
	"time"

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

type Session struct {
	Name            string
	Adapter         string
	Config          map[string]string
	Secrets         map[string]string // resolved secret values (WS13)
	OnCrash         string
	Capabilities    []string // cached from plug.Info() at Open time
	PermissionState *permissionState
	handle          Handle
	respawned       bool
	closing         atomic.Bool
	SandboxCleanup  func() // removes transient cgroup dirs, etc.

	// WS15: session-level log stream lifecycle and heartbeat tracking.
	cancelLog     func()
	lastHeartbeat atomic.Int64 // Unix nanoseconds
	currentSink   adapter.EventSink
	currentSinkMu sync.Mutex

	// WS15: MergeBuffer interleaves log and adapter events by timestamp.
	mergeBuf *log.MergeBuffer
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
	sess := &Session{
		Name:           name,
		Adapter:        adapterName,
		Config:         cloneConfig(config),
		Secrets:        cloneConfig(secrets),
		OnCrash:        normalizeOnCrash(onCrash),
		Capabilities:   caps,
		handle:         plug,
		SandboxCleanup: cleanup,
	}
	m.sessions[name] = sess

	// WS16: start the session-scoped Permissions stream if the adapter supports it.
	sess.PermissionState = NewPermissionState(name, m.Audit)
	if streamer, ok := plug.(PermissionStreamer); ok {
		cancel, err := streamer.StartPermissionStream(ctx, name, sess.PermissionState.Requests())
		if err != nil {
			slog.Warn("adapter permission stream start failed", "session", name, "err", err)
		} else {
			sess.PermissionState.SetStreamCancel(cancel)
		}
	}

	// WS15: start the dedicated per-session Log stream if the adapter supports it.
	if starter, ok := plug.(LogStreamStarter); ok {
		logAdapterSink := &sessionLogAdapterSink{sess: sess}
		// Wrap with redaction so idle-period log lines are also scrubbed.
		redactedLogSink := m.wrapSink(logAdapterSink)
		sess.mergeBuf = log.NewMergeBuffer(redactedLogSink, 500*time.Millisecond)
		logSink := &logForwardSink{
			sink: sess.mergeBuf,
			onHeartbeat: func() {
				sess.lastHeartbeat.Store(time.Now().UnixNano())
			},
		}
		cancel, err := starter.StartLogStream(ctx, name, logSink)
		if err != nil {
			slog.Warn("adapter log stream start failed", "session", name, "err", err)
		} else {
			sess.cancelLog = cancel
			sess.lastHeartbeat.Store(time.Now().UnixNano())
		}
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
			m.RedactionRegistry.Register(outVal)
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
	lastHB := time.Unix(0, sess.lastHeartbeat.Load())
	if sess.cancelLog != nil && time.Since(lastHB) > 90*time.Second {
		return m.handleCrash(ctx, name, step, sink, sess, errors.New("heartbeat stall (>90s)"))
	}

	// WS16: build combined policy for this step and wire it into the session's
	// PermissionState.  EnvPolicy resolution from the graph is a future hook;
	// for now only allow_tools is enforced.
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

	// Route session-level log lines to this step's sink while Execute is in flight.
	sess.currentSinkMu.Lock()
	sess.currentSink = sink
	sess.currentSinkMu.Unlock()
	defer func() {
		if sess.mergeBuf != nil {
			sess.mergeBuf.Flush()
		}
		sess.currentSinkMu.Lock()
		sess.currentSink = nil
		sess.currentSinkMu.Unlock()
	}()

	// Pass mergeBuf as the Execute sink so that adapter events also flow through
	// the timestamp-ordered merge pipeline together with log stream events.
	execSink := sink
	if sess.mergeBuf != nil {
		execSink = sess.mergeBuf
	}

	// WS16: wrap the sink so permission.request events are intercepted and
	// evaluated against the step's policy. Denied requests are tracked so the
	// outcome can be overridden after Execute.
	permSink := &permissionInterceptSink{
		inner:     execSink,
		permState: sess.PermissionState,
		session:   sess,
	}

	result, execErr := sess.handle.Execute(ctx, name, step, permSink)

	// WS16: if any permission was denied, override a success outcome to
	// signal that review is needed.
	if permSink.anyDenied && result.Outcome == "success" {
		result.Outcome = "needs_review"
	}

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

	// WS16: restart the permission stream for the new handle.
	if sess.PermissionState != nil {
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

	// WS15: restart the log stream for the new handle and reset heartbeat tracking.
	m.restartLogStream(ctx, sess, plug)

	return nil
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
				sess.lastHeartbeat.Store(time.Now().UnixNano())
			},
		}
		cancel, err := starter.StartLogStream(ctx, sess.Name, logSink)
		if err != nil {
			slog.Warn("adapter log stream restart failed after respawn", "session", sess.Name, "err", err)
			sess.cancelLog = nil
		} else {
			sess.cancelLog = cancel
			sess.lastHeartbeat.Store(time.Now().UnixNano())
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
