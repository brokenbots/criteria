package workflow

import "testing"

func TestStateDeclaration_Validate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		decl    *StateDeclaration
		wantErr string
	}{
		{
			name: "nil declaration is valid",
			decl: nil,
		},
		{
			name: "explicit none is valid",
			decl: &StateDeclaration{Mode: StateModeNone},
		},
		{
			name: "empty mode is valid",
			decl: &StateDeclaration{Mode: ""},
		},
		{
			name: "blob with schema is valid",
			decl: &StateDeclaration{Mode: StateModeBlob, Schema: "harness.v1", MaxBytes: 64 * 1024, Granularity: StateGranularityPerTurn},
		},
		{
			name: "ref with schema is valid",
			decl: &StateDeclaration{Mode: StateModeRef, Schema: "session-token.v1", Granularity: StateGranularityOnDemand},
		},
		{
			name:    "blob without schema is invalid",
			decl:    &StateDeclaration{Mode: StateModeBlob, Granularity: StateGranularityPerTurn},
			wantErr: "state mode \"blob\" requires a non-empty schema version tag",
		},
		{
			name:    "ref without schema is invalid",
			decl:    &StateDeclaration{Mode: StateModeRef},
			wantErr: "state mode \"ref\" requires a non-empty schema version tag",
		},
		{
			name:    "unknown mode fails loudly",
			decl:    &StateDeclaration{Mode: "snapshot", Schema: "x.v1"},
			wantErr: "unknown state mode \"snapshot\" (known: none, blob, ref)",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.decl.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v; want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil; want error containing %q", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("Validate() = %q; want %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestStateDeclaration_EffectiveMaxBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		decl *StateDeclaration
		want uint32
	}{
		{
			name: "nil declaration falls back to engine default",
			decl: nil,
			want: DefaultStateMaxBytes,
		},
		{
			name: "zero MaxBytes falls back to engine default",
			decl: &StateDeclaration{Mode: StateModeBlob, Schema: "harness.v1"},
			want: 400 * 1024,
		},
		{
			name: "adapter-declared cap wins",
			decl: &StateDeclaration{Mode: StateModeBlob, Schema: "harness.v1", MaxBytes: 64 * 1024},
			want: 64 * 1024,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.decl.EffectiveMaxBytes(); got != tc.want {
				t.Fatalf("EffectiveMaxBytes() = %d; want %d", got, tc.want)
			}
		})
	}
}

func TestDefaultStateMaxBytes(t *testing.T) {
	t.Parallel()
	if DefaultStateMaxBytes != 409600 {
		t.Fatalf("DefaultStateMaxBytes = %d; want 409600 (~400K engine default)", DefaultStateMaxBytes)
	}
}