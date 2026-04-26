package workflow

import (
	"strings"
	"testing"
)

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
