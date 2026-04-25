package plugin

import (
	"testing"
)

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
