package signing

import (
	"testing"
)

func TestPolicyFor(t *testing.T) {
	tests := []struct {
		name    string
		ctx     PullContext
		want    Policy
		wantErr bool
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
			name: "allow_unsigned forces off",
			ctx: PullContext{
				AllowUnsigned:        true,
				WorkflowVerification: "strict",
			},
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
