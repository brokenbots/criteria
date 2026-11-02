package workflowtest_test

import (
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/workflowtest"
)

func TestStandardBuilder(t *testing.T) {
	hcl := workflowtest.NewStandard().
		WithStep("start", workflowtest.Next("state.done")).
		Build()

	spec, diags := workflow.Parse("test.hcl", []byte(hcl))
	if diags.HasErrors() {
		t.Fatalf("parse failed:\n%s\nHCL:\n%s", diags.Error(), hcl)
	}

	graph, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile failed:\n%s\nHCL:\n%s", diags.Error(), hcl)
	}

	if graph.InitialState != "start" {
		t.Errorf("InitialState = %q, want start", graph.InitialState)
	}
	if graph.TargetState != "done" {
		t.Errorf("TargetState = %q, want done", graph.TargetState)
	}
}

func TestChainBuilder(t *testing.T) {
	hcl := workflowtest.Chain("s1", "s2", "s3").
		WithMaxTotalSteps(5).
		Build()

	spec, diags := workflow.Parse("test.hcl", []byte(hcl))
	if diags.HasErrors() {
		t.Fatalf("parse failed:\n%s\nHCL:\n%s", diags.Error(), hcl)
	}

	_, diags = workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile failed:\n%s\nHCL:\n%s", diags.Error(), hcl)
	}
}

func TestLinearBuilder(t *testing.T) {
	hcl := workflowtest.Linear(10,
		workflowtest.LinearName("bench_10"),
		workflowtest.LinearAdapter("exec", "default"),
		workflowtest.LinearInput(func(i int) string {
			return `input { command = "echo test" }`
		}),
	).Build()

	spec, diags := workflow.Parse("test.hcl", []byte(hcl))
	if diags.HasErrors() {
		t.Fatalf("parse failed:\n%s\nHCL:\n%s", diags.Error(), hcl)
	}

	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile failed:\n%s\nHCL:\n%s", diags.Error(), hcl)
	}

	if len(g.Steps) != 10 {
		t.Errorf("len(g.Steps) = %d, want 10", len(g.Steps))
	}
}

func TestStandaloneFormatters(t *testing.T) {
	stepSnippet := workflowtest.Step("custom", workflowtest.Next("state.done"))
	if !strings.Contains(stepSnippet, `step "custom"`) {
		t.Errorf("unexpected step snippet: %s", stepSnippet)
	}

	switchSnippet := workflowtest.Switch("route",
		workflowtest.Match(`var.env == "prod"`, "state.done"),
		workflowtest.DefaultNext("state.failed"),
	)
	if !strings.Contains(switchSnippet, `switch "route"`) {
		t.Errorf("unexpected switch snippet: %s", switchSnippet)
	}
}

func TestFixtureHelpers(t *testing.T) {
	tmpDir := t.TempDir()
	dir := workflowtest.WriteCalleeDir(t, tmpDir, "inner", map[string]bool{"req": false, "opt": true})

	spec, diags := workflow.ParseDir(dir)
	if diags.HasErrors() {
		t.Fatalf("parse failed:\n%s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile failed:\n%s", diags.Error())
	}
	if g.Name != "inner" {
		t.Errorf("g.Name = %q, want inner", g.Name)
	}

	parentHCL := workflowtest.ParentWithSubworkflow("inner_task", "./inner", `req = "val"`)
	pSpec, diags := workflow.Parse("parent.hcl", []byte(parentHCL))
	if diags.HasErrors() {
		t.Fatalf("parse parent failed:\n%s", diags.Error())
	}
	pGraph, diags := workflow.CompileWithOpts(pSpec, nil, workflow.CompileOpts{
		WorkflowDir:         tmpDir,
		SubWorkflowResolver: &workflow.LocalSubWorkflowResolver{},
	})
	if diags.HasErrors() {
		t.Fatalf("compile parent failed:\n%s", diags.Error())
	}
	if _, ok := pGraph.Subworkflows["inner_task"]; !ok {
		t.Error("expected inner_task in parent subworkflows")
	}

	subHCL := workflowtest.NewStandard().
		WithSubworkflow("remote_inner", "git::example.com/repo",
			workflowtest.SubworkflowRef("v1.2.3"),
			workflowtest.SubworkflowInput(`foo = "bar"`),
		).
		Build()
	if !strings.Contains(subHCL, `ref    = "v1.2.3"`) || !strings.Contains(subHCL, `foo = "bar"`) {
		t.Errorf("expected subworkflow ref and input in generated HCL, got:\n%s", subHCL)
	}
}
