package semver

import "testing"

func TestIsValid(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"v1.0.0", true},
		{"v0.5.8+dirty", true},
		{"v0.5.9-rc1", true},
		{"v1.2", true},     // partial accepted by x/mod/semver
		{"v1", true},       // partial accepted by x/mod/semver
		{"1.0.0", false},   // missing v prefix
		{"vv1.0.0", false}, // double v prefix
		{"", false},
		{"not-a-version", false},
		{"v1.0.0.0", false}, // too many components
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			if got := IsValid(tc.version); got != tc.want {
				t.Errorf("IsValid(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"1.0.0", "v1.0.0"},
		{"v1.0.0", "v1.0.0"},
		{"  1.0.0  ", "v1.0.0"},
		{"v1.0.0-rc1", "v1.0.0-rc1"},
		{"0.5.9-rc1+build", "v0.5.9-rc1+build"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.1", -1},
		{"v2.0.0", "v1.9.9", 1},
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.0-rc1", "v1.0.0", -1},
		{"v1.0.0+build", "v1.0.0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.a+"_"+tc.b, func(t *testing.T) {
			if got := Compare(tc.a, tc.b); got != tc.want {
				t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestParseConstraint(t *testing.T) {
	cases := []struct {
		name       string
		constraint string
		wantErr    bool
	}{
		{"empty", "", false},
		{"latest", "latest", false},
		{"star", "*", false},
		{"x", "x", false},
		{"exact", "1.2.3", false},
		{"exact with v", "v1.2.3", false},
		{"caret", "^1.2.3", false},
		{"tilde", "~1.2.3", false},
		{"wildcard minor", "1.2.x", false},
		{"wildcard major", "1.x", false},
		{"partial major", "2", false},
		{"partial minor", "1.2", false},
		{"wildcard major not supported", "x.0.0", true},
		{"invalid", "not-a-version", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConstraint(tc.constraint)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseConstraint(%q) expected error", tc.constraint)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseConstraint(%q) unexpected error: %v", tc.constraint, err)
			}
		})
	}
}

func TestConstraintMatches(t *testing.T) {
	cases := []struct {
		name       string
		constraint string
		version    string
		want       bool
	}{
		{"exact match", "1.2.3", "v1.2.3", true},
		{"exact mismatch", "1.2.3", "v1.2.4", false},
		{"latest any", "latest", "v9.9.9", true},
		{"caret allows patch", "^1.2.3", "v1.2.4", true},
		{"caret excludes next major", "^1.2.3", "v2.0.0", false},
		{"caret zero-major locks minor", "^0.2.3", "v0.3.0", false},
		{"tilde allows patch", "~1.2.0", "v1.2.3", true},
		{"tilde excludes minor", "~1.2.0", "v1.3.0", false},
		{"wildcard minor", "1.2.x", "v1.2.9", true},
		{"wildcard minor excludes next", "1.2.x", "v1.3.0", false},
		{"wildcard major", "1.x", "v1.9.0", true},
		{"wildcard major excludes next", "1.x", "v2.0.0", false},
		{"partial major", "2", "v2.1.0", true},
		{"partial major excludes next", "2", "v3.0.0", false},
		{"partial minor", "1.2", "v1.2.3", true},
		{"partial minor excludes next", "1.2", "v1.3.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseConstraint(tc.constraint)
			if err != nil {
				t.Fatalf("ParseConstraint(%q) error: %v", tc.constraint, err)
			}
			if got := c.Matches(tc.version); got != tc.want {
				t.Errorf("ParseConstraint(%q).Matches(%q) = %v, want %v", tc.constraint, tc.version, got, tc.want)
			}
		})
	}
}

func TestConstraintIsExact(t *testing.T) {
	cases := []struct {
		constraint string
		want       bool
	}{
		{"1.2.3", true},
		{"v1.2.3", true},
		{"^1.2", false},
		{"~1.2.0", false},
		{"1.x", false},
		{"1.2", false},
		{"latest", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.constraint, func(t *testing.T) {
			c, err := ParseConstraint(tc.constraint)
			if err != nil {
				t.Fatalf("ParseConstraint(%q) error: %v", tc.constraint, err)
			}
			if got := c.IsExact(); got != tc.want {
				t.Errorf("ParseConstraint(%q).IsExact() = %v, want %v", tc.constraint, got, tc.want)
			}
		})
	}
}
