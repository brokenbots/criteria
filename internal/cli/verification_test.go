package cli

import (
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/signing"
)

// TestResolveVerification_Precedence asserts the override precedence:
// --allow-unsigned flag > CRITERIA_ALLOW_UNSIGNED env > workflow `verification`
// attr > transition default. The flag and env only carry the override into the
// resolved PullContext; whether it may apply is decided downstream by
// signing.PolicyFor (see TestResolveSigningPolicy_StrictRefusesOverride).
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
			name:         "flag + strict carries override to policy resolution",
			flag:         true,
			workflowAttr: "strict",
			wantAllow:    true,
			wantWorkflow: "strict",
		},
		{
			name:         "env + strict carries override to policy resolution",
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
// override combination, including the secure-by-explicit strict path. The
// override lowerings (flag/env vs warn/unset) apply; strict-vs-override
// refusal lives in TestResolveSigningPolicy_StrictRefusesOverride.
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
		{name: "flag + warn -> off", flag: true, workflowAttr: "warn", wantMode: signing.ModeOff},
		{name: "env + warn -> off", env: "true", workflowAttr: "warn", wantMode: signing.ModeOff},
		{name: "workflow off with flag stays off", flag: true, workflowAttr: "off", wantMode: signing.ModeOff},
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

// TestResolveSigningPolicy_StrictRefusesOverride asserts that the unsigned
// override (flag or CRITERIA_ALLOW_UNSIGNED env) is refused with a diagnostic
// when the workflow posture is explicitly strict: an override must never
// silently defeat an explicit strict trust decision.
func TestResolveSigningPolicy_StrictRefusesOverride(t *testing.T) {
	tests := []struct {
		name         string
		flag         bool
		env          string
		workflowAttr string
		trustedKeys  []signing.KeyIdentity
	}{
		{name: "flag vs strict", flag: true, workflowAttr: "strict"},
		{name: "env vs strict", env: "1", workflowAttr: "strict"},
		{
			// An explicit key-trust configuration is not a bypass either: the
			// refusal keys off the posture, which stays strict.
			name:         "flag vs strict with explicit keys",
			flag:         true,
			workflowAttr: "strict",
			trustedKeys:  []signing.KeyIdentity{{Algorithm: "ed25519", Fingerprint: "fp"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(allowUnsignedEnv, tt.env)

			_, err := resolveSigningPolicy(tt.flag, tt.workflowAttr, tt.trustedKeys)
			if err == nil {
				t.Fatal("expected refusal error for strict posture with override, got nil")
			}
			for _, sub := range []string{"strict verification conflicts with", "CRITERIA_ALLOW_UNSIGNED"} {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("refusal error %q does not contain %q", err.Error(), sub)
				}
			}
			// The refusal names both override surfaces so the operator can
			// tell whether a stray env value is at fault.
			if !strings.Contains(err.Error(), "--allow-unsigned flag") {
				t.Errorf("refusal error %q does not name the flag surface", err.Error())
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
