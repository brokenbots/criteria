package workflow

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// compileWorkflowV1Fixture compiles an adapter in the named environment type
// with the given adapter capabilities and reports whether a DiagWarning
// mentioning the workflow.v1 advisory fired.
func compileWorkflowV1Fixture(t *testing.T, envType string, caps []string) []string {
	t.Helper()
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}

environment "` + envType + `" "default" {}

adapter "child" "bot" {
  environment = ` + envType + `.default
}

step "work" {
  target = adapter.child.bot
  outcome "success" { next = state.done }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	schemas := map[string]AdapterInfo{
		"child": {Capabilities: caps},
	}
	_, diags = Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	var warnings []string
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && strings.Contains(d.Summary, "workflow.v1") {
			warnings = append(warnings, d.Summary)
		}
	}
	return warnings
}

// TestCompileWorkflowV1Advisory_ContainerEnv warns when a workflow.v1-capable
// adapter is bound to a container environment: containers are typically built
// without the criteria binary, so the pairing is advisory (ADR-0008 D2).
func TestCompileWorkflowV1Advisory_ContainerEnv(t *testing.T) {
	warnings := compileWorkflowV1Fixture(t, "container", []string{workflowV1Capability})
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one workflow.v1 warning, got %v", warnings)
	}
	if !strings.Contains(warnings[0], "container") {
		t.Errorf("warning should name the environment type, got: %s", warnings[0])
	}
}

// TestCompileWorkflowV1Advisory_RemoteEnv warns for the remote shim type as
// well: only local shell processes are legitimate standalone child hosts.
func TestCompileWorkflowV1Advisory_RemoteEnv(t *testing.T) {
	warnings := compileWorkflowV1Fixture(t, "remote", []string{workflowV1Capability})
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one workflow.v1 warning, got %v", warnings)
	}
	if !strings.Contains(warnings[0], "remote") {
		t.Errorf("warning should name the environment type, got: %s", warnings[0])
	}
}

// TestCompileWorkflowV1Advisory_ShellEnv is the reference deployment (two
// processes on one machine): no advisory.
func TestCompileWorkflowV1Advisory_ShellEnv(t *testing.T) {
	if warnings := compileWorkflowV1Fixture(t, "shell", []string{workflowV1Capability}); len(warnings) != 0 {
		t.Fatalf("shell environments host criteria legitimately; unexpected warnings: %v", warnings)
	}
}

// TestCompileWorkflowV1Advisory_OtherAdapterNoWarning keeps ordinary adapters
// (no workflow.v1 capability) silent in container/remote environments.
func TestCompileWorkflowV1Advisory_OtherAdapterNoWarning(t *testing.T) {
	for _, envType := range []string{"container", "remote", "shell", "sandbox"} {
		if warnings := compileWorkflowV1Fixture(t, envType, nil); len(warnings) != 0 {
			t.Fatalf("non-workflow adapter in %q: unexpected warnings: %v", envType, warnings)
		}
	}
}
