package run

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// stripANSI removes ANSI SGR escapes so assertions can match plain text.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				i = j
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestConsoleSink_HappyPath(t *testing.T) {
	var buf bytes.Buffer
	sink := NewConsoleSink(&buf, []string{"open", "run", "close"}, false)

	sink.OnRunStarted("wf", "open")
	sink.OnStepEntered("open", "demo", 1)
	sink.OnStepOutcome("open", "success", 12*time.Millisecond, nil)
	sink.OnStepTransition("open", "run", "success")
	sink.OnStepEntered("run", "demo", 1)
	stepSink := sink.StepEventSink("run")
	stepSink.Adapter("agent.message", map[string]any{"event_type": "assistant.message_delta", "delta": "hel"})
	stepSink.Adapter("agent.message", map[string]any{"event_type": "assistant.message", "content": "hello there"})
	stepSink.Adapter("tool.invocation", map[string]any{"name": "edit", "arguments": `{"path":"/x/y/foo.go"}`})
	stepSink.Log("agent", []byte("noisy stdout chunk\n"))
	sink.OnStepOutcome("run", "success", 1500*time.Millisecond, nil)
	sink.OnStepEntered("close", "demo", 1)
	sink.OnStepOutcome("close", "success", 5*time.Millisecond, nil)
	sink.OnStepTransition("close", "done", "success")
	sink.OnRunCompleted("done", true)

	out := stripANSI(buf.String())
	wantSubstrings := []string{
		"▶ wf  steps=3",
		"[1/3] open  (demo)",
		"✓ success in 12ms",
		"[2/3] run  (demo)",
		"agent: hello there",
		"→ edit path=foo.go",
		"✓ success in 1.5s",
		"[3/3] close  (demo)",
		"→ done",
		"✔ run completed",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	// Per-token deltas and raw stdout chunks must NOT appear.
	for _, banned := range []string{"hel\n", "noisy stdout chunk", "assistant.message_delta"} {
		if strings.Contains(out, banned) {
			t.Errorf("forbidden token %q present in concise output:\n%s", banned, out)
		}
	}
}

func TestConsoleSink_FailureRendersErrorAndDuration(t *testing.T) {
	var buf bytes.Buffer
	sink := NewConsoleSink(&buf, []string{"only"}, false)
	sink.OnRunStarted("wf", "only")
	sink.OnStepEntered("only", "demo", 1)
	sink.OnStepOutcome("only", "failure", 250*time.Millisecond, &stringErr{msg: "boom"})
	sink.OnRunFailed("boom", "only")

	out := stripANSI(buf.String())
	if !strings.Contains(out, "✗ failure: boom (250ms)") {
		t.Errorf("missing failure line:\n%s", out)
	}
	if !strings.Contains(out, "✗ run failed at only: boom") {
		t.Errorf("missing run-failed line:\n%s", out)
	}
}

func TestConsoleSink_PermissionAndLimits(t *testing.T) {
	var buf bytes.Buffer
	sink := NewConsoleSink(&buf, []string{"s"}, false)
	stepSink := sink.StepEventSink("s")
	stepSink.Adapter("permission.granted", map[string]any{"tool": "write"})
	stepSink.Adapter("permission.denied", map[string]any{"tool": "exec"})
	stepSink.Adapter("limit.reached", map[string]any{"reason": "max_turns"})
	stepSink.Adapter("tool.result", map[string]any{"name": "edit"}) // dropped

	out := stripANSI(buf.String())
	if !strings.Contains(out, "permission granted: write") {
		t.Errorf("missing granted line:\n%s", out)
	}
	if !strings.Contains(out, "permission denied: exec") {
		t.Errorf("missing denied line:\n%s", out)
	}
	if !strings.Contains(out, "limit reached") {
		t.Errorf("missing limit line:\n%s", out)
	}
	if strings.Contains(out, "tool.result") {
		t.Errorf("tool.result must not be rendered:\n%s", out)
	}
}

func TestConsoleSink_AgentMessageEmptyContentDropped(t *testing.T) {
	var buf bytes.Buffer
	sink := NewConsoleSink(&buf, []string{"s"}, false)
	stepSink := sink.StepEventSink("s")
	stepSink.Adapter("agent.message", map[string]any{"event_type": "assistant.message", "content": ""})
	stepSink.Adapter("agent.message", map[string]any{"event_type": "assistant.message", "content": "   "})
	if strings.Contains(buf.String(), "agent:") {
		t.Errorf("empty agent message should be dropped: %q", buf.String())
	}
}

func TestConsoleSink_ToolArgsCommandSummary(t *testing.T) {
	var buf bytes.Buffer
	sink := NewConsoleSink(&buf, []string{"s"}, false)
	stepSink := sink.StepEventSink("s")
	stepSink.Adapter("tool.invocation", map[string]any{"name": "bash", "arguments": `{"cmd":"go test ./..."}`})
	out := stripANSI(buf.String())
	if !strings.Contains(out, "→ bash cmd=go test ./...") {
		t.Errorf("expected cmd summary, got:\n%s", out)
	}
}

func TestConsoleSink_TransitionToTerminalStateRendered(t *testing.T) {
	var buf bytes.Buffer
	sink := NewConsoleSink(&buf, []string{"a"}, false)
	// "done" is not a step → should render arrow
	sink.OnStepTransition("a", "done", "success")
	if !strings.Contains(buf.String(), "→ done") {
		t.Errorf("terminal-state transition not rendered: %q", buf.String())
	}

	// Transition to a known step should be silent.
	buf.Reset()
	sink2 := NewConsoleSink(&buf, []string{"a", "b"}, false)
	sink2.OnStepTransition("a", "b", "success")
	if buf.Len() != 0 {
		t.Errorf("step→step transition should be silent, got: %q", buf.String())
	}
}

type stringErr struct{ msg string }

func (e *stringErr) Error() string { return e.msg }

// TestConsoleSink_LifecycleTag verifies that OnAdapterLifecycle events are
// accumulated per step and rendered as an [adapter: ...] tag on the step
// outcome line (W12).
func TestConsoleSink_LifecycleTag(t *testing.T) {
	var buf bytes.Buffer
	sink := NewConsoleSink(&buf, []string{"build"}, false)

	sink.OnAdapterLifecycle("build", "shell", "started", "")
	sink.OnAdapterLifecycle("build", "shell", "exited", "")
	sink.OnStepOutcome("build", "success", 2300*time.Millisecond, nil)

	out := buf.String()
	if !strings.Contains(out, "[adapter: started → exited]") {
		t.Errorf("expected lifecycle tag in output, got: %q", out)
	}
}

// TestConsoleSink_LifecycleTagCrash verifies that a crashed adapter renders
// the detail string in the lifecycle tag (W12).
func TestConsoleSink_LifecycleTagCrash(t *testing.T) {
	var buf bytes.Buffer
	sink := NewConsoleSink(&buf, []string{"review"}, false)

	sink.OnAdapterLifecycle("review", "copilot", "started", "")
	sink.OnAdapterLifecycle("review", "copilot", "crashed", "connection refused")
	sink.OnStepOutcome("review", "failure", 8100*time.Millisecond, &stringErr{"adapter crashed"})

	out := buf.String()
	if !strings.Contains(out, "[adapter: started → crashed: connection refused]") {
		t.Errorf("expected crash lifecycle tag in output, got: %q", out)
	}
}

// TestConsoleSink_LifecycleTagAbsent verifies that steps without any
// lifecycle events do not render an [adapter: ...] tag (W12).
func TestConsoleSink_LifecycleTagAbsent(t *testing.T) {
	var buf bytes.Buffer
	sink := NewConsoleSink(&buf, []string{"build"}, false)

	sink.OnStepOutcome("build", "success", 500*time.Millisecond, nil)

	out := buf.String()
	if strings.Contains(out, "[adapter:") {
		t.Errorf("expected no lifecycle tag when no events emitted, got: %q", out)
	}
}
