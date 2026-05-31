package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdapterCmd_NoSubcommand(t *testing.T) {
	cmd := NewAdapterCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{})
	_ = cmd.Execute()
	// Without subcommand it should print help.
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected usage output, got: %s", out.String())
	}
}

func TestAdapterPullCmd_ArgCount(t *testing.T) {
	cmd := newAdapterPullCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{}) // no ref
	_ = cmd.Execute()
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected usage for missing arg, got stdout:\n%s\nstderr:\n%s", out.String(), errOut.String())
	}
}

func TestAdapterLockCmd_NoArgs(t *testing.T) {
	cmd := newAdapterLockCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{t.TempDir()})
	_ = cmd.Execute()
	// Empty workflow dir with no .hcl files returns an error about missing workflow.
}

func TestAdapterListCmd_FlagParsing(t *testing.T) {
	cmd := newAdapterListCmd()
	cmd.SetArgs([]string{"--installed"})
	if err := cmd.Execute(); err != nil {
		// May fail because cache is empty, but flags must parse.
		if strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("flag parsing failed: %v", err)
		}
	}
}

func TestAdapterInfoCmd_MissingArg(t *testing.T) {
	cmd := newAdapterInfoCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{})
	_ = cmd.Execute()
	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("expected usage for missing arg, got: %s", out.String())
	}
}

func TestAdapterWhereCmd_MissingArg(t *testing.T) {
	cmd := newAdapterWhereCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{})
	_ = cmd.Execute()
	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("expected usage for missing arg, got: %s", out.String())
	}
}

func TestAdapterRemoveCmd_MissingArg(t *testing.T) {
	cmd := newAdapterRemoveCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{})
	_ = cmd.Execute()
	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("expected usage for missing arg, got: %s", out.String())
	}
}

func TestAdapterPruneCmd_FlagParsing(t *testing.T) {
	cmd := newAdapterPruneCmd()
	cmd.SetArgs([]string{"--older-than", "30d"})
	if err := cmd.Execute(); err != nil {
		if strings.Contains(err.Error(), "unknown flag") {
			t.Fatalf("flag parsing failed: %v", err)
		}
	}
}

func TestAdapterDevCmd_MissingArg(t *testing.T) {
	cmd := newAdapterDevCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{})
	_ = cmd.Execute()
	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("expected usage for missing arg, got: %s", out.String())
	}
}

func TestAdapterPublishCmd_MissingArg(t *testing.T) {
	cmd := newAdapterPublishCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{})
	_ = cmd.Execute()
	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("expected usage for missing arg, got: %s", out.String())
	}
}

func TestAdapterResolve_ExactReference(t *testing.T) {
	ctx := ResolveContext{}
	ref, err := Resolve(ctx, "ghcr.io/brokenbots/criteria-adapter-noop:1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.String() != "ghcr.io/brokenbots/criteria-adapter-noop:1.0.0" {
		t.Fatalf("unexpected ref: %s", ref.String())
	}
}

func TestAdapterResolve_AliasMissing(t *testing.T) {
	ctx := ResolveContext{}
	_, err := Resolve(ctx, "myalias:1.0.0")
	if err == nil {
		t.Fatal("expected error for missing alias")
	}
}

func TestHasOCIReferences_NoReference(t *testing.T) {
	dir := t.TempDir()
	hcl := `workflow {
  name = "test"
  version = "0.1"
  initial_state = "run"
  target_state = "done"
}

step "run" {
  target = adapter.noop.default
  input { command = "echo hi" }
  outcome "ok" { next = step.done }
}

state "done" {
  terminal = true
}

adapter "noop" "default" {
}
`
	if err := writeFile(filepath.Join(dir, "test.hcl"), []byte(hcl)); err != nil {
		t.Fatal(err)
	}
	spec, _, err := parseCompileForCli(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	oci, err := hasOCIReferences(dir, spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if oci {
		t.Fatal("expected no OCI references")
	}
}

func writeFile(path string, data []byte) error {
	return writeOutput(path, nil, data)
}
