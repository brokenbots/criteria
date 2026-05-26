package workflow

import (
	"fmt"
	"testing"
)

func TestDebugOutputParse(t *testing.T) {
	src := []byte(`
workflow {
  name = "test"
  version      = "1"
  initial_state = "start"
  target_state  = "end"
}
  
output "result" {
  type = string
  value = "hello"
}
  
state "start" {}
state "end" {
  terminal = true
}
`)
	spec, diags := Parse("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	for i, o := range spec.Outputs {
		rng := o.Type.Range()
		fmt.Printf("Output %d: type=%v start=%v end=%v same=%v\n", i, o.Type, rng.Start, rng.End, rng.Start == rng.End)
	}
}
