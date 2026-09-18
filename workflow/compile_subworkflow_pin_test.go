package workflow

// compile_subworkflow_pin_test.go — CRI-226: operator-declared expected pins
// on subworkflow sources must be enforced at resolve time (ADR-0005 D7).
// A declared ref is compared against the fetcher's resolved pin (git commit
// SHA or archive sha256 digest); a mismatch fails closed before the callee is
// parsed or executed, and a pinless resolution is unchanged.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

// pinStubResolver is a SubWorkflowResolver stub answering each declared source
// with a fixed resolved directory and pin, so compile-time pin enforcement can
// be exercised without a real fetcher. The wildcard "*" entry matches any
// source.
type pinStubResolver struct {
	entries map[string]pinStubEntry
	err     error
}

type pinStubEntry struct {
	dir string
	pin *lockfile.LockedWorkflowRef
}

func (r *pinStubResolver) ResolveSource(_ context.Context, _, source string) (string, *lockfile.LockedWorkflowRef, error) {
	if r.err != nil {
		return "", nil, r.err
	}
	entry, ok := r.entries[source]
	if !ok {
		entry, ok = r.entries["*"]
	}
	if !ok {
		return "", nil, fmt.Errorf("unexpected source %q", source)
	}
	return entry.dir, entry.pin, nil
}

func fixedPinResolver(dir string, pin *lockfile.LockedWorkflowRef) *pinStubResolver {
	return &pinStubResolver{entries: map[string]pinStubEntry{"*": {dir: dir, pin: pin}}}
}

func gitPin(resolvedRef string) *lockfile.LockedWorkflowRef {
	return &lockfile.LockedWorkflowRef{Name: "", Source: "git::example", ResolvedRef: resolvedRef, Kind: "git"}
}

func archivePin(digest string) *lockfile.LockedWorkflowRef {
	return &lockfile.LockedWorkflowRef{Name: "", Source: "https://example/flow.tar.gz", ResolvedRef: digest, Kind: "archive"}
}

// parentWithPinnedSubworkflow returns a parent workflow whose subworkflow
// declares the given expected ref ("ref" attr). With inputAttrs != "" the
// subworkflow also carries an input block, pinning that ref coexists with it.
func parentWithPinnedSubworkflow(swName, source, ref, inputAttrs string) string {
	attrs := fmt.Sprintf("  source = %q\n", source)
	if ref != "" {
		attrs += fmt.Sprintf("  ref    = %q\n", ref)
	}
	if inputAttrs != "" {
		attrs += "  input = {\n    " + inputAttrs + "\n  }\n"
	}
	return "workflow {\n" +
		"  name = \"pin-parent\"\n" +
		"  version = \"1\"\n" +
		"  initial_state = \"run_inner\"\n" +
		"  target_state  = \"done\"\n" +
		"}\n\n" +
		"subworkflow \"" + swName + "\" {\n" +
		attrs +
		"}\n\n" +
		"step \"run_inner\" {\n" +
		"  target = subworkflow." + swName + "\n" +
		"  outcome \"success\" { next = step.done }\n" +
		"}\n\n" +
		"state \"done\" {\n  terminal = true\n  success  = true\n}\n"
}

func compileParent(t *testing.T, tmpDir, parentHCL string, resolver SubWorkflowResolver) hclDiagnostics {
	t.Helper()
	spec, diags := Parse("parent.hcl", []byte(parentHCL))
	if diags.HasErrors() {
		t.Fatalf("parse failed: %s", diags.Error())
	}
	_, compileDiags := CompileWithContext(context.Background(), spec, nil, CompileOpts{
		WorkflowDir:         tmpDir,
		SubWorkflowResolver: resolver,
	})
	return compileDiags
}

// TestCompileSubworkflowPin_Match verifies that a subworkflow whose declared
// ref equals the resolved pin compiles cleanly (match passes).
func TestCompileSubworkflowPin_Match(t *testing.T) {
	tmpDir := t.TempDir()
	swDir := writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", nil))

	diags := compileParent(t, tmpDir,
		parentWithPinnedSubworkflow("inner_task", "https://example/flow.tar.gz", "sha256:cafe", ""),
		fixedPinResolver(swDir, archivePin("sha256:cafe")))
	assert.False(t, diags.HasErrors(), "matching pin must resolve: %s", diags.Error())
}

// TestCompileSubworkflowPin_MismatchGit fails closed on a git SHA mismatch,
// naming both the expected and the resolved values.
func TestCompileSubworkflowPin_MismatchGit(t *testing.T) {
	tmpDir := t.TempDir()
	swDir := writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", nil))

	diags := compileParent(t, tmpDir,
		parentWithPinnedSubworkflow("inner_task", "git::example?ref=main", "1111111111111111111111111111111111111111", ""),
		fixedPinResolver(swDir, gitPin("2222222222222222222222222222222222222222")))
	if !diags.HasErrors() {
		t.Fatal("expected fail-closed mismatch error, got none")
	}
	msg := diags.Error()
	for _, want := range []string{
		"expected-pin mismatch",
		"1111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222",
		"inner_task",
		"refusing to run",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("mismatch error must name %q, got: %s", want, msg)
		}
	}
}

// TestCompileSubworkflowPin_MismatchArchive fails closed on an archive digest
// mismatch, naming both values.
func TestCompileSubworkflowPin_MismatchArchive(t *testing.T) {
	tmpDir := t.TempDir()
	swDir := writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", nil))

	diags := compileParent(t, tmpDir,
		parentWithPinnedSubworkflow("inner_task", "https://example/flow.tar.gz", "sha256:cafe", ""),
		fixedPinResolver(swDir, archivePin("sha256:beef")))
	if !diags.HasErrors() {
		t.Fatal("expected fail-closed mismatch error, got none")
	}
	msg := diags.Error()
	assert.Contains(t, msg, "expected-pin mismatch")
	assert.Contains(t, msg, "sha256:cafe", "error must name the expected digest")
	assert.Contains(t, msg, "sha256:beef", "error must name the resolved digest")
}

// TestCompileSubworkflowPin_AbsentLeavesBehaviorUnchanged verifies that with
// no declared ref, resolution with a pin (or without one) behaves exactly as
// before CRI-226.
func TestCompileSubworkflowPin_AbsentLeavesBehaviorUnchanged(t *testing.T) {
	tmpDir := t.TempDir()
	swDir := writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", nil))
	parentHCL := parentWithPinnedSubworkflow("inner_task", "./inner", "", "")

	// No declared ref, resolver returns a pin: unchanged success.
	diags := compileParent(t, tmpDir, parentHCL, fixedPinResolver(swDir, gitPin("2222222222222222222222222222222222222222")))
	assert.False(t, diags.HasErrors(), "absent pin must not affect resolution: %s", diags.Error())

	// No declared ref, resolver returns no pin (local resolver shape): unchanged.
	diags = compileParent(t, tmpDir, parentHCL, fixedPinResolver(swDir, nil))
	assert.False(t, diags.HasErrors(), "absent pin and nil pin must stay unchanged: %s", diags.Error())
}

// TestCompileSubworkflowPin_DeclaredOnLocalSource refuses a declared ref on a
// source that resolves without a pin (local directory): the expectation
// cannot be verified there, so the run fails closed instead of ignoring it.
func TestCompileSubworkflowPin_DeclaredOnLocalSource(t *testing.T) {
	tmpDir := t.TempDir()
	writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", nil))

	diags := compileParent(t, tmpDir,
		parentWithPinnedSubworkflow("inner_task", "./inner", "1111111111111111111111111111111111111111", ""),
		fixedPinResolver(tmpDir, nil))
	if !diags.HasErrors() {
		t.Fatal("expected fail-closed error for a declared ref on a pinless source")
	}
	msg := diags.Error()
	assert.Contains(t, msg, "1111111111111111111111111111111111111111")
	assert.Contains(t, msg, "./inner")
	assert.Contains(t, msg, "local")
}

// TestCompileSubworkflowPin_WithInputBlock verifies that a subworkflow block
// carrying both ref and input decodes cleanly and still enforces the pin.
func TestCompileSubworkflowPin_WithInputBlock(t *testing.T) {
	tmpDir := t.TempDir()
	writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", map[string]bool{"greeting": true}))

	// A pinless source refuses the declared ref (fail closed) — and the ref
	// attribute did not leak into the unknown-attribute check alongside input.
	diags := compileParent(t, tmpDir,
		parentWithPinnedSubworkflow("inner_task", "./inner", "sha256:cafe", "greeting = \"hi\""),
		fixedPinResolver(tmpDir, nil))
	if !diags.HasErrors() {
		t.Fatal("expected fail-closed error for a declared ref on a pinless source")
	}
	assert.Contains(t, diags.Error(), `ref "sha256:cafe" declared but source "./inner" is local`)
	assert.NotContains(t, diags.Error(), `unknown attribute "ref"`)
}

// TestCompileSubworkflowPin_NestedCascaded enforces the pin on a cascaded
// (subworkflow-of-subworkflow) source: the mismatch surfaces for the inner
// callee, and a matching cascade compiles.
func TestCompileSubworkflowPin_NestedCascaded(t *testing.T) {
	tmpDir := t.TempDir()
	innerDir := writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", nil))
	midDir := writeSubworkflowDir(t, tmpDir, "mid", parentWithPinnedSubworkflow("inner_task", "./inner", "sha256:cafe", ""))

	resolver := &pinStubResolver{entries: map[string]pinStubEntry{
		"./mid":   {dir: midDir},
		"./inner": {dir: innerDir, pin: archivePin("sha256:beef")},
	}}

	diags := compileParent(t, tmpDir,
		parentWithPinnedSubworkflow("mid_task", "./mid", "", ""), resolver)
	if !diags.HasErrors() {
		t.Fatal("expected cascaded mismatch to fail the compile")
	}
	assert.Contains(t, diags.Error(), `expected-pin mismatch: expected "sha256:cafe", resolved "sha256:beef"`)

	// Matching cascade compiles cleanly.
	resolver = &pinStubResolver{entries: map[string]pinStubEntry{
		"./mid":   {dir: midDir},
		"./inner": {dir: innerDir, pin: archivePin("sha256:cafe")},
	}}
	diags = compileParent(t, tmpDir,
		parentWithPinnedSubworkflow("mid_task", "./mid", "", ""), resolver)
	assert.False(t, diags.HasErrors(), "matching cascaded pin must compile: %s", diags.Error())
}

// TestCompileSubworkflowPin_ResolverErrorPropagates verifies a resolver
// failure still surfaces as a compile error (unchanged behavior).
func TestCompileSubworkflowPin_ResolverErrorPropagates(t *testing.T) {
	tmpDir := t.TempDir()
	diags := compileParent(t, tmpDir,
		parentWithPinnedSubworkflow("inner_task", "git::example?ref=main", "", ""),
		&pinStubResolver{err: errors.New("boom")})
	if !diags.HasErrors() {
		t.Fatal("expected resolver error to propagate")
	}
	assert.Contains(t, diags.Error(), "boom")
}

// TestCompileSubworkflowPin_MismatchRedactsCredentials guards against
// credential disclosure in compile-time diagnostics (CRI-226 review R1): a
// source carrying URL userinfo must never leak its credentials into the pin
// mismatch Summary or Detail, while the host and path stay identifiable.
func TestCompileSubworkflowPin_MismatchRedactsCredentials(t *testing.T) {
	tmpDir := t.TempDir()
	swDir := writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", nil))

	diags := compileParent(t, tmpDir,
		parentWithPinnedSubworkflow("inner_task",
			"http://leakuser:SUPERSECRET@127.0.0.1:8731/callee.tar.gz", "sha256:beef", ""),
		fixedPinResolver(swDir, archivePin("sha256:cafe")))
	if !diags.HasErrors() {
		t.Fatal("expected fail-closed mismatch error, got none")
	}
	msg := diags.Error()
	assert.NotContains(t, msg, "SUPERSECRET", "the password must never reach diagnostics")
	assert.NotContains(t, msg, "leakuser", "the userinfo must never reach diagnostics")
	assert.Contains(t, msg, "redacted@", "the source must still be identifiable as credentialed")
	assert.Contains(t, msg, "127.0.0.1:8731/callee.tar.gz", "the host and path must stay identifiable")
	assert.Contains(t, msg, "sha256:cafe", "error must name the resolved digest")
	assert.Contains(t, msg, "sha256:beef", "error must name the expected digest")
}

// TestCompileSubworkflowPin_LocalRefusalRedactsCredentials holds the same
// no-credential guarantee for the declared-ref-on-local-source refusal path.
func TestCompileSubworkflowPin_LocalRefusalRedactsCredentials(t *testing.T) {
	tmpDir := t.TempDir()
	writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", nil))

	diags := compileParent(t, tmpDir,
		parentWithPinnedSubworkflow("inner_task",
			"http://leakuser:SUPERSECRET@127.0.0.1:8731/callee", "sha256:cafe", ""),
		fixedPinResolver(tmpDir, nil))
	if !diags.HasErrors() {
		t.Fatal("expected fail-closed error for a declared ref on a pinless source")
	}
	msg := diags.Error()
	assert.NotContains(t, msg, "SUPERSECRET", "the password must never reach diagnostics")
	assert.NotContains(t, msg, "leakuser", "the userinfo must never reach diagnostics")
	assert.Contains(t, msg, "redacted@")
	assert.Contains(t, msg, "127.0.0.1:8731/callee", "the host and path must stay identifiable")
	assert.Contains(t, msg, "sha256:cafe", "error must name the declared pin")
	assert.Contains(t, msg, "is local", "the refusal must explain why the pin cannot be verified")
}
