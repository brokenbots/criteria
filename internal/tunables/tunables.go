// Package tunables is the single source of truth for the criteria engine's
// tunable timing and budget settings (KB-172, ruling card 127): one typed
// Settings shape, in-package defaults, and a registry (Envvars) that maps
// each operator-facing environment override to its knob and drives the
// generated table in docs/env-vars.md.
//
// Production code must not declare its own timing defaults or read timing
// values through ad-hoc os.Getenv calls: consume Settings (via FromEnv at
// configuration points) or the exported Default* constants, and register any
// new operator-facing override here. The registry names exactly one
// environment variable per knob; no CRITERIA_TEST_* production escape
// hatches may exist for timing.
//
// Two override postures exist, and both are preserved:
//
//   - Host knobs (Settings) are lenient: an unset, malformed, or
//     non-positive value keeps the built-in default. The only field where
//     zero is meaningful is StepStallWindow (0 = stall detection disabled).
//   - Peer overrides (CRITERIA_PEER_*) and the CLI's local-approval
//     file timeout are applied strictly: an unparseable value aborts
//     startup (peer) or the run (CLI) instead of silently defaulting.
//     The registry documents them; the strict parsers (peer/config.go,
//     internal/cli) own the reading and take their defaults from the
//     exported constants in this package.
package tunables

import (
	"os"
	"strings"
	"time"
)

// Built-in defaults. Each is the single source for its knob; consumer
// packages alias these values into their exported surface and tests.
const (
	// DefaultHeartbeatInterval is the adapter log-stream heartbeat cadence
	// and the peer Supervise stream idle heartbeat cadence (one source for
	// the 30s heartbeat family; survives the transitional
	// internal/adapterhost/heartbeatutil helper's removal).
	DefaultHeartbeatInterval = 30 * time.Second
	// DefaultHeartbeatStallThreshold is how long a session log stream may
	// idle with no chunks or heartbeats before the host treats the adapter
	// as wedged (CRI-271).
	DefaultHeartbeatStallThreshold = 90 * time.Second
	// DefaultStepTimeoutTeardownWindow is how long a CRI-287 step-timeout
	// teardown mark keeps reclassifying transport closes as teardown
	// consequences instead of session crashes.
	DefaultStepTimeoutTeardownWindow = 10 * time.Second
	// DefaultStepStallWindow is how long an unbounded step may run with no
	// observable adapter activity before the KB-25 watchdog tears it down.
	// Zero (or a negative override) disables stall detection.
	DefaultStepStallWindow = 30 * time.Minute
	// DefaultAgentHeartbeatInterval is the operator CLI's run heartbeat
	// cadence against a server-compatible orchestrator while a run is in
	// flight (KB-53).
	DefaultAgentHeartbeatInterval = 10 * time.Second
	// DefaultPauseToolCallDrainWindow is the drain-first pause window
	// (CRI-169): Session.Pause waits this long for in-flight nested tool
	// calls to settle before cancelling them.
	DefaultPauseToolCallDrainWindow = 60 * time.Second
	// DefaultLogMergeDelay is the log merge buffer's accumulation window
	// before a buffered chunk is flushed.
	DefaultLogMergeDelay = 500 * time.Millisecond
	// DefaultFilePollingInterval is the local resume status-file polling
	// budget.
	DefaultFilePollingInterval = 2 * time.Second
	// DefaultLocalApprovalFileTimeout is how long local approval file mode
	// waits for the operator's decision file before failing the pause.
	DefaultLocalApprovalFileTimeout = time.Hour
	// DefaultPeerBackoffMin is the floor for the peer's host reconnect
	// backoff (ADR-0007).
	DefaultPeerBackoffMin = time.Second
	// DefaultPeerBackoffMax is the ceiling for the peer's host reconnect
	// backoff (ADR-0007).
	DefaultPeerBackoffMax = 30 * time.Second
	// DefaultPeerJournalLimit is the bounded supervision journal capacity
	// in events (ADR-0007).
	DefaultPeerJournalLimit = 4096
)

// Operator-facing environment overrides. Each name is registered exactly
// once in Envvars.
const (
	// EnvHeartbeatInterval overrides DefaultHeartbeatInterval (lenient). It
	// replaces the removed CRITERIA_TEST_HEARTBEAT_INTERVAL_MS test hatch:
	// conformance tests set it (a Go duration such as "50ms") in the test
	// process and adapter fixture subprocesses inherit it.
	EnvHeartbeatInterval = "CRITERIA_HEARTBEAT_INTERVAL"
	// EnvHeartbeatStallThreshold overrides DefaultHeartbeatStallThreshold
	// (lenient; CRI-271).
	EnvHeartbeatStallThreshold = "CRITERIA_SESSION_HEARTBEAT_STALL"
	// EnvStepTimeoutTeardownWindow overrides
	// DefaultStepTimeoutTeardownWindow (lenient; CRI-287).
	EnvStepTimeoutTeardownWindow = "CRITERIA_STEP_TIMEOUT_TEARDOWN_WINDOW"
	// EnvStepStallWindow overrides DefaultStepStallWindow (lenient, but a
	// zero or negative value disables stall detection; KB-25).
	EnvStepStallWindow = "CRITERIA_STEP_STALL_WINDOW"
	// EnvAgentHeartbeatInterval overrides DefaultAgentHeartbeatInterval
	// (lenient; KB-53).
	EnvAgentHeartbeatInterval = "CRITERIA_AGENT_HEARTBEAT_INTERVAL"
	// EnvLocalApprovalFileTimeout overrides DefaultLocalApprovalFileTimeout
	// (strict — a malformed value fails the run loudly; internal/cli
	// localResumerOptions owns the strict parsing).
	EnvLocalApprovalFileTimeout = "CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT"
	// EnvPeerBackoffMin overrides DefaultPeerBackoffMin (strict — a
	// malformed value aborts peer startup; ADR-0007).
	EnvPeerBackoffMin = "CRITERIA_PEER_BACKOFF_MIN"
	// EnvPeerBackoffMax overrides DefaultPeerBackoffMax (strict; ADR-0007).
	EnvPeerBackoffMax = "CRITERIA_PEER_BACKOFF_MAX"
	// EnvPeerJournalLimit overrides DefaultPeerJournalLimit (strict
	// positive integer; ADR-0007).
	EnvPeerJournalLimit = "CRITERIA_PEER_JOURNAL_LIMIT"
)

// Registry value kinds (Envar.Kind).
const (
	KindDuration = "duration"
	KindInt      = "int"
)

// Settings carries the host-applied, lenient timing knobs: unset,
// malformed, and non-positive overrides keep the built-in default, except
// StepStallWindow where a zero value is the "stall detection disabled"
// posture (KB-25). Build it with New or FromEnv; a hand-made zero Settings
// is not a contract shape (every field would read as disabled), the
// zero-value fallback paths live in the consuming accessors.
//
// The strict peer overrides are not Settings fields: internal/peer parses
// them itself so a malformed value is a startup error, and takes its
// defaults from the DefaultPeer* constants here.
type Settings struct {
	// HeartbeatInterval is the adapter log-stream heartbeat cadence (the
	// transitional heartbeatutil helper, the MCP bridge, and in-tree
	// fixture adapters).
	HeartbeatInterval time.Duration
	// HeartbeatStallThreshold is the CRI-271 session log-stream stall
	// boundary (SessionManager.HeartbeatStallThreshold).
	HeartbeatStallThreshold time.Duration
	// StepTimeoutTeardownWindow is the CRI-287 teardown reclassification
	// window (SessionManager.StepTimeoutTeardownWindow).
	StepTimeoutTeardownWindow time.Duration
	// StepStallWindow is the KB-25 step-stall watchdog window; 0 disables
	// stall detection.
	StepStallWindow time.Duration
	// AgentHeartbeatInterval is the KB-53 operator CLI run heartbeat
	// cadence against a server-compatible orchestrator.
	AgentHeartbeatInterval time.Duration
}

// Defaults returns the built-in settings.
func Defaults() Settings {
	return Settings{
		HeartbeatInterval:         DefaultHeartbeatInterval,
		HeartbeatStallThreshold:   DefaultHeartbeatStallThreshold,
		StepTimeoutTeardownWindow: DefaultStepTimeoutTeardownWindow,
		StepStallWindow:           DefaultStepStallWindow,
		AgentHeartbeatInterval:    DefaultAgentHeartbeatInterval,
	}
}

// New resolves Settings from the provided environment lookup. Overrides are
// lenient: an unset, malformed, or non-positive value keeps the built-in
// default, except CRITERIA_STEP_STALL_WINDOW where a parsed zero or negative
// value disables stall detection (the KB-25 semantics).
func New(lookup func(string) string) Settings {
	s := Defaults()
	if lookup == nil {
		return s
	}
	if d, ok := resolvePositiveDuration(lookup(EnvHeartbeatInterval)); ok {
		s.HeartbeatInterval = d
	}
	if d, ok := resolvePositiveDuration(lookup(EnvHeartbeatStallThreshold)); ok {
		s.HeartbeatStallThreshold = d
	}
	if d, ok := resolvePositiveDuration(lookup(EnvStepTimeoutTeardownWindow)); ok {
		s.StepTimeoutTeardownWindow = d
	}
	// The stall window's zero is meaningful: "0" (or a negative value)
	// disables stall detection, a malformed value keeps the default (KB-25).
	if raw := strings.TrimSpace(lookup(EnvStepStallWindow)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			if d < 0 {
				d = 0
			}
			s.StepStallWindow = d
		}
	}
	if d, ok := resolvePositiveDuration(lookup(EnvAgentHeartbeatInterval)); ok {
		s.AgentHeartbeatInterval = d
	}
	return s
}

// FromEnv resolves Settings from the process environment.
func FromEnv() Settings {
	return New(os.Getenv)
}

// resolvePositiveDuration parses one lenient duration override. An empty,
// malformed, or non-positive value means "keep the built-in default".
func resolvePositiveDuration(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// Envar is one registry row: an operator-facing environment override for a
// tunable. Envvars is the single source for the generated table in
// docs/env-vars.md (a test drift-checks that the doc's marked block matches
// the rendered rows) and it pins that every override name is unique.
type Envar struct {
	// Name is the environment variable, e.g. CRITERIA_SESSION_HEARTBEAT_STALL.
	Name string
	// Kind is KindDuration or KindInt, matching the parsed value shape.
	Kind string
	// Default is the built-in default, rendered for the docs table.
	Default string
	// Doc is the one-line table description of what the knob tunes.
	Doc string
}

// Envvars returns the registered operator-facing timing and budget
// overrides, sorted by variable name. Host knobs are applied leniently via
// Settings; the peer rows are applied strictly by peer/config.go (malformed
// values abort peer startup rather than silently defaulting).
func Envvars() []Envar {
	return []Envar{
		{
			Name:    EnvAgentHeartbeatInterval,
			Kind:    KindDuration,
			Default: "10s",
			Doc:     "Operator CLI heartbeat cadence against a server-compatible orchestrator while a run is in flight (`criteria agent`, `criteria apply --server`).",
		},
		{
			Name:    EnvHeartbeatInterval,
			Kind:    KindDuration,
			Default: "30s",
			Doc:     "Adapter log-stream heartbeat cadence (the transitional heartbeatutil helper used by in-tree fixture adapters and the MCP bridge) and the peer Supervise stream idle heartbeat.",
		},
		{
			Name:    EnvLocalApprovalFileTimeout,
			Kind:    KindDuration,
			Default: "1h",
			Doc:     "How long file-mode local approval waits for the operator's decision file before the pause fails. Strict override: a malformed value fails the run loudly.",
		},
		{
			Name:    EnvPeerBackoffMax,
			Kind:    KindDuration,
			Default: "30s",
			Doc:     "Peer host reconnect backoff ceiling; must be >= the backoff floor. Peer-owned override: a malformed value aborts peer startup.",
		},
		{
			Name:    EnvPeerBackoffMin,
			Kind:    KindDuration,
			Default: "1s",
			Doc:     "Peer host reconnect backoff floor. Peer-owned override: a malformed value aborts peer startup.",
		},
		{
			Name:    EnvPeerJournalLimit,
			Kind:    KindInt,
			Default: "4096",
			Doc:     "Bounded supervision journal capacity in events. Peer-owned override: a non-positive or malformed value aborts peer startup.",
		},
		{
			Name:    EnvHeartbeatStallThreshold,
			Kind:    KindDuration,
			Default: "90s",
			Doc:     "How long a session log stream may idle with no chunks or heartbeats before the adapter is treated as wedged (CRI-271).",
		},
		{
			Name:    EnvStepStallWindow,
			Kind:    KindDuration,
			Default: "30m",
			Doc:     "How long an unbounded step may run with no observable adapter activity before the engine tears it down (KB-25). A zero or negative value disables stall detection.",
		},
		{
			Name:    EnvStepTimeoutTeardownWindow,
			Kind:    KindDuration,
			Default: "10s",
			Doc:     "Window, opened by an engine-initiated step teardown, during which transport closes are reclassified as teardown consequences instead of session crashes (CRI-287).",
		},
	}
}

// RespawnLogStreamDrain derives the CRI-271 bounded wait granted to a
// server-driven log stream to drain after a respawn: a third of the stall
// threshold clamped to [5s, 30s], so the drain stays proportional to how
// long the stream is allowed to idle without ballooning.
func RespawnLogStreamDrain(stallThreshold time.Duration) time.Duration {
	third := stallThreshold / 3
	if third < 5*time.Second {
		return 5 * time.Second
	}
	if third > 30*time.Second {
		return 30 * time.Second
	}
	return third
}

// StepStallPollInterval derives the KB-25 stall watchdog's sampling
// cadence: a quarter of the window clamped to [10ms, 1s], so the detection
// latency stays within a quarter of the window without polling hot on long
// windows.
func StepStallPollInterval(window time.Duration) time.Duration {
	poll := window / 4
	if poll > time.Second {
		poll = time.Second
	}
	if poll < 10*time.Millisecond {
		poll = 10 * time.Millisecond
	}
	return poll
}

// PauseSettleGrace derives the CRI-169 bounded settle grace the pause drain
// grants each cancelled straggler's goroutine to deliver its typed reply
// after its pending registration is cleared: a tenth of the window clamped
// to [100ms, 2s], so a short conformance window keeps a proportionally
// short grace.
func PauseSettleGrace(window time.Duration) time.Duration {
	grace := window / 10
	if grace < 100*time.Millisecond {
		grace = 100 * time.Millisecond
	}
	if grace > 2*time.Second {
		grace = 2 * time.Second
	}
	return grace
}
