package adapterhost

import (
	"strings"
	"testing"
)

// TestPermissionDenialSuggestionDeterministicOrder verifies that
// PermissionDenialSuggestion always returns aliases in sorted order
// regardless of map-iteration order.  It uses a temporary adapter
// entry with multiple aliases mapping to the same canonical kind.
func TestPermissionDenialSuggestionDeterministicOrder(t *testing.T) {
	adapterPermissionAliases["test-order"] = map[string]string{
		"get_file":   "read",
		"read_file":  "read",
		"fetch_file": "read",
	}
	t.Cleanup(func() { delete(adapterPermissionAliases, "test-order") })

	const iterations = 20
	first := PermissionDenialSuggestion("test-order", "read")
	if first == "" {
		t.Fatal("expected a suggestion for test-order/read")
	}
	for i := 1; i < iterations; i++ {
		s := PermissionDenialSuggestion("test-order", "read")
		if s != first {
			t.Fatalf("non-deterministic output on iteration %d:\n  got  %q\n  want %q", i, s, first)
		}
	}
	if !strings.Contains(first, "fetch_file, get_file, read_file") {
		t.Fatalf("aliases not sorted in suggestion: %q", first)
	}
}

func TestPolicyDefaultDeny(t *testing.T) {
	p := NewPolicy(nil)
	allow, reason := p.Decide(PermissionRequest{ID: "1", Tool: "read_file"})
	if allow {
		t.Fatal("default policy must deny")
	}
	if reason == "" {
		t.Fatal("reason must be non-empty")
	}
}

func TestPolicyEmptyPatternsDeny(t *testing.T) {
	p := NewPolicy([]string{})
	allow, _ := p.Decide(PermissionRequest{ID: "1", Tool: "write_file"})
	if allow {
		t.Fatal("empty allowlist must deny")
	}
}

func TestPolicyLiteralMatch(t *testing.T) {
	p := NewPolicy([]string{"read_file"})

	allow, reason := p.Decide(PermissionRequest{ID: "1", Tool: "read_file"})
	if !allow {
		t.Fatalf("expected allow for literal match, got deny (reason=%q)", reason)
	}
	if reason != "matched: read_file" {
		t.Fatalf("unexpected reason %q", reason)
	}

	allow, _ = p.Decide(PermissionRequest{ID: "2", Tool: "write_file"})
	if allow {
		t.Fatal("non-matching tool must be denied")
	}
}

func TestPolicyGlobStar(t *testing.T) {
	p := NewPolicy([]string{"shell:git *"})

	allow, _ := p.Decide(PermissionRequest{ID: "1", Tool: "shell:git status"})
	if !allow {
		t.Fatal("expected allow for glob star match")
	}

	allow, _ = p.Decide(PermissionRequest{ID: "2", Tool: "shell:git log --oneline"})
	if !allow {
		t.Fatal("expected allow for glob star with longer arg")
	}

	allow, _ = p.Decide(PermissionRequest{ID: "3", Tool: "shell:curl https://example.com"})
	if allow {
		t.Fatal("expected deny for non-matching shell command")
	}
}

func TestPolicyGlobQuestion(t *testing.T) {
	p := NewPolicy([]string{"tool_?"})

	allow, _ := p.Decide(PermissionRequest{ID: "1", Tool: "tool_a"})
	if !allow {
		t.Fatal("expected allow for single-char glob match")
	}

	allow, _ = p.Decide(PermissionRequest{ID: "2", Tool: "tool_ab"})
	if allow {
		t.Fatal("expected deny: '?' should not match two chars")
	}
}

func TestPolicyPrefixGlob(t *testing.T) {
	p := NewPolicy([]string{"shell:*"})

	allow, _ := p.Decide(PermissionRequest{ID: "1", Tool: "shell:anything goes here"})
	if !allow {
		t.Fatal("expected allow for prefix:* match")
	}

	allow, _ = p.Decide(PermissionRequest{ID: "2", Tool: "read_file"})
	if allow {
		t.Fatal("expected deny for non-shell tool")
	}
}

func TestPolicyFirstMatchWins(t *testing.T) {
	// Both patterns match "read_file", first one should win.
	p := NewPolicy([]string{"read_file", "read_*"})

	allow, reason := p.Decide(PermissionRequest{ID: "1", Tool: "read_file"})
	if !allow {
		t.Fatal("expected allow")
	}
	if reason != "matched: read_file" {
		t.Fatalf("expected first pattern to match, got %q", reason)
	}
}

func TestPolicyStepAndWorkflowUnion(t *testing.T) {
	// Simulates the union of step-level and workflow-level patterns (done at
	// compile time into StepNode.AllowTools; this test exercises the policy layer
	// with the unioned slice).
	stepTools := []string{"read_file"}
	workflowTools := []string{"shell:echo *"}
	all := append(append([]string(nil), stepTools...), workflowTools...)

	p := NewPolicy(all)

	allow, _ := p.Decide(PermissionRequest{ID: "1", Tool: "read_file"})
	if !allow {
		t.Fatal("step-level pattern must allow read_file")
	}

	allow, _ = p.Decide(PermissionRequest{ID: "2", Tool: "shell:echo hello"})
	if !allow {
		t.Fatal("workflow-level pattern must allow shell:echo hello")
	}

	allow, _ = p.Decide(PermissionRequest{ID: "3", Tool: "write_file"})
	if allow {
		t.Fatal("non-listed tool must be denied")
	}
}

func TestPolicyInvalidPatternSkipped(t *testing.T) {
	// An invalid glob pattern (unmatched '[') should be skipped gracefully.
	p := NewPolicy([]string{"[invalid", "read_file"})

	allow, _ := p.Decide(PermissionRequest{ID: "1", Tool: "read_file"})
	if !allow {
		t.Fatal("valid pattern after invalid one must still match")
	}
}

func TestPolicyWithAliasesReadFile(t *testing.T) {
	// UF#02: allow_tools = ["read_file"] must grant the SDK kind "read".
	aliases := map[string]string{"read_file": "read", "write_file": "write"}
	p := NewPolicyWithAliases([]string{"read_file"}, aliases)

	allow, reason := p.Decide(PermissionRequest{ID: "1", Tool: "read"})
	if !allow {
		t.Fatalf("read_file alias must allow canonical 'read', got deny (reason=%q)", reason)
	}
	if !strings.Contains(reason, "read_file") {
		t.Fatalf("reason should reference the matched alias pattern, got %q", reason)
	}
	if !strings.Contains(reason, "alias for read") {
		t.Fatalf("reason should note the canonical form, got %q", reason)
	}
}

func TestPolicyWithAliasesWriteFile(t *testing.T) {
	aliases := map[string]string{"read_file": "read", "write_file": "write"}
	p := NewPolicyWithAliases([]string{"write_file"}, aliases)

	allow, _ := p.Decide(PermissionRequest{ID: "1", Tool: "write"})
	if !allow {
		t.Fatal("write_file alias must allow canonical 'write'")
	}

	allow, _ = p.Decide(PermissionRequest{ID: "2", Tool: "read"})
	if allow {
		t.Fatal("write_file alias must not allow 'read'")
	}
}

func TestPolicyWithAliasesCanonicalStillWorks(t *testing.T) {
	// Canonical name in allow_tools continues to work alongside aliases.
	aliases := map[string]string{"read_file": "read", "write_file": "write"}
	p := NewPolicyWithAliases([]string{"read"}, aliases)

	allow, reason := p.Decide(PermissionRequest{ID: "1", Tool: "read"})
	if !allow {
		t.Fatalf("canonical 'read' must allow 'read', got deny (reason=%q)", reason)
	}
	if reason != "matched: read" {
		t.Fatalf("expected simple matched reason, got %q", reason)
	}
}

func TestPolicyWithAliasesNonAliasUnaffected(t *testing.T) {
	// A tool that has no alias should not be accidentally granted.
	aliases := map[string]string{"read_file": "read", "write_file": "write"}
	p := NewPolicyWithAliases([]string{"read_file"}, aliases)

	allow, _ := p.Decide(PermissionRequest{ID: "1", Tool: "shell"})
	if allow {
		t.Fatal("alias map must not affect unrelated tools")
	}
}

func TestPermissionDenialSuggestionWithAliases(t *testing.T) {
	s := PermissionDenialSuggestion("copilot", "read")
	if s == "" {
		t.Fatal("expected suggestion for known copilot alias")
	}
	if !strings.Contains(s, "read_file") {
		t.Fatalf("suggestion should mention the alias 'read_file', got %q", s)
	}
}

func TestPermissionDenialSuggestionNoAlias(t *testing.T) {
	s := PermissionDenialSuggestion("copilot", "shell")
	if s != "" {
		t.Fatalf("no suggestion expected for 'shell' kind, got %q", s)
	}
}

func TestPermissionDenialSuggestionUnknownAdapter(t *testing.T) {
	s := PermissionDenialSuggestion("noop", "read")
	if s != "" {
		t.Fatalf("no suggestion expected for unknown adapter, got %q", s)
	}
}

func TestPolicyShellCommandFingerprintLiteral(t *testing.T) {
	p := NewPolicy([]string{"shell:git status"})

	allow, reason := p.Decide(PermissionRequest{
		ID:      "1",
		Tool:    "shell",
		Details: map[string]string{"commands": "git status"},
	})
	if !allow {
		t.Fatalf("expected allow for shell command fingerprint, got deny (%q)", reason)
	}
	if reason != "matched: shell:git status" {
		t.Fatalf("unexpected reason: %q", reason)
	}
}

func TestPolicyShellCommandFingerprintGlob(t *testing.T) {
	p := NewPolicy([]string{"shell:git *"})

	allow, _ := p.Decide(PermissionRequest{
		ID:      "1",
		Tool:    "shell",
		Details: map[string]string{"commands": "git status"},
	})
	if !allow {
		t.Fatal("expected allow for git command via details fingerprint")
	}

	allow, _ = p.Decide(PermissionRequest{
		ID:      "2",
		Tool:    "shell",
		Details: map[string]string{"commands": "npm test"},
	})
	if allow {
		t.Fatal("expected deny for non-git command with shell:git * pattern")
	}
}
