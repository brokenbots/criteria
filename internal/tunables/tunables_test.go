package tunables

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestDefaultsMatchConstants(t *testing.T) {
	want := Settings{
		HeartbeatInterval:         DefaultHeartbeatInterval,
		HeartbeatStallThreshold:   DefaultHeartbeatStallThreshold,
		StepTimeoutTeardownWindow: DefaultStepTimeoutTeardownWindow,
		StepStallWindow:           DefaultStepStallWindow,
		AgentHeartbeatInterval:    DefaultAgentHeartbeatInterval,
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
	default:
		t.Fatalf("unexpected env knob %q", env)
		return 0
	}
}

// TestEnvvarsRegistry pins the registry invariants: unique names, sorted
// order, every exported env-name constant registered exactly once, and each
// row's default matching its owning tunable.
func TestEnvvarsRegistry(t *testing.T) {
	rows := Envvars()
	if len(rows) == 0 {
		t.Fatal("no registered overrides")
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("registry rows must be sorted by name, got %v", names)
	}
	seen := map[string]bool{}
	for i, r := range rows {
		if r.Name == "" || r.Doc == "" || (r.Kind != KindDuration && r.Kind != KindInt) {
			t.Errorf("row %d incomplete: %+v", i, r)
		}
		if seen[r.Name] {
			t.Errorf("duplicate registry row for %s", r.Name)
		}
		seen[r.Name] = true
	}
	for _, name := range []string{
		EnvAgentHeartbeatInterval, EnvHeartbeatInterval, EnvHeartbeatStallThreshold,
		EnvLocalApprovalFileTimeout,
		EnvPeerBackoffMax, EnvPeerBackoffMin, EnvPeerJournalLimit,
		EnvStepStallWindow, EnvStepTimeoutTeardownWindow,
	} {
		if !seen[name] {
			t.Errorf("env constant %s is not registered in Envvars", name)
		}
	}
	// Defaults render true to their constants, so the docs table never
	// outlives a default change.
	wantDefault := map[string]string{
		EnvAgentHeartbeatInterval:    "10s",
		EnvHeartbeatInterval:         "30s",
		EnvLocalApprovalFileTimeout:  "1h",
		EnvPeerBackoffMax:            "30s",
		EnvPeerBackoffMin:            "1s",
		EnvPeerJournalLimit:          "4096",
		EnvHeartbeatStallThreshold:   "90s",
		EnvStepStallWindow:           "30m",
		EnvStepTimeoutTeardownWindow: "10s",
	}
	for _, r := range rows {
		if wantDefault[r.Name] != r.Default {
			t.Errorf("%s registered default %q, want %q", r.Name, r.Default, wantDefault[r.Name])
		}
		if r.Kind == KindDuration {
			if _, err := time.ParseDuration(r.Default); err != nil {
				t.Errorf("%s default %q is not a Go duration: %v", r.Name, r.Default, err)
			}
		}
	}
}

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

// The docs/env-vars.md generated block must match the registry exactly
// (workstream KB-172 acceptance: "docs table current").

const (
	docsPath    = "../../docs/env-vars.md"
	beginMarker = "<!-- BEGIN AUTO:TUNABLES -->"
	endMarker   = "<!-- END AUTO:TUNABLES -->"
)

// renderDocRows is the canonical registry-to-docs-row rendering; the
// production block in docs/env-vars.md is generated by this same shape.
func renderDocRows() string {
	rows := Envvars()
	lines := make([]string, 0, len(rows)+2)
	lines = append(lines, "| Variable | Type | Default | Description |", "| --- | --- | --- | --- |")
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("| `%s` | %s | `%s` | %s |", r.Name, r.Kind, r.Default, r.Doc))
	}
	return strings.Join(lines, "\n")
}

func TestEnvVarsDocDrift(t *testing.T) {
	raw, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("read %s: %v", docsPath, err)
	}
	text := string(raw)
	begin := strings.Index(text, beginMarker)
	end := strings.Index(text, endMarker)
	if begin < 0 || end < 0 || begin > end {
		t.Fatalf("%s must contain a %s ... %s block", docsPath, beginMarker, endMarker)
	}
	block := text[begin+len(beginMarker) : end]
	block = strings.Trim(block, "\n")
	if block != renderDocRows() {
		t.Errorf("docs/env-vars.md generated block is stale; regenerate it with the rows Envvars renders:\n%s", renderDocRows())
	}
}
