package shell_test

import (
	"context"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapters/shell"
	"github.com/brokenbots/criteria/workflow"
)

type noopSink struct{}

func (noopSink) Log(string, []byte)  {}
func (noopSink) Adapter(string, any) {}

func makeStep(input map[string]string) *workflow.StepNode {
	return &workflow.StepNode{
		Name:    "test",
		Adapter: shell.Name,
		Input:   input,
		Outcomes: map[string]string{
			"success": "__done__",
			"failure": "__done__",
		},
	}
}

func TestShellAdapter_CapturesStdout(t *testing.T) {
	a := shell.New()
	result, err := a.Execute(context.Background(), makeStep(map[string]string{
		"command": "printf 'hello world'",
	}), noopSink{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != "success" {
		t.Errorf("outcome = %q, want 'success'", result.Outcome)
	}
	stdout, ok := result.Outputs["stdout"]
	if !ok {
		t.Fatal("missing 'stdout' in Outputs")
	}
	// chunk-based reader preserves exact output; printf without \n produces no trailing newline.
	if stdout != "hello world" {
		t.Errorf("stdout = %q, want 'hello world'", stdout)
	}
	exitCode, ok := result.Outputs["exit_code"]
	if !ok {
		t.Fatal("missing 'exit_code' in Outputs")
	}
	if exitCode != "0" {
		t.Errorf("exit_code = %q, want '0'", exitCode)
	}
}

func TestShellAdapter_CapturesExitCode(t *testing.T) {
	a := shell.New()
	result, err := a.Execute(context.Background(), makeStep(map[string]string{
		"command": "exit 2",
	}), noopSink{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != "failure" {
		t.Errorf("outcome = %q, want 'failure'", result.Outcome)
	}
	exitCode, ok := result.Outputs["exit_code"]
	if !ok {
		t.Fatal("missing 'exit_code' in Outputs")
	}
	if exitCode != "2" {
		t.Errorf("exit_code = %q, want '2'", exitCode)
	}
}

func TestShellAdapter_OutputSchema(t *testing.T) {
	a := shell.New()
	info := a.Info()
	if _, ok := info.OutputSchema["stdout"]; !ok {
		t.Error("OutputSchema missing 'stdout'")
	}
	if _, ok := info.OutputSchema["exit_code"]; !ok {
		t.Error("OutputSchema missing 'exit_code'")
	}
}

func TestShellAdapter_OutputsContainedInResult(t *testing.T) {
	a := shell.New()
	result, err := a.Execute(context.Background(), makeStep(map[string]string{
		"command": "echo line1 && echo line2",
	}), noopSink{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = result.Outputs["stdout"] // must exist; content checked elsewhere
	if result.Outputs == nil {
		t.Error("Outputs map is nil")
	}
}

func TestShellAdapter_MissingCommand(t *testing.T) {
	a := shell.New()
	result, err := a.Execute(context.Background(), makeStep(map[string]string{}), noopSink{})
	if err == nil {
		t.Error("expected error for missing command")
	}
	_ = result
}

// Verify that the adapter.Adapter interface is satisfied.
var _ adapter.Adapter = (*shell.Adapter)(nil)

func TestShellAdapter_StdoutCappedAtDefaultLimit(t *testing.T) {
	// The default per-stream cap is 4 MiB (W05). We generate ~72 KB of output
	// which is well under the cap; all of it should be captured without truncation.
	a := shell.New()
	// Print a 66-byte line 1100 times → ~72 KB raw.
	result, err := a.Execute(context.Background(), makeStep(map[string]string{
		"command": `for i in $(seq 1 1100); do printf '%064d\n' "$i"; done`,
	}), noopSink{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	stdout := result.Outputs["stdout"]
	const defaultCap = 4 * 1024 * 1024
	if len(stdout) > defaultCap {
		t.Errorf("stdout length %d exceeds default cap of %d bytes", len(stdout), defaultCap)
	}
	if stdout == "" {
		t.Error("stdout is empty; expected some captured output")
	}
	// No truncation sentinel should be present.
	if result.Outputs["_truncated_stdout"] != "" {
		t.Errorf("unexpected truncation sentinel; stdout len=%d", len(stdout))
	}
}

func TestShellAdapter_StdoutExplicitSmallCapTriggersEvent(t *testing.T) {
	// Use an explicit output_limit_bytes well below the generated output so we
	// can assert the truncation event and sentinel key are present.
	a := shell.New()
	// 1100 * ~65 bytes ≈ 72 KB; limit to 1 KiB to guarantee truncation.
	var events []map[string]any
	evSink := &collectSink{events: &events}
	result, err := a.Execute(context.Background(), makeStep(map[string]string{
		"command":            `python3 -c "import sys; sys.stdout.write('x'*131072)"`,
		"output_limit_bytes": "1024",
	}), evSink)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	stdout := result.Outputs["stdout"]
	if len(stdout) > 1024 {
		t.Errorf("stdout length %d exceeds explicit cap of 1024", len(stdout))
	}
	if stdout == "" {
		t.Error("stdout is empty; expected some captured output")
	}
	if result.Outputs["_truncated_stdout"] != "true" {
		t.Error("expected _truncated_stdout sentinel to be set")
	}
	found := false
	for _, ev := range events {
		if ev["event_type"] == "output_truncated" && ev["stream"] == "stdout" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected output_truncated adapter event; got: %v", events)
	}
}

// collectSink records Adapter events for test assertions.
type collectSink struct {
	events *[]map[string]any
}

func (s *collectSink) Log(string, []byte) {}
func (s *collectSink) Adapter(_ string, data any) {
	if m, ok := data.(map[string]any); ok {
		*s.events = append(*s.events, m)
	}
}
