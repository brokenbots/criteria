package run

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/workflow"
)

// minimalGraph builds a minimal FSMGraph with a single adapter and step for
// testing per-line prefix rendering.
func minimalGraph(stepName, adapterType, adapterName string) *workflow.FSMGraph {
	adapterRef := adapterType + "." + adapterName
	return &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			adapterRef: {Type: adapterType, Name: adapterName},
		},
		Steps: map[string]*workflow.StepNode{
			stepName: {Name: stepName, AdapterRef: adapterRef},
		},
	}
}

func TestConsoleSink_PerLineFormat_AgentMessage(t *testing.T) {
	var buf bytes.Buffer
	g := minimalGraph("build", "shell", "compile")
	sink := NewConsoleSink(&buf, []string{"build"}, false, g)
	sink.OnRunStarted("wf", "build")
	sink.OnStepEntered("build", "shell", 1)
	stepSink := sink.StepEventSink("build")
	stepSink.Adapter("agent.message", map[string]any{"event_type": "assistant.message", "content": "hello"})
	sink.OnStepOutcome("build", "success", 1*time.Second, nil)

	out := stripANSI(buf.String())
	if !strings.Contains(out, "[1/1 build · shell(compile)]") {
		t.Errorf("agent message line missing prefix, output:\n%s", out)
	}
	if !strings.Contains(out, "agent: hello") {
		t.Errorf("agent message content missing, output:\n%s", out)
	}
}

func TestConsoleSink_PerLineFormat_ToolInvocation_HappyEmoji(t *testing.T) {
	var buf bytes.Buffer
	g := minimalGraph("build", "shell", "compile")
	sink := NewConsoleSink(&buf, []string{"build"}, false, g)
	sink.OnStepEntered("build", "shell", 1)
	stepSink := sink.StepEventSink("build")
	stepSink.Adapter("tool.invocation", map[string]any{"name": "read_file", "arguments": `{}`})

	out := stripANSI(buf.String())
	if !strings.Contains(out, "[1/1 build · shell(compile)]") {
		t.Errorf("tool line missing prefix, output:\n%s", out)
	}
	if !strings.Contains(out, "📄") {
		t.Errorf("file emoji missing from tool line, output:\n%s", out)
	}
	if !strings.Contains(out, "read_file") {
		t.Errorf("tool name missing from tool line, output:\n%s", out)
	}
}

func TestConsoleSink_PerLineFormat_ToolInvocation_ShellEmoji(t *testing.T) {
	var buf bytes.Buffer
	g := minimalGraph("run", "shell", "exec")
	sink := NewConsoleSink(&buf, []string{"run"}, false, g)
	sink.OnStepEntered("run", "shell", 1)
	stepSink := sink.StepEventSink("run")
	stepSink.Adapter("tool.invocation", map[string]any{"name": "shell_exec", "arguments": `{}`})

	out := stripANSI(buf.String())
	if !strings.Contains(out, "⚡") {
		t.Errorf("shell emoji missing from tool line, output:\n%s", out)
	}
}

func TestConsoleSink_PerLineFormat_ToolInvocation_NetworkEmoji(t *testing.T) {
	var buf bytes.Buffer
	g := minimalGraph("deploy", "copilot", "agent")
	sink := NewConsoleSink(&buf, []string{"deploy"}, false, g)
	sink.OnStepEntered("deploy", "copilot", 1)
	stepSink := sink.StepEventSink("deploy")
	stepSink.Adapter("tool.invocation", map[string]any{"name": "http_get", "arguments": `{}`})

	out := stripANSI(buf.String())
	if !strings.Contains(out, "🌐") {
		t.Errorf("network emoji missing from tool line, output:\n%s", out)
	}
}

func TestConsoleSink_PerLineFormat_ToolInvocation_SearchEmoji(t *testing.T) {
	var buf bytes.Buffer
	g := minimalGraph("search_step", "copilot", "agent")
	sink := NewConsoleSink(&buf, []string{"search_step"}, false, g)
	sink.OnStepEntered("search_step", "copilot", 1)
	stepSink := sink.StepEventSink("search_step")
	// grep_files: search wins over file via priority order.
	stepSink.Adapter("tool.invocation", map[string]any{"name": "grep_files", "arguments": `{}`})

	out := stripANSI(buf.String())
	if !strings.Contains(out, "🔍") {
		t.Errorf("search emoji missing from tool line, output:\n%s", out)
	}
}

func TestConsoleSink_PerLineFormat_ToolInvocation_WriteEmoji(t *testing.T) {
	var buf bytes.Buffer
	g := minimalGraph("edit_step", "copilot", "agent")
	sink := NewConsoleSink(&buf, []string{"edit_step"}, false, g)
	sink.OnStepEntered("edit_step", "copilot", 1)
	stepSink := sink.StepEventSink("edit_step")
	// edit_file: write wins over file via priority order.
	stepSink.Adapter("tool.invocation", map[string]any{"name": "edit_file", "arguments": `{}`})

	out := stripANSI(buf.String())
	if !strings.Contains(out, "✏️") {
		t.Errorf("write emoji missing from tool line, output:\n%s", out)
	}
}

func TestConsoleSink_PerLineFormat_ToolInvocation_FallbackArrow(t *testing.T) {
	var buf bytes.Buffer
	g := minimalGraph("s", "shell", "exec")
	sink := NewConsoleSink(&buf, []string{"s"}, false, g)
	sink.OnStepEntered("s", "shell", 1)
	stepSink := sink.StepEventSink("s")
	stepSink.Adapter("tool.invocation", map[string]any{"name": "unknown_thing", "arguments": `{}`})

	out := stripANSI(buf.String())
	if !strings.Contains(out, "→") {
		t.Errorf("fallback arrow missing from tool line, output:\n%s", out)
	}
}

func TestConsoleSink_PerLineFormat_MultilineAgent_PrefixOnEveryLine(t *testing.T) {
	var buf bytes.Buffer
	g := minimalGraph("s", "copilot", "agent")
	sink := NewConsoleSink(&buf, []string{"s"}, false, g)
	sink.OnStepEntered("s", "copilot", 1)
	stepSink := sink.StepEventSink("s")
	stepSink.Adapter("agent.message", map[string]any{
		"event_type": "assistant.message",
		"content":    "line1\nline2\nline3",
	})

	out := stripANSI(buf.String())
	// Every output line must carry the prefix.
	prefixCount := strings.Count(out, "[1/1 s · copilot(agent)]")
	// 3 agent lines + 1 header line = 4 occurrences of the prefix pattern.
	// Count only agent lines: each has "agent: lineN".
	agentLines := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "agent:") {
			if !strings.Contains(line, "[1/1 s · copilot(agent)]") {
				t.Errorf("agent line missing prefix: %q", line)
			}
			agentLines++
		}
	}
	if agentLines != 3 {
		t.Errorf("expected 3 agent lines, got %d; prefix count=%d\noutput:\n%s", agentLines, prefixCount, out)
	}
}

func TestConsoleSink_PerLineFormat_NoColorMode_PrefixIsPlain(t *testing.T) {
	var buf bytes.Buffer
	g := minimalGraph("s", "shell", "exec")
	sink := NewConsoleSink(&buf, []string{"s"}, false, g)
	sink.OnStepEntered("s", "shell", 1)
	stepSink := sink.StepEventSink("s")
	stepSink.Adapter("agent.message", map[string]any{"event_type": "assistant.message", "content": "hi"})

	raw := buf.String()
	// No ANSI escape codes at all.
	if strings.Contains(raw, "\x1b[") {
		t.Errorf("color=false output must contain no ANSI escapes:\n%s", raw)
	}
	// Prefix must be plain text.
	if !strings.Contains(raw, "[1/1 s · shell(exec)]") {
		t.Errorf("plain prefix not found in:\n%s", raw)
	}
}

func TestConsoleSink_PerLineFormat_ColorMode_PrefixIsDim(t *testing.T) {
	var buf bytes.Buffer
	g := minimalGraph("s", "shell", "exec")
	sink := NewConsoleSink(&buf, []string{"s"}, true, g)
	sink.OnStepEntered("s", "shell", 1)
	stepSink := sink.StepEventSink("s")
	stepSink.Adapter("agent.message", map[string]any{"event_type": "assistant.message", "content": "hi"})

	raw := buf.String()
	// The prefix in the agent line should be wrapped in dim ANSI (\x1b[2m...\x1b[0m).
	if !strings.Contains(raw, "\x1b[2m[1/1 s · shell(exec)]\x1b[0m") {
		t.Errorf("dim prefix not found in color output:\n%q", raw)
	}
}

func TestConsoleSink_PerLineFormat_UnknownStep_NoPrefix(t *testing.T) {
	var buf bytes.Buffer
	// "other" is not registered in the steps list.
	sink := NewConsoleSink(&buf, []string{"known"}, false, nil)
	// StepEventSink for unknown step: buildLinePrefix returns "".
	stepSink := sink.StepEventSink("unknown_step")
	// Must not panic; line has no prefix.
	stepSink.Adapter("agent.message", map[string]any{"event_type": "assistant.message", "content": "hi"})

	out := buf.String()
	// No "[N/N ..." prefix, just the raw agent line.
	if strings.Contains(out, "[") && strings.Contains(out, "/") && strings.Contains(out, "·") {
		// Contains step prefix pattern — not expected.
		if strings.Contains(out, "[1/") {
			t.Errorf("unexpected step prefix in output for unknown step:\n%s", out)
		}
	}
	if !strings.Contains(out, "agent: hi") {
		t.Errorf("agent content missing for unknown step:\n%s", out)
	}
}

func TestConsoleSink_PerLineFormat_StepEnteredHeader_NewFormat(t *testing.T) {
	var buf bytes.Buffer
	g := minimalGraph("build", "shell", "compile")
	sink := NewConsoleSink(&buf, []string{"build"}, false, g)
	sink.OnStepEntered("build", "shell", 1)

	out := stripANSI(buf.String())
	// Header uses ▶ and the new [I/N step · type(name)] format.
	if !strings.Contains(out, "▶ [1/1 build · shell(compile)]") {
		t.Errorf("step header in wrong format:\n%s", out)
	}
}

func TestConsoleSink_PerLineFormat_StepOutcome_Success(t *testing.T) {
	var buf bytes.Buffer
	g := minimalGraph("build", "shell", "compile")
	sink := NewConsoleSink(&buf, []string{"build"}, false, g)
	sink.OnStepEntered("build", "shell", 1)
	sink.OnStepOutcome("build", "success", 1*time.Second, nil)

	out := stripANSI(buf.String())
	if !strings.Contains(out, "[1/1 build · shell(compile)] ✓ success in 1.0s") {
		t.Errorf("outcome line in wrong format:\n%s", out)
	}
}

func TestConsoleSink_PerLineFormat_StepOutcome_Error(t *testing.T) {
	var buf bytes.Buffer
	g := minimalGraph("build", "shell", "compile")
	sink := NewConsoleSink(&buf, []string{"build"}, false, g)
	sink.OnStepEntered("build", "shell", 1)
	sink.OnStepOutcome("build", "failure", 500*time.Millisecond, &stringErr{"something broke"})

	out := stripANSI(buf.String())
	if !strings.Contains(out, "[1/1 build · shell(compile)] ✗ failure: something broke (500ms)") {
		t.Errorf("error outcome line in wrong format:\n%s", out)
	}
}

func TestConsoleSink_PerLineFormat_LineWidth_LongPrefix(t *testing.T) {
	var buf bytes.Buffer
	longStep := strings.Repeat("a", 50)
	longTool := strings.Repeat("b", 60)
	g := minimalGraph(longStep, "shell", "compile")
	sink := NewConsoleSink(&buf, []string{longStep}, false, g)
	sink.OnStepEntered(longStep, "shell", 1)
	stepSink := sink.StepEventSink(longStep)
	stepSink.Adapter("tool.invocation", map[string]any{"name": longTool, "arguments": `{}`})

	out := buf.String()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if len(line) > 160 {
			t.Errorf("line exceeds 160 chars (%d): %q", len(line), line)
		}
	}
}

// TestConsoleSink_PerLineFormat_JsonModeUnchanged confirms that JSON mode
// (LocalSink-only path) is not affected by the new concise rendering.
func TestConsoleSink_PerLineFormat_JsonModeUnchanged(t *testing.T) {
	// Drive a LocalSink directly — the JSON record must be independent of
	// ConsoleSink formatting changes.
	var buf bytes.Buffer
	local := &LocalSink{RunID: "run-json-1", Out: &buf}
	local.OnRunStarted("wf", "step1")
	local.OnStepEntered("step1", "shell", 1)
	stepSink := local.StepEventSink("step1")
	stepSink.Adapter("tool.invocation", map[string]any{"name": "read_file", "arguments": `{"path":"/x/y/z.go"}`})
	local.OnStepOutcome("step1", "success", 100*time.Millisecond, nil)
	local.OnRunCompleted("done", true)

	out := buf.String()
	// JSON mode must not contain any concise-mode prefixes or emojis.
	if strings.Contains(out, "📄") || strings.Contains(out, "⚡") || strings.Contains(out, "✏️") {
		t.Errorf("emoji found in JSON mode output: %s", out)
	}
	if strings.Contains(out, "[1/") {
		t.Errorf("step prefix found in JSON mode output: %s", out)
	}
	// Must be valid ND-JSON (non-empty lines are JSON objects).
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			t.Errorf("non-JSON line in JSON output: %q", line)
		}
	}
}
