package oci

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// SelectVersion picks the highest tag in tags that satisfies the npm-style
// constraint. Supported constraint forms:
//
//	""  / "latest" / "*" / "x"   – highest published semver tag
//	"1.2.3"                       – exact match
//	"^1.2" / "^1.2.3" / "^1"      – compatible: same left-most non-zero element
//	"~1.2.0" / "~1.2" / "~1"      – approximately equivalent (patch-level, or minor for "~1")
//	"1.x" / "1.2.x"               – wildcard on the trailing component
//
// Tags that are not valid semver are ignored. Both "1.2.3" and "v1.2.3" tag
// spellings are accepted; the original (un-normalized) tag string is returned
// so it round-trips back to the registry.
func SelectVersion(constraint string, tags []string) (string, error) {
	c, err := parseConstraint(strings.TrimSpace(constraint))
	if err != nil {
		return "", err
	}

	best := ""  // original tag spelling of the current best match
	bestN := "" // normalized (v-prefixed canonical) form of best
	for _, tag := range tags {
		n := normalizeSemver(tag)
		if !semver.IsValid(n) {
			continue
		}
		if !c.matches(n) {
			continue
		}
		if bestN == "" || semver.Compare(n, bestN) > 0 {
			best, bestN = tag, n
		}
	}

	if best == "" {
		return "", fmt.Errorf("oci: no published tag satisfies version constraint %q", constraint)
	}
	return best, nil
}

// constraint is a resolved [lower, upper) semver window. Bounds are normalized
// (v-prefixed) canonical versions. upper == "" means unbounded above. When
// exact is set, only versions equal to lower match.
type constraint struct {
	lower string // inclusive, may be "" meaning unbounded below (any version)
	upper string // exclusive, may be "" meaning unbounded above
	exact bool
}

func (c constraint) matches(n string) bool {
	if c.exact {
		return semver.Compare(n, c.lower) == 0
	}
	if c.lower != "" && semver.Compare(n, c.lower) < 0 {
		return false
	}
	if c.upper != "" && semver.Compare(n, c.upper) >= 0 {
		return false
	}
	return true
}

// parseConstraint translates a constraint string into a [lower, upper) window.
func parseConstraint(s string) (constraint, error) {
	switch s {
	case "", "latest", "*", "x", "X":
		return constraint{}, nil // any version
	}

	switch s[0] {
	case '^':
		return parseCaret(s[1:])
	case '~':
		return parseTilde(s[1:])
	}

	pv, err := parsePartial(s)
	if err != nil {
		return constraint{}, err
	}
	if pv.wild || pv.spec < 3 {
		// "1.x"/"1" -> [1.0.0, 2.0.0); "1.2.x"/"1.2" -> [1.2.0, 1.3.0)
		if pv.spec == 1 {
			return constraint{lower: ver(pv.major, 0, 0), upper: ver(pv.major+1, 0, 0)}, nil
		}
		return constraint{lower: ver(pv.major, pv.minor, 0), upper: ver(pv.major, pv.minor+1, 0)}, nil
	}
	// Fully specified, no operator: exact match.
	return constraint{lower: ver(pv.major, pv.minor, pv.patch), exact: true}, nil
}

// parseCaret implements npm caret semantics: allow changes that do not modify
// the left-most non-zero element.
func parseCaret(s string) (constraint, error) {
	pv, err := parsePartial(s)
	if err != nil {
		return constraint{}, err
	}
	lower := ver(pv.major, pv.minor, pv.patch)
	switch {
	case pv.major > 0:
		return constraint{lower: lower, upper: ver(pv.major+1, 0, 0)}, nil
	case pv.minor > 0:
		return constraint{lower: lower, upper: ver(0, pv.minor+1, 0)}, nil
	default:
		return constraint{lower: lower, upper: ver(0, 0, pv.patch+1)}, nil
	}
}

// parseTilde implements npm tilde semantics: allow patch-level changes when a
// minor is specified, or minor-level changes when only a major is specified.
func parseTilde(s string) (constraint, error) {
	pv, err := parsePartial(s)
	if err != nil {
		return constraint{}, err
	}
	lower := ver(pv.major, pv.minor, pv.patch)
	if pv.spec == 1 {
		// "~1" -> [1.0.0, 2.0.0)
		return constraint{lower: lower, upper: ver(pv.major+1, 0, 0)}, nil
	}
	// "~1.2" or "~1.2.3" -> [x.y.0|x.y.z, x.(y+1).0)
	return constraint{lower: lower, upper: ver(pv.major, pv.minor+1, 0)}, nil
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
		return partialVersion{}, fmt.Errorf("oci: empty version in constraint")
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return partialVersion{}, fmt.Errorf("oci: invalid version %q", s)
	}
	nums := [3]int{}
	var pv partialVersion
	for i, p := range parts {
		if p == "x" || p == "X" || p == "*" {
			if i == 0 {
				return partialVersion{}, fmt.Errorf("oci: wildcard major in %q not supported; use \"latest\"", s)
			}
			pv.wild = true
			pv.spec = i // explicit components precede the wildcard
			pv.major, pv.minor, pv.patch = nums[0], nums[1], nums[2]
			return pv, nil
		}
		n, convErr := strconv.Atoi(p)
		if convErr != nil || n < 0 {
			return partialVersion{}, fmt.Errorf("oci: invalid version component %q in %q", p, s)
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

// normalizeSemver ensures a leading "v" so the tag can be fed to x/mod/semver.
func normalizeSemver(tag string) string {
	tag = strings.TrimSpace(tag)
	if strings.HasPrefix(tag, "v") {
		return tag
	}
	return "v" + tag
}

// IsExactVersion reports whether constraint names a single fully-specified
// version (so the lock command can skip listing tags and resolve it directly).
func IsExactVersion(constraint string) bool {
	c, err := parseConstraint(strings.TrimSpace(constraint))
	return err == nil && c.exact
}
