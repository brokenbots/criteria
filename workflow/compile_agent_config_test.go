package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentConfigFoldsVarRef verifies that a var.* reference in agent.config
// resolves at compile time without a "Variables not allowed" error.
func TestAgentConfigFoldsVarRef(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  variable "prompt_val" {
    type    = "string"
    default = "You are a helpful assistant."
  }
  agent "bot" {
    adapter = "copilot"
    config {
      system_prompt = var.prompt_val
    }
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, testSchemas)
	if diags.HasErrors() {
		t.Fatalf("expected var.* in agent.config to compile without error, got: %s", diags.Error())
	}
	got := g.Agents["bot"].Config["system_prompt"]
	if got != "You are a helpful assistant." {
		t.Errorf("system_prompt = %q, want 'You are a helpful assistant.'", got)
	}
}

// TestAgentConfigFoldsLocalRef verifies that a local.* reference in agent.config
// resolves at compile time without a "Variables not allowed" error.
func TestAgentConfigFoldsLocalRef(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  local "greeting" {
    value = "You are a helpful assistant."
  }
  agent "bot" {
    adapter = "copilot"
    config {
      system_prompt = local.greeting
    }
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, testSchemas)
	if diags.HasErrors() {
		t.Fatalf("expected local.* in agent.config to compile without error, got: %s", diags.Error())
	}
	got := g.Agents["bot"].Config["system_prompt"]
	if got != "You are a helpful assistant." {
		t.Errorf("system_prompt = %q, want 'You are a helpful assistant.'", got)
	}
}

// TestAgentConfigFileVarPath_SuccessNoSpuriousError verifies that
// file(var.prompt_file) in agent.config with an *existing* file compiles
// without any error — specifically without the old "Variables not allowed"
// spurious diagnostic.
func TestAgentConfigFileVarPath_SuccessNoSpuriousError(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("Be helpful.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  variable "prompt_file" {
    type    = "string"
    default = "prompt.txt"
  }
  agent "bot" {
    adapter = "copilot"
    config {
      system_prompt = file(var.prompt_file)
    }
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := CompileWithOpts(spec, testSchemas, CompileOpts{WorkflowDir: dir})
	if diags.HasErrors() {
		t.Fatalf("expected file(var.prompt_file) with existing file to compile without error, got: %s", diags.Error())
	}
	got := g.Agents["bot"].Config["system_prompt"]
	if got != "Be helpful.\n" {
		t.Errorf("system_prompt = %q, want 'Be helpful.\\n'", got)
	}
}

// TestAgentConfigLocalDerivedFilePath verifies that a local that derives a file
// path and is then used with file(local.path) in agent.config compiles correctly.
func TestAgentConfigLocalDerivedFilePath(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "world_prompt.txt")
	if err := os.WriteFile(promptPath, []byte("Hello from world.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  variable "name" {
    type    = "string"
    default = "world"
  }
  local "prompt_path" {
    value = "${var.name}_prompt.txt"
  }
  agent "bot" {
    adapter = "copilot"
    config {
      system_prompt = file(local.prompt_path)
    }
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := CompileWithOpts(spec, testSchemas, CompileOpts{WorkflowDir: dir})
	if diags.HasErrors() {
		t.Fatalf("expected file(local.prompt_path) with existing file to compile without error, got: %s", diags.Error())
	}
	got := g.Agents["bot"].Config["system_prompt"]
	if got != "Hello from world.\n" {
		t.Errorf("system_prompt = %q, want 'Hello from world.\\n'", got)
	}
}

func TestAgentConfigValidatedAgainstSchema(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  agent "bot" {
    adapter = "copilot"
    config {
      unknown_field = "bad"
    }
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for unknown agent config field")
	}
	if !strings.Contains(diags.Error(), `unknown field "unknown_field"`) {
		t.Errorf("unexpected error: %s", diags.Error())
	}
}

func TestAgentConfigPopulatedOnAgentNode(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  agent "bot" {
    adapter = "copilot"
    config {
      max_turns     = 8
      system_prompt = "You are a bot."
    }
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "run" }
  }
  step "run" {
    agent = "bot"
    input {
      prompt = "Hello"
    }
    outcome "success" { transition_to = "close" }
  }
  step "close" {
    agent     = "bot"
    lifecycle = "close"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, testSchemas)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	bot := g.Agents["bot"]
	if bot == nil {
		t.Fatal("agent 'bot' not found")
	}
	if bot.Config["max_turns"] != "8" {
		t.Errorf("expected max_turns=8, got %q", bot.Config["max_turns"])
	}
	if bot.Config["system_prompt"] != "You are a bot." {
		t.Errorf("expected system_prompt set, got %q", bot.Config["system_prompt"])
	}

	run := g.Steps["run"]
	if run.Input["prompt"] != "Hello" {
		t.Errorf("expected input prompt=Hello, got %q", run.Input["prompt"])
	}
}

func TestAgentConfigPermissiveWhenNoSchema(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  agent "bot" {
    adapter = "someadapter"
    config {
      any_key = "ok"
    }
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("expected permissive compile to succeed: %s", diags.Error())
	}
	if g.Agents["bot"].Config["any_key"] != "ok" {
		t.Errorf("unexpected agent config: %v", g.Agents["bot"].Config)
	}
}

func TestAgentWithoutConfigBlockHasNilConfig(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  agent "bot" { adapter = "copilot" }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.Agents["bot"].Config != nil {
		t.Errorf("expected nil Config for agent without config block, got %v", g.Agents["bot"].Config)
	}
}

func TestAgentConfigTypeMismatch_StringForNumber(t *testing.T) {
	// max_turns is ConfigFieldNumber; a string literal should fail.
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  agent "bot" {
    adapter = "copilot"
    config {
      max_turns = "not-a-number"
    }
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected type mismatch error for string passed to number field")
	}
	if !strings.Contains(diags.Error(), `field "max_turns" must be a number`) {
		t.Errorf("unexpected error: %s", diags.Error())
	}
}

func TestAgentConfigTypeMismatch_NumberForString(t *testing.T) {
	// system_prompt is ConfigFieldString; passing a number should fail.
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  agent "bot" {
    adapter = "copilot"
    config {
      system_prompt = 99
    }
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected type mismatch error for number passed to string field")
	}
	if !strings.Contains(diags.Error(), `field "system_prompt" must be a string`) {
		t.Errorf("unexpected error: %s", diags.Error())
	}
}

func TestAgentConfigListStringAcceptsStringTupleLiteral(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  agent "bot" {
    adapter = "listy"
    config {
      items = ["one", "two"]
    }
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if diags.HasErrors() {
		t.Fatalf("expected list_string tuple literal to pass for agent config: %s", diags.Error())
	}
}

// TestAgentConfigFileFunctionResolved exercises the compile-time-resolved
// agent.config{} mode: file()/trimfrontmatter() applied to an existing file
// must populate the agent's config map, not silently produce an empty string.
func TestAgentConfigFileFunctionResolved(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.md")
	body := "---\ntitle: bot\n---\nYou are a helpful bot.\n"
	if err := os.WriteFile(promptPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  agent "bot" {
    adapter = "copilot"
    config {
      system_prompt = trimfrontmatter(file("prompt.md"))
    }
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := CompileWithOpts(spec, testSchemas, CompileOpts{WorkflowDir: dir})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	got := g.Agents["bot"].Config["system_prompt"]
	if got != "You are a helpful bot.\n" {
		t.Errorf("expected system_prompt to be the trimmed file contents, got %q", got)
	}
}

// TestAgentConfigFileFunctionMissingFile exercises the loud-failure path:
// file() in agent.config that points at a non-existent path must surface a
// compile diagnostic, not silently empty the field.
func TestAgentConfigFileFunctionMissingFile(t *testing.T) {
	dir := t.TempDir()
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  agent "bot" {
    adapter = "copilot"
    config {
      system_prompt = file("does_not_exist.md")
    }
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = CompileWithOpts(spec, testSchemas, CompileOpts{WorkflowDir: dir})
	if !diags.HasErrors() {
		t.Fatal("expected compile error for missing file in agent config")
	}
	errMsg := diags.Error()
	if !strings.Contains(errMsg, "does_not_exist.md") {
		t.Errorf("expected diagnostic to reference missing file %q, got: %s", "does_not_exist.md", errMsg)
	}
	if !strings.Contains(errMsg, "file(") && !strings.Contains(errMsg, "file()") {
		t.Errorf("expected diagnostic to mention file() usage, got: %s", errMsg)
	}
	if diags[0].Subject == nil {
		t.Error("expected diagnostic to carry a source range")
	}
}

func TestAgentConfigListStringRejectsMixedTupleLiteral(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  agent "bot" {
    adapter = "listy"
    config {
      items = ["one", 2]
    }
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected list_string type mismatch for agent config")
	}
	if !strings.Contains(diags.Error(), `field "items" must be a list of strings`) {
		t.Errorf("unexpected error: %s", diags.Error())
	}
}
