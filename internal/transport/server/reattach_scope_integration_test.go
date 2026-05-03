package servertrans

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapters/shell"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/plugin"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
	"github.com/brokenbots/criteria/workflow"
)

// scopeServer is a minimal fakeServer variant that returns a fixed
// ReattachRunResponse. We embed the existing fakeServer so all other methods
// are covered by the unimplemented handlers.
type scopeServer struct {
	fakeServer
	scope string // VariableScope JSON to return from ReattachRun
}

func (s *scopeServer) ReattachRun(_ context.Context, req *connect.Request[pb.ReattachRunRequest]) (*connect.Response[pb.ReattachRunResponse], error) {
	return connect.NewResponse(&pb.ReattachRunResponse{
		Status:        "running",
		CanResume:     true,
		CurrentStep:   "deploy",
		VariableScope: s.scope,
	}), nil
}

// resumeWorkflow has a single "deploy" step whose command is interpolated from
// a prior step output: ${steps.build.stdout}.
const resumeWorkflow = `
workflow "resume" {
  version       = "0.1"
  initial_state = "deploy"
  target_state  = "__done__"

  step "deploy" {
    adapter = "shell"
    input {
      command = "echo ${steps.build.stdout}"
    }
    outcome "success" { transition_to = "__done__" }
    outcome "failure" { transition_to = "__done__" }
  }
  state "__done__" { terminal = true }
}
`

// TestReattachRun_RestoresVarScope is a true integration test that:
//  1. Spins up a fake server Connect server that returns a VariableScope JSON
//     containing a prior step output.
//  2. Calls client.ReattachRun to retrieve the scope.
//  3. Calls workflow.RestoreVarScope to rebuild the cty vars map.
//  4. Runs the engine with engine.WithResumedVars so step input expressions
//     interpolate from the restored scope.
//
// This exercises the full server ReattachRun → RestoreVarScope → resumed
// interpolation path end-to-end.
func TestReattachRun_RestoresVarScope(t *testing.T) {
	// Scope contains a "build" step output that "deploy" will interpolate.
	scopeJSON := `{"var":{},"steps":{"build":{"stdout":"artifact-42.bin","exit_code":"0"}}}`

	srv := &scopeServer{
		fakeServer: *newFakeServer(),
		scope:      scopeJSON,
	}
	url := startScopeServer(t, srv)

	// Build a server client (no auth needed for h2c test server).
	client, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx := context.Background()
	resp, err := client.ReattachRun(ctx, "run-1", srv.criteriaID)
	if err != nil {
		t.Fatalf("ReattachRun: %v", err)
	}
	if !resp.CanResume {
		t.Fatal("expected can_resume=true")
	}
	if resp.VariableScope == "" {
		t.Fatal("expected VariableScope to be populated")
	}

	// Parse and compile the resume workflow (build already done; only deploy remains).
	spec, diags := workflow.Parse("resume.hcl", []byte(resumeWorkflow))
	if diags.HasErrors() {
		t.Fatalf("workflow.Parse: %s", diags)
	}
	graph, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		"shell": {
			InputSchema: map[string]workflow.ConfigField{
				"command": {Type: workflow.ConfigFieldString, Required: true},
			},
			OutputSchema: map[string]workflow.ConfigField{
				"stdout":    {Type: workflow.ConfigFieldString},
				"exit_code": {Type: workflow.ConfigFieldString},
			},
		},
	})
	if diags.HasErrors() {
		t.Fatalf("workflow.Compile: %s", diags)
	}

	// Restore the scope returned by the server.
	restoredVars, _, err := workflow.RestoreVarScope(resp.VariableScope, graph)
	if err != nil {
		t.Fatalf("RestoreVarScope: %v", err)
	}

	// Verify steps.build.stdout is present in restored vars.
	stepsVal, ok := restoredVars["steps"]
	if !ok {
		t.Fatal("restored vars missing 'steps' key")
	}
	buildVal := stepsVal.GetAttr("build")
	stdoutVal := buildVal.GetAttr("stdout")
	if stdoutVal == cty.NilVal || stdoutVal.AsString() != "artifact-42.bin" {
		t.Errorf("restored steps.build.stdout = %v, want 'artifact-42.bin'", stdoutVal)
	}

	// Run the engine with the restored vars. Use a recording adapter that
	// captures the resolved "command" input so we can assert interpolation
	// produced the expected string at execution time.
	rec := &recordingAdapter{inner: shell.New()}
	loader := &integrationLoader{plugins: map[string]plugin.Plugin{
		"shell": plugin.BuiltinFactoryForAdapter(rec)(),
	}}
	sink := &integrationSink{}
	eng := engine.New(graph, loader, sink, engine.WithResumedVars(restoredVars))
	if runErr := eng.Run(ctx); runErr != nil {
		t.Fatalf("engine.Run: %v", runErr)
	}

	// Assert that the resolved command interpolated steps.build.stdout correctly.
	got := rec.lastCommand
	want := "echo artifact-42.bin"
	if got != want {
		t.Errorf("deploy step command = %q, want %q", got, want)
	}
}

// startScopeServer starts an httptest Connect server using any service handler.
func startScopeServer(t *testing.T, h criteriav1connect.CriteriaServiceHandler) string {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := criteriav1connect.NewCriteriaServiceHandler(h)
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// integrationLoader implements plugin.Loader using pre-built plugin instances.
type integrationLoader struct {
	plugins map[string]plugin.Plugin
}

func (l *integrationLoader) Resolve(_ context.Context, name string) (plugin.Plugin, error) {
	p, ok := l.plugins[name]
	if !ok {
		return nil, fmt.Errorf("adapter %q not registered", name)
	}
	return p, nil
}

func (l *integrationLoader) Shutdown(_ context.Context) error { return nil }

// integrationSink is a no-op engine.Sink used in the integration test.
type integrationSink struct{}

func (s *integrationSink) OnRunStarted(workflowName, initialStep string)                    {}
func (s *integrationSink) OnRunCompleted(finalState string, success bool)                   {}
func (s *integrationSink) OnRunFailed(reason, step string)                                  {}
func (s *integrationSink) OnStepEntered(step, adapterName string, attempt int)              {}
func (s *integrationSink) OnStepOutcome(step, outcome string, dur time.Duration, err error) {}
func (s *integrationSink) OnStepTransition(from, to, viaOutcome string)                     {}
func (s *integrationSink) OnStepResumed(step string, attempt int, reason string)            {}
func (s *integrationSink) OnVariableSet(name, value, source string)                         {}
func (s *integrationSink) OnStepOutputCaptured(step string, outputs map[string]string)      {}
func (s *integrationSink) OnRunPaused(node, mode, signal string)                            {}
func (s *integrationSink) OnWaitEntered(node, mode, duration, signal string)                {}
func (s *integrationSink) OnWaitResumed(node, mode, signal string, payload map[string]string) {
}
func (s *integrationSink) OnApprovalRequested(node string, approvers []string, reason string) {}
func (s *integrationSink) OnApprovalDecision(node, decision, actor string, payload map[string]string) {
}
func (s *integrationSink) OnBranchEvaluated(node, matchedArm, target, condition string) {}
func (s *integrationSink) OnForEachEntered(string, int)                                 {}
func (s *integrationSink) OnStepIterationStarted(string, int, string, bool)             {}
func (s *integrationSink) OnStepIterationCompleted(string, string, string)              {}
func (s *integrationSink) OnStepIterationItem(string, int, string)                      {}
func (s *integrationSink) OnScopeIterCursorSet(string)                                  {}
func (s *integrationSink) OnAdapterLifecycle(string, string, string, string)            {}
func (s *integrationSink) OnRunOutputs([]map[string]string)                             {}
func (s *integrationSink) StepEventSink(step string) adapter.EventSink                  { return s }
func (s *integrationSink) Log(stream string, line []byte)                               {}
func (s *integrationSink) Adapter(kind string, data any)                                {}

// recordingAdapter wraps an adapter.Adapter and records the resolved "command"
// input so tests can assert interpolation produced the expected string.
type recordingAdapter struct {
	inner       adapter.Adapter
	lastCommand string
}

func (r *recordingAdapter) Name() string               { return r.inner.Name() }
func (r *recordingAdapter) Info() workflow.AdapterInfo { return r.inner.Info() }
func (r *recordingAdapter) Execute(ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	r.lastCommand = step.Input["command"]
	return r.inner.Execute(ctx, step, sink)
}
