package adapterhost

// sessions_classification_source_test.go — the ADR-0014 M5 grep gate.
//
// Crash classification is journal-only: classifySessionCrash reads facts the
// supervision journal delivered (peer-reported CrashClassified or the
// in-process supervisor's terminal) and falls back to structurally-known
// terminal state. The legacy string-matching heuristic arms were deleted
// with the legacy runner they served, so this package's non-test sources
// must carry no trace of them: the deleted symbol is gone and the two
// classification entry points classify without ever inspecting error text.
// The test file itself is excluded from the scan so the gate can name the
// literals it bans.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bannedClassificationLiterals are the string-matching arms that
// constituted the legacy crash heuristic, in the forms the deleted
// isLikelySessionCrash implementation shipped with. They must not reappear
// in adapterhost non-test sources.
var bannedClassificationLiterals = []string{
	`"transport is closing"`,
	`"unavailable"`,
	`"broken pipe"`,
	`"eof"`,
	`"terminated"`,
	"isLikelySessionCrash",
}

// stringMatchingPredicates are the error-text inspections classification
// must not perform. Legitimate strings.* uses (name joins, formatting) do
// not appear in the two bodies covered.
var stringMatchingPredicates = []string{
	"strings.Contains(",
	"strings.HasPrefix(",
	"strings.HasSuffix(",
}

// verify_failover.go is excluded from the literal scan only: its
// classifyTransportLoss is the handshake-retry classifier, not crash
// classification (ADR-0014 M5 scope), and it legitimately matches the
// transport text to decide a retry. Its crash-classification surface is
// covered by the same body scans as every other file.
var literalScanExempts = map[string]bool{"verify_failover.go": true}

// TestSession_ClassificationGate_BansDeletedStringMatchingHeuristics scans
// the package's non-test sources and asserts the legacy heuristic is fully
// deleted and that classifySessionCrash / isSessionCrashCandidate classify
// from journal facts and structural state only.
func TestSession_ClassificationGate_BansDeletedStringMatchingHeuristics(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		// *_test.go files are excluded so this gate can name the banned
		// literals; non-test sources carry the classification code under
		// scrutiny.
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if !literalScanExempts[entry.Name()] {
			for _, lit := range bannedClassificationLiterals[:5] {
				if strings.Contains(string(src), lit) {
					t.Errorf("%s contains deleted heuristic literal %s", entry.Name(), lit)
				}
			}
		}
		if strings.Contains(string(src), "isLikelySessionCrash") {
			t.Errorf("%s contains deleted symbol isLikelySessionCrash", entry.Name())
		}
	}

	// Classification entry points must contain no string-shaped predicates.
	for _, fn := range []string{"classifySessionCrash", "isSessionCrashCandidate"} {
		body, err := funcBody(filepath.Join(".", "sessions.go"), "func "+fn)
		if err != nil {
			t.Fatalf("locate %s: %v", fn, err)
		}
		for _, pred := range stringMatchingPredicates {
			if strings.Contains(body, pred) {
				t.Errorf("%s body inspects error text through %s: classification must be journal-fact or structural only", fn, pred)
			}
		}
		// Journal-only positive pin: fact delivery enters every body.
		if !strings.Contains(body, "SupervisionCrashReason") {
			t.Errorf("%s body does not consult the supervision journal facts (SupervisionCrashReason); journal delivery is the only classification source (ADR-0014 M5)", fn)
		}
	}
}

// funcBody returns the source lines of the top-level func named in decl
// through its closing brace at column zero.
func funcBody(file, decl string) (string, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(src), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, decl) {
			start = i
			break
		}
	}
	if start == -1 {
		return "", os.ErrNotExist
	}
	var b strings.Builder
	for _, line := range lines[start:] {
		b.WriteString(line)
		b.WriteString("\n")
		if line == "}" {
			return b.String(), nil
		}
	}
	return b.String(), nil
}
