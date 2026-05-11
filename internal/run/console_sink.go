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
	"github.com/brokenbots/criteria/workflow"
)

// ConsoleSink renders engine events as concise human-readable output. It is
// intended for use alongside (not in place of) LocalSink in standalone mode:
// LocalSink writes the canonical ND-JSON record while ConsoleSink renders a
// progress view to a terminal.
type ConsoleSink struct {
	Out   io.Writer
	Steps []string // workflow step order (for "[i/N] step" rendering)
	Color bool     // emit ANSI color codes
	// Graph is an optional reference to the compiled FSMGraph. When non-nil,
	// adapter type and name information is sourced from the graph for richer
	// per-line prefix rendering.
	Graph *workflow.FSMGraph

	mu            sync.Mutex
	runStart      time.Time
	stepStart     map[string]time.Time
	idxByStep     map[string]int
	stepLifecycle map[string][]string // stepName → lifecycle event strings
	// adapterByStep maps step name to the adapter's instance name (refName) and
	// type (kind), populated by OnStepEntered. Used to build the per-line prefix
	// in name(type) order, e.g. "default(shell)".
	adapterByStep map[string]struct{ refName, kind string }
}

// NewConsoleSink builds a sink rendering to out. steps is the workflow step
// order from FSMGraph.StepOrder(); color toggles ANSI escapes; graph, when
// non-nil, enriches per-line prefix rendering with adapter type information.
func NewConsoleSink(out io.Writer, steps []string, color bool, graph *workflow.FSMGraph) *ConsoleSink {
	idx := make(map[string]int, len(steps))
	for i, s := range steps {
		idx[s] = i + 1
	}
	return &ConsoleSink{
		Out:           out,
		Steps:         steps,
		Color:         color,
		Graph:         graph,
		stepStart:     make(map[string]time.Time),
		idxByStep:     idx,
		stepLifecycle: make(map[string][]string),
		adapterByStep: make(map[string]struct{ refName, kind string }),
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
	// Populate adapterByStep before building prefix so buildLinePrefix finds it.
	c.adapterByStep[step] = c.resolveAdapter(step, adapterName)
	c.mu.Unlock()

	adapterRef, adapterType := c.adapterFor(step)
	if adapterRef == "" {
		adapterRef = "?"
	}
	if adapterType == "" {
		adapterType = "?"
	}
	line := fmt.Sprintf("[%d/%d %s · %s(%s)]", idx, total, c.color("1", step), adapterRef, adapterType)
	if attempt > 1 {
		line += fmt.Sprintf(" attempt=%d", attempt)
	}
	c.writeln(c.color("1;36", "▶") + " " + line)
}

func (c *ConsoleSink) OnStepOutcome(step, outcome string, duration time.Duration, err error) {
	c.mu.Lock()
	delete(c.stepStart, step)
	events := c.stepLifecycle[step]
	delete(c.stepLifecycle, step)
	c.mu.Unlock()

	tag := c.adapterLifecycleTag(events)
	prefix := c.buildLinePrefix(step)
	if err == nil && (outcome == "success" || outcome == "ok") {
		c.writeln(prefix + c.color("1;32", "✓") + " " + outcome + " in " + formatDuration(duration) + tag)
		return
	}
	var body string
	if err != nil {
		body = outcome + ": " + err.Error() + " (" + formatDuration(duration) + ")" + tag
	} else {
		body = outcome + " (" + formatDuration(duration) + ")" + tag
	}
	c.writeln(prefix + c.color("1;31", "✗") + " " + body)
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
	prefix := c.buildLinePrefix(step)
	c.writeln(fmt.Sprintf("%s%s resumed (attempt %d, %s)", prefix, c.color("33", "↻"), attempt, reason))
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
	prefix := c.buildLinePrefix(step)
	c.writeln(prefix + "· outputs: " + strings.Join(keys, ", "))
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
	prefix := c.buildLinePrefix(node)
	c.writeln(fmt.Sprintf("%s↻ iterating %s (%d items)", prefix, node, count))
}

func (c *ConsoleSink) OnStepIterationStarted(node string, index int, value string, anyFailed bool) {
	prefix := c.buildLinePrefix(node)
	c.writeln(fmt.Sprintf("%s↻ %s [%d] = %s", prefix, node, index, truncate(value, 60)))
}

func (c *ConsoleSink) OnStepIterationCompleted(node, outcome, target string) {
	prefix := c.buildLinePrefix(node)
	c.writeln(fmt.Sprintf("%s↻ %s → %s (%s)", prefix, node, target, outcome))
}

func (c *ConsoleSink) OnStepIterationItem(node string, index int, step string) {
	prefix := c.buildLinePrefix(node)
	c.writeln(fmt.Sprintf("%s↻ %s [%d] → %s", prefix, node, index, step))
}

func (c *ConsoleSink) OnScopeIterCursorSet(cursorJSON string) {}

// OnAdapterLifecycle records the adapter lifecycle status for the step. The
// accumulated events are rendered as an [adapter: ...] tag in OnStepOutcome.
func (c *ConsoleSink) OnAdapterLifecycle(stepName, adapterName, status, detail string) {
	entry := status
	if detail != "" {
		entry = status + ": " + truncate(detail, 60)
	}
	c.mu.Lock()
	c.stepLifecycle[stepName] = append(c.stepLifecycle[stepName], entry)
	c.mu.Unlock()
}

// OnRunOutputs renders workflow outputs to the console (W09).
// Outputs are rendered after the terminal state line in concise output mode.
func (c *ConsoleSink) OnRunOutputs(outputs []map[string]string) {
	if len(outputs) == 0 {
		return
	}
	for _, out := range outputs {
		name := out["name"]
		value := out["value"]
		typeStr := out["declared_type"]
		if typeStr != "" {
			c.writeln(fmt.Sprintf("  output %s (%s) = %s", name, typeStr, value))
		} else {
			c.writeln(fmt.Sprintf("  output %s = %s", name, value))
		}
	}
}

// OnStepOutcomeDefaulted logs a warning when an unknown outcome is mapped to
// the default_outcome (W15).
func (c *ConsoleSink) OnStepOutcomeDefaulted(step, original, mapped string) {
	prefix := c.buildLinePrefix(step)
	c.writeln(prefix + fmt.Sprintf("⚠ unknown outcome %q mapped to default_outcome %q", original, mapped))
}

// OnStepOutcomeUnknown logs a warning before the run fails due to an unmapped
// outcome and no default_outcome (W15).
func (c *ConsoleSink) OnStepOutcomeUnknown(step, outcome string) {
	prefix := c.buildLinePrefix(step)
	c.writeln(prefix + fmt.Sprintf("✗ unmapped outcome %q (no default_outcome declared)", outcome))
}

func (c *ConsoleSink) StepEventSink(step string) adapter.EventSink {
	prefix := c.buildLinePrefix(step)
	return &consoleStepSink{parent: c, step: step, prefix: prefix}
}

// --- step-level adapter events ---

type consoleStepSink struct {
	parent *ConsoleSink
	step   string
	// prefix is the precomputed "[I/N step · adapter(type)] " string built at
	// construction time. Empty string disables prefixing (defensive default).
	prefix string
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
		ss.parent.writeln(ss.prefix + "· permission granted: " + lookupString(data, "tool"))
	case "permission.denied":
		ss.parent.writeln(ss.prefix + "· permission denied: " + lookupString(data, "tool"))
	case "limit.reached":
		ss.parent.writeln(ss.prefix + ss.parent.color("33", "limit reached"))
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
	agentTag := ss.parent.color("36", "agent:")
	for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		ss.parent.writeln(ss.prefix + agentTag + " " + line)
	}
}

func (ss *consoleStepSink) renderToolInvocation(data any) {
	name := lookupString(data, "name")
	if name == "" {
		name = "tool"
	}
	args := lookupString(data, "arguments")
	summary := summariseToolArgs(args)
	emoji := toolEmoji(name)
	line := ss.prefix + emoji + " " + name
	if summary != "" {
		line += " " + summary
	}
	ss.parent.writeln(truncateLine(line, 160))
}

// --- helpers ---

// buildLinePrefix returns "[I/N step · adapter(type)] " for per-event lines.
// The prefix is dim-colored when Color is enabled. Returns "" when the step is
// not registered in idxByStep (defensive: no crash, just no prefix).
func (c *ConsoleSink) buildLinePrefix(step string) string {
	c.mu.Lock()
	idx, ok := c.idxByStep[step]
	c.mu.Unlock()
	if !ok {
		return ""
	}
	total := len(c.Steps)
	adapterRef, adapterType := c.adapterFor(step)
	if adapterRef == "" {
		adapterRef = "?"
	}
	if adapterType == "" {
		adapterType = "?"
	}
	inner := fmt.Sprintf("[%d/%d %s · %s(%s)]", idx, total, step, adapterRef, adapterType)
	return c.color("2", inner) + " "
}

// adapterFor returns the adapter instance name (refName) and type (kind) for a
// step, as stored by OnStepEntered in adapterByStep. The rendered prefix uses
// these in name(type) order, e.g. refName="default", kind="shell" → "default(shell)".
func (c *ConsoleSink) adapterFor(step string) (refName, kind string) {
	c.mu.Lock()
	a, ok := c.adapterByStep[step]
	c.mu.Unlock()
	if !ok {
		return "", ""
	}
	return a.refName, a.kind
}

// resolveAdapter returns the adapter ref-name and type for a step. When the
// Graph is available, values are sourced from the compiled adapter declaration:
// refName is the instance name (second HCL literal, e.g. "default") and kind
// is the adapter type (first HCL literal, e.g. "shell"). The format rendered
// in the prefix is therefore name(type), e.g. "default(shell)".
// Without a Graph, adapterName (the type string from the engine) is used as
// refName with an empty kind rendered as "?".
func (c *ConsoleSink) resolveAdapter(step, adapterName string) struct{ refName, kind string } {
	if c.Graph != nil {
		if stepNode, ok := c.Graph.Steps[step]; ok && stepNode.AdapterRef != "" {
			if adapterDecl, ok := c.Graph.Adapters[stepNode.AdapterRef]; ok {
				return struct{ refName, kind string }{refName: adapterDecl.Name, kind: adapterDecl.Type}
			}
		}
	}
	// Fallback: engine passes the type as adapterName; kind is unknown.
	return struct{ refName, kind string }{refName: adapterName, kind: ""}
}

// adapterLifecycleTag returns the [adapter: ...] tag string for the step's
// accumulated lifecycle events, or "" when no events were recorded.
func (c *ConsoleSink) adapterLifecycleTag(events []string) string {
	if len(events) == 0 {
		return ""
	}
	return "  " + c.color("2", "[adapter: "+strings.Join(events, " → ")+"]")
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
