package run

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/brokenbots/criteria/internal/adapter"
)

// ConsoleSink renders engine events as concise human-readable output. It is
// intended for use alongside (not in place of) LocalSink in standalone mode:
// LocalSink writes the canonical ND-JSON record while ConsoleSink renders a
// progress view to a terminal.
type ConsoleSink struct {
	Out   io.Writer
	Steps []string // workflow step order (for "[i/N] step" rendering)
	Color bool     // emit ANSI color codes

	mu        sync.Mutex
	runStart  time.Time
	stepStart map[string]time.Time
	idxByStep map[string]int
}

// NewConsoleSink builds a sink rendering to out. steps is the workflow step
// order from FSMGraph.StepOrder(); color toggles ANSI escapes.
func NewConsoleSink(out io.Writer, steps []string, color bool) *ConsoleSink {
	idx := make(map[string]int, len(steps))
	for i, s := range steps {
		idx[s] = i + 1
	}
	return &ConsoleSink{
		Out:       out,
		Steps:     steps,
		Color:     color,
		stepStart: make(map[string]time.Time),
		idxByStep: idx,
	}
}

func (c *ConsoleSink) writeln(s string) {
	if c == nil || c.Out == nil {
		return
	}
	fmt.Fprintln(c.Out, s)
}

func (c *ConsoleSink) color(code, s string) string {
	if !c.Color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// --- engine.Sink methods ---

func (c *ConsoleSink) OnRunStarted(workflowName, initialStep string) {
	c.mu.Lock()
	c.runStart = time.Now()
	c.mu.Unlock()
	header := fmt.Sprintf("%s %s  steps=%d", c.color("1;36", "▶"), workflowName, len(c.Steps))
	c.writeln(header)
}

func (c *ConsoleSink) OnRunCompleted(finalState string, success bool) {
	c.mu.Lock()
	elapsed := time.Since(c.runStart)
	c.mu.Unlock()
	if success {
		c.writeln(fmt.Sprintf("%s run completed in %s", c.color("1;32", "✔"), formatDuration(elapsed)))
	} else {
		c.writeln(fmt.Sprintf("%s run finished: %s (%s)", c.color("1;31", "✗"), finalState, formatDuration(elapsed)))
	}
}

func (c *ConsoleSink) OnRunFailed(reason, step string) {
	if step != "" {
		c.writeln(fmt.Sprintf("%s run failed at %s: %s", c.color("1;31", "✗"), step, reason))
	} else {
		c.writeln(fmt.Sprintf("%s run failed: %s", c.color("1;31", "✗"), reason))
	}
}

func (c *ConsoleSink) OnStepEntered(step, adapterName string, attempt int) {
	c.mu.Lock()
	c.stepStart[step] = time.Now()
	idx := c.idxByStep[step]
	total := len(c.Steps)
	c.mu.Unlock()

	prefix := ""
	if idx > 0 && total > 0 {
		prefix = fmt.Sprintf("[%d/%d] ", idx, total)
	}
	suffix := ""
	if adapterName != "" {
		suffix = "  (" + adapterName + ")"
	}
	if attempt > 1 {
		suffix += fmt.Sprintf(" attempt=%d", attempt)
	}
	c.writeln(c.color("1", prefix+step) + suffix)
}

func (c *ConsoleSink) OnStepOutcome(step, outcome string, duration time.Duration, err error) {
	c.mu.Lock()
	delete(c.stepStart, step)
	c.mu.Unlock()
	if outcome == "success" && err == nil {
		c.writeln("  " + c.color("32", "✓") + " success in " + formatDuration(duration))
		return
	}
	msg := "  " + c.color("31", "✗") + " " + outcome
	if err != nil {
		msg += ": " + err.Error()
	}
	msg += " (" + formatDuration(duration) + ")"
	c.writeln(msg)
}

func (c *ConsoleSink) OnStepTransition(from, to, viaOutcome string) {
	// Implied by next OnStepEntered; render only when transitioning to a
	// terminal state (no further StepEntered will fire).
	if _, isStep := c.idxByStep[to]; isStep {
		return
	}
	if to == "" {
		return
	}
	c.writeln("  → " + to)
}

func (c *ConsoleSink) OnStepResumed(step string, attempt int, reason string) {
	c.writeln(fmt.Sprintf("%s resumed %s (attempt %d, %s)", c.color("33", "↻"), step, attempt, reason))
}

func (c *ConsoleSink) OnVariableSet(name, value, source string) {
	if source == "" || source == "default" || source == "graph" {
		return
	}
	c.writeln(fmt.Sprintf("  · var %s=%s (%s)", name, truncate(value, 60), source))
}

func (c *ConsoleSink) OnStepOutputCaptured(step string, outputs map[string]string) {
	if len(outputs) == 0 {
		return
	}
	keys := make([]string, 0, len(outputs))
	for k := range outputs {
		keys = append(keys, k)
	}
	c.writeln("  · outputs: " + strings.Join(keys, ", "))
}

func (c *ConsoleSink) OnRunPaused(node, mode, signal string) {}

func (c *ConsoleSink) OnWaitEntered(node, mode, duration, signal string) {
	detail := mode
	if mode == "duration" && duration != "" {
		detail = "duration=" + duration
	} else if signal != "" {
		detail = "signal=" + signal
	}
	c.writeln(fmt.Sprintf("%s wait %s (%s)", c.color("33", "⏸"), node, detail))
}

func (c *ConsoleSink) OnWaitResumed(node, mode, signal string, payload map[string]string) {
	c.writeln(fmt.Sprintf("%s resume %s", c.color("33", "▶"), node))
}

func (c *ConsoleSink) OnApprovalRequested(node string, approvers []string, reason string) {
	c.writeln(fmt.Sprintf("%s approval requested at %s", c.color("33", "⏸"), node))
}

func (c *ConsoleSink) OnApprovalDecision(node, decision, actor string, payload map[string]string) {
	c.writeln(fmt.Sprintf("  · approval %s by %s at %s", decision, actor, node))
}

func (c *ConsoleSink) OnBranchEvaluated(node, matchedArm, target, condition string) {
	c.writeln(fmt.Sprintf("  ↳ branch %s → %s", node, target))
}

func (c *ConsoleSink) OnForEachEntered(node string, count int) {
	c.writeln(fmt.Sprintf("  ↻ iterating %s (%d items)", node, count))
}

func (c *ConsoleSink) OnStepIterationStarted(node string, index int, value string, anyFailed bool) {
	c.writeln(fmt.Sprintf("  ↻ %s [%d] = %s", node, index, truncate(value, 60)))
}

func (c *ConsoleSink) OnStepIterationCompleted(node, outcome, target string) {
	c.writeln(fmt.Sprintf("  ↻ %s → %s (%s)", node, target, outcome))
}

func (c *ConsoleSink) OnStepIterationItem(node string, index int, step string) {
	c.writeln(fmt.Sprintf("  ↻ %s [%d] → %s", node, index, step))
}

func (c *ConsoleSink) OnScopeIterCursorSet(cursorJSON string) {}

func (c *ConsoleSink) StepEventSink(step string) adapter.EventSink {
	return &consoleStepSink{parent: c, step: step}
}

// --- step-level adapter events ---

type consoleStepSink struct {
	parent *ConsoleSink
	step   string
}

// Log drops raw stdout/stderr/agent stream chunks. The complete assistant
// message is rendered from AdapterEvent kind=agent.message instead, and tool
// stdout is summarised by the outcome line.
func (ss *consoleStepSink) Log(stream string, chunk []byte) {}

func (ss *consoleStepSink) Adapter(kind string, data any) {
	switch kind {
	case "agent.message":
		ss.renderAgentMessage(data)
	case "tool.invocation":
		ss.renderToolInvocation(data)
	case "permission.granted":
		ss.parent.writeln("  · permission granted: " + lookupString(data, "tool"))
	case "permission.denied":
		ss.parent.writeln("  · permission denied: " + lookupString(data, "tool"))
	case "limit.reached":
		ss.parent.writeln("  · " + ss.parent.color("33", "limit reached"))
	default:
		// Drop unknown kinds in concise mode; they remain in the ND-JSON record.
	}
}

func (ss *consoleStepSink) renderAgentMessage(data any) {
	// Skip token deltas; only render the complete assistant message.
	eventType := lookupString(data, "event_type")
	if eventType == "assistant.message_delta" {
		return
	}
	content := lookupString(data, "content")
	if strings.TrimSpace(content) == "" {
		return
	}
	prefix := "  " + ss.parent.color("36", "agent:") + " "
	for i, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		if i == 0 {
			ss.parent.writeln(prefix + line)
		} else {
			ss.parent.writeln("    " + line)
		}
	}
}

func (ss *consoleStepSink) renderToolInvocation(data any) {
	name := lookupString(data, "name")
	if name == "" {
		name = "tool"
	}
	args := lookupString(data, "arguments")
	summary := summariseToolArgs(args)
	line := "  " + ss.parent.color("35", "→") + " " + name
	if summary != "" {
		line += " " + summary
	}
	ss.parent.writeln(truncateLine(line, 120))
}

// --- helpers ---

func lookupString(data any, key string) string {
	switch v := data.(type) {
	case map[string]any:
		if s, ok := v[key].(string); ok {
			return s
		}
	case *structpb.Struct:
		if v == nil {
			return ""
		}
		if val, ok := v.Fields[key]; ok && val != nil {
			return val.GetStringValue()
		}
	}
	return ""
}

// summariseToolArgs picks a concise one-line summary from a JSON-encoded
// arguments string. Heuristics: prefer "path"/"file_path" → basename; "cmd"/
// "command" → quoted text; otherwise list keys.
func summariseToolArgs(jsonArgs string) string {
	jsonArgs = strings.TrimSpace(jsonArgs)
	if jsonArgs == "" || jsonArgs == "{}" {
		return ""
	}
	st := &structpb.Struct{}
	// Best-effort: ignore parse errors and fall through to raw truncated string.
	if err := protojsonUnmarshalLoose([]byte(jsonArgs), st); err != nil {
		return truncate(jsonArgs, 80)
	}
	for _, key := range []string{"path", "file_path"} {
		if v, ok := st.Fields[key]; ok {
			if p := v.GetStringValue(); p != "" {
				return key + "=" + filepath.Base(p)
			}
		}
	}
	for _, key := range []string{"cmd", "command"} {
		if v, ok := st.Fields[key]; ok {
			if cmd := v.GetStringValue(); cmd != "" {
				return key + "=" + truncate(strings.TrimSpace(cmd), 60)
			}
		}
	}
	keys := make([]string, 0, len(st.Fields))
	for k := range st.Fields {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return ""
	}
	return "(" + strings.Join(keys, ",") + ")"
}

// protojsonUnmarshalLoose decodes a JSON object into a structpb.Struct. It
// returns an error for non-object JSON; callers fall back to a raw truncated
// string in that case.
func protojsonUnmarshalLoose(b []byte, m *structpb.Struct) error {
	return protojson.Unmarshal(b, m)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func truncateLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%ds", m, s)
}

// IsTerminal reports whether f refers to a character device (TTY).
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return (st.Mode() & os.ModeCharDevice) != 0
}

// ColorEnabled returns true when ANSI color should be emitted for the given
// stream: stream is a TTY and NO_COLOR is unset.
func ColorEnabled(f *os.File) bool {
	if !IsTerminal(f) {
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	return true
}
