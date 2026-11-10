// Package tunables is the single source of truth for the criteria engine's
// tunable timing and budget settings: one typed
// Settings shape, in-package defaults, and typed accessors that map
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
	"strconv"
	"strings"
	"time"

	"github.com/brokenbots/criteria/internal/envreg"
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
	// as wedged.
	DefaultHeartbeatStallThreshold = 90 * time.Second
	// DefaultStepTimeoutTeardownWindow is how long an engine-initiated step-timeout
	// teardown mark keeps reclassifying transport closes as teardown
	// consequences instead of session crashes.
	DefaultStepTimeoutTeardownWindow = 10 * time.Second
	// DefaultCrashCloseSettle is how long a supervised-container crash
	// classification waits for the in-process peer's journal to carry the
	// child's exit fact after transport death: the go-plugin client flips
	// its process-exited flag strictly after the transport fails, and the
	// journal facts ride that flip.
	DefaultCrashCloseSettle = 2 * time.Second
	// DefaultRemoteJournalSettlePoll is the wake cadence of the remote
	// peer handle's settled journal classification (JournalCrashSettler):
	// the peer's exit watcher emits its terminal facts at child-death
	// observation or on the next poll tick, so the settled query re-reads
	// the journal on this cadence until the facts arrive on the live
	// Supervise stream or the settle budget runs out.
	DefaultRemoteJournalSettlePoll = 10 * time.Millisecond
	// DefaultStepStallWindow is how long an unbounded step may run with no
	// observable adapter activity before the step-stall watchdog tears it down.
	// Zero (or a negative override) disables stall detection.
	DefaultStepStallWindow = 30 * time.Minute
	// DefaultAgentHeartbeatInterval is the operator CLI's run heartbeat
	// cadence against a server-compatible orchestrator while a run is in
	// flight.
	DefaultAgentHeartbeatInterval = 10 * time.Second
	// DefaultBootGateProbeTimeout bounds the boot gate's operator
	// view request at every server-mode start (fresh or adoption).
	DefaultBootGateProbeTimeout = 5 * time.Second
	// DefaultVerifyFailoverBudget is how long the transport-loss
	// convergence lane in phase-B adapter verify keeps re-waiting for a
	// fresh adapter phone-home line: a healthy re-dial converges in
	// seconds (first-entry evidence ~6s), so a one-minute cap converges
	// every healthy pod without letting one lost line burn a
	// scheduling-scale window.
	DefaultVerifyFailoverBudget = time.Minute
	// DefaultPauseToolCallDrainWindow is the drain-first pause window
	// Session.Pause waits this long for in-flight nested tool
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
	// DefaultFileAuditDropLogInterval is the minimum interval between
	// per-drop error emissions in the file permission-audit writer: every
	// drop is counted and surfaced, but sustained drop storms do not flood
	// the log (the first drop is always logged immediately).
	DefaultFileAuditDropLogInterval = 30 * time.Second
	// DefaultFileAuditLossWarnThreshold is the cumulative drop count that
	// escalates the file permission-audit writer's reporting from per-drop
	// error logs to a single WARN.
	DefaultFileAuditLossWarnThreshold = 5
)

// Operator-facing override names, aliased from the central CRITERIA_*
// registry: internal/envreg owns every name, default, consumer, and doc
// row (docs/env-vars.md is generated from it), while tunables keeps the
// typed lenient resolution. Consumers may keep using these aliases; a
// t.Setenv in tests works either way.
var (
	// EnvHeartbeatInterval overrides DefaultHeartbeatInterval (lenient).
	EnvHeartbeatInterval = envreg.CRITERIAHeartbeatInterval.Env()
	// EnvHeartbeatStallThreshold overrides DefaultHeartbeatStallThreshold
	// (lenient).
	EnvHeartbeatStallThreshold = envreg.CRITERIASessionHeartbeatStall.Env()
	// EnvStepTimeoutTeardownWindow overrides
	// DefaultStepTimeoutTeardownWindow (lenient).
	EnvStepTimeoutTeardownWindow = envreg.CRITERIAStepTimeoutTeardownWindow.Env()
	// EnvStepStallWindow overrides DefaultStepStallWindow (lenient, but a
	// zero or negative value disables stall detection).
	EnvStepStallWindow = envreg.CRITERIAStepStallWindow.Env()
	// EnvAgentHeartbeatInterval overrides DefaultAgentHeartbeatInterval
	// (lenient).
	EnvAgentHeartbeatInterval = envreg.CRITERIAAgentHeartbeatInterval.Env()
	// EnvBootGateProbeTimeout overrides DefaultBootGateProbeTimeout
	// (lenient).
	EnvBootGateProbeTimeout = envreg.CRITERIABootGateProbeTimeout.Env()
	// EnvLocalApprovalFileTimeout overrides DefaultLocalApprovalFileTimeout
	// (strict — a malformed value fails the run loudly; internal/cli
	// localResumerOptions owns the strict parsing).
	EnvLocalApprovalFileTimeout = envreg.CRITERIALocalApprovalFileTimeout.Env()
	// EnvPeerBackoffMin overrides DefaultPeerBackoffMin (strict — a
	// malformed value aborts peer startup; ADR-0007).
	EnvPeerBackoffMin = envreg.CRITERIAPeerBackoffMin.Env()
	// EnvPeerBackoffMax overrides DefaultPeerBackoffMax (strict; ADR-0007).
	EnvPeerBackoffMax = envreg.CRITERIAPeerBackoffMax.Env()
	// EnvPeerJournalLimit overrides DefaultPeerJournalLimit (strict
	// positive integer; ADR-0007).
	EnvPeerJournalLimit = envreg.CRITERIAPeerJournalLimit.Env()
	// EnvServeAdapterConcurrency overrides DefaultServeAdapterConcurrency
	// (lenient positive integer; ADR-0008).
	EnvServeAdapterConcurrency = envreg.CRITERIAServeAdapterConcurrency.Env()
	// EnvVerifyFailoverBudget overrides DefaultVerifyFailoverBudget
	// (lenient).
	EnvVerifyFailoverBudget = envreg.CRITERIAVerifyFailoverBudget.Env()
)

// DefaultServeAdapterConcurrency is the built-in shared container budget for
// the workflow's own step-adapter sessions in serve-adapter mode (ADR-0008).
const DefaultServeAdapterConcurrency = 8

// Settings carries the host-applied, lenient timing knobs: unset,
// malformed, and non-positive overrides keep the built-in default, except
// StepStallWindow where a zero value is the "stall detection disabled"
// posture. Build it with New or FromEnv; a hand-made zero Settings
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
	// HeartbeatStallThreshold is the session log-stream stall
	// boundary (SessionManager.HeartbeatStallThreshold).
	HeartbeatStallThreshold time.Duration
	// StepTimeoutTeardownWindow is the engine-initiated teardown reclassification
	// window (SessionManager.StepTimeoutTeardownWindow).
	StepTimeoutTeardownWindow time.Duration
	// StepStallWindow is the step-stall watchdog window; 0 disables
	// stall detection.
	StepStallWindow time.Duration
	// AgentHeartbeatInterval is the operator CLI run heartbeat
	// cadence against a server-compatible orchestrator.
	AgentHeartbeatInterval time.Duration
	// BootGateProbeTimeout bounds the boot gate's operator view
	// request at every server-mode start (fresh or adoption).
	BootGateProbeTimeout time.Duration
	// ServeAdapterConcurrency is the ADR-0008 shared container budget for
	// the workflow's own step-adapter sessions while
	// `criteria serve-adapter` serves a workflow.
	ServeAdapterConcurrency int
	// VerifyFailoverBudget is the transport-loss convergence
	// budget (SessionManager.VerifyHandshakeFailoverBudget).
	VerifyFailoverBudget time.Duration
}

// Defaults returns the built-in settings.
func Defaults() Settings {
	return Settings{
		HeartbeatInterval:         DefaultHeartbeatInterval,
		HeartbeatStallThreshold:   DefaultHeartbeatStallThreshold,
		StepTimeoutTeardownWindow: DefaultStepTimeoutTeardownWindow,
		StepStallWindow:           DefaultStepStallWindow,
		AgentHeartbeatInterval:    DefaultAgentHeartbeatInterval,
		BootGateProbeTimeout:      DefaultBootGateProbeTimeout,
		ServeAdapterConcurrency:   DefaultServeAdapterConcurrency,
		VerifyFailoverBudget:      DefaultVerifyFailoverBudget,
	}
}

// New resolves Settings from the provided environment lookup. Overrides are
// lenient: an unset, malformed, or non-positive value keeps the built-in
// default, except CRITERIA_STEP_STALL_WINDOW where a parsed zero or negative
// value disables stall detection.
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
	// disables stall detection, a malformed value keeps the default.
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
	if d, ok := resolvePositiveDuration(lookup(EnvBootGateProbeTimeout)); ok {
		s.BootGateProbeTimeout = d
	}
	if n, ok := resolvePositiveInt(lookup(EnvServeAdapterConcurrency)); ok {
		s.ServeAdapterConcurrency = n
	}
	if d, ok := resolvePositiveDuration(lookup(EnvVerifyFailoverBudget)); ok {
		s.VerifyFailoverBudget = d
	}
	return s
}

// FromEnv resolves Settings from the process environment through the
// registry's audit point.
func FromEnv() Settings {
	return New(envreg.Lookup)
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

// resolvePositiveInt parses one lenient integer override. An empty,
// malformed, or non-positive value means "keep the built-in default".
func resolvePositiveInt(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// RespawnLogStreamDrain derives the bounded wait granted to a
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

// StepStallPollInterval derives the stall watchdog's sampling
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

// PauseSettleGrace derives the bounded settle grace the pause drain
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
