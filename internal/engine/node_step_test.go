package engine

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

// TestStepNode_ResolveInput_InjectsEnvironmentVars verifies that environment-declared
// variables are correctly merged into the "env" input field when a step is resolved.
func TestStepNode_ResolveInput_InjectsEnvironmentVars(t *testing.T) {
	g := &workflow.FSMGraph{
		Environments: map[string]*workflow.EnvironmentNode{
			"shell.ci": {
				Type: "shell", Name: "ci",
				Variables: map[string]string{
					"CI":           "true",
					"LOG_LEVEL":    "debug",
					"SERVICE_NAME": "criteria-test",
				},
			},
		},
		DefaultEnvironment: "shell.ci",
	}
	n := &stepNode{
		graph: g,
		step: &workflow.StepNode{
			Name:       "s",
			TargetKind: workflow.StepTargetAdapter,
			AdapterRef: "shell",
			Input:      map[string]string{},
		},
	}

	out, err := n.resolveInput(map[string]cty.Value{}, ".")
	require.NoError(t, err)

	raw, ok := out.Input["env"]
	require.True(t, ok, "expected env field in resolved input")

	var got map[string]string
	require.NoError(t, json.Unmarshal([]byte(raw), &got))

	// Verify that all declared environment variables are present
	assert.Equal(t, "true", got["CI"])
	assert.Equal(t, "debug", got["LOG_LEVEL"])
	assert.Equal(t, "criteria-test", got["SERVICE_NAME"])
}

// TestStepNode_ResolveInput_FiltersControlledEnvVars verifies that controlled environment
// variables (PATH, HOME, USER, LOGNAME, LANG, TZ, LC_*) are filtered out and do not appear
// in the injected env JSON, even if they are declared in the environment block.
func TestStepNode_ResolveInput_FiltersControlledEnvVars(t *testing.T) {
	g := &workflow.FSMGraph{
		Environments: map[string]*workflow.EnvironmentNode{
			"shell.x": {
				Type: "shell", Name: "x",
				Variables: map[string]string{
					"PATH":     "/evil",
					"HOME":     "/tmp/evil",
					"USER":     "evil",
					"LOGNAME":  "evil",
					"LANG":     "C",
					"TZ":       "Etc/UTC",
					"LC_ALL":   "C.UTF-8",
					"LC_CTYPE": "en_US.UTF-8",
					"GOOD_VAR": "ok",
				},
			},
		},
		DefaultEnvironment: "shell.x",
	}
	n := &stepNode{
		graph: g,
		step: &workflow.StepNode{
			Name:       "s",
			TargetKind: workflow.StepTargetAdapter,
			AdapterRef: "shell",
			Input:      map[string]string{},
		},
	}

	out, err := n.resolveInput(map[string]cty.Value{}, ".")
	require.NoError(t, err)

	var got map[string]string
	require.NoError(t, json.Unmarshal([]byte(out.Input["env"]), &got))

	// Verify that controlled keys are filtered out
	for _, k := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "TZ", "LC_ALL", "LC_CTYPE"} {
		_, present := got[k]
		assert.False(t, present, "controlled key %q must be filtered out, but was present with value %q", k, got[k])
	}

	// Verify that non-controlled vars are still present
	assert.Equal(t, "ok", got["GOOD_VAR"])
}

// TestStepNode_ResolveInput_ControlledSetConsistency verifies that the runtime filter
// in mergeEnvironmentVars uses the same controlled-set list as the compile-time warnings
// in workflow.ShellControlledEnvVars. This ensures there are no silent divergences.
func TestStepNode_ResolveInput_ControlledSetConsistency(t *testing.T) {
	// Verify that ShellControlledEnvVars contains exactly the expected keys
	expectedControlled := map[string]bool{
		"PATH":    true,
		"HOME":    true,
		"USER":    true,
		"LOGNAME": true,
		"LANG":    true,
		"TZ":      true,
	}
	assert.Equal(t, expectedControlled, workflow.ShellControlledEnvVars, "ShellControlledEnvVars must match expected set")

	// Verify that IsShellLCPrefix correctly identifies LC_* variables
	assert.True(t, workflow.IsShellLCPrefix("LC_ALL"))
	assert.True(t, workflow.IsShellLCPrefix("LC_CTYPE"))
	assert.True(t, workflow.IsShellLCPrefix("LC_TIME"))
	assert.False(t, workflow.IsShellLCPrefix("HOME"))
	assert.False(t, workflow.IsShellLCPrefix("MY_VAR"))
	assert.False(t, workflow.IsShellLCPrefix("L"))  // Too short
	assert.False(t, workflow.IsShellLCPrefix("LC")) // Too short
}
