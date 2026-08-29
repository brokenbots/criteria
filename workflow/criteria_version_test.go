package workflow

import (
	"strings"
	"testing"

	"github.com/blang/semver"
	"github.com/hashicorp/hcl/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/workflow/version"
)

func mustParse(t *testing.T, s string) semver.Version {
	t.Helper()
	v, err := semver.Parse(strings.TrimPrefix(s, "v"))
	require.NoError(t, err)
	return v
}

func TestParseCriteriaVersionConstraint(t *testing.T) {
	cases := []struct {
		input string
		want  []struct {
			op  string
			ver semver.Version
		}
	}{
		{"0.5.8", []struct {
			op  string
			ver semver.Version
		}{{"=", mustParse(t, "0.5.8")}}},
		{"=0.5.8", []struct {
			op  string
			ver semver.Version
		}{{"=", mustParse(t, "0.5.8")}}},
		{"==0.5.8", []struct {
			op  string
			ver semver.Version
		}{{"==", mustParse(t, "0.5.8")}}},
		{">=0.5.8", []struct {
			op  string
			ver semver.Version
		}{{">=", mustParse(t, "0.5.8")}}},
		{">0.5.7", []struct {
			op  string
			ver semver.Version
		}{{">", mustParse(t, "0.5.7")}}},
		{"<=0.6.0", []struct {
			op  string
			ver semver.Version
		}{{"<=", mustParse(t, "0.6.0")}}},
		{"<0.6.0", []struct {
			op  string
			ver semver.Version
		}{{"<", mustParse(t, "0.6.0")}}},
		{"v0.5.8", []struct {
			op  string
			ver semver.Version
		}{{"=", mustParse(t, "0.5.8")}}},
		{">=0.5.8, <0.6.0", []struct {
			op  string
			ver semver.Version
		}{{">=", mustParse(t, "0.5.8")}, {"<", mustParse(t, "0.6.0")}}},
		{"!=0.5.7", []struct {
			op  string
			ver semver.Version
		}{{"!=", mustParse(t, "0.5.7")}}},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			c, err := ParseCriteriaVersionConstraint(tc.input)
			require.NoError(t, err)
			require.Len(t, c.ranges, len(tc.want))
			for i, w := range tc.want {
				assert.Equal(t, w.op, c.ranges[i].op)
				assert.Equal(t, 0, c.ranges[i].ver.Compare(w.ver))
			}
		})
	}
}

func TestParseCriteriaVersionConstraintErrors(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"latest",
		"~0.5.8",
		">=0.5.8,",
		", <0.6.0",
		">=not-a-version",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			_, err := ParseCriteriaVersionConstraint(tc)
			require.Error(t, err)
		})
	}
}

func TestCriteriaVersionConstraintAllow(t *testing.T) {
	cases := []struct {
		name       string
		constraint string
		version    string
		want       bool
	}{
		// exact, bounds, conjunction
		{"exact pass", "=0.5.8", "0.5.8", true},
		{"exact fail", "=0.5.8", "0.5.9", false},
		{"lower bound pass", ">=0.5.8", "0.5.9", true},
		{"lower bound fail", ">=0.5.8", "0.5.7", false},
		{"upper bound pass", "<0.6.0", "0.5.9", true},
		{"upper bound fail", "<0.6.0", "0.6.0", false},
		{"conjunction pass", ">=0.5.8, <0.6.0", "0.5.9", true},
		{"conjunction low fail", ">=0.5.8, <0.6.0", "0.5.7", false},
		{"conjunction high fail", ">=0.5.8, <0.6.0", "0.6.0", false},

		// leading v and build metadata
		{"leading v in constraint", "v0.5.8", "0.5.8", true},
		{"leading v in version", "=0.5.8", "v0.5.8", true},
		{"build metadata ignored", ">=0.5.8", "v0.5.8+dirty", true},
		{"exact build metadata allowed", "=0.5.8", "0.5.8+dirty", true},

		// prerelease precedence
		{"prerelease below stable lower bound", ">=0.5.8", "0.5.9-rc1", false},
		{"prerelease satisfies explicit prerelease bound", ">=0.5.9-rc1", "0.5.9-rc1", true},
		{"prerelease satisfies explicit stable+prerelease bounds", ">=0.5.8, >=0.5.9-rc1", "0.5.9-rc1", true},
		{"prerelease below explicit prerelease bound", ">=0.5.9-rc2", "0.5.9-rc1", false},
		{"prerelease upper bound", "<0.6.0", "0.5.9-rc1", true},
		{"prerelease exact", "=0.5.9-rc1", "0.5.9-rc1", true},
		{"prerelease exact fail", "=0.5.9-rc1", "0.5.9-rc2", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseCriteriaVersionConstraint(tc.constraint)
			require.NoError(t, err)
			v := mustParse(t, tc.version)
			assert.Equal(t, tc.want, c.Allow(v))
		})
	}
}

func TestCriteriaVersionConstraintExampleVersion(t *testing.T) {
	cases := []struct {
		constraint string
		want       semver.Version
	}{
		{">=0.5.8, <0.6.0", semver.Version{Major: 0, Minor: 5, Patch: 8}},
		{"=0.5.8", semver.Version{Major: 0, Minor: 5, Patch: 8}},
		{"<0.6.0", semver.Version{}},
		{"<=0.6.0", semver.Version{Major: 0, Minor: 6, Patch: 0}},
		{"!=0.5.7", semver.Version{}},
		{">=0.5.9-rc1", semver.Version{}},
	}
	for _, tc := range cases {
		t.Run(tc.constraint, func(t *testing.T) {
			c, err := ParseCriteriaVersionConstraint(tc.constraint)
			require.NoError(t, err)
			assert.Equal(t, tc.want, c.ExampleVersion())
		})
	}
}

func TestCheckCriteriaVersionDiagnostics(t *testing.T) {
	orig := version.Version
	defer func() { version.Version = orig }()

	makeRange := func() *hcl.Range {
		return &hcl.Range{
			Filename: "workflow.chcl",
			Start:    hcl.Pos{Line: 2, Column: 3},
			End:      hcl.Pos{Line: 2, Column: 25},
		}
	}

	t.Run("empty constraint passes", func(t *testing.T) {
		version.Version = "dev"
		diags := checkCriteriaVersion("wf", "", nil, nil)
		assert.False(t, diags.HasErrors())
	})

	t.Run("known release matches", func(t *testing.T) {
		version.Version = "v0.5.8"
		diags := checkCriteriaVersion("wf", ">=0.5.8, <0.6.0", makeRange(), nil)
		assert.False(t, diags.HasErrors())
	})

	t.Run("known release rejects", func(t *testing.T) {
		version.Version = "v0.5.7"
		diags := checkCriteriaVersion("wf", ">=0.5.8, <0.6.0", makeRange(), nil)
		require.True(t, diags.HasErrors())
		assert.Contains(t, diags.Error(), `workflow "wf" requires Criteria >=0.5.8, <0.6.0; running engine is v0.5.7`)
		assert.NotNil(t, diags[0].Subject)
	})

	t.Run("nested chain", func(t *testing.T) {
		version.Version = "v0.5.8"
		diags := checkCriteriaVersion("child", ">=0.5.9", makeRange(), []string{"parent"})
		require.True(t, diags.HasErrors())
		assert.Contains(t, diags.Error(), `subworkflow "child" requires Criteria >=0.5.9; running engine is v0.5.8`)
		assert.Contains(t, diags.Error(), "required by parent -> child")
	})

	t.Run("unknown dev build fails closed", func(t *testing.T) {
		version.Version = "dev"
		t.Setenv(version.OverrideEnv(), "")
		diags := checkCriteriaVersion("wf", ">=0.5.8", makeRange(), nil)
		require.True(t, diags.HasErrors())
		assert.Contains(t, diags.Error(), "running engine version is unknown")
		assert.Contains(t, diags.Error(), version.OverrideEnv())
		assert.Contains(t, diags.Error(), "set CRITERIA_OVERRIDE_VERSION to an explicit semver such as \"v0.5.8\"")
		assert.Contains(t, diags.Error(), version.LdflagsExample("v0.5.8"))
	})

	t.Run("invalid constraint", func(t *testing.T) {
		version.Version = "v0.5.8"
		diags := checkCriteriaVersion("wf", "latest", makeRange(), nil)
		require.True(t, diags.HasErrors())
		assert.Contains(t, diags.Error(), `workflow "wf" declares invalid criteria_version "latest"`)
	})
}
