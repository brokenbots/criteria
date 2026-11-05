package tunables

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/envreg"
)

func TestDefaultsMatchConstants(t *testing.T) {
	want := Settings{
		HeartbeatInterval:         DefaultHeartbeatInterval,
		HeartbeatStallThreshold:   DefaultHeartbeatStallThreshold,
		StepTimeoutTeardownWindow: DefaultStepTimeoutTeardownWindow,
		StepStallWindow:           DefaultStepStallWindow,
		AgentHeartbeatInterval:    DefaultAgentHeartbeatInterval,
		BootGateProbeTimeout:      DefaultBootGateProbeTimeout,
		ServeAdapterConcurrency:   DefaultServeAdapterConcurrency,
		VerifyFailoverBudget:      DefaultVerifyFailoverBudget,
	}
	if got := Defaults(); got != want {
		t.Errorf("Defaults() = %+v, want %+v", got, want)
	}
	if got := New(nil); got != want {
		t.Errorf("New(nil) = %+v, want built-in defaults", got)
	}
}

// override wants per-knob: base value applied (lenient posture) and value
// shape. Each case's want is the parsed base value.
func TestNewLenientOverrides(t *testing.T) {
	cases := []struct {
		env   string
		value string
		want  time.Duration
	}{
		{EnvHeartbeatInterval, "1500ms", 1500 * time.Millisecond},
		{EnvHeartbeatStallThreshold, "5m", 5 * time.Minute},
		{EnvStepTimeoutTeardownWindow, "2m", 2 * time.Minute},
		{EnvAgentHeartbeatInterval, "7s", 7 * time.Second},
		{EnvBootGateProbeTimeout, "3s", 3 * time.Second},
		{EnvVerifyFailoverBudget, "90s", 90 * time.Second},
	}
	lookupWith := func(env, raw string) func(string) string {
		return func(name string) string {
			if name == env {
				return raw
			}
			return ""
		}
	}
	for _, tc := range cases {
		// A valid value applies, including with surrounding whitespace.
		for _, raw := range []string{tc.value, " " + tc.value + " "} {
			got := New(lookupWith(tc.env, raw))
			if fieldFor(t, got, tc.env) != tc.want {
				t.Errorf("%s=%q: got %s, want %s", tc.env, raw, fieldFor(t, got, tc.env), tc.want)
			}
		}
		// Empty, malformed, and non-positive values keep the built-in default.
		for _, raw := range []string{"", "   ", "garbage", "0s", "-1s"} {
			got := New(lookupWith(tc.env, raw))
			if want := fieldFor(t, Defaults(), tc.env); fieldFor(t, got, tc.env) != want {
				t.Errorf("%s=%q: got %s, want default %s", tc.env, raw, fieldFor(t, got, tc.env), want)
			}
		}
	}
}

// TestNewServeAdapterConcurrency pins the ADR-0008 concurrency cap override
// semantics: a positive value applies, while empty, malformed, and
// non-positive values keep the built-in default (lenient posture).
func TestNewServeAdapterConcurrency(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"4", 4},
		{" 12 ", 12},
		{"0", DefaultServeAdapterConcurrency},
		{"-3", DefaultServeAdapterConcurrency},
		{"", DefaultServeAdapterConcurrency},
		{"garbage", DefaultServeAdapterConcurrency},
	}
	for _, tc := range cases {
		got := New(func(string) string { return tc.raw })
		if got.ServeAdapterConcurrency != tc.want {
			t.Errorf("New(%s=%q).ServeAdapterConcurrency = %d, want %d", EnvServeAdapterConcurrency, tc.raw, got.ServeAdapterConcurrency, tc.want)
		}
	}
}

// TestNewStepStallWindow pins the KB-25 override semantics: a zero or
// negative value disables stall detection while a malformed value keeps the
// built-in default.
func TestNewStepStallWindow(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"45m", 45 * time.Minute},
		{" 30s ", 30 * time.Second},
		{"0", 0},
		{"-5m", 0},
		{"", DefaultStepStallWindow},
		{"garbage", DefaultStepStallWindow},
	}
	for _, tc := range cases {
		got := New(func(string) string { return tc.raw })
		if got.StepStallWindow != tc.want {
			t.Errorf("New(StepStallWindow=%q).StepStallWindow = %s, want %s", tc.raw, got.StepStallWindow, tc.want)
		}
	}
}

// TestFromEnvReadsProcessEnvironment pins the production wiring: FromEnv
// consumes the process environment (t.Setenv exercises exactly that path).
func TestFromEnvReadsProcessEnvironment(t *testing.T) {
	prev := os.Getenv(EnvHeartbeatStallThreshold)
	t.Cleanup(func() { os.Setenv(EnvHeartbeatStallThreshold, prev) })
	if err := os.Setenv(EnvHeartbeatStallThreshold, "5m"); err != nil {
		t.Fatal(err)
	}
	if got := FromEnv(); got.HeartbeatStallThreshold != 5*time.Minute {
		t.Errorf("FromEnv().HeartbeatStallThreshold = %s, want 5m", got.HeartbeatStallThreshold)
	}
	if err := os.Setenv(EnvHeartbeatStallThreshold, "0s"); err != nil {
		t.Fatal(err)
	}
	if got := FromEnv(); got.HeartbeatStallThreshold != DefaultHeartbeatStallThreshold {
		t.Errorf("FromEnv() with %s=0s = %s, want default", EnvHeartbeatStallThreshold, got.HeartbeatStallThreshold)
	}
}

func fieldFor(t *testing.T, s Settings, env string) time.Duration {
	t.Helper()
	switch env {
	case EnvHeartbeatInterval:
		return s.HeartbeatInterval
	case EnvHeartbeatStallThreshold:
		return s.HeartbeatStallThreshold
	case EnvStepTimeoutTeardownWindow:
		return s.StepTimeoutTeardownWindow
	case EnvAgentHeartbeatInterval:
		return s.AgentHeartbeatInterval
	case EnvBootGateProbeTimeout:
		return s.BootGateProbeTimeout
	case EnvVerifyFailoverBudget:
		return s.VerifyFailoverBudget
	default:
		t.Fatalf("unexpected env knob %q", env)
		return 0
	}
}

// TestEnvvarsRegistry pins the registry invariants: unique names, sorted
// order, every exported env-name constant registered exactly once, and each
// row's default matching its owning tunable.
func TestDerivedBudgets(t *testing.T) {
	// RespawnLogStreamDrain: a third of the stall threshold, clamped.
	if got := RespawnLogStreamDrain(DefaultHeartbeatStallThreshold); got != 30*time.Second {
		t.Errorf("RespawnLogStreamDrain(90s) = %s, want the 30s cap", got)
	}
	if got := RespawnLogStreamDrain(90 * time.Second); got != 30*time.Second {
		t.Errorf("RespawnLogStreamDrain(90s) = %s, want 30s", got)
	}
	if got := RespawnLogStreamDrain(36 * time.Second); got != 12*time.Second {
		t.Errorf("RespawnLogStreamDrain(36s) = %s, want 12s", got)
	}
	if got := RespawnLogStreamDrain(6 * time.Second); got != 5*time.Second {
		t.Errorf("RespawnLogStreamDrain(6s) = %s, want the 5s floor", got)
	}

	// StepStallPollInterval: a quarter of the window, clamped.
	if got := StepStallPollInterval(DefaultStepStallWindow); got != time.Second {
		t.Errorf("StepStallPollInterval(30m) = %s, want the 1s cap", got)
	}
	if got := StepStallPollInterval(400 * time.Millisecond); got != 100*time.Millisecond {
		t.Errorf("StepStallPollInterval(400ms) = %s, want 100ms", got)
	}
	if got := StepStallPollInterval(time.Millisecond); got != 10*time.Millisecond {
		t.Errorf("StepStallPollInterval(1ms) = %s, want the 10ms floor", got)
	}

	// PauseSettleGrace: a tenth of the window, clamped.
	if got := PauseSettleGrace(DefaultPauseToolCallDrainWindow); got != 2*time.Second {
		t.Errorf("PauseSettleGrace(60s) = %s, want the 2s cap", got)
	}
	if got := PauseSettleGrace(time.Second); got != 100*time.Millisecond {
		t.Errorf("PauseSettleGrace(1s) = %s, want the 100ms floor", got)
	}
	if got := PauseSettleGrace(10 * time.Second); got != time.Second {
		t.Errorf("PauseSettleGrace(10s) = %s, want 1s", got)
	}
}

// TestAliasesPointAtRegistry pins the tunables↔envreg seam: every override
// alias carries the registry's env name, and every timing row's registry
// default matches the Default constant it documents (so the generated docs
// table never outlives a default change).
func TestAliasesPointAtRegistry(t *testing.T) {
	checks := []struct {
		alias    string
		rowName  func() string
		rowDef   func() string
		defConst string
		kind     envreg.Kind
	}{
		{EnvHeartbeatInterval, envreg.CRITERIAHeartbeatInterval.Env, envreg.CRITERIAHeartbeatInterval.Default, DefaultHeartbeatInterval.String(), envreg.KindDuration},
		{EnvHeartbeatStallThreshold, envreg.CRITERIASessionHeartbeatStall.Env, envreg.CRITERIASessionHeartbeatStall.Default, DefaultHeartbeatStallThreshold.String(), envreg.KindDuration},
		{EnvStepTimeoutTeardownWindow, envreg.CRITERIAStepTimeoutTeardownWindow.Env, envreg.CRITERIAStepTimeoutTeardownWindow.Default, DefaultStepTimeoutTeardownWindow.String(), envreg.KindDuration},
		{EnvStepStallWindow, envreg.CRITERIAStepStallWindow.Env, envreg.CRITERIAStepStallWindow.Default, DefaultStepStallWindow.String(), envreg.KindDuration},
		{EnvAgentHeartbeatInterval, envreg.CRITERIAAgentHeartbeatInterval.Env, envreg.CRITERIAAgentHeartbeatInterval.Default, DefaultAgentHeartbeatInterval.String(), envreg.KindDuration},
		{EnvBootGateProbeTimeout, envreg.CRITERIABootGateProbeTimeout.Env, envreg.CRITERIABootGateProbeTimeout.Default, DefaultBootGateProbeTimeout.String(), envreg.KindDuration},
		{EnvLocalApprovalFileTimeout, envreg.CRITERIALocalApprovalFileTimeout.Env, envreg.CRITERIALocalApprovalFileTimeout.Default, DefaultLocalApprovalFileTimeout.String(), envreg.KindDuration},
		{EnvPeerBackoffMin, envreg.CRITERIAPeerBackoffMin.Env, envreg.CRITERIAPeerBackoffMin.Default, DefaultPeerBackoffMin.String(), envreg.KindDuration},
		{EnvPeerBackoffMax, envreg.CRITERIAPeerBackoffMax.Env, envreg.CRITERIAPeerBackoffMax.Default, DefaultPeerBackoffMax.String(), envreg.KindDuration},
		{EnvPeerJournalLimit, envreg.CRITERIAPeerJournalLimit.Env, envreg.CRITERIAPeerJournalLimit.Default, strconv.Itoa(DefaultPeerJournalLimit), envreg.KindInt},
		{EnvServeAdapterConcurrency, envreg.CRITERIAServeAdapterConcurrency.Env, envreg.CRITERIAServeAdapterConcurrency.Default, strconv.Itoa(DefaultServeAdapterConcurrency), envreg.KindInt},
		{EnvVerifyFailoverBudget, envreg.CRITERIAVerifyFailoverBudget.Env, envreg.CRITERIAVerifyFailoverBudget.Default, DefaultVerifyFailoverBudget.String(), envreg.KindDuration},
	}
	for _, tc := range checks {
		if tc.alias != tc.rowName() {
			t.Errorf("alias %q points at registry name %q", tc.alias, tc.rowName())
		}
		got := tc.rowDef()
		if tc.kind == envreg.KindDuration {
			want, errW := time.ParseDuration(tc.defConst)
			have, errH := time.ParseDuration(got)
			if errW != nil || errH != nil || want != have {
				t.Errorf("%s registry default %q does not match tunables constant %s", tc.alias, got, tc.defConst)
			}
		} else if got != tc.defConst {
			t.Errorf("%s registry default %q does not match tunables constant %s", tc.alias, got, tc.defConst)
		}
		if tc.kind != envreg.KindDuration && tc.kind != envreg.KindInt {
			t.Errorf("%s kind %q is not a parsed value kind", tc.alias, tc.kind)
		}
	}
}

// The generated docs table (docs/env-vars.md) is owned by internal/envreg's
// drift test; regenerating it is CRITERIA_REGEN_DOCS=1 go test ./internal/envreg.
