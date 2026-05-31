package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintSpec_SpecOnly(t *testing.T) {
	var buf bytes.Buffer
	if err := printSpec(&buf, false); err != nil {
		t.Fatalf("printSpec error: %v", err)
	}
	out := buf.String()
	// Spec must contain the normative section headers
	for _, anchor := range []string{"## Blocks", "## Functions", "## Iteration semantics", "## Outcome model"} {
		if !strings.Contains(out, anchor) {
			t.Errorf("spec output missing expected anchor %q", anchor)
		}
	}
}

func TestPrintSpec_WithPatterns(t *testing.T) {
	var buf bytes.Buffer
	if err := printSpec(&buf, true); err != nil {
		t.Fatalf("printSpec error: %v", err)
	}
	out := buf.String()
	// All eight patterns must appear
	for _, marker := range []string{
		"Pattern: Linear", "Pattern: Branching switch",
		"Pattern: Sequential iteration", "Pattern: Concurrent iteration",
		"Pattern: Subworkflow", "Pattern: Human-in-the-loop",
		"Pattern: Mutable shared state", "Pattern: File-driven",
	} {
		if !strings.Contains(out, marker) {
			t.Errorf("combined output missing pattern marker %q", marker)
		}
	}
}
