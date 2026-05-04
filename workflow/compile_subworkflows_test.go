package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalCalleeHCL returns a minimal valid callee workflow HCL with the given name.
// Variables is a map from varName -> hasDefault (true = has default, false = required).
func minimalCalleeHCL(name string, variables map[string]bool) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("workflow %q {\n  version = \"1\"\n  initial_state = \"done\"\n  target_state  = \"done\"\n\n", name))
	for varName, hasDefault := range variables {
		if hasDefault {
			sb.WriteString(fmt.Sprintf("  variable %q {\n    type    = \"string\"\n    default = \"default_value\"\n  }\n", varName))
		} else {
			sb.WriteString(fmt.Sprintf("  variable %q {\n    type = \"string\"\n  }\n", varName))
		}
	}
	sb.WriteString("  state \"done\" {\n    terminal = true\n    success  = true\n  }\n}\n")
	return sb.String()
}

// writeSubworkflowDir creates a directory with a single main.hcl file containing the given HCL content.
func writeSubworkflowDir(t *testing.T, parent, name, content string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("create subworkflow dir %q: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write main.hcl in %q: %v", dir, err)
	}
	return dir
}

// parentHCLWithSubworkflow returns a parent workflow HCL that declares a subworkflow with the given source.
// inputAttrs is the raw content of the input = { ... } block, or "" for no input.
func parentHCLWithSubworkflow(swName, source, inputAttrs string) string {
	var sw string
	if inputAttrs != "" {
		sw = fmt.Sprintf("  subworkflow %q {\n    source = %q\n    input = {\n      %s\n    }\n  }\n", swName, source, inputAttrs)
	} else {
		sw = fmt.Sprintf("  subworkflow %q {\n    source = %q\n  }\n", swName, source)
	}
	return fmt.Sprintf("workflow \"parent\" {\n  version = \"1\"\n  initial_state = \"done\"\n  target_state  = \"done\"\n\n%s  state \"done\" {\n    terminal = true\n    success  = true\n  }\n}\n", sw)
}

// compileParentSpec parses and compiles a workflow HCL with a LocalSubWorkflowResolver.
func compileParentSpec(t *testing.T, parentHCL, parentDir string) (graph *FSMGraph, diags hclDiags) {
	t.Helper()
	var parseDiags hclDiags
	var spec *Spec
	spec, parseDiags = Parse("parent.hcl", []byte(parentHCL))
	if parseDiags.HasErrors() {
		t.Fatalf("parse failed: %s", parseDiags.Error())
	}
	graph, diags = CompileWithOpts(spec, nil, CompileOpts{
		WorkflowDir:         parentDir,
		SubWorkflowResolver: &LocalSubWorkflowResolver{},
	})
	return graph, diags
}

// hclDiags is an alias for convenience in test signatures.
type hclDiags = interface {
	HasErrors() bool
	Error() string
}

// TestCompileSubworkflows_Basic verifies that a well-formed subworkflow declaration
// compiles successfully and populates FSMGraph.Subworkflows.
func TestCompileSubworkflows_Basic(t *testing.T) {
	tmpDir := t.TempDir()
	writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", nil))

	parentHCL := parentHCLWithSubworkflow("inner_task", "./inner", "")
	graph, diags := compileParentSpec(t, parentHCL, tmpDir)
	if diags.HasErrors() {
		t.Fatalf("expected no errors, got: %s", diags.Error())
	}
	if graph == nil {
		t.Fatal("expected non-nil graph")
	}
	if _, ok := graph.Subworkflows["inner_task"]; !ok {
		t.Error("expected subworkflow 'inner_task' in graph.Subworkflows")
	}
	if len(graph.SubworkflowOrder) != 1 || graph.SubworkflowOrder[0] != "inner_task" {
		t.Errorf("unexpected SubworkflowOrder: %v", graph.SubworkflowOrder)
	}
}

// TestCompileSubworkflows_RelativeSource verifies that a relative source path
// is resolved against the parent workflow's directory.
func TestCompileSubworkflows_RelativeSource(t *testing.T) {
	tmpDir := t.TempDir()
	// Create a nested directory structure.
	subDir := filepath.Join(tmpDir, "subworkflows")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("create subworkflows dir: %v", err)
	}
	writeSubworkflowDir(t, subDir, "inner", minimalCalleeHCL("inner", nil))

	parentHCL := parentHCLWithSubworkflow("inner_task", "./subworkflows/inner", "")
	graph, diags := compileParentSpec(t, parentHCL, tmpDir)
	if diags.HasErrors() {
		t.Fatalf("expected no errors, got: %s", diags.Error())
	}
	sw := graph.Subworkflows["inner_task"]
	if sw == nil {
		t.Fatal("subworkflow 'inner_task' not found in graph")
	}
	// Source path must be absolute.
	if !filepath.IsAbs(sw.SourcePath) {
		t.Errorf("expected absolute SourcePath, got: %q", sw.SourcePath)
	}
}

// TestCompileSubworkflows_AbsoluteSource verifies that an absolute source path works.
func TestCompileSubworkflows_AbsoluteSource(t *testing.T) {
	tmpDir := t.TempDir()
	innerDir := writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", nil))

	// Use the absolute path in the parent HCL.
	parentHCL := parentHCLWithSubworkflow("inner_task", innerDir, "")
	// Parse the HCL with inline source — Go test can't embed variable content in HCL string literals,
	// so we call Parse and CompileWithOpts directly here.
	spec, pDiags := Parse("parent.hcl", []byte(parentHCL))
	if pDiags.HasErrors() {
		t.Fatalf("parse failed: %s", pDiags.Error())
	}
	graph, diags := CompileWithOpts(spec, nil, CompileOpts{
		WorkflowDir:         tmpDir,
		SubWorkflowResolver: &LocalSubWorkflowResolver{},
	})
	if diags.HasErrors() {
		t.Fatalf("expected no errors, got: %s", diags.Error())
	}
	if _, ok := graph.Subworkflows["inner_task"]; !ok {
		t.Error("expected subworkflow 'inner_task' in graph.Subworkflows")
	}
}

// TestCompileSubworkflows_RemoteScheme_Errors verifies that remote scheme sources
// produce a descriptive compile error (Phase 4 forward-pointer).
func TestCompileSubworkflows_RemoteScheme_Errors(t *testing.T) {
	tmpDir := t.TempDir()
	parentHCL := parentHCLWithSubworkflow("remote", "https://github.com/example/repo", "")
	_, diags := compileParentSpec(t, parentHCL, tmpDir)
	if !diags.HasErrors() {
		t.Fatal("expected error for remote scheme, got none")
	}
	if !strings.Contains(diags.Error(), "https") && !strings.Contains(diags.Error(), "remote") {
		t.Errorf("error message should mention remote scheme or 'https', got: %s", diags.Error())
	}
}

// TestCompileSubworkflows_DirNotExist verifies that a missing source directory
// produces a compile error.
func TestCompileSubworkflows_DirNotExist(t *testing.T) {
	tmpDir := t.TempDir()
	parentHCL := parentHCLWithSubworkflow("missing", "./nonexistent_dir", "")
	_, diags := compileParentSpec(t, parentHCL, tmpDir)
	if !diags.HasErrors() {
		t.Fatal("expected error for nonexistent directory, got none")
	}
}

// TestCompileSubworkflows_DirEmptyOfHCL verifies that a directory with no .hcl files
// produces a compile error.
func TestCompileSubworkflows_DirEmptyOfHCL(t *testing.T) {
	tmpDir := t.TempDir()
	emptyDir := filepath.Join(tmpDir, "empty")
	if err := os.Mkdir(emptyDir, 0o755); err != nil {
		t.Fatalf("create empty dir: %v", err)
	}

	parentHCL := parentHCLWithSubworkflow("empty_sw", "./empty", "")
	_, diags := compileParentSpec(t, parentHCL, tmpDir)
	if !diags.HasErrors() {
		t.Fatal("expected error for directory with no .hcl files, got none")
	}
}

// TestCompileSubworkflows_Cycle_Direct verifies that a direct cycle A→A is detected.
func TestCompileSubworkflows_Cycle_Direct(t *testing.T) {
	tmpDir := t.TempDir()
	// A subworkflow that references itself.
	cycleHCL := fmt.Sprintf("workflow \"cycle\" {\n  version = \"1\"\n  initial_state = \"done\"\n  target_state  = \"done\"\n\n  subworkflow \"self\" {\n    source = %q\n  }\n\n  state \"done\" {\n    terminal = true\n    success  = true\n  }\n}\n", tmpDir+"/cycle")
	innerDir := filepath.Join(tmpDir, "cycle")
	if err := os.Mkdir(innerDir, 0o755); err != nil {
		t.Fatalf("create cycle dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(innerDir, "main.hcl"), []byte(cycleHCL), 0o644); err != nil {
		t.Fatalf("write cycle main.hcl: %v", err)
	}

	parentHCL := fmt.Sprintf("workflow \"parent\" {\n  version = \"1\"\n  initial_state = \"done\"\n  target_state  = \"done\"\n\n  subworkflow \"cycle\" {\n    source = %q\n  }\n\n  state \"done\" {\n    terminal = true\n    success  = true\n  }\n}\n", "./cycle")
	_, diags := compileParentSpec(t, parentHCL, tmpDir)
	if !diags.HasErrors() {
		t.Fatal("expected cycle detection error, got none")
	}
	if !strings.Contains(diags.Error(), "cycle") {
		t.Errorf("error message should mention 'cycle', got: %s", diags.Error())
	}
}

// TestCompileSubworkflows_Cycle_Indirect verifies that an indirect cycle A→B→A is detected.
func TestCompileSubworkflows_Cycle_Indirect(t *testing.T) {
	tmpDir := t.TempDir()

	// Directory A references B.
	aDir := filepath.Join(tmpDir, "wf_a")
	bDir := filepath.Join(tmpDir, "wf_b")
	if err := os.Mkdir(aDir, 0o755); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	if err := os.Mkdir(bDir, 0o755); err != nil {
		t.Fatalf("mkdir b: %v", err)
	}

	// wf_b references wf_a (back-edge).
	bHCL := fmt.Sprintf("workflow \"wf_b\" {\n  version = \"1\"\n  initial_state = \"done\"\n  target_state  = \"done\"\n\n  subworkflow \"back\" {\n    source = %q\n  }\n\n  state \"done\" {\n    terminal = true\n    success  = true\n  }\n}\n", aDir)
	if err := os.WriteFile(filepath.Join(bDir, "main.hcl"), []byte(bHCL), 0o644); err != nil {
		t.Fatalf("write b main.hcl: %v", err)
	}

	// wf_a references wf_b.
	aHCL := fmt.Sprintf("workflow \"wf_a\" {\n  version = \"1\"\n  initial_state = \"done\"\n  target_state  = \"done\"\n\n  subworkflow \"forward\" {\n    source = %q\n  }\n\n  state \"done\" {\n    terminal = true\n    success  = true\n  }\n}\n", bDir)
	if err := os.WriteFile(filepath.Join(aDir, "main.hcl"), []byte(aHCL), 0o644); err != nil {
		t.Fatalf("write a main.hcl: %v", err)
	}

	// Parent references A.
	parentHCL := fmt.Sprintf("workflow \"parent\" {\n  version = \"1\"\n  initial_state = \"done\"\n  target_state  = \"done\"\n\n  subworkflow \"wf_a\" {\n    source = %q\n  }\n\n  state \"done\" {\n    terminal = true\n    success  = true\n  }\n}\n", "./wf_a")
	_, diags := compileParentSpec(t, parentHCL, tmpDir)
	if !diags.HasErrors() {
		t.Fatal("expected cycle detection error for indirect cycle A→B→A, got none")
	}
	if !strings.Contains(diags.Error(), "cycle") {
		t.Errorf("error message should mention 'cycle', got: %s", diags.Error())
	}
}

// TestCompileSubworkflows_InputMissingRequiredVar verifies that a missing required
// variable binding produces a compile error.
func TestCompileSubworkflows_InputMissingRequiredVar(t *testing.T) {
	tmpDir := t.TempDir()
	// Callee has a required variable "target" (no default).
	writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", map[string]bool{"target": false}))

	// Parent declares subworkflow but provides no input.
	parentHCL := parentHCLWithSubworkflow("inner_task", "./inner", "")
	_, diags := compileParentSpec(t, parentHCL, tmpDir)
	if !diags.HasErrors() {
		t.Fatal("expected error for missing required variable binding, got none")
	}
	if !strings.Contains(diags.Error(), "target") {
		t.Errorf("error should mention variable name 'target', got: %s", diags.Error())
	}
}

// TestCompileSubworkflows_InputExtraKey verifies that providing an input key
// that is not a declared variable produces a compile error.
func TestCompileSubworkflows_InputExtraKey(t *testing.T) {
	tmpDir := t.TempDir()
	// Callee has no variables declared.
	writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", nil))

	// Parent provides an extra key "undeclared_key".
	parentHCL := parentHCLWithSubworkflow("inner_task", "./inner", `undeclared_key = "value"`)
	_, diags := compileParentSpec(t, parentHCL, tmpDir)
	if !diags.HasErrors() {
		t.Fatal("expected error for extra input key, got none")
	}
	if !strings.Contains(diags.Error(), "undeclared_key") && !strings.Contains(diags.Error(), "not declared") {
		t.Errorf("error should mention the extra key or 'not declared', got: %s", diags.Error())
	}
}

// TestCompileSubworkflows_DeclaredEnvironmentResolves verifies that a subworkflow
// with an environment reference stores it in the compiled SubworkflowNode.
func TestCompileSubworkflows_DeclaredEnvironmentResolves(t *testing.T) {
	tmpDir := t.TempDir()
	writeSubworkflowDir(t, tmpDir, "inner", minimalCalleeHCL("inner", nil))

	// Parent workflow with a declared environment and a subworkflow referencing it.
	parentHCL := `workflow "parent" {
  version       = "1"
  initial_state = "done"
  target_state  = "done"

  environment "shell" "ci" {
    variables = { CI = "true" }
  }

  subworkflow "inner_task" {
    source      = "./inner"
    environment = "shell.ci"
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`
	graph, diags := compileParentSpec(t, parentHCL, tmpDir)
	if diags.HasErrors() {
		t.Fatalf("expected no errors, got: %s", diags.Error())
	}
	sw := graph.Subworkflows["inner_task"]
	if sw == nil {
		t.Fatal("subworkflow 'inner_task' not found in graph")
	}
	if sw.Environment != "shell.ci" {
		t.Errorf("expected Environment = 'shell.ci', got %q", sw.Environment)
	}
}

// TestCompileSubworkflows_MultipleDeclarations verifies that two distinct subworkflow
// declarations compile independently and are both present in the graph.
func TestCompileSubworkflows_MultipleDeclarations(t *testing.T) {
	tmpDir := t.TempDir()
	writeSubworkflowDir(t, tmpDir, "alpha", minimalCalleeHCL("alpha", nil))
	writeSubworkflowDir(t, tmpDir, "beta", minimalCalleeHCL("beta", nil))

	parentHCL := `workflow "parent" {
  version       = "1"
  initial_state = "done"
  target_state  = "done"

  subworkflow "alpha_task" {
    source = "./alpha"
  }

  subworkflow "beta_task" {
    source = "./beta"
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`
	graph, diags := compileParentSpec(t, parentHCL, tmpDir)
	if diags.HasErrors() {
		t.Fatalf("expected no errors, got: %s", diags.Error())
	}
	if _, ok := graph.Subworkflows["alpha_task"]; !ok {
		t.Error("expected 'alpha_task' in Subworkflows")
	}
	if _, ok := graph.Subworkflows["beta_task"]; !ok {
		t.Error("expected 'beta_task' in Subworkflows")
	}
	if len(graph.SubworkflowOrder) != 2 {
		t.Errorf("expected 2 subworkflows in order, got %d", len(graph.SubworkflowOrder))
	}
}

// TestCompileSubworkflows_MultiFileDirectory verifies that multiple .hcl files
// in a subworkflow directory are merged into a single compiled spec. Both files
// must use the workflow {} wrapper; declarations are merged field-by-field.
func TestCompileSubworkflows_MultiFileDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	innerDir := filepath.Join(tmpDir, "inner")
	if err := os.Mkdir(innerDir, 0o755); err != nil {
		t.Fatalf("create inner dir: %v", err)
	}

	// main.hcl: workflow header + step + terminal state.
	mainHCL := `workflow "inner" {
  version       = "1"
  initial_state = "done"
  target_state  = "done"

  state "done" {
    terminal = true
    success  = true
  }
}
`
	// vars.hcl: a second file in the same directory declaring a variable.
	// Each file in a multi-file subworkflow directory must have a workflow{} wrapper
	// with the required initial_state/target_state fields (the merge uses the first file's values).
	varsHCL := `workflow "inner" {
  version       = "1"
  initial_state = "done"
  target_state  = "done"

  variable "task_name" {
    type    = "string"
    default = "default_task"
  }
}
`
	if err := os.WriteFile(filepath.Join(innerDir, "main.hcl"), []byte(mainHCL), 0o644); err != nil {
		t.Fatalf("write main.hcl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(innerDir, "vars.hcl"), []byte(varsHCL), 0o644); err != nil {
		t.Fatalf("write vars.hcl: %v", err)
	}

	parentHCL := parentHCLWithSubworkflow("inner_task", "./inner", "")
	graph, diags := compileParentSpec(t, parentHCL, tmpDir)
	if diags.HasErrors() {
		t.Fatalf("expected no errors, got: %s", diags.Error())
	}
	sw, ok := graph.Subworkflows["inner_task"]
	if !ok {
		t.Fatal("expected subworkflow 'inner_task' in graph")
	}
	// The merged callee should have the variable declared in vars.hcl.
	if _, hasVar := sw.Body.Variables["task_name"]; !hasVar {
		t.Error("expected variable 'task_name' merged from vars.hcl into callee graph")
	}
}

// TestCompileSubworkflows_NilResolver errors when SubWorkflowResolver is nil.
func TestCompileSubworkflows_NilResolver(t *testing.T) {
	parentHCL := `workflow "parent" {
  version       = "1"
  initial_state = "done"
  target_state  = "done"

  subworkflow "inner" {
    source = "./inner"
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`
	spec, diags := Parse("parent.hcl", []byte(parentHCL))
	if diags.HasErrors() {
		t.Fatalf("parse failed: %s", diags.Error())
	}
	// Compile without resolver.
	_, diags = CompileWithOpts(spec, nil, CompileOpts{
		WorkflowDir:         t.TempDir(),
		SubWorkflowResolver: nil,
	})
	if !diags.HasErrors() {
		t.Fatal("expected error when SubWorkflowResolver is nil, got none")
	}
}

// TestLocalSubWorkflowResolver_LocalRelative resolves a relative source path.
func TestLocalSubWorkflowResolver_LocalRelative(t *testing.T) {
	tmpDir := t.TempDir()
	swDir := filepath.Join(tmpDir, "inner")
	if err := os.Mkdir(swDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(swDir, "main.hcl"), []byte("// content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	resolver := &LocalSubWorkflowResolver{}
	resolved, err := resolver.ResolveSource(context.Background(), tmpDir, "./inner")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if !filepath.IsAbs(resolved) {
		t.Errorf("expected absolute path, got: %q", resolved)
	}
	expected, _ := filepath.Abs(swDir)
	if resolved != expected {
		t.Errorf("expected %q, got %q", expected, resolved)
	}
}

// TestLocalSubWorkflowResolver_LocalAbsolute resolves an absolute source path.
func TestLocalSubWorkflowResolver_LocalAbsolute(t *testing.T) {
	tmpDir := t.TempDir()
	swDir := filepath.Join(tmpDir, "inner")
	if err := os.Mkdir(swDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(swDir, "main.hcl"), []byte("// content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	resolver := &LocalSubWorkflowResolver{}
	// Resolve using the absolute path directly.
	resolved, err := resolver.ResolveSource(context.Background(), "/irrelevant/caller/dir", swDir)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	expected, _ := filepath.Abs(swDir)
	if resolved != expected {
		t.Errorf("expected %q, got %q", expected, resolved)
	}
}

// TestLocalSubWorkflowResolver_RemoteScheme_Error verifies that remote URLs
// produce a descriptive error mentioning Phase 4.
func TestLocalSubWorkflowResolver_RemoteScheme_Error(t *testing.T) {
	for _, scheme := range []string{"https://github.com/org/repo", "git://example.com/repo"} {
		resolver := &LocalSubWorkflowResolver{}
		_, err := resolver.ResolveSource(context.Background(), "/caller", scheme)
		if err == nil {
			t.Errorf("expected error for %q, got none", scheme)
			continue
		}
		if !strings.Contains(err.Error(), "Phase 4") && !strings.Contains(err.Error(), "not supported") {
			t.Errorf("expected Phase 4 forward-pointer in error for %q, got: %v", scheme, err)
		}
	}
}

// TestLocalSubWorkflowResolver_AllowedRootsRestriction verifies that paths outside
// AllowedRoots are rejected.
func TestLocalSubWorkflowResolver_AllowedRootsRestriction(t *testing.T) {
	tmpDir := t.TempDir()
	swDir := filepath.Join(tmpDir, "inner")
	if err := os.Mkdir(swDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(swDir, "main.hcl"), []byte("// content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A separate root that does NOT contain swDir.
	otherRoot := t.TempDir()

	resolver := &LocalSubWorkflowResolver{AllowedRoots: []string{otherRoot}}
	_, err := resolver.ResolveSource(context.Background(), tmpDir, "./inner")
	if err == nil {
		t.Fatal("expected error: path outside allowed roots, got none")
	}
	if !strings.Contains(err.Error(), "allowed root") {
		t.Errorf("error should mention 'allowed root', got: %v", err)
	}

	// Same resolver, but use the actual tmpDir as an allowed root — should succeed.
	resolver2 := &LocalSubWorkflowResolver{AllowedRoots: []string{tmpDir}}
	_, err = resolver2.ResolveSource(context.Background(), tmpDir, "./inner")
	if err != nil {
		t.Fatalf("expected success when path is under allowed root, got: %v", err)
	}
}

// TestLocalSubWorkflowResolver_NotADirectory_Error verifies that pointing to a file
// (not a directory) produces an error.
func TestLocalSubWorkflowResolver_NotADirectory_Error(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "myfile.hcl")
	if err := os.WriteFile(filePath, []byte("// content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	resolver := &LocalSubWorkflowResolver{}
	_, err := resolver.ResolveSource(context.Background(), tmpDir, "./myfile.hcl")
	if err == nil {
		t.Fatal("expected error: source is a file not a directory, got none")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error should mention 'directory', got: %v", err)
	}
}

// TestLocalSubWorkflowResolver_SymlinkBypass verifies that a symlink inside
// AllowedRoots pointing outside the trusted tree is rejected.
func TestLocalSubWorkflowResolver_SymlinkBypass(t *testing.T) {
	allowedRoot := t.TempDir()
	outsideDir := t.TempDir()

	// Create an actual subworkflow directory outside the allowed root.
	outerSWDir := filepath.Join(outsideDir, "secret")
	if err := os.Mkdir(outerSWDir, 0o755); err != nil {
		t.Fatalf("mkdir outside dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outerSWDir, "main.hcl"), []byte("// secret workflow"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}

	// Create a symlink inside allowedRoot that points outside.
	symlinkPath := filepath.Join(allowedRoot, "escape")
	if err := os.Symlink(outerSWDir, symlinkPath); err != nil {
		t.Skip("cannot create symlink (possible permission issue):", err)
	}

	resolver := &LocalSubWorkflowResolver{AllowedRoots: []string{allowedRoot}}
	_, err := resolver.ResolveSource(context.Background(), allowedRoot, "./escape")
	if err == nil {
		t.Fatal("expected error: symlink escapes allowed root, got none")
	}
	if !strings.Contains(err.Error(), "allowed root") {
		t.Errorf("error should mention 'allowed root', got: %v", err)
	}
}

// TestCompileSubworkflows_InputTypeMismatch verifies that a literal input value
// whose type is incompatible with the callee variable type produces a compile error.
func TestCompileSubworkflows_InputTypeMismatch(t *testing.T) {
	tmpDir := t.TempDir()

	// Callee declares a number variable.
	calleeHCL := `workflow "inner" {
  version       = "1"
  initial_state = "done"
  target_state  = "done"

  variable "count" {
    type = "number"
  }

  state "done" {
    terminal = true
    success  = true
  }
}
`
	innerDir := writeSubworkflowDir(t, tmpDir, "inner", calleeHCL)
	_ = innerDir

	// Parent passes a clearly-incompatible literal string.
	parentHCL := parentHCLWithSubworkflow("inner_task", "./inner", `count = "not-a-number"`)
	spec, diags := Parse("parent.hcl", []byte(parentHCL))
	if diags.HasErrors() {
		t.Fatalf("parse failed: %s", diags.Error())
	}
	resolver := &LocalSubWorkflowResolver{}
	_, diags = CompileWithOpts(spec, nil, CompileOpts{
		WorkflowDir:         tmpDir,
		SubWorkflowResolver: resolver,
	})
	if !diags.HasErrors() {
		t.Fatal("expected type mismatch error, got none")
	}
	if !strings.Contains(diags.Error(), "type mismatch") {
		t.Errorf("expected 'type mismatch' in error, got: %s", diags.Error())
	}
}
