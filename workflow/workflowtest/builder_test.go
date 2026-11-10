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

func TestEnvironmentAndContainerAdapterBuilder(t *testing.T) {
	done := true
	hcl := workflowtest.New().
		WithHeader("t", "0.1", "start", "done").
		WithState("done", true, &done).
		WithEnvironment("container", "e2e", "").
		WithAdapter("noop", "default",
			workflowtest.AdapterEnvironment(workflowtest.EnvironmentRef("container", "e2e")),
			workflowtest.AdapterSource("localhost:5000/e2e-noop"),
			workflowtest.AdapterVersion("v1.0.0"),
		).
		WithStep("start", workflowtest.Next("state.done")).
		Build()

	if !strings.Contains(hcl, `environment "container" "e2e" {`) {
		t.Errorf("expected environment block in generated HCL, got:\n%s", hcl)
	}
	if !strings.Contains(hcl, "environment = container.e2e") ||
		!strings.Contains(hcl, `source      = "localhost:5000/e2e-noop"`) ||
		!strings.Contains(hcl, `version     = "v1.0.0"`) {
		t.Errorf("expected adapter environment/source/version in generated HCL, got:\n%s", hcl)
	}

	spec, diags := workflow.Parse("test.hcl", []byte(hcl))
	if diags.HasErrors() {
		t.Fatalf("parse failed:\n%s\nHCL:\n%s", diags.Error(), hcl)
	}
	graph, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile failed:\n%s\nHCL:\n%s", diags.Error(), hcl)
	}
	adapter, ok := graph.Adapters["noop.default"]
	if !ok {
		t.Fatalf("expected noop.default adapter in graph")
	}
	if adapter.Environment != "container.e2e" {
		t.Errorf("adapter Environment = %q, want container.e2e", adapter.Environment)
	}
	if adapter.Source != "localhost:5000/e2e-noop" {
		t.Errorf("adapter Source = %q, want localhost:5000/e2e-noop", adapter.Source)
	}
	env, ok := graph.Environments["container.e2e"]
	if !ok {
		t.Fatalf("expected container.e2e environment in graph")
	}
	if env.Type != "container" || env.Name != "e2e" {
		t.Errorf("environment = %s.%s, want container.e2e", env.Type, env.Name)
	}
}

func TestWithEnvironmentReplaceDedupes(t *testing.T) {
	hcl := workflowtest.New().
		WithEnvironment("shell", "ci", "variables = { A = \"1\" }").
		WithEnvironment("shell", "ci", "variables = { A = \"2\" }").
		Build()
	if !strings.Contains(hcl, `A = "2"`) || strings.Contains(hcl, `A = "1"`) {
		t.Errorf("expected later WithEnvironment to replace the earlier declaration, got:\n%s", hcl)
	}
	if got := strings.Count(hcl, `environment "shell" "ci"`); got != 1 {
		t.Errorf("expected exactly one shell.ci block, got %d in:\n%s", got, hcl)
	}
}

func TestAdapterOverridesCompose(t *testing.T) {
	hcl := workflowtest.NewStandard().
		WithAdapter("noop", "default", workflowtest.AdapterVersion("v1")).
		WithAdapter("noop", "default", workflowtest.AdapterSource("reg.io/a/b")).
		WithAdapterFate("noop", "default", "reuse").
		Build()
	if !strings.Contains(hcl, `continuation = "reuse"`) {
		t.Errorf("expected continuation override, got:\n%s", hcl)
	}
	if strings.Contains(hcl, "source") {
		t.Errorf("expected WithAdapterFate without opts to reset source, got:\n%s", hcl)
	}
}
