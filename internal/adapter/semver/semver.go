package semver

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// IsValid reports whether s is a valid semantic version in canonical
// "vMAJOR.MINOR.PATCH" form. Build metadata and prerelease are allowed.
// Callers that need to accept both "1.2.3" and "v1.2.3" should normalize s with
// Normalize before validating.
func IsValid(s string) bool {
	return semver.IsValid(s)
}

// Normalize ensures s has a leading "v" so it can be fed to IsValid and Compare.
// It does not otherwise canonicalize s; the caller should ensure s contains a
// valid semver after normalization.
func Normalize(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "v") {
		return s
	}
	return "v" + s
}

// Compare returns -1, 0, or 1 if a is less than, equal to, or greater than b.
// Both a and b must already be normalized canonical versions.
func Compare(a, b string) int {
	return semver.Compare(a, b)
}

// Constraint is a parsed npm-style version constraint.
//
//	""  / "latest" / "*" / "x"   – any version
//	"1.2.3"                       – exact match
//	"^1.2" / "^1.2.3" / "^1"      – compatible: same left-most non-zero element
//	"~1.2.0" / "~1.2" / "~1"      – approximately equivalent
//	"1.x" / "1.2.x"               – wildcard on the trailing component
type Constraint struct {
	lower string // inclusive, may be "" meaning unbounded below (any version)
	upper string // exclusive, may be "" meaning unbounded above
	exact bool
}

// ParseConstraint parses an npm-style version constraint string.
func ParseConstraint(s string) (Constraint, error) {
	switch s {
	case "", "latest", "*", "x", "X":
		return Constraint{}, nil // any version
	}

	switch s[0] {
	case '^':
		return parseCaret(s[1:])
	case '~':
		return parseTilde(s[1:])
	}

	pv, err := parsePartial(s)
	if err != nil {
		return Constraint{}, err
	}
	if pv.wild || pv.spec < 3 {
		// "1.x"/"1" -> [1.0.0, 2.0.0); "1.2.x"/"1.2" -> [1.2.0, 1.3.0)
		if pv.spec == 1 {
			return Constraint{lower: ver(pv.major, 0, 0), upper: ver(pv.major+1, 0, 0)}, nil
		}
		return Constraint{lower: ver(pv.major, pv.minor, 0), upper: ver(pv.major, pv.minor+1, 0)}, nil
	}
	// Fully specified, no operator: exact match.
	return Constraint{lower: ver(pv.major, pv.minor, pv.patch), exact: true}, nil
}

// Matches reports whether v satisfies the constraint. v must be normalized.
func (c Constraint) Matches(v string) bool {
	if c.exact {
		return Compare(v, c.lower) == 0
	}
	if c.lower != "" && Compare(v, c.lower) < 0 {
		return false
	}
	if c.upper != "" && Compare(v, c.upper) >= 0 {
		return false
	}
	return true
}

// IsExact reports whether the constraint names a single fully-specified version.
func (c Constraint) IsExact() bool {
	return c.exact
}

// parseCaret implements npm caret semantics: allow changes that do not modify
// the left-most non-zero element.
func parseCaret(s string) (Constraint, error) {
	pv, err := parsePartial(s)
	if err != nil {
		return Constraint{}, err
	}
	lower := ver(pv.major, pv.minor, pv.patch)
	switch {
	case pv.major > 0:
		return Constraint{lower: lower, upper: ver(pv.major+1, 0, 0)}, nil
	case pv.minor > 0:
		return Constraint{lower: lower, upper: ver(0, pv.minor+1, 0)}, nil
	default:
		return Constraint{lower: lower, upper: ver(0, 0, pv.patch+1)}, nil
	}
}

// parseTilde implements npm tilde semantics: allow patch-level changes when a
// minor is specified, or minor-level changes when only a major is specified.
func parseTilde(s string) (Constraint, error) {
	pv, err := parsePartial(s)
	if err != nil {
		return Constraint{}, err
	}
	lower := ver(pv.major, pv.minor, pv.patch)
	if pv.spec == 1 {
		// "~1" -> [1.0.0, 2.0.0)
		return Constraint{lower: lower, upper: ver(pv.major+1, 0, 0)}, nil
	}
	// "~1.2" or "~1.2.3" -> [x.y.0|x.y.z, x.(y+1).0)
	return Constraint{lower: lower, upper: ver(pv.major, pv.minor+1, 0)}, nil
}

// partialVersion is a possibly-partial version parse result. spec is the number
// of explicit numeric components (1-3); wild is true when a trailing component
// is a wildcard (x/X/*).
type partialVersion struct {
	major, minor, patch int
	spec                int
	wild                bool
}

// parsePartial parses a possibly-partial version (e.g. "1", "1.2", "1.2.3",
// "1.x", "1.2.x").
func parsePartial(s string) (partialVersion, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return partialVersion{}, fmt.Errorf("empty version in constraint")
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return partialVersion{}, fmt.Errorf("invalid version %q", s)
	}
	nums := [3]int{}
	var pv partialVersion
	for i, p := range parts {
		if p == "x" || p == "X" || p == "*" {
			if i == 0 {
				return partialVersion{}, fmt.Errorf("wildcard major in %q not supported; use \"latest\"", s)
			}
			pv.wild = true
			pv.spec = i // explicit components precede the wildcard
			pv.major, pv.minor, pv.patch = nums[0], nums[1], nums[2]
			return pv, nil
		}
		n, convErr := strconv.Atoi(p)
		if convErr != nil || n < 0 {
			return partialVersion{}, fmt.Errorf("invalid version component %q in %q", p, s)
		}
		nums[i] = n
		pv.spec = i + 1
	}
	pv.major, pv.minor, pv.patch = nums[0], nums[1], nums[2]
	return pv, nil
}

// ver formats a normalized (v-prefixed) canonical semver string.
func ver(maj, mnr, pat int) string {
	return fmt.Sprintf("v%d.%d.%d", maj, mnr, pat)
}
