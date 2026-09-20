package version

import (
	"testing"

	"github.com/blang/semver"
	"github.com/stretchr/testify/assert"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		input   string
		want    semver.Version
		wantOK  bool
		display string
	}{
		{"dev", semver.Version{}, false, "dev"},
		{"", semver.Version{}, false, ""},
		{"v0.5.8", semver.Version{Major: 0, Minor: 5, Patch: 8}, true, "v0.5.8"},
		{"0.5.8", semver.Version{Major: 0, Minor: 5, Patch: 8}, true, "v0.5.8"},
		{"v0.5.8+dirty", semver.Version{Major: 0, Minor: 5, Patch: 8, Build: []string{"dirty"}}, true, "v0.5.8+dirty"},
		{"0.5.9-rc1", semver.Version{Major: 0, Minor: 5, Patch: 9, Pre: []semver.PRVersion{{VersionStr: "rc1"}}}, true, "v0.5.9-rc1"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, ok := parseVersion(tc.input)
			assert.Equal(t, tc.wantOK, ok)
			if ok {
				assert.Equal(t, tc.want, got)
				assert.Equal(t, tc.display, display(got, false))
			}
		})
	}
}

func TestParseVersionGitDescribe(t *testing.T) {
	cases := []struct {
		input      string
		wantOK     bool
		want       semver.Version
		display    string
		wantStable bool
	}{
		// Git describe dev builds are N commits AHEAD of the stable tag: they
		// normalize to the stable base version with build metadata, never a
		// prerelease (CRI-266).
		{
			input:      "v0.5.24-12-g870e28f",
			wantOK:     true,
			want:       semver.Version{Major: 0, Minor: 5, Patch: 24, Build: []string{"build", "12", "g870e28f"}},
			display:    "v0.5.24+build.12.g870e28f",
			wantStable: true,
		},
		{
			input:      "0.5.24-1-gabcdef0",
			wantOK:     true,
			want:       semver.Version{Major: 0, Minor: 5, Patch: 24, Build: []string{"build", "1", "gabcdef0"}},
			display:    "v0.5.24+build.1.gabcdef0",
			wantStable: true,
		},

		// Invalid describes are unchanged: they keep the plain semver parse,
		// which yields a prerelease that the version gate rejects against
		// stable lower bounds.
		{
			input:   "v0.5.24-12-g870e28f-dirty",
			wantOK:  true,
			want:    semver.Version{Major: 0, Minor: 5, Patch: 24, Pre: []semver.PRVersion{{VersionStr: "12-g870e28f-dirty"}}},
			display: "v0.5.24-12-g870e28f-dirty",
		},
		{
			input:   "v0.5.25-rc1-3-g870e28f",
			wantOK:  true,
			want:    semver.Version{Major: 0, Minor: 5, Patch: 25, Pre: []semver.PRVersion{{VersionStr: "rc1-3-g870e28f"}}},
			display: "v0.5.25-rc1-3-g870e28f",
		},
		{
			input:   "v0.5.24-abc-g870e28f",
			wantOK:  true,
			want:    semver.Version{Major: 0, Minor: 5, Patch: 24, Pre: []semver.PRVersion{{VersionStr: "abc-g870e28f"}}},
			display: "v0.5.24-abc-g870e28f",
		},
		{
			input:   "v0.5.24-12-gzzz",
			wantOK:  true,
			want:    semver.Version{Major: 0, Minor: 5, Patch: 24, Pre: []semver.PRVersion{{VersionStr: "12-gzzz"}}},
			display: "v0.5.24-12-gzzz",
		},
		{
			input:   "1.2.3-4-2-gabc",
			wantOK:  true,
			want:    semver.Version{Major: 1, Minor: 2, Patch: 3, Pre: []semver.PRVersion{{VersionStr: "4-2-gabc"}}},
			display: "v1.2.3-4-2-gabc",
		},

		// A bare short SHA (git describe --always with no tags) stays
		// unparseable and fails closed.
		{input: "870e28f", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, ok := parseVersion(tc.input)
			assert.Equal(t, tc.wantOK, ok)
			if !ok {
				return
			}
			assert.Equal(t, 0, got.Compare(tc.want))
			if tc.wantStable {
				assert.Empty(t, got.Pre, "git-describe dev builds must not evaluate as prereleases")
			}
			assert.Equal(t, tc.display, display(got, false))
		})
	}
}

func TestCurrentGitDescribeBuild(t *testing.T) {
	orig := Version
	defer func() { Version = orig }()

	Version = "v0.5.24-12-g870e28f"
	t.Setenv(overrideEnv, "0.9.9") // describe output parses; override stays ignored
	info := Current()
	assert.True(t, info.Known)
	assert.False(t, info.Override)
	assert.Equal(t, "v0.5.24+build.12.g870e28f", info.Display)
	assert.Empty(t, info.Version.Pre)
}

func TestCurrentReleaseBuild(t *testing.T) {
	orig := Version
	defer func() { Version = orig }()

	Version = "v0.5.8"
	t.Setenv(overrideEnv, "0.9.9") // ignored for release builds
	info := Current()
	assert.True(t, info.Known)
	assert.Equal(t, "v0.5.8", info.Display)
	assert.False(t, info.Override)
}

func TestCurrentDevOverride(t *testing.T) {
	orig := Version
	defer func() { Version = orig }()

	Version = "dev"
	t.Setenv(overrideEnv, "0.5.8")
	info := Current()
	assert.True(t, info.Known)
	assert.Equal(t, "v0.5.8 (overridden by CRITERIA_OVERRIDE_VERSION)", info.Display)
	assert.True(t, info.Override)
}

func TestCurrentDevUnknown(t *testing.T) {
	orig := Version
	defer func() { Version = orig }()

	Version = "dev"
	t.Setenv(overrideEnv, "")
	info := Current()
	assert.False(t, info.Known)
	assert.Equal(t, "dev", info.Display)
}

func TestWith(t *testing.T) {
	info := With("v0.6.0")
	assert.True(t, info.Known)
	assert.Equal(t, "v0.6.0", info.Display)
	assert.False(t, info.Override)

	bad := With("dev")
	assert.False(t, bad.Known)
	assert.Equal(t, "dev", bad.Display)
}
