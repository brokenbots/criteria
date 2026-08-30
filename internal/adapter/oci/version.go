package oci

import (
	"fmt"
	"strings"

	"github.com/brokenbots/criteria/internal/adapter/semver"
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
	c, err := semver.ParseConstraint(strings.TrimSpace(constraint))
	if err != nil {
		return "", fmt.Errorf("oci: %w", err)
	}

	best := ""  // original tag spelling of the current best match
	bestN := "" // normalized (v-prefixed canonical) form of best
	for _, tag := range tags {
		n := semver.Normalize(tag)
		if !semver.IsValid(n) {
			continue
		}
		if !c.Matches(n) {
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

// IsExactVersion reports whether constraint names a single fully-specified
// version (so the lock command can skip listing tags and resolve it directly).
func IsExactVersion(constraint string) bool {
	c, err := semver.ParseConstraint(strings.TrimSpace(constraint))
	return err == nil && c.IsExact()
}
