package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// TestVarOverrides_CLIComplexTypesReachScope is an engine-level seam test that
// exercises the full path from raw CLI strings (as produced by parseVarOverrides)
// through WithVarOverrides/seedRunVars into var.*. It pauses at a signal wait
// and inspects VarScope to assert the declared list/map types were produced.
func TestVarOverrides_CLIComplexTypesReachScope(t *testing.T) {
	g := compile(t, `
workflow {
  name = "seam"
  version       = "0.1"
  initial_state = "pause"
  target_state  = "done"
}

variable "tags"   { type = list(string) }
variable "labels" { type = map(string) }

wait "pause" {
  signal = "resume"
  outcome "resumed" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`)

	sink := &fakeSink{}
	overrides := map[string]cty.Value{
		"tags":   cty.StringVal(`["a","b"]`),
		"labels": cty.StringVal(`{env="prod"}`),
	}
	eng := NewTestEngine(g, &fakeLoader{}, sink, WithVarOverrides(overrides))

	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	scope := eng.VarScope()
	if scope == nil {
		t.Fatal("VarScope is nil after paused run")
	}

	varObj := scope["var"]
	if !varObj.Type().IsObjectType() {
		t.Fatalf("var scope is not an object: %s", varObj.Type().FriendlyName())
	}

	tags := varObj.GetAttr("tags")
	if !tags.Type().IsListType() {
		t.Errorf("var.tags type = %s, want list", tags.Type().FriendlyName())
	}
	if l := tags.LengthInt(); l != 2 {
		t.Errorf("var.tags length = %d, want 2", l)
	}
	if got := tags.Index(cty.NumberIntVal(0)).AsString(); got != "a" {
		t.Errorf("var.tags[0] = %q, want a", got)
	}

	labels := varObj.GetAttr("labels")
	if !labels.Type().IsMapType() {
		t.Errorf("var.labels type = %s, want map", labels.Type().FriendlyName())
	}
	if got := labels.Index(cty.StringVal("env")).AsString(); got != "prod" {
		t.Errorf("var.labels.env = %q, want prod", got)
	}
}

// TestVarOverrides_IncompatibleOverrideAbortsRun verifies that a malformed or
// type-incompatible CLI override aborts Run with an error naming the variable.
func TestVarOverrides_IncompatibleOverrideAbortsRun(t *testing.T) {
	g := compile(t, `
workflow {
  name = "seam"
  version       = "0.1"
  initial_state = "pause"
  target_state  = "done"
}

variable "tags" { type = list(string) }

wait "pause" {
  signal = "resume"
  outcome "resumed" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`)

	sink := &fakeSink{}
	overrides := map[string]cty.Value{
		"tags": cty.StringVal("not-a-list"),
	}
	eng := NewTestEngine(g, &fakeLoader{}, sink, WithVarOverrides(overrides))

	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for incompatible override")
	}
	if !strings.Contains(err.Error(), `"tags"`) {
		t.Errorf("error = %q, want it to name variable tags", err.Error())
	}
}

// varEventSink records OnVariableSet events for inspection.
type varEventSink struct {
	fakeSink
	events []struct {
		name, value, source string
	}
}

func (s *varEventSink) OnVariableSet(name, value, source string) {
	s.events = append(s.events, struct {
		name, value, source string
	}{name, value, source})
}

// TestVarOverrides_SecretParseErrorRedactsRawValue verifies that a parse error
// for a secret complex-typed variable does not echo the raw override value.
func TestVarOverrides_SecretParseErrorRedactsRawValue(t *testing.T) {
	g := compile(t, `
workflow {
  name = "seam"
  version       = "0.1"
  initial_state = "pause"
  target_state  = "done"
}

variable "creds" {
  type = object({ user = string, pass = string })
  secret = true
}

wait "pause" {
  signal = "resume"
  outcome "resumed" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`)

	sink := &fakeSink{}
	overrides := map[string]cty.Value{
		"creds": cty.StringVal(`{user="a",pass="hunter2"`),
	}
	eng := NewTestEngine(g, &fakeLoader{}, sink, WithVarOverrides(overrides))

	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed secret override")
	}

	msg := err.Error()
	if !strings.Contains(msg, `"creds"`) {
		t.Errorf("error = %q, want it to name variable creds", msg)
	}
	if strings.Contains(msg, "hunter2") {
		t.Errorf("error leaked raw secret value: %q", msg)
	}
	if !strings.Contains(msg, "(sensitive)") {
		t.Errorf("error = %q, want masked secret parse error", msg)
	}
}

// TestVarOverrides_EmitsConvertedValueAndRedactsSecrets verifies that
// OnVariableSet emits the converted run-scope value (not the raw override) and
// that secret variables are reported as (sensitive).
func TestVarOverrides_EmitsConvertedValueAndRedactsSecrets(t *testing.T) {
	g := compile(t, `
workflow {
  name = "seam"
  version       = "0.1"
  initial_state = "pause"
  target_state  = "done"
}

variable "tags" {
  type = list(string)
}

variable "token" {
  type   = string
  secret = true
}

wait "pause" {
  signal = "resume"
  outcome "resumed" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`)

	sink := &varEventSink{}
	overrides := map[string]cty.Value{
		"tags":  cty.StringVal(`["a","b"]`),
		"token": cty.StringVal("super-secret"),
	}
	eng := NewTestEngine(g, &fakeLoader{}, sink, WithVarOverrides(overrides))

	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	byName := make(map[string]string, len(sink.events))
	for _, e := range sink.events {
		byName[e.name] = e.value
	}

	// The converted list value renders as compact JSON so non-scalar values are
	// readable in the event stream.
	if got, ok := byName["tags"]; !ok {
		t.Error("no OnVariableSet event for tags")
	} else if got != `[a,b]` {
		t.Errorf("tags event value = %q, want [a,b]", got)
	}

	if got, ok := byName["token"]; !ok {
		t.Error("no OnVariableSet event for token")
	} else if got != "(sensitive)" {
		t.Errorf("secret token event value = %q, want (sensitive)", got)
	}
}

// TestVarOverrides_DisplayComplexValuesInEvents verifies that map and object
// variable overrides render as compact JSON in OnVariableSet events rather than
// cty Go debug syntax.
func TestVarOverrides_DisplayComplexValuesInEvents(t *testing.T) {
	g := compile(t, `
workflow {
  name = "seam"
  version       = "0.1"
  initial_state = "pause"
  target_state  = "done"
}

variable "labels" { type = map(string) }
variable "cfg"    { type = object({ env = string }) }

wait "pause" {
  signal = "resume"
  outcome "resumed" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`)

	sink := &varEventSink{}
	overrides := map[string]cty.Value{
		"labels": cty.StringVal(`{env="prod",tier="web"}`),
		"cfg":    cty.StringVal(`{env="prod"}`),
	}
	eng := NewTestEngine(g, &fakeLoader{}, sink, WithVarOverrides(overrides))

	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	byName := make(map[string]string, len(sink.events))
	for _, e := range sink.events {
		byName[e.name] = e.value
	}

	if got, ok := byName["labels"]; !ok {
		t.Error("no OnVariableSet event for labels")
	} else if got != `{"env":"prod","tier":"web"}` {
		t.Errorf("labels event value = %q, want compact JSON", got)
	}

	if got, ok := byName["cfg"]; !ok {
		t.Error("no OnVariableSet event for cfg")
	} else if got != `{"env":"prod"}` {
		t.Errorf("cfg event value = %q, want compact JSON", got)
	}
}
