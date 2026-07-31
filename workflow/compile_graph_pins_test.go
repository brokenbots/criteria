package workflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

// TestCompile_MergesTreePinSet verifies that the compiler reads every workflow
// directory's lockfile once and merges them into the root graph's PinSet.
func TestCompile_MergesTreePinSet(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	writePinTestFile(t, filepath.Join(root, "main.chcl"), `
workflow {
  name          = "root"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
}

subworkflow "child" { source = "./child" }

adapter "noop" "rootbot" {
  source  = "ghcr.io/example/criteria-adapter-noop"
  version = "1.0.0"
  config { system_prompt = file("./prompts/root.md") }
}

step "start" {
  target = adapter.noop.rootbot
  input {}
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`)
	writePinTestFile(t, filepath.Join(root, "prompts", "root.md"), "root prompt")
	writePinTestLockfile(t, root, &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{{
			Type:               "noop",
			Name:               "rootbot",
			Reference:          "ghcr.io/example/criteria-adapter-noop",
			Version:            "1.0.0",
			ResolvedDigest:     "sha256:rootdigest",
			SourceURL:          "https://example.com",
			SDKProtocolVersion: 2,
		}},
	})

	writePinTestFile(t, filepath.Join(child, "main.chcl"), `
workflow {
  name          = "child"
  version       = "1"
  initial_state = "work"
  target_state  = "done"
}

adapter "shell" "runner" {
  source  = "ghcr.io/example/criteria-adapter-shell"
  version = "2.0.0"
  config { working_directory = "/tmp" }
}

step "work" {
  target = adapter.shell.runner
  input { command = "echo ok" }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`)
	writePinTestLockfile(t, child, &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{{
			Type:               "shell",
			Name:               "runner",
			Reference:          "ghcr.io/example/criteria-adapter-shell",
			Version:            "2.0.0",
			ResolvedDigest:     "sha256:childdigest",
			SourceURL:          "https://example.com",
			SDKProtocolVersion: 2,
		}},
	})

	resolver := &LocalSubWorkflowResolver{}
	spec, diags := ParseDir(root)
	if diags.HasErrors() {
		t.Fatalf("parse root: %s", diags.Error())
	}

	g, diags := CompileWithContext(context.Background(), spec, nil, CompileOpts{
		WorkflowDir:         root,
		SubWorkflowResolver: resolver,
	})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	if g.PinSet == nil {
		t.Fatal("expected graph.PinSet to be set after compile")
	}
	want := map[string]string{
		"noop.rootbot": "sha256:rootdigest",
		"shell.runner": "sha256:childdigest",
	}
	for key, wantDigest := range want {
		la := findPinTestLocked(g.PinSet, key)
		if la == nil {
			t.Fatalf("expected pin for %q in merged PinSet", key)
		}
		if la.ResolvedDigest != wantDigest {
			t.Fatalf("pin %q digest = %q, want %q", key, la.ResolvedDigest, wantDigest)
		}
	}
}

// TestCompile_CapturesFileCacheForAdapterConfig verifies that file() references
// in adapter config are captured into the graph's FileCache at compile time.
func TestCompile_CapturesFileCacheForAdapterConfig(t *testing.T) {
	t.Helper()
	root := t.TempDir()

	writePinTestFile(t, filepath.Join(root, "main.chcl"), `
workflow {
  name          = "root"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
}

adapter "noop" "bot" {
  config {
    system_prompt = file("./prompts/bot.md")
  }
}

step "start" {
  target = adapter.noop.bot
  input {}
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
`)
	writePinTestFile(t, filepath.Join(root, "prompts", "bot.md"), "compiled prompt content")

	spec, diags := ParseDir(root)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := CompileWithContext(context.Background(), spec, nil, CompileOpts{WorkflowDir: root})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	promptPath := filepath.Join(root, "prompts", "bot.md")
	abs, err := filepath.Abs(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := g.FileCache[abs]
	if !ok {
		t.Fatalf("expected %q in FileCache, got %v", abs, g.FileCache)
	}
	if got != "compiled prompt content" {
		t.Fatalf("cached prompt = %q, want %q", got, "compiled prompt content")
	}
}

func writePinTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writePinTestLockfile(t *testing.T, dir string, lf *lockfile.Lockfile) {
	t.Helper()
	if err := lockfile.Write(filepath.Join(dir, lockfile.LockfileName), lf); err != nil {
		t.Fatal(err)
	}
}

func findPinTestLocked(lf *lockfile.Lockfile, key string) *lockfile.LockedAdapter {
	if lf == nil {
		return nil
	}
	for i := range lf.Adapters {
		la := &lf.Adapters[i]
		if la.Type+"."+la.Name == key {
			return la
		}
	}
	return nil
}
