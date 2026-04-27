package workflow

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// TestDefaultPolicyMatchesDoc verifies that DefaultPolicy.MaxTotalSteps matches
// the value documented in docs/workflow.md. This test intentionally crosses the
// code/doc boundary so that the CI gate catches semantic drift (e.g. someone
// changes the code default without updating the doc, or vice versa).
//
// If this test fails, either update DefaultPolicy in schema.go OR update the
// max_total_steps documentation in docs/workflow.md — but keep them in sync.
func TestDefaultPolicyMatchesDoc(t *testing.T) {
	docDefault := parseDocDefault(t, "../docs/workflow.md")
	if docDefault != DefaultPolicy.MaxTotalSteps {
		t.Errorf("DefaultPolicy.MaxTotalSteps = %d but docs/workflow.md documents the default as %d; keep them in sync",
			DefaultPolicy.MaxTotalSteps, docDefault)
	}
}

// parseDocDefault extracts the max_total_steps default value from the
// "default N" pattern in the max_total_steps documentation line.
// It matches lines like:  **`max_total_steps`** (default 100):
func parseDocDefault(t *testing.T, path string) int {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("cannot open %s: %v", path, err)
	}
	defer f.Close()

	// Match a line that documents max_total_steps with its default, e.g.:
	//   - **`max_total_steps`** (default 100):
	re := regexp.MustCompile(`max_total_steps.*\bdefault\s+(\d+)\b`)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if m := re.FindStringSubmatch(line); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("cannot parse documented default %q as int: %v", m[1], err)
			}
			return n
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	t.Fatalf("could not find max_total_steps default documentation in %s", path)
	return 0
}
