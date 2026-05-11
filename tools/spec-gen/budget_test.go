package main

import (
	"os"
	"strings"
	"testing"
)

// TestSpecTokenBudget_UnderEightThousandWords checks that docs/LANGUAGE-SPEC.md
// stays under the 5,700-word budget (approximately 8,000 cl100k_base tokens).
// This limit prevents LLM context overrun when the spec is injected verbatim.
func TestSpecTokenBudget_UnderEightThousandWords(t *testing.T) {
	const maxWords = 5700
	data, err := os.ReadFile("../../docs/LANGUAGE-SPEC.md")
	if err != nil {
		t.Fatalf("docs/LANGUAGE-SPEC.md not found: %v", err)
	}
	words := strings.Fields(string(data))
	if count := len(words); count > maxWords {
		t.Errorf("docs/LANGUAGE-SPEC.md has %d words; must be ≤ %d", count, maxWords)
	} else {
		t.Logf("docs/LANGUAGE-SPEC.md: %d / %d words (%.0f%%)", count, maxWords, float64(count)*100/float64(maxWords))
	}
}
