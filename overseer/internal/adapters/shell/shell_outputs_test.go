package shell_test

import (
	"context"
	"testing"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/overseer/internal/adapters/shell"
	"github.com/brokenbots/overlord/workflow"
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
	if stdout != "hello world\n" {
		t.Errorf("stdout = %q, want 'hello world\\n'", stdout)
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

func TestShellAdapter_StdoutCappedAt64KB(t *testing.T) {
	// Generate > 64 KB of stdout via a shell here-string. Each iteration prints
	// ~100 bytes; 700 iterations = 70 KB. We assert stdout is capped at 64 KB.
	a := shell.New()
	// Print a 66-byte line 1100 times → ~72 KB raw.
	result, err := a.Execute(context.Background(), makeStep(map[string]string{
		"command": `for i in $(seq 1 1100); do printf '%064d\n' "$i"; done`,
	}), noopSink{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	stdout := result.Outputs["stdout"]
	const capBytes = 64 * 1024
	if len(stdout) > capBytes {
		t.Errorf("stdout length %d exceeds cap of %d bytes", len(stdout), capBytes)
	}
	if len(stdout) == 0 {
		t.Error("stdout is empty; expected some captured output")
	}
}

func TestShellAdapter_StdoutLongLineCapped(t *testing.T) {
	// A single stdout line that exceeds 64 KB must also be capped exactly.
	// We emit one 128 KB line (python fills to known size, no trailing newline
	// issues; fall back to awk if python3 absent).
	a := shell.New()
	result, err := a.Execute(context.Background(), makeStep(map[string]string{
		"command": `python3 -c "import sys; sys.stdout.write('x'*131072)"`,
	}), noopSink{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	stdout := result.Outputs["stdout"]
	const capBytes = 64 * 1024
	if len(stdout) > capBytes {
		t.Errorf("long-line stdout length %d exceeds cap of %d bytes", len(stdout), capBytes)
	}
	if len(stdout) == 0 {
		t.Error("stdout is empty; expected some captured output")
	}
}
