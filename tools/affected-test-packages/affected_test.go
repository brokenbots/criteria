package main

import (
	"reflect"
	"sort"
	"testing"
)

func mockPackages() []*PackageInfo {
	return []*PackageInfo{
		{
			ImportPath:  "github.com/brokenbots/criteria/cmd/criteria",
			Dir:         "/mock/cmd/criteria",
			GoFiles:     []string{"main.go"},
			TestGoFiles: nil,
			Imports: []string{
				"github.com/brokenbots/criteria/internal/cli",
			},
		},
		{
			ImportPath:   "github.com/brokenbots/criteria/internal/cli",
			Dir:          "/mock/internal/cli",
			GoFiles:      []string{"compile.go", "apply.go"},
			TestGoFiles:  []string{"compile_test.go"},
			XTestGoFiles: nil,
			Imports: []string{
				"github.com/brokenbots/criteria/internal/engine",
				"github.com/brokenbots/criteria/workflow",
			},
		},
		{
			ImportPath:  "github.com/brokenbots/criteria/internal/engine",
			Dir:         "/mock/internal/engine",
			GoFiles:     []string{"engine.go"},
			TestGoFiles: []string{"engine_test.go"},
			Imports: []string{
				"github.com/brokenbots/criteria/workflow",
				"github.com/brokenbots/criteria/sdk",
			},
		},
		{
			ImportPath:  "github.com/brokenbots/criteria/workflow",
			Dir:         "/mock/workflow",
			GoFiles:     []string{"workflow.go"},
			TestGoFiles: []string{"workflow_test.go"},
			Imports:     nil,
		},
		{
			ImportPath:  "github.com/brokenbots/criteria/sdk",
			Dir:         "/mock/sdk",
			GoFiles:     []string{"sdk.go"},
			TestGoFiles: []string{"sdk_test.go"},
			Imports:     nil,
		},
	}
}

func TestComputeAffected_NonCodeFilesOnly(t *testing.T) {
	pkgs := mockPackages()
	res := ComputeAffected("/mock", pkgs, []string{"README.md", "docs/architecture.md", "examples/hello.hcl"}, false)

	if res.All {
		t.Error("expected All=false for doc changes")
	}
	if len(res.Affected) != 0 {
		t.Errorf("expected 0 affected packages, got %v", res.Affected)
	}
	if len(res.Testable) != 0 {
		t.Errorf("expected 0 testable packages, got %v", res.Testable)
	}
	if res.ConformanceAffected {
		t.Error("expected ConformanceAffected=false")
	}
}

func TestComputeAffected_GlobalTrigger(t *testing.T) {
	pkgs := mockPackages()
	for _, file := range []string{"go.work", "go.mod", "Makefile", "tools/affected-test-packages/main.go"} {
		res := ComputeAffected("/mock", pkgs, []string{file}, false)
		if !res.All {
			t.Errorf("%s: expected All=true", file)
		}
		if len(res.Affected) != len(pkgs) {
			t.Errorf("%s: expected %d packages, got %d", file, len(pkgs), len(res.Affected))
		}
		if !res.ConformanceAffected {
			t.Errorf("%s: expected ConformanceAffected=true", file)
		}
	}
}

func TestComputeAffected_LeafPackageChanged(t *testing.T) {
	pkgs := mockPackages()
	// Change in internal/cli/compile.go
	res := ComputeAffected("/mock", pkgs, []string{"internal/cli/compile.go"}, false)

	if res.All {
		t.Error("expected All=false")
	}

	// Directly changed: internal/cli
	wantDirect := []string{"github.com/brokenbots/criteria/internal/cli"}
	if !reflect.DeepEqual(res.DirectlyChanged, wantDirect) {
		t.Errorf("DirectlyChanged = %v; want %v", res.DirectlyChanged, wantDirect)
	}

	// Affected: internal/cli + cmd/criteria (which imports internal/cli)
	wantAffected := []string{
		"github.com/brokenbots/criteria/cmd/criteria",
		"github.com/brokenbots/criteria/internal/cli",
	}
	sort.Strings(wantAffected)
	if !reflect.DeepEqual(res.Affected, wantAffected) {
		t.Errorf("Affected = %v; want %v", res.Affected, wantAffected)
	}

	// Testable: cmd/criteria has no test files, so only internal/cli
	wantTestable := []string{"github.com/brokenbots/criteria/internal/cli"}
	if !reflect.DeepEqual(res.Testable, wantTestable) {
		t.Errorf("Testable = %v; want %v", res.Testable, wantTestable)
	}

	// Neither sdk, workflow, nor internal/adapter changed
	if res.ConformanceAffected {
		t.Error("expected ConformanceAffected=false for internal/cli change")
	}
}

func TestComputeAffected_RootDependencyChanged(t *testing.T) {
	pkgs := mockPackages()
	// Change in workflow/workflow.go -> workflow, engine, cli, cmd are all affected
	res := ComputeAffected("/mock", pkgs, []string{"workflow/workflow.go"}, false)

	wantAffected := []string{
		"github.com/brokenbots/criteria/cmd/criteria",
		"github.com/brokenbots/criteria/internal/cli",
		"github.com/brokenbots/criteria/internal/engine",
		"github.com/brokenbots/criteria/workflow",
	}
	sort.Strings(wantAffected)
	if !reflect.DeepEqual(res.Affected, wantAffected) {
		t.Errorf("Affected = %v; want %v", res.Affected, wantAffected)
	}

	// sdk does not import workflow, so sdk is not affected
	for _, imp := range res.Affected {
		if imp == "github.com/brokenbots/criteria/sdk" {
			t.Errorf("sdk should not be affected by workflow change")
		}
	}

	// Workflow is conformance-critical
	if !res.ConformanceAffected {
		t.Error("expected ConformanceAffected=true for workflow change")
	}
}

func TestComputeAffected_ModuleGoModChanged(t *testing.T) {
	pkgs := mockPackages()
	// Change in sdk/go.mod
	res := ComputeAffected("/mock", pkgs, []string{"sdk/go.mod"}, false)

	// sdk is changed, and engine depends on sdk, cli depends on engine, cmd depends on cli
	foundSDK := false
	foundEngine := false
	for _, imp := range res.Affected {
		if imp == "github.com/brokenbots/criteria/sdk" {
			foundSDK = true
		}
		if imp == "github.com/brokenbots/criteria/internal/engine" {
			foundEngine = true
		}
	}
	if !foundSDK || !foundEngine {
		t.Errorf("expected sdk and engine in affected, got %v", res.Affected)
	}
	if !res.ConformanceAffected {
		t.Error("expected ConformanceAffected=true for sdk/go.mod change")
	}
}
