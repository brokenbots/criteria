package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestPlanGolden(t *testing.T) {
	repoRoot, fixtures := workflowFixtures(t)
	// Some fixtures reference files outside their own directory via file().
	// Allow the whole repo root so those references resolve at compile.
	t.Setenv("CRITERIA_WORKFLOW_ALLOWED_PATHS", repoRoot)
	for _, path := range fixtures {
		path := path
		relPath, _ := filepath.Rel(repoRoot, path)
		name := stripHCLExt(filepath.Base(path)) + "__" + sanitizeFixturePath(relPath)
		t.Run(name, func(t *testing.T) {
			out, err := renderPlanOutput(context.Background(), path, nil)
			if err != nil {
				t.Fatalf("plan output: %v", err)
			}
			assertGoldenFile(t, filepath.Join("testdata", "plan", name+".golden"), []byte(out))
		})
	}
}

// TestPlanOutput_SecretVariablesRedacted verifies that variables declared with
// secret = true are rendered as (sensitive) in plan output when supplied via
// --var or via a declared default, while an unset secret variable still shows
// (required). Non-secret variables continue to render normally.
func TestPlanOutput_SecretVariablesRedacted(t *testing.T) {
	path := writeWorkflowFile(t, `
workflow {
  name          = "secret-plan-test"
  version       = "0.0.1"
  initial_state = "start"
  target_state  = "start"
}

state "start" {
  terminal = true
  success  = true
}

variable "token" {
  type   = string
  secret = true
}

variable "api_key" {
  type   = string
  secret = true
}

variable "secret_region" {
  type    = string
  secret  = true
  default = "eu-central-1"
}

variable "region" {
  type    = string
  default = "us-east-1"
}
`)

	overrides := map[string]cty.Value{
		"token":  cty.StringVal("ghp_realsecret"),
		"region": cty.StringVal("us-west-2"),
	}

	out, err := renderPlanOutput(context.Background(), path, overrides)
	if err != nil {
		t.Fatalf("renderPlanOutput: %v", err)
	}

	if strings.Contains(out, "ghp_realsecret") || strings.Contains(out, "eu-central-1") {
		t.Errorf("plan output leaks secret value:\n%s", out)
	}
	if !strings.Contains(out, "token: string = (sensitive)  (override)") {
		t.Errorf("plan output did not mask secret override; want 'token: string = (sensitive)  (override)', got:\n%s", out)
	}
	if !strings.Contains(out, "api_key: string = (required)") {
		t.Errorf("plan output did not show unset secret as required; want 'api_key: string = (required)', got:\n%s", out)
	}
	if !strings.Contains(out, "secret_region: string = (sensitive)") {
		t.Errorf("plan output did not mask secret default; want 'secret_region: string = (sensitive)', got:\n%s", out)
	}
	if !strings.Contains(out, "region: string = us-west-2  (override)") {
		t.Errorf("plan output did not render non-secret override; got:\n%s", out)
	}
}
