package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/brokenbots/criteria/internal/adapter"
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

	mu       sync.Mutex
	sessions map[string]*Session
}

type Session struct {
	Name      string
	Adapter   string
	Config    map[string]string
	OnCrash   string
	plugin    Plugin
	respawned bool
}

func NewSessionManager(loader Loader) *SessionManager {
	return &SessionManager{
		loader:   loader,
		sessions: map[string]*Session{},
	}
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

	plug, err := m.loader.Resolve(ctx, adapterName)
	if err != nil {
		return err
	}
	if err := plug.OpenSession(ctx, name, config); err != nil {
		plug.Kill()
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[name]; exists {
		plug.CloseSession(ctx, name)
		plug.Kill()
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	m.sessions[name] = &Session{
		Name:    name,
		Adapter: adapterName,
		Config:  cloneConfig(config),
		OnCrash: normalizeOnCrash(onCrash),
		plugin:  plug,
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
	err := sess.plugin.CloseSession(ctx, name)
	sess.plugin.Kill()
	return err
}

func (m *SessionManager) Execute(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	sess, err := m.lookup(name)
	if err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}

	result, execErr := sess.plugin.Execute(ctx, name, step, sink)
	if execErr == nil {
		return result, nil
	}

	if !isLikelySessionCrash(execErr) {
		return result, execErr
	}

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
		result, retryErr := sess.plugin.Execute(ctx, name, step, sink)
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
		if err := sess.plugin.CloseSession(ctx, sess.Name); err != nil {
			errs = append(errs, err)
		}
		sess.plugin.Kill()
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
	sess.plugin.Kill()
	plug, err := m.loader.Resolve(ctx, sess.Adapter)
	if err != nil {
		return err
	}
	if err := plug.OpenSession(ctx, sess.Name, sess.Config); err != nil {
		plug.Kill()
		return err
	}
	sess.plugin = plug
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

func isLikelySessionCrash(err error) bool {
	if err == nil {
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
