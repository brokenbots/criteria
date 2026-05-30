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
	"github.com/brokenbots/criteria/workflow"
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

// PermissionState holds runtime permission audit data for a session.
// Populated by WS16; present as a stub field in this workstream.
type PermissionState struct{}

type Session struct {
	Name            string
	Adapter         string
	Config          map[string]string
	OnCrash         string
	Capabilities    []string // cached from plug.Info() at Open time
	PermissionState PermissionState
	handle          Handle
	respawned       bool
	closing         atomic.Bool
}

func NewSessionManager(loader Loader) *SessionManager {
	return &SessionManager{
		loader:   loader,
		sessions: map[string]*Session{},
	}
}

// buildSandboxCustomizer returns a function that applies the sandbox
// configuration to an exec.Cmd, or nil if the adapter is not bound to a
// sandbox environment. The second return value is an error that is only
// non-nil when the sandbox policy is strict and a required primitive is
// unavailable; in that case the caller must abort the session before
// Resolve.
func (m *SessionManager) buildSandboxCustomizer(instanceID string) (func(name string, cmd *exec.Cmd), error) {
	if m.graph == nil {
		return nil, nil
	}
	adapterNode, ok := m.graph.Adapters[instanceID]
	if !ok {
		return nil, nil
	}
	envKey := adapterNode.Environment
	if envKey == "" {
		envKey = m.graph.DefaultEnvironment
	}
	if envKey == "" {
		return nil, nil
	}
	envNode, ok := m.graph.Environments[envKey]
	if !ok {
		return nil, nil
	}
	if envNode.Type != "sandbox" {
		return nil, nil
	}

	cacheKey := instanceID + ":" + envKey
	rp, ok := m.graph.ResolvedPolicies[cacheKey]
	if !ok {
		return nil, nil
	}

	caps := sandbox.Probe()
	if missing := caps.Missing(); len(missing) > 0 {
		slog.Info("sandbox primitives missing", "missing", missing, "instance", instanceID)
	}

	ctx := sandbox.PrepareContext{
		Policy: rp,
		Caps:   caps,
	}
	prep, err := sandbox.Handler{}.Prepare(ctx)
	if err != nil {
		if rp.PolicyMode == "strict" {
			return nil, fmt.Errorf("sandbox strict mode: %w", err)
		}
		slog.Info("sandbox permissive degradation", "instance", instanceID, "error", err)
		return nil, nil
	}

	return func(_ string, cmd *exec.Cmd) {
		_ = (&prep).ApplyToCmd(cmd, os.Args[0])
	}, nil
}

func (m *SessionManager) Open(ctx context.Context, name, adapterName, onCrash string, config map[string]string) error {
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

	customizer, sandboxErr := m.buildSandboxCustomizer(name)
	if dl, ok := m.loader.(*DefaultLoader); ok {
		dl.SetCommandCustomizer(customizer)
		defer dl.SetCommandCustomizer(nil)
	}
	if sandboxErr != nil {
		return fmt.Errorf("session %q: %w", name, sandboxErr)
	}

	plug, err := m.loader.Resolve(ctx, adapterName)
	if err != nil {
		return err
	}

	// Cache capabilities so HasCapability can be called without a separate Info RPC.
	// On error, capabilities default to nil — the runtime gate rejects parallel use.
	var caps []string
	if info, infoErr := plug.Info(ctx); infoErr == nil {
		caps = append([]string(nil), info.Capabilities...)
	}

	if err := plug.OpenSession(ctx, name, config); err != nil {
		plug.Kill()
		return err
	}

	return m.registerSession(ctx, name, adapterName, onCrash, config, caps, plug)
}

func (m *SessionManager) registerSession(ctx context.Context, name, adapterName, onCrash string, config map[string]string, caps []string, plug Handle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[name]; exists {
		_ = plug.CloseSession(ctx, name)
		plug.Kill()
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	m.sessions[name] = &Session{
		Name:         name,
		Adapter:      adapterName,
		Config:       cloneConfig(config),
		OnCrash:      normalizeOnCrash(onCrash),
		Capabilities: caps,
		handle:       plug,
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
	err := sess.handle.CloseSession(ctx, name)
	sess.handle.Kill()
	return err
}

func (m *SessionManager) Execute(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	sess, err := m.lookup(name)
	if err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}

	result, execErr := sess.handle.Execute(ctx, name, step, sink)
	if execErr == nil {
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
	customizer, sandboxErr := m.buildSandboxCustomizer(sess.Name)
	if dl, ok := m.loader.(*DefaultLoader); ok {
		dl.SetCommandCustomizer(customizer)
		defer dl.SetCommandCustomizer(nil)
	}
	if sandboxErr != nil {
		return fmt.Errorf("session %q respawn: %w", sess.Name, sandboxErr)
	}
	plug, err := m.loader.Resolve(ctx, sess.Adapter)
	if err != nil {
		return err
	}
	if err := plug.OpenSession(ctx, sess.Name, sess.Config); err != nil {
		plug.Kill()
		return err
	}
	sess.handle = plug
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
