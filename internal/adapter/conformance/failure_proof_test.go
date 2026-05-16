package conformance_test

import (
	"os/exec"
	"strings"
	"testing"
)

func runBrokenAdapterFixtureAssertion(t *testing.T) {
	cmd := exec.Command(
		"go",
		"test",
		"-tags",
		"conformancefail",
		"./internal/adapter/conformance",
		"-run",
		"TestBrokenAdapterConformanceFixture",
	)
	cmd.Dir = "../../.."
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected broken fixture conformance test to fail, but it passed\noutput:\n%s", out)
	}
	outText := string(out)
	if !strings.Contains(outText, "outcome_domain") {
		t.Fatalf("expected failing sub-test outcome_domain in output\noutput:\n%s", outText)
	}
}

func TestConformanceHarnessDetectsBrokenAdapterFixture(t *testing.T) {
	runBrokenAdapterFixtureAssertion(t)
}
