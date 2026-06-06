package cli

import (
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/signing"
)

// TestResolveVerification_Precedence asserts the override precedence:
// --allow-unsigned flag > CRITERIA_ALLOW_UNSIGNED env > workflow `verification`
// attr > transition default.
func TestResolveVerification_Precedence(t *testing.T) {
	tests := []struct {
		name         string
		flag         bool
		env          string // value for CRITERIA_ALLOW_UNSIGNED ("" = unset)
		workflowAttr string
		wantAllow    bool
		wantWorkflow string
	}{
		{
			name:         "default is warn transition",
			wantAllow:    false,
			wantWorkflow: string(transitionDefaultMode),
		},
		{
			name:         "flag forces allow-unsigned",
			flag:         true,
			wantAllow:    true,
			wantWorkflow: string(transitionDefaultMode),
		},
		{
			name:         "env forces allow-unsigned",
			env:          "1",
			wantAllow:    true,
			wantWorkflow: string(transitionDefaultMode),
		},
		{
			name:         "env truthy variants",
			env:          "true",
			wantAllow:    true,
			wantWorkflow: string(transitionDefaultMode),
		},
		{
			name:         "env falsey leaves override off",
			env:          "0",
			wantAllow:    false,
			wantWorkflow: string(transitionDefaultMode),
		},
		{
			name:         "workflow attr honored when no override",
			workflowAttr: "strict",
			wantAllow:    false,
			wantWorkflow: "strict",
		},
		{
			name:         "flag beats workflow strict",
			flag:         true,
			workflowAttr: "strict",
			wantAllow:    true,
			wantWorkflow: "strict",
		},
		{
			name:         "env beats workflow strict",
			env:          "yes",
			workflowAttr: "strict",
			wantAllow:    true,
			wantWorkflow: "strict",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env == "" {
				t.Setenv(allowUnsignedEnv, "")
			} else {
				t.Setenv(allowUnsignedEnv, tt.env)
			}

			got := resolveVerification(tt.flag, tt.workflowAttr, nil)
			if got.AllowUnsigned != tt.wantAllow {
				t.Errorf("AllowUnsigned = %v, want %v", got.AllowUnsigned, tt.wantAllow)
			}
			if got.WorkflowVerification != tt.wantWorkflow {
				t.Errorf("WorkflowVerification = %q, want %q", got.WorkflowVerification, tt.wantWorkflow)
			}
		})
	}
}

// TestResolveSigningPolicy_Modes asserts the resolved policy mode for each
// override combination, including the secure-by-explicit strict path.
func TestResolveSigningPolicy_Modes(t *testing.T) {
	tests := []struct {
		name         string
		flag         bool
		env          string
		workflowAttr string
		wantMode     signing.VerificationMode
	}{
		{name: "default transition", wantMode: transitionDefaultMode},
		{name: "flag -> off", flag: true, wantMode: signing.ModeOff},
		{name: "env -> off", env: "1", wantMode: signing.ModeOff},
		{name: "workflow off", workflowAttr: "off", wantMode: signing.ModeOff},
		{name: "workflow warn", workflowAttr: "warn", wantMode: signing.ModeWarn},
		{name: "workflow strict", workflowAttr: "strict", wantMode: signing.ModeStrict},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(allowUnsignedEnv, tt.env)

			policy, err := resolveSigningPolicy(tt.flag, tt.workflowAttr, nil)
			if err != nil {
				t.Fatalf("resolveSigningPolicy: %v", err)
			}
			if policy.Mode != tt.wantMode {
				t.Errorf("policy.Mode = %q, want %q", policy.Mode, tt.wantMode)
			}
		})
	}
}

// TestResolveSigningPolicy_InvalidWorkflowMode confirms an unknown workflow
// verification value is rejected (mirrors the compile-time validation, defends
// the runtime path).
func TestResolveSigningPolicy_InvalidWorkflowMode(t *testing.T) {
	t.Setenv(allowUnsignedEnv, "")
	if _, err := resolveSigningPolicy(false, "bogus", nil); err == nil {
		t.Fatal("expected error for invalid workflow verification mode, got nil")
	}
}
