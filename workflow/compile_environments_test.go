package workflow

// compile_environments_test.go — unit tests for environment block compilation.

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// environmentWorkflow wraps environment and step blocks into a minimal compilable workflow HCL.
func environmentWorkflow(envBlocks, extraHeaderAttrs string) string {
	header := `workflow {
  name = "test"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
` + extraHeaderAttrs + `}

state "done" {
  terminal = true
  success  = true
}
`
	return header + envBlocks
}

func TestCompileEnvironments_Single(t *testing.T) {
	// Single environment block should compile without error.
	src := environmentWorkflow(`
  environment "shell" "default" {
    variables = {
      CI = "true"
      LOG_LEVEL = "debug"
    }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	env, ok := g.Environments["shell.default"]
	if !ok {
		t.Fatal("environment shell.default not found in graph")
	}

	if env.Type != "shell" {
		t.Errorf("expected type 'shell', got %q", env.Type)
	}
	if env.Name != "default" {
		t.Errorf("expected name 'default', got %q", env.Name)
	}

	if env.Variables["CI"] != "true" {
		t.Errorf("expected CI=true, got %q", env.Variables["CI"])
	}
	if env.Variables["LOG_LEVEL"] != "debug" {
		t.Errorf("expected LOG_LEVEL=debug, got %q", env.Variables["LOG_LEVEL"])
	}

	// Verify that single environment becomes the default.
	if g.DefaultEnvironment != "shell.default" {
		t.Errorf("expected default environment 'shell.default', got %q", g.DefaultEnvironment)
	}
}

func TestCompileEnvironments_DuplicateTypeAndName(t *testing.T) {
	// Duplicate <type>.<name> should error.
	src := environmentWorkflow(`
  environment "shell" "default" {
    variables = { X = "1" }
  }
  environment "shell" "default" {
    variables = { Y = "2" }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	diags = nil // Reset diags to capture compile result
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected error for duplicate environment")
	}
}

func TestCompileEnvironments_UnknownType(t *testing.T) {
	// Unknown environment type should error.
	src := environmentWorkflow(`
  environment "docker" "default" {
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	diags = nil // Reset diags to capture compile result
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected error for unknown environment type")
	}
	if !strings.Contains(diags.Error(), "not registered") {
		t.Errorf("expected 'not registered' in error, got: %v", diags.Error())
	}
}

func TestCompileEnvironments_InvalidName(t *testing.T) {
	// Names starting with digits should error.
	src := environmentWorkflow(`
  environment "shell" "123invalid" {
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	diags = nil // Reset diags to capture compile result
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected error for invalid name starting with digit")
	}
}

func TestCompileEnvironments_ValidNameFormats(t *testing.T) {
	// Valid names: letters, underscores, hyphens.
	validNames := []string{"dev", "prod_1", "my-env"}
	for _, name := range validNames {
		src := environmentWorkflow(`
  environment "shell" "`+name+`" {
    variables = { X = "1" }
  }
`, "")
		spec, diags := Parse("test.hcl", []byte(src))
		if diags.HasErrors() {
			t.Fatalf("parse(%s): %s", name, diags.Error())
		}
		g, diags := Compile(spec, nil)
		if diags.HasErrors() {
			t.Fatalf("compile(%s): %s", name, diags.Error())
		}
		key := "shell." + name
		if _, ok := g.Environments[key]; !ok {
			t.Errorf("environment %s not found", key)
		}
	}
}

func TestCompileEnvironments_VariablesFold(t *testing.T) {
	// Variables expression should fold at compile time.
	src := environmentWorkflow(`
  environment "shell" "test" {
    variables = {
      X = "42"
      Y = "true"
      Z = 99
    }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	env := g.Environments["shell.test"]
	if env.Variables["X"] != "42" {
		t.Errorf("expected X=42, got %q", env.Variables["X"])
	}
	if env.Variables["Y"] != "true" {
		t.Errorf("expected Y=true, got %q", env.Variables["Y"])
	}
	if env.Variables["Z"] != "99" {
		t.Errorf("expected Z=99, got %q", env.Variables["Z"])
	}
}

func TestCompileEnvironments_VariablesRuntimeRef(t *testing.T) {
	// Variables with runtime references should error.
	src := environmentWorkflow(`
  environment "shell" "test" {
    variables = {
      X = each.value
    }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	diags = nil // Reset diags to capture compile result
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected error for runtime reference in variables")
	}
}

func TestCompileEnvironments_ConfigFold(t *testing.T) {
	// Config expression should fold at compile time.
	src := environmentWorkflow(`
  environment "shell" "test" {
    config = {
      foo = "bar"
      num = 42
      nested = {
        key = "value"
      }
    }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	env := g.Environments["shell.test"]
	if env.Config == nil {
		t.Fatal("config is nil")
	}

	if env.Config["foo"].AsString() != "bar" {
		t.Errorf("expected foo=bar, got %v", env.Config["foo"])
	}
}

func TestCompileEnvironments_MultipleNoDefault(t *testing.T) {
	// Multiple environments without explicit default should not error at compile,
	// but DefaultEnvironment should be empty. When consumer-binding surface lands
	// (WS11/WS14 for adapter and step environment= attributes), add
	// TestCompileEnvironments_DefaultMultipleNoDefault to verify compilation fails
	// when a consumer references an unbound environment with multiple available options.
	src := environmentWorkflow(`
  environment "shell" "dev" {
  }
  environment "shell" "prod" {
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	// Multiple environments with no explicit default should not set a default.
	if g.DefaultEnvironment != "" {
		t.Errorf("expected empty default environment, got %q", g.DefaultEnvironment)
	}
}

func TestCompileEnvironments_ExplicitDefault(t *testing.T) {
	// Explicit default environment should be respected.
	src := environmentWorkflow(`
  environment "shell" "dev" {
  }
  environment "shell" "prod" {
  }
`, `  environment = shell.prod
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	if g.DefaultEnvironment != "shell.prod" {
		t.Errorf("expected default environment 'shell.prod', got %q", g.DefaultEnvironment)
	}
}

func TestCompileEnvironments_NonexistentDefault(t *testing.T) {
	// Explicit default referencing a non-existent environment should error.
	src := environmentWorkflow(`
  environment "shell" "real" {
  }
`, `  environment = shell.nonexistent
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	diags = nil // Reset diags to capture compile result
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected error for nonexistent default environment")
	}
}

func TestCompileEnvironments_WithVariablesAndConfig(t *testing.T) {
	// Environment with both variables and config.
	src := environmentWorkflow(`
  environment "shell" "complete" {
    variables = {
      ENV_NAME = "prod"
      DEBUG = "false"
    }
    config = {
      timeout_seconds = 300
      retry_limit = 3
    }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	env := g.Environments["shell.complete"]
	if env.Variables["ENV_NAME"] != "prod" {
		t.Errorf("expected ENV_NAME=prod, got %q", env.Variables["ENV_NAME"])
	}
	if !env.Config["timeout_seconds"].RawEquals(cty.NumberIntVal(300)) {
		t.Errorf("expected timeout_seconds=300, got %v", env.Config["timeout_seconds"])
	}
}

func TestCompileEnvironments_Empty(t *testing.T) {
	// Workflow with no environment blocks should compile fine.
	src := environmentWorkflow("", "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	if len(g.Environments) != 0 {
		t.Errorf("expected no environments, got %d", len(g.Environments))
	}
	if g.DefaultEnvironment != "" {
		t.Errorf("expected empty default, got %q", g.DefaultEnvironment)
	}
}

func TestCompileEnvironments_ControlledSetConflictWarning(t *testing.T) {
	// Environment with variables that conflict with shell's controlled set should produce warnings.
	src := environmentWorkflow(`
  environment "shell" "prod" {
    variables = {
      PATH = "/custom/bin"
      HOME = "/tmp"
      X    = "y"
    }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	// Should compile successfully but with warnings
	if diags.HasErrors() {
		t.Fatalf("compile had errors: %s", diags.Error())
	}

	// Should have warnings for PATH and HOME
	hasPathWarning := false
	hasHomeWarning := false
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning {
			if strings.Contains(d.Summary, "PATH") {
				hasPathWarning = true
			}
			if strings.Contains(d.Summary, "HOME") {
				hasHomeWarning = true
			}
		}
	}

	if !hasPathWarning {
		t.Error("expected warning for PATH conflict")
	}
	if !hasHomeWarning {
		t.Error("expected warning for HOME conflict")
	}

	// Environment should still be stored with the conflicting variables
	if env, ok := g.Environments["shell.prod"]; !ok {
		t.Fatal("environment shell.prod not found")
	} else if env.Variables["PATH"] != "/custom/bin" || env.Variables["HOME"] != "/tmp" {
		t.Error("environment variables not stored correctly")
	}
}

func TestCompileEnvironments_PolicyMode(t *testing.T) {
	// Environment with policy_mode = "strict" should parse correctly.
	src := environmentWorkflow(`
  environment "shell" "strict_env" {
    policy_mode = "strict"
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	env := g.Environments["shell.strict_env"]
	if env == nil {
		t.Fatal("environment shell.strict_env not found")
	}
	if env.PolicyMode != "strict" {
		t.Errorf("expected policy_mode 'strict', got %q", env.PolicyMode)
	}
}

func TestCompileEnvironments_InvalidPolicyMode(t *testing.T) {
	// Invalid policy_mode should error.
	src := environmentWorkflow(`
  environment "shell" "bad" {
    policy_mode = "lax"
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for invalid policy_mode")
	}
	if !strings.Contains(diags.Error(), "policy_mode") {
		t.Errorf("expected error about policy_mode, got: %s", diags.Error())
	}
}

func TestCompileEnvironments_WorkingDirectory(t *testing.T) {
	// shell, sandbox, and remote environments accept working_directory and store
	// it on the compiled node; container environments reject it.
	cases := []struct {
		name      string
		envType   string
		extra     string
		wantError bool
	}{
		{name: "shell", envType: "shell"},
		{name: "sandbox", envType: "sandbox"},
		{name: "remote", envType: "remote", extra: "    listen_address = \"127.0.0.1:0\"\n"},
		{name: "container rejected", envType: "container", extra: "    image = \"alpine\"\n", wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := environmentWorkflow(`
  environment "`+tc.envType+`" "wd" {
    working_directory = "/tmp/worktree"
`+tc.extra+`  }
`, "")
			spec, diags := Parse("test.hcl", []byte(src))
			if diags.HasErrors() {
				t.Fatalf("parse: %s", diags.Error())
			}
			g, diags := Compile(spec, nil)
			if tc.wantError {
				if !diags.HasErrors() {
					t.Fatal("expected compile error for container working_directory")
				}
				if !strings.Contains(diags.Error(), "working_directory") {
					t.Errorf("expected error to mention working_directory, got: %s", diags.Error())
				}
				return
			}
			if diags.HasErrors() {
				t.Fatalf("compile: %s", diags.Error())
			}
			env := g.Environments[tc.envType+".wd"]
			if env == nil {
				t.Fatalf("environment %s.wd not found", tc.envType)
			}
			if env.WorkingDirectory != "/tmp/worktree" {
				t.Errorf("WorkingDirectory = %q, want %q", env.WorkingDirectory, "/tmp/worktree")
			}
			// working_directory must not leak into TypeSpecific.
			if _, ok := env.TypeSpecific["working_directory"]; ok {
				t.Error("working_directory should not be stored in TypeSpecific")
			}
		})
	}
}

func TestCompileEnvironments_WorkingDirectory_VarAndLocalRefs(t *testing.T) {
	// working_directory folds against the compile-time closure, so it can
	// reference declared variables and locals and use interpolation.
	src := `workflow {
  name          = "t"
  version       = "1"
  initial_state = "done"
  target_state  = "done"
}
variable "root" {
  type    = string
  default = "/work"
}
local "sub" {
  value = "${var.root}/sub"
}
environment "shell" "e" {
  working_directory = "${local.sub}/wt"
}
state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if got := g.Environments["shell.e"].WorkingDirectory; got != "/work/sub/wt" {
		t.Errorf("WorkingDirectory = %q, want %q", got, "/work/sub/wt")
	}
}

func TestCompileEnvironments_WorkingDirectory_RuntimeRefRejected(t *testing.T) {
	// Runtime-only references (each.*, steps.*) are not foldable and must error.
	src := environmentWorkflow(`
  environment "shell" "e" {
    working_directory = each.value
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for runtime-only working_directory reference")
	}
	if !strings.Contains(diags.Error(), "working_directory") {
		t.Errorf("expected error to mention working_directory, got: %s", diags.Error())
	}
}

func TestCompileEnvironments_OSAttribute(t *testing.T) {
	// Environment with os attribute should parse correctly. Pin the host OS so
	// the compile-time OS gate is satisfied regardless of the test host.
	old := envRegistryHostOS
	envRegistryHostOS = "linux"
	defer func() { envRegistryHostOS = old }()

	src := environmentWorkflow(`
  environment "shell" "linux_env" {
    os = "linux"
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	env := g.Environments["shell.linux_env"]
	if env == nil {
		t.Fatal("environment shell.linux_env not found")
	}
	if env.OS != "linux" {
		t.Errorf("expected os 'linux', got %q", env.OS)
	}
}

func TestCompileEnvironments_TypeSpecific(t *testing.T) {
	// Environment with unknown attributes should be collected in TypeSpecific
	// when the registry handler permits them.
	src := environmentWorkflow(`
  environment "permissive" "custom" {
    custom_attr = "value"
    another     = 42
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	// Use a permissive test registry that allows any attribute.
	registry := &testEnvRegistry{handler: &testPermissiveHandler{typ: "permissive"}}
	g := newFSMGraph(spec)
	diags = compileEnvironments(g, spec, CompileOpts{}, registry)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	// Resolve default environment manually since compileEnvironments does it.
	g.DefaultEnvironment = "permissive.custom"
	env := g.Environments["permissive.custom"]
	if env == nil {
		t.Fatal("environment permissive.custom not found")
	}
	if len(env.TypeSpecific) == 0 {
		t.Fatal("expected TypeSpecific to contain unknown attributes")
	}
	val := env.TypeSpecific["custom_attr"]
	if val == cty.NilVal || !val.IsKnown() || val.AsString() != "value" {
		t.Errorf("expected TypeSpecific['custom_attr'] = 'value', got %v", val)
	}
	val2 := env.TypeSpecific["another"]
	if val2 == cty.NilVal || !val2.IsKnown() || !cty.NumberIntVal(42).RawEquals(val2) {
		t.Errorf("expected TypeSpecific['another'] = 42, got %v", val2)
	}
}

// testEnvRegistry is a test double that returns a single handler.
type testEnvRegistry struct {
	handler EnvHandler
}

func (r *testEnvRegistry) Lookup(envType string) EnvHandler {
	if r.handler != nil && r.handler.Type() == envType {
		return r.handler
	}
	return nil
}

func (r *testEnvRegistry) Registered() []string {
	if r.handler == nil {
		return nil
	}
	return []string{r.handler.Type()}
}

// testPermissiveHandler is a test double that accepts any environment attribute.
type testPermissiveHandler struct {
	typ string
}

func (h *testPermissiveHandler) Type() string                                { return h.typ }
func (h *testPermissiveHandler) SupportedOSes() []string                     { return nil }
func (h *testPermissiveHandler) ValidateFields(_ hcl.Body) hcl.Diagnostics   { return nil }
func (h *testPermissiveHandler) IsolationKind() EnvIsolationKind             { return EnvIsolationNone }
func (h *testPermissiveHandler) Prepare(_ context.Context, _ hcl.Body) error { return nil }

func TestCompileEnvironments_OSGateMismatch(t *testing.T) {
	// If the environment declares an os that does not match the host GOOS,
	// compilation should fail with a clear error.
	old := envRegistryHostOS
	envRegistryHostOS = "darwin" // override host OS for this test
	defer func() { envRegistryHostOS = old }()

	src := environmentWorkflow(`
  environment "shell" "wasi_env" {
    os = "wasi"
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for OS mismatch")
	}
	if !strings.Contains(diags.Error(), "wasi") {
		t.Errorf("expected error mentioning 'wasi', got: %s", diags.Error())
	}
	if !strings.Contains(diags.Error(), "darwin") {
		t.Errorf("expected error mentioning host OS 'darwin', got: %s", diags.Error())
	}
}

// TestResolveEnvironmentPolicy_ThreeRules verifies D37's three-rule field
// resolution with table-driven cases covering all combinations.
func TestResolveEnvironmentPolicy_ThreeRules(t *testing.T) {
	fsRO := &FilesystemPolicy{ReadOnly: true}
	fsRW := &FilesystemPolicy{ReadOnly: false}
	netAllow := &NetworkPolicy{AllowEgress: true}
	netDeny := &NetworkPolicy{AllowEgress: false}
	secVault := &SecretsPolicy{Provider: "vault"}
	res1G := &ResourcesPolicy{MaxMemory: "1G"}

	tests := []struct {
		name  string
		env   *EnvironmentNode
		hints *PolicyHints
		want  *ResolvedPolicy
	}{
		{
			name: "rule1_env_wins_over_hint_and_mode",
			env: &EnvironmentNode{
				PolicyMode:   "permissive",
				OS:           "linux",
				Filesystem:   fsRO,
				Network:      netDeny,
				Secrets:      secVault,
				Resources:    res1G,
				TypeSpecific: map[string]cty.Value{"runtime": cty.StringVal("docker")},
			},
			hints: &PolicyHints{
				OS:           "darwin",
				Filesystem:   fsRW,
				Network:      netAllow,
				Secrets:      &SecretsPolicy{Provider: "aws"},
				Resources:    &ResourcesPolicy{MaxMemory: "2G"},
				TypeSpecific: map[string]cty.Value{"runtime": cty.StringVal("podman")},
			},
			want: &ResolvedPolicy{
				PolicyMode:   "permissive",
				OS:           "linux",
				Filesystem:   fsRO,
				Network:      netDeny,
				Secrets:      secVault,
				Resources:    res1G,
				TypeSpecific: map[string]cty.Value{"runtime": cty.StringVal("docker")},
			},
		},
		{
			name: "rule2_permissive_falls_back_to_hint",
			env: &EnvironmentNode{
				PolicyMode: "permissive",
				OS:         "",
				Filesystem: nil,
				Network:    nil,
				Secrets:    nil,
				Resources:  nil,
			},
			hints: &PolicyHints{
				OS:           "darwin",
				Filesystem:   fsRW,
				Network:      netAllow,
				Secrets:      &SecretsPolicy{Provider: "aws"},
				Resources:    &ResourcesPolicy{MaxMemory: "2G"},
				TypeSpecific: map[string]cty.Value{"runtime": cty.StringVal("podman")},
			},
			want: &ResolvedPolicy{
				PolicyMode:   "permissive",
				OS:           "darwin",
				Filesystem:   fsRW,
				Network:      netAllow,
				Secrets:      &SecretsPolicy{Provider: "aws"},
				Resources:    &ResourcesPolicy{MaxMemory: "2G"},
				TypeSpecific: map[string]cty.Value{"runtime": cty.StringVal("podman")},
			},
		},
		{
			name: "rule3_strict_default_deny",
			env: &EnvironmentNode{
				PolicyMode: "strict",
				OS:         "",
				Filesystem: nil,
				Network:    nil,
				Secrets:    nil,
				Resources:  nil,
			},
			hints: &PolicyHints{
				OS:           "darwin",
				Filesystem:   fsRW,
				Network:      netAllow,
				Secrets:      &SecretsPolicy{Provider: "aws"},
				Resources:    &ResourcesPolicy{MaxMemory: "2G"},
				TypeSpecific: map[string]cty.Value{"runtime": cty.StringVal("podman")},
			},
			want: &ResolvedPolicy{
				PolicyMode: "strict",
				OS:         "",
				// All other fields remain nil / zero → default-deny.
			},
		},
		{
			name: "rule1_partial_env_wins_rest_hint_in_permissive",
			env: &EnvironmentNode{
				PolicyMode: "permissive",
				OS:         "linux",
				Filesystem: fsRO,
				// Network, Secrets, Resources, TypeSpecific left unset.
			},
			hints: &PolicyHints{
				OS:           "darwin",
				Filesystem:   fsRW,
				Network:      netAllow,
				Secrets:      &SecretsPolicy{Provider: "aws"},
				Resources:    &ResourcesPolicy{MaxMemory: "2G"},
				TypeSpecific: map[string]cty.Value{"runtime": cty.StringVal("podman")},
			},
			want: &ResolvedPolicy{
				PolicyMode:   "permissive",
				OS:           "linux",
				Filesystem:   fsRO,
				Network:      netAllow,
				Secrets:      &SecretsPolicy{Provider: "aws"},
				Resources:    &ResourcesPolicy{MaxMemory: "2G"},
				TypeSpecific: map[string]cty.Value{"runtime": cty.StringVal("podman")},
			},
		},
		{
			name: "nil_env_returns_defaults",
			env:  nil,
			hints: &PolicyHints{
				OS: "darwin",
			},
			want: &ResolvedPolicy{PolicyMode: "permissive"},
		},
		{
			name: "permissive_no_hints_all_zero",
			env: &EnvironmentNode{
				PolicyMode: "permissive",
			},
			hints: nil,
			want:  &ResolvedPolicy{PolicyMode: "permissive"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveEnvironmentPolicy(tt.env, tt.hints)
			if got.PolicyMode != tt.want.PolicyMode {
				t.Errorf("PolicyMode = %q, want %q", got.PolicyMode, tt.want.PolicyMode)
			}
			if got.OS != tt.want.OS {
				t.Errorf("OS = %q, want %q", got.OS, tt.want.OS)
			}
			if !filesystemPolicyEqual(got.Filesystem, tt.want.Filesystem) {
				t.Errorf("Filesystem = %v, want %v", got.Filesystem, tt.want.Filesystem)
			}
			if !networkPolicyEqual(got.Network, tt.want.Network) {
				t.Errorf("Network = %v, want %v", got.Network, tt.want.Network)
			}
			if !secretsPolicyEqual(got.Secrets, tt.want.Secrets) {
				t.Errorf("Secrets = %v, want %v", got.Secrets, tt.want.Secrets)
			}
			if !resourcesPolicyEqual(got.Resources, tt.want.Resources) {
				t.Errorf("Resources = %v, want %v", got.Resources, tt.want.Resources)
			}
			if !ctyMapEqual(got.TypeSpecific, tt.want.TypeSpecific) {
				t.Errorf("TypeSpecific = %v, want %v", got.TypeSpecific, tt.want.TypeSpecific)
			}
		})
	}
}

// TestResolvedPolicy_CachedOnFSMGraph verifies that compiling an adapter bound
// to an environment stores a ResolvedPolicy in FSMGraph.ResolvedPolicies.
func TestResolvedPolicy_CachedOnFSMGraph(t *testing.T) {
	schemas := map[string]AdapterInfo{
		"shell": {
			ConfigSchema: map[string]ConfigField{},
			PolicyHints: &PolicyHints{
				OS:         "linux",
				Filesystem: &FilesystemPolicy{ReadOnly: true},
			},
		},
	}

	old := envRegistryHostOS
	envRegistryHostOS = "linux"
	defer func() { envRegistryHostOS = old }()

	src := `
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

environment "shell" "prod" {
  os = "linux"
}

adapter "shell" "default" {
  environment = shell.prod
}

state "done" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	cacheKey := "shell.default:shell.prod"
	rp, ok := g.ResolvedPolicies[cacheKey]
	if !ok {
		t.Fatalf("expected ResolvedPolicies[%q] to exist", cacheKey)
	}
	// Rule 1: env.OS = "linux" wins over hint.OS = "linux" (same value, but env set it).
	if rp.OS != "linux" {
		t.Errorf("expected OS='linux' (env wins), got %q", rp.OS)
	}
	// Rule 1: env.Filesystem is nil, but we're in permissive mode (default).
	// Since env didn't set it, hint should apply.
	if rp.Filesystem == nil || rp.Filesystem.ReadOnly != true {
		t.Errorf("expected Filesystem.ReadOnly=true from hint, got %v", rp.Filesystem)
	}
}

func filesystemPolicyEqual(a, b *FilesystemPolicy) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.ReadOnly == b.ReadOnly
}

func networkPolicyEqual(a, b *NetworkPolicy) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.AllowEgress == b.AllowEgress
}

func secretsPolicyEqual(a, b *SecretsPolicy) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Provider == b.Provider
}

func resourcesPolicyEqual(a, b *ResourcesPolicy) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.MaxMemory == b.MaxMemory
}

func ctyMapEqual(a, b map[string]cty.Value) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || !va.RawEquals(vb) {
			return false
		}
	}
	return true
}
