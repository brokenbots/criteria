package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeProfile writes a synthetic coverage profile and returns its path.
func writeProfile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cover.out")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAddProfile_StatementWeighted(t *testing.T) {
	// internal/cli: 3 covered + 1 covered of 6 total stmts -> 4/6 = 66.66%.
	// workflow: 10 of 10 -> 100%.
	prof := writeProfile(t, `mode: atomic
github.com/brokenbots/criteria/internal/cli/a.go:1.1,3.2 3 1
github.com/brokenbots/criteria/internal/cli/a.go:4.1,6.2 2 0
github.com/brokenbots/criteria/internal/cli/b.go:1.1,2.2 1 5
github.com/brokenbots/criteria/workflow/eval.go:1.1,9.2 10 1
`)
	p := newPkgCov()
	if err := p.addProfile(prof); err != nil {
		t.Fatalf("addProfile: %v", err)
	}

	if got, ok := p.pct("internal/cli"); !ok || got < 66.6 || got > 66.7 {
		t.Errorf("internal/cli pct = %.2f, ok=%v; want ~66.67", got, ok)
	}
	if got, ok := p.pct("workflow"); !ok || got != 100 {
		t.Errorf("workflow pct = %.2f, ok=%v; want 100", got, ok)
	}
	if _, ok := p.pct("does/not/exist"); ok {
		t.Error("unmeasured package reported as measured")
	}
}

func TestAddProfile_Aggregates(t *testing.T) {
	// Two profiles contributing to the same package must sum.
	p := newPkgCov()
	for _, body := range []string{
		"mode: atomic\ngithub.com/brokenbots/criteria/workflow/a.go:1.1,2.2 4 1\n",
		"mode: set\ngithub.com/brokenbots/criteria/workflow/b.go:1.1,2.2 4 0\n",
	} {
		if err := p.addProfile(writeProfile(t, body)); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := p.pct("workflow"); got != 50 {
		t.Errorf("workflow pct = %.1f; want 50 (4 of 8 stmts)", got)
	}
}

func TestAddProfile_Malformed(t *testing.T) {
	prof := writeProfile(t, "mode: atomic\ngithub.com/x/y.go:1.1,2.2 notanumber 1\n")
	if err := newPkgCov().addProfile(prof); err == nil {
		t.Fatal("expected error on malformed statement count")
	}
}

func TestFloorDown(t *testing.T) {
	cases := map[float64]float64{87.34: 87.0, 87.55: 87.5, 87.0: 87.0, 99.99: 99.5, 100: 100}
	for in, want := range cases {
		if got := floorDown(in); got != want {
			t.Errorf("floorDown(%.2f) = %.2f; want %.2f", in, got, want)
		}
	}
}

func TestIsExcluded(t *testing.T) {
	excluded := []string{
		"internal/adapter/environment/sandbox",
		"internal/adapter/environment/sandbox/testfixture",
		"internal/adapterhost",
		"cmd/criteria-adapter-mcp",
		"sdk/pb/criteria/v1",
	}
	for _, p := range excluded {
		if !isExcluded(p) {
			t.Errorf("%s should be excluded", p)
		}
	}
	for _, p := range []string{"internal/cli", "workflow", "internal/adapter/manifest"} {
		if isExcluded(p) {
			t.Errorf("%s should not be excluded", p)
		}
	}
}

func TestCheck_BelowAndAbove(t *testing.T) {
	p := newPkgCov()
	p.total["internal/cli"] = 100
	p.covered["internal/cli"] = 70 // 70%
	p.total["workflow"] = 100
	p.covered["workflow"] = 90 // 90%

	// workflow above floor, internal/cli below -> fail.
	if !check(p, []floor{{"workflow", 85}, {"internal/cli", 75}}) {
		t.Error("expected failure when a package is below floor")
	}
	// both at/above floor -> pass.
	if check(p, []floor{{"workflow", 90}, {"internal/cli", 70}}) {
		t.Error("expected pass when all packages meet floor")
	}
}

func TestCheck_MissingPackage(t *testing.T) {
	if !check(newPkgCov(), []floor{{"internal/cli", 50}}) {
		t.Error("expected failure when a floored package has no coverage data")
	}
}

func TestReadFloors(t *testing.T) {
	f := writeProfile(t, "# comment\n\ninternal/cli 72.5\nworkflow 85.0\n")
	floors, err := readFloors(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(floors) != 2 || floors[0] != (floor{"internal/cli", 72.5}) {
		t.Errorf("unexpected floors: %+v", floors)
	}
}
