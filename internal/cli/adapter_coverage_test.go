package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

// TestLoadTreePinSet_MergesSubworkflowPins verifies that loadTreePinSet resolves
// and merges lockfiles from the root workflow and every reachable local
// subworkflow directory into one in-memory pin set. This is the same merged set
// used by the startup coverage gate and by the engine, so the two cannot
// disagree.
func TestLoadTreePinSet_MergesSubworkflowPins(t *testing.T) {
	ctx := context.Background()
	rootDir, childDir := makeCoverageTree(t)

	pinSet, err := loadTreePinSet(ctx, rootDir)
	if err != nil {
		t.Fatalf("loadTreePinSet: %v", err)
	}
	if pinSet == nil {
		t.Fatal("expected a merged pin set, got nil")
	}

	rootEntry := findLocked(pinSet, "noop", "root")
	if rootEntry == nil {
		t.Fatal("root adapter pin missing from merged pin set")
	}
	if rootEntry.ResolvedDigest != "sha256:rootsuffix" {
		t.Errorf("root digest = %q, want sha256:rootsuffix", rootEntry.ResolvedDigest)
	}

	childEntry := findLocked(pinSet, "noop", "child")
	if childEntry == nil {
		t.Fatal("child adapter pin missing from merged pin set")
	}
	if childEntry.ResolvedDigest != "sha256:childsuffix" {
		t.Errorf("child digest = %q, want sha256:childsuffix", childEntry.ResolvedDigest)
	}

	// The engine and coverage helpers both use the merged set, so an adapter
	// reachable only through a subworkflow is visible to startup verification.
	unpinned, err := collectUnpinnedAdaptersWithPinSet(ctx, rootDir, pinSet)
	if err != nil {
		t.Fatalf("collectUnpinnedAdaptersWithPinSet: %v", err)
	}
	if len(unpinned) != 0 {
		t.Fatalf("expected no unpinned adapters with merged pin set, got %v", unpinned)
	}

	_ = childDir // kept for symmetry; child path is derived inside makeCoverageTree
}

// TestCollectUnpinnedAdaptersWithPinSet_NamesSubworkflowDir verifies that when
// a subworkflow adapter has no lockfile entry, the reported error names the
// subworkflow directory and the correct remediation command.
func TestCollectUnpinnedAdaptersWithPinSet_NamesSubworkflowDir(t *testing.T) {
	ctx := context.Background()
	rootDir, childDir := makeCoverageTree(t)

	// Use only the root lockfile as the pin set, simulating a disagreement
	// between what apply setup saw and the compiled graph the engine will use.
	pinSet, err := lockfile.ReadFromDir(rootDir)
	if err != nil {
		t.Fatalf("read root lockfile: %v", err)
	}
	if pinSet == nil {
		t.Fatal("root lockfile missing")
	}

	unpinned, err := collectUnpinnedAdaptersWithPinSet(ctx, rootDir, pinSet)
	if err != nil {
		t.Fatalf("collectUnpinnedAdaptersWithPinSet: %v", err)
	}
	if len(unpinned) != 1 {
		t.Fatalf("expected one unpinned adapter, got %d: %v", len(unpinned), unpinned)
	}
	e := unpinned[0]
	if e.WorkflowDir != childDir {
		t.Errorf("error workflow dir = %q, want %q", e.WorkflowDir, childDir)
	}
	if e.AdapterKey != "noop.child" {
		t.Errorf("error adapter key = %q, want noop.child", e.AdapterKey)
	}
	wantCmd := "criteria adapter lock " + childDir
	if !strings.Contains(e.Error(), wantCmd) {
		t.Errorf("error %q does not contain remediation command %q", e.Error(), wantCmd)
	}
}

func makeCoverageTree(t *testing.T) (rootDir, childDir string) {
	t.Helper()
	rootDir = t.TempDir()
	var err error
	if rootDir, err = filepath.EvalSymlinks(rootDir); err != nil {
		t.Fatalf("resolve root dir: %v", err)
	}
	childDir = filepath.Join(rootDir, "child")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}

	writeCoverageFile(t, filepath.Join(rootDir, "main.chcl"), `
workflow {
  name          = "root"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

subworkflow "child" {
  source = "./child"
}

adapter "noop" "root" {
  source = "oci://example/root"
}
`)
	writeCoverageLockfile(t, rootDir, lockfile.LockedAdapter{
		Type:           "noop",
		Name:           "root",
		Reference:      "oci://example/root:latest",
		ResolvedDigest: "sha256:rootsuffix",
		SourceURL:      "oci://example/root@sha256:rootsuffix",
	})

	writeCoverageFile(t, filepath.Join(childDir, "main.chcl"), `
workflow {
  name          = "child"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

adapter "noop" "child" {
  source = "oci://example/child"
}
`)
	writeCoverageLockfile(t, childDir, lockfile.LockedAdapter{
		Type:           "noop",
		Name:           "child",
		Reference:      "oci://example/child:latest",
		ResolvedDigest: "sha256:childsuffix",
		SourceURL:      "oci://example/child@sha256:childsuffix",
	})

	return rootDir, childDir
}

func writeCoverageFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func writeCoverageLockfile(t *testing.T, dir string, adapters ...lockfile.LockedAdapter) {
	t.Helper()
	lf := &lockfile.Lockfile{SchemaVersion: 1, Adapters: adapters}
	if err := lockfile.Write(filepath.Join(dir, lockfile.LockfileName), lf); err != nil {
		t.Fatalf("write lockfile: %v", err)
	}
}
