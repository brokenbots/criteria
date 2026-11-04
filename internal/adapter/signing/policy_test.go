package signing

import (
	"strings"
	"testing"
)

func TestPolicyFor(t *testing.T) {
	tests := []struct {
		name        string
		ctx         PullContext
		want        Policy
		wantErr     bool
		errContains []string
	}{
		{
			name: "default strict when empty",
			ctx:  PullContext{},
			want: Policy{
				Mode:            ModeStrict,
				TrustedIssuers:  DefaultTrustedIssuers,
				SubjectPatterns: []string{"*"},
			},
		},
		{
			name:    "allow_unsigned_refused_for_strict_workflow",
			ctx:     PullContext{WorkflowVerification: "strict", AllowUnsigned: true},
			wantErr: true,
			errContains: []string{
				"strict verification conflicts with",
				"--allow-unsigned flag or CRITERIA_ALLOW_UNSIGNED env",
			},
		},
		{
			// The SDK default (no workflow attribute) is strict: an override
			// present in a direct-signing call is refused, matching the
			// post-transition-default behavior of the CLI.
			name:    "allow_unsigned_refused_for_unset_default",
			ctx:     PullContext{AllowUnsigned: true},
			wantErr: true,
			errContains: []string{
				"strict verification conflicts with",
				"CRITERIA_ALLOW_UNSIGNED",
			},
		},
		{
			name: "allow_unsigned_lowers_warn_to_off",
			ctx:  PullContext{WorkflowVerification: "warn", AllowUnsigned: true},
			want: Policy{Mode: ModeOff},
		},
		{
			name: "allow_unsigned_with_off_stays_off",
			ctx:  PullContext{WorkflowVerification: "off", AllowUnsigned: true},
			want: Policy{Mode: ModeOff},
		},
		{
			name: "workflow_off",
			ctx:  PullContext{WorkflowVerification: "off"},
			want: Policy{
				Mode:            ModeOff,
				TrustedIssuers:  DefaultTrustedIssuers,
				SubjectPatterns: []string{"*"},
			},
		},
		{
			name: "workflow_warn",
			ctx:  PullContext{WorkflowVerification: "warn"},
			want: Policy{
				Mode:            ModeWarn,
				TrustedIssuers:  DefaultTrustedIssuers,
				SubjectPatterns: []string{"*"},
			},
		},
		{
			name: "workflow_strict",
			ctx:  PullContext{WorkflowVerification: "strict"},
			want: Policy{
				Mode:            ModeStrict,
				TrustedIssuers:  DefaultTrustedIssuers,
				SubjectPatterns: []string{"*"},
			},
		},
		{
			name:    "workflow_invalid_mode",
			ctx:     PullContext{WorkflowVerification: "permissive"},
			wantErr: true,
		},
		{
			name: "case_insensitive",
			ctx:  PullContext{WorkflowVerification: "WARN"},
			want: Policy{
				Mode:            ModeWarn,
				TrustedIssuers:  DefaultTrustedIssuers,
				SubjectPatterns: []string{"*"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PolicyFor(tt.ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("PolicyFor() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				for _, sub := range tt.errContains {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("PolicyFor() error %q does not contain %q", err.Error(), sub)
					}
				}
				return
			}
			if got.Mode != tt.want.Mode {
				t.Errorf("Mode = %q, want %q", got.Mode, tt.want.Mode)
			}
			if len(got.TrustedIssuers) != len(tt.want.TrustedIssuers) {
				t.Errorf("TrustedIssuers len = %d, want %d", len(got.TrustedIssuers), len(tt.want.TrustedIssuers))
			}
			for i := range got.TrustedIssuers {
				if got.TrustedIssuers[i] != tt.want.TrustedIssuers[i] {
					t.Errorf("TrustedIssuers[%d] = %q, want %q", i, got.TrustedIssuers[i], tt.want.TrustedIssuers[i])
				}
			}
			if len(got.SubjectPatterns) != len(tt.want.SubjectPatterns) {
				t.Errorf("SubjectPatterns len = %d, want %d", len(got.SubjectPatterns), len(tt.want.SubjectPatterns))
			}
			for i := range got.SubjectPatterns {
				if got.SubjectPatterns[i] != tt.want.SubjectPatterns[i] {
					t.Errorf("SubjectPatterns[%d] = %q, want %q", i, got.SubjectPatterns[i], tt.want.SubjectPatterns[i])
				}
			}
		})
	}
}
