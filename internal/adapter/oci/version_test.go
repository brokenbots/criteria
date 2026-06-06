package oci

import "testing"

func TestSelectVersion(t *testing.T) {
	tags := []string{"1.0.0", "1.2.0", "1.2.3", "1.3.0", "2.0.0", "2.1.0", "v0.9.0", "nightly", "0.2.3", "0.2.9", "0.3.0"}

	cases := []struct {
		name       string
		constraint string
		want       string
		wantErr    bool
	}{
		{"exact", "1.2.0", "1.2.0", false},
		{"exact missing", "1.2.99", "", true},
		{"latest", "latest", "2.1.0", false},
		{"empty means latest", "", "2.1.0", false},
		{"star", "*", "2.1.0", false},
		{"caret minor floor", "^1.2", "1.3.0", false},
		{"caret patch floor", "^1.2.3", "1.3.0", false},
		{"caret major only", "^1", "1.3.0", false},
		{"caret excludes next major", "^2.0.0", "2.1.0", false},
		{"caret zero-major locks minor", "^0.2.3", "0.2.9", false},
		{"tilde patch", "~1.2.0", "1.2.3", false},
		{"tilde minor", "~1.2", "1.2.3", false},
		{"tilde major", "~1", "1.3.0", false},
		{"wildcard major", "1.x", "1.3.0", false},
		{"wildcard minor", "1.2.x", "1.2.3", false},
		{"partial major", "2", "2.1.0", false},
		{"partial minor", "1.2", "1.2.3", false},
		{"no match", "^9", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectVersion(tc.constraint, tags)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SelectVersion(%q) = %q, want error", tc.constraint, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SelectVersion(%q) unexpected error: %v", tc.constraint, err)
			}
			if got != tc.want {
				t.Fatalf("SelectVersion(%q) = %q, want %q", tc.constraint, got, tc.want)
			}
		})
	}
}

func TestSelectVersionRoundTripsOriginalSpelling(t *testing.T) {
	got, err := SelectVersion("^1", []string{"v1.0.0", "v1.4.0"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1.4.0" {
		t.Fatalf("got %q, want v1.4.0 (original spelling preserved)", got)
	}
}

func TestSelectVersionEmptyTags(t *testing.T) {
	if _, err := SelectVersion("^1", nil); err == nil {
		t.Fatal("expected error for empty tag list")
	}
}

func TestIsExactVersion(t *testing.T) {
	cases := map[string]bool{
		"1.2.3":  true,
		"v1.2.3": true,
		"^1.2":   false,
		"~1.2.0": false,
		"1.x":    false,
		"1.2":    false,
		"latest": false,
		"":       false,
	}
	for in, want := range cases {
		if got := IsExactVersion(in); got != want {
			t.Errorf("IsExactVersion(%q) = %v, want %v", in, got, want)
		}
	}
}
