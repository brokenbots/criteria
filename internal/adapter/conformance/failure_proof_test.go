package conformance_test

import (
	"os/exec"
	"strings"
	"testing"
)

func runConformanceFailFixture(t *testing.T, fixtureTestName, requiredSubtest string) {
	t.Helper()
	cmd := exec.Command(
		"go",
		"test",
		"-tags",
		"conformancefail",
		"./internal/adapter/conformance",
		"-run",
		fixtureTestName,
	)
	cmd.Dir = "../../.."
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected %q conformance test to fail, but it passed\noutput:\n%s", fixtureTestName, out)
	}
	outText := string(out)
	if !strings.Contains(outText, requiredSubtest) {
		t.Fatalf("expected failing sub-test %q in %q output\noutput:\n%s", requiredSubtest, fixtureTestName, outText)
	}
}

func TestConformanceHarnessDetectsBrokenOutcomeDomainFixture(t *testing.T) {
	// The broken fixture returns an empty outcome; only outcome_domain should fail.
	runConformanceFailFixture(t, "TestBrokenAdapterConformanceFixture", "outcome_domain")
}

func TestConformanceHarnessDetectsNonHeartbeatingFixture(t *testing.T) {
	// The nonheartbeating fixture returns from Log immediately; heartbeats must fail.
	runConformanceFailFixture(t, "TestNonHeartbeatingAdapterConformanceFixture", "heartbeats")
}
