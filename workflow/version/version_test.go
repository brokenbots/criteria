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
