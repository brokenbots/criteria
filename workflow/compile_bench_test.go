package workflow

import (
	"os"
	"path/filepath"
	"runtime"
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

// BenchmarkCompile_Perf1000Logs measures Parse+Compile on the large perf
// fixture (1 000-line shell loop workflow).
func BenchmarkCompile_Perf1000Logs(b *testing.B) {
	path, src := fixtureHCL(b, "perf_1000_logs.hcl")
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
