// Package version provides the single authoritative Criteria engine version.
//
// Release builds inject Version via ldflags (e.g.
// -X github.com/brokenbots/criteria/workflow/version.Version=v0.5.8). Git
// describe output from development builds cut after a stable release tag
// (vX.Y.Z-N-g<sha>) is normalized to X.Y.Z+build.N.g<sha> so the engine
// version gate evaluates those builds as the stable base version they are
// ahead of, never as a prerelease. When the embedded value cannot be parsed
// as a semantic version, the engine falls back to the CRITERIA_OVERRIDE_VERSION
// environment variable for development and testing, but only when the build
// itself did not ship a release version.
package version

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/blang/semver"
)

// Version is the Criteria engine version embedded by the release build
// process. It defaults to "dev" for builds that do not inject a version
// identifier.
var Version = "dev"

const overrideEnv = "CRITERIA_OVERRIDE_VERSION"

// OverrideEnv returns the name of the environment variable that can supply an
// explicit semantic version for development builds whose embedded Version is
// not parseable.
func OverrideEnv() string { return overrideEnv }

// LdflagsExample returns a go build -ldflags string that injects the given
// version into the running binary. It is used in diagnostics and tests.
func LdflagsExample(v string) string {
	return fmt.Sprintf("-X github.com/brokenbots/criteria/workflow/version.Version=%s", v)
}

// Info holds the resolved engine version and metadata used for diagnostics.
type Info struct {
	// Version is the parsed semantic version, if known.
	Version semver.Version
	// Known is true when Version was parsed successfully from the build or an
	// explicit development override.
	Known bool
	// Display is the human-readable engine version shown in diagnostics and the
	// CLI version command. It includes an override notice when one is active.
	Display string
	// Override is true when CRITERIA_OVERRIDE_VERSION supplied the version.
	Override bool
}

// Current resolves the running engine's semantic version.
//
// If the embedded Version is a valid semantic version (with an optional leading
// "v"), it is used unchanged and CRITERIA_OVERRIDE_VERSION is ignored. This
// prevents environment variables from weakening release builds. Git describe
// output from a build cut after a stable tag (vX.Y.Z-N-g<sha>) is normalized to
// X.Y.Z+build.N.g<sha> and likewise counts as a known version.
//
// If the embedded Version is "dev" or otherwise unparseable, the function
// consults CRITERIA_OVERRIDE_VERSION. When that variable is set to a valid
// semantic version, the returned Info reflects it and marks Override.
//
// If no usable version can be determined, Known is false and Display is the
// raw embedded value.
func Current() Info {
	raw := Version
	override := os.Getenv(overrideEnv)

	// Release builds win: an explicit, parseable embedded version cannot be
	// overridden by the environment.
	if v, ok := parseVersion(raw); ok {
		return Info{
			Version: v,
			Known:   true,
			Display: display(v, false),
		}
	}

	if override != "" {
		if v, ok := parseVersion(override); ok {
			return Info{
				Version:  v,
				Known:    true,
				Display:  display(v, true),
				Override: true,
			}
		}
	}

	return Info{
		Version: semver.Version{},
		Known:   false,
		Display: raw,
	}
}

// With simulates a release build for tests and diagnostics. It returns an
// Info that behaves as if Version were set to s. The environment override is
// ignored because this is treated as a known release version.
func With(s string) Info {
	if v, ok := parseVersion(s); ok {
		return Info{Version: v, Known: true, Display: display(v, false)}
	}
	return Info{Display: s}
}

func parseVersion(s string) (semver.Version, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return semver.Version{}, false
	}
	// Git describe dev builds (vX.Y.Z-N-g<sha>) are N commits AHEAD of the
	// stable tag X.Y.Z, not prereleases of it; encode the distance as build
	// metadata so the version gate evaluates them as the stable base version.
	if v, ok := parseDescribe(s); ok {
		return v, true
	}
	v, err := semver.Parse(strings.TrimPrefix(s, "v"))
	if err != nil {
		return semver.Version{}, false
	}
	return v, true
}

// describeRe matches git describe output for a build cut after a stable
// release tag: vX.Y.Z-<N>-g<sha>, where N is the decimal commit distance from
// the tag and the last segment is "g" followed by the abbreviated commit SHA.
var describeRe = regexp.MustCompile(`^v?(\d+\.\d+\.\d+)-(\d+)-(g[0-9a-f]+)$`)

// parseDescribe rewrites git describe output into the semantic version it
// denotes: vX.Y.Z-N-g<sha> becomes X.Y.Z+build.N.g<sha>, so constraint
// evaluation treats the build as a stable version with build metadata.
//
// Describes cut from prerelease tags (e.g. v0.5.25-rc1-3-g<sha>) do not match:
// their extra label segment keeps them plain-prerelease parses, which the
// engine version gate rejects against stable lower bounds. Malformed describes
// (dirty suffixes, non-numeric distances, non-hex shas) likewise keep the
// plain parse.
func parseDescribe(s string) (semver.Version, bool) {
	m := describeRe.FindStringSubmatch(s)
	if m == nil {
		return semver.Version{}, false
	}
	v, err := semver.Parse(m[1] + "+build." + m[2] + "." + m[3])
	if err != nil {
		return semver.Version{}, false
	}
	return v, true
}

func display(v semver.Version, overridden bool) string {
	out := "v" + v.String()
	if overridden {
		out += fmt.Sprintf(" (overridden by %s)", overrideEnv)
	}
	return out
}
