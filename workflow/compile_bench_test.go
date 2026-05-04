package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixtureHCL returns the bytes for a named workflow fixture relative to this
// file. Benchmarks call this once in b.ReportAllocs() preamble to isolate I/O
// from the timed section.
func fixtureHCL(b *testing.B, name string) (path string, src []byte) {
	b.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("resolve caller path")
	}
	p := filepath.Join(filepath.Dir(file), "..", "examples", name)
	data, err := os.ReadFile(p)
	if err != nil {
		b.Fatalf("read fixture %s: %v", name, err)
	}
	return p, data
}

// BenchmarkCompile_Hello measures Parse+Compile on the minimal hello workflow.
func BenchmarkCompile_Hello(b *testing.B) {
	path, src := fixtureHCL(b, "hello.hcl")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		spec, diags := Parse(path, src)
		if diags.HasErrors() {
			b.Fatalf("parse: %s", diags.Error())
		}
		if _, diags = Compile(spec, nil); diags.HasErrors() {
			b.Fatalf("compile: %s", diags.Error())
		}
	}
}

// gen1000StepHCL generates a deterministic HCL workflow with n sequential
// step nodes, each using adapter "shell" with a no-op echo command. The
// generated string exercises the HCL parser and compiler at scale.
func gen1000StepHCL(n int) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `workflow "perf_%d_steps" {
  version       = "0.1"
  initial_state = "step_0"
  target_state  = "done"
  policy { max_total_steps = %d }
`, n, n+10)
	for i := 0; i < n; i++ {
		next := fmt.Sprintf("step_%d", i+1)
		if i == n-1 {
			next = "done"
		}
		fmt.Fprintf(&b, `
  step "step_%d" {
    adapter = "shell.default"
    input { command = "echo step_%d" }
    outcome "success" { transition_to = "%s" }
    outcome "failure" { transition_to = "done" }
  }
`, i, i, next)
	}
	b.WriteString(`
  state "done" {
    terminal = true
    success  = true
  }
}
`)
	return []byte(b.String())
}

// BenchmarkCompile_1000Steps measures Parse+Compile on an in-memory generated
// workflow with 1 000 sequential step nodes, stressing the HCL compiler.
func BenchmarkCompile_1000Steps(b *testing.B) {
	src := gen1000StepHCL(1000)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		spec, diags := Parse("perf_1000_steps.hcl", src)
		if diags.HasErrors() {
			b.Fatalf("parse: %s", diags.Error())
		}
		if _, diags = Compile(spec, nil); diags.HasErrors() {
			b.Fatalf("compile: %s", diags.Error())
		}
	}
}

// BenchmarkCompile_WorkstreamLoop measures a realistic multi-step loop workflow.
func BenchmarkCompile_WorkstreamLoop(b *testing.B) {
	path, src := fixtureHCL(b, "workstream_review_loop.hcl")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		spec, diags := Parse(path, src)
		if diags.HasErrors() {
			b.Fatalf("parse: %s", diags.Error())
		}
		if _, diags = Compile(spec, nil); diags.HasErrors() {
			b.Fatalf("compile: %s", diags.Error())
		}
	}
}
