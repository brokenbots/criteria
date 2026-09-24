package engine

// prompt_multi_attempt_test.go — engine-level regression test for the
// PromptRouter's per-attempt-loop inFlight balance (CRI-259). The router's
// beginExecute runs once per attempt inside runStepFromAttempt, while its
// endStep counterpart closes once per attempt loop; this test drives the
// real loop (policy max_step_retries = 1, two iterations over the same step
// name) and pins that the router is balanced after the loop exits for any
// attempt count: a prompt arriving after the loop exits is classified
// NO_ACTIVE_SESSION at arrival (no hold, no event, no Prompt RPC), and a
// later visit of the same step neither delivers a parked prompt nor emits an
// AgentPromptInjected for a window the caller never addressed.

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/criteria/internal/adapterhost"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func routerInFlight(r *PromptRouter, step string) int {
	if r == nil {
		return -1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inFlight[step]
}

func routerExecuting(r *PromptRouter, step string) int {
	if r == nil {
		return -1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.executing[step]
}

func routerHeldCount(r *PromptRouter, step string) int {
	if r == nil {
		return -1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.held[step])
}

// promptFailureSignal observes prompt-failure log records as they are
// written, in delivery order, so a test can observe a routing decision at
// arrival time. The inner handler owns the output buffer; the class channel
// is buffered so the router pump never blocks on it.
type promptFailureSignal struct {
	slog.Handler
	classes chan string
}

func (h *promptFailureSignal) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "agent prompt delivery failed" {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "class" {
				select {
				case h.classes <- a.Value.String():
				default: // never block the pump
				}
				return false
			}
			return true
		})
	}
	return h.Handler.Handle(ctx, r)
}

func (h *promptFailureSignal) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &promptFailureSignal{Handler: h.Handler.WithAttrs(attrs), classes: h.classes}
}

func (h *promptFailureSignal) WithGroup(name string) slog.Handler {
	return &promptFailureSignal{Handler: h.Handler.WithGroup(name), classes: h.classes}
}

// TestEngineAgentPromptMultiAttemptBalance drives the real runStepFromAttempt
// attempt loop through a retry (fail_first makes the first Execute fail with
// a transient error, the retry succeeds) and a second iteration over the
// same step name, and pins the router balance at the loop exit:
//
//   - after iteration 1's attempt loop exits (2 attempts consumed), the
//     router's inFlight/executing/held state for the step is fully drained —
//     asserted directly, before iteration 2's beginExecute re-opens a window;
//   - a prompt arriving in that window is classified NO_ACTIVE_SESSION at
//     arrival (no hold, no AgentPromptInjected event, no Prompt RPC);
//   - iteration 2 (the later visit) delivers nothing retroactively.
func TestEngineAgentPromptMultiAttemptBalance(t *testing.T) {
	adapterBin := buildPromptableAdapter(t)
	callLog := filepath.Join(t.TempDir(), "promptable-calls.log")
	t.Setenv("PROMPTABLE_CALL_LOG", callLog)

	loader := adapterhost.NewLoaderWithDiscovery(func(string) (string, error) {
		return adapterBin, nil
	})
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	// Two iterations over the same step with max_step_retries = 1:
	// iteration 1 consumes 2 attempts (attempt 1 fails transiently, the
	// retry succeeds), iteration 2 is the later visit.
	g := compile(t, `
workflow {
  name = "prompt-multi-attempt"
  version = "0.1"
  initial_state = "a"
  target_state  = "done"

  policy { max_step_retries = 1 }
}
step "a" {
  target = adapter.promptable.default
  count  = 2
  input { fail_first = "true" }
  outcome "all_succeeded" { next = state.done }
}
state "done" { terminal = true }
adapter "promptable" "default" {}`)

	runID := "run-prompt-multi-attempt"
	ownerID := "agent-cri-259"
	promptCh := make(chan *pb.AgentPrompt, 8)
	classes := make(chan string, 8)

	var e *Engine
	var router *PromptRouter
	var arrivalClass string
	var gateSeen bool
	sink := &promptInjectSink{
		promptCh: promptCh,
		runID:    runID,
		owner:    ownerID,
		// The gate sits exactly in the window between iteration 1's
		// attempt-loop close (the deferred endStep has run) and iteration
		// 2's beginExecute.
		iterationGate: func(index int) {
			if index != 1 {
				return // iteration 0 announces before its attempt loop opens
			}
			gateSeen = true
			r := e.livePromptRouter()
			if r == nil {
				t.Error("no live prompt router between iterations")
				return
			}
			router = r
			// Iteration 1's attempt loop consumed 2 attempts; the router
			// must be balanced now, before iteration 2's beginExecute.
			if n := routerInFlight(r, "a"); n != 0 {
				t.Errorf(`inFlight["a"] = %d after iteration 1's attempt loop exited (2 attempts consumed), want 0`, n)
			}
			if n := routerExecuting(r, "a"); n != 0 {
				t.Errorf(`executing["a"] = %d after iteration 1's attempt loop exited, want 0`, n)
			}
			if n := routerHeldCount(r, "a"); n != 0 {
				t.Errorf("prompts held for step %q after its attempt loop exited = %d, want 0", "a", n)
			}

			// A prompt arriving after the attempt loop exited is classified
			// NO_ACTIVE_SESSION at arrival: no hold, no event, no Prompt RPC.
			select {
			case promptCh <- &pb.AgentPrompt{
				RunId:            runID,
				Step:             "a",
				Prompt:           "after the attempt loop",
				CallerCriteriaId: ownerID,
				IssuedAt:         timestamppb.Now(),
			}:
			case <-time.After(5 * time.Second):
				t.Error("router pump did not consume the post-loop prompt within 5s")
				return
			}
			select {
			case arrivalClass = <-classes:
			case <-time.After(5 * time.Second):
				t.Error("prompt arriving after the attempt loop exited was not classified at arrival (it was held)")
			}
			if n := routerHeldCount(r, "a"); n != 0 {
				t.Errorf("prompt arriving after the attempt loop exited was parked (%d held), want immediate NO_ACTIVE_SESSION", n)
			}
		},
	}

	var logBuf bytes.Buffer
	logger := slog.New(&promptFailureSignal{
		Handler: slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}),
		classes: classes,
	})

	e = NewTestEngine(g, loader, sink,
		WithRunID(runID),
		WithAgentPrompts(promptCh, ownerID, runID),
		WithLogger(logger),
	)
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !gateSeen {
		t.Fatal("iteration gate never ran (no OnStepIterationStarted with index 1)")
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal state: %s (ok=%v)", sink.terminal, sink.terminalOK)
	}
	if n := sink.sendFailureCount(); n != 0 {
		t.Fatalf("%d prompt send(s) timed out; router pump not consuming", n)
	}
	if arrivalClass != PromptFailNoActiveSession {
		t.Errorf("prompt arriving after the attempt loop exited classified %q, want NO_ACTIVE_SESSION", arrivalClass)
	}

	// The later visit must not have delivered the prompt sent between the
	// attempts: zero AgentPromptInjected events for the whole run.
	if n := sink.promptInjectionCount(); n != 0 {
		t.Fatalf("AgentPromptInjected emitted for a prompt sent after the attempt loop exited (%d events)", n)
	}

	// The router drained fully: after the final runStepFromAttempt returned,
	// every counter for the step is zeroed.
	if router != nil {
		router.mu.Lock()
		if len(router.inFlight) != 0 || len(router.executing) != 0 || len(router.held) != 0 {
			t.Errorf("router not drained after the run: inFlight=%v executing=%v held=%v",
				router.inFlight, router.executing, router.held)
		}
		router.mu.Unlock()
	}

	// No Prompt RPC on the wire, and the attempt loop is real: exactly three
	// Execute calls (attempt 1 fails, attempt 2 retries, iteration 2 runs).
	logged := readCallLog(t, callLog)
	executes, promptCalls := 0, 0
	for _, m := range logged {
		switch m {
		case "execute":
			executes++
		case "prompt":
			promptCalls++
		}
	}
	if promptCalls != 0 {
		t.Errorf("Prompt RPC issued for a prompt sent after the attempt loop exited; call log: %v", logged)
	}
	if executes != 3 {
		t.Errorf("execute calls = %d, want 3 (attempt 1 fails, attempt 2 retries, iteration 2 executes); call log: %v", executes, logged)
	}

	// Exactly one classification for the whole run — the arrival-time one the
	// gate consumed; nothing else fired (e.g. no deferred flush at Stop).
	var remaining []string
	for {
		select {
		case c := <-classes:
			remaining = append(remaining, c)
			continue
		default:
		}
		break
	}
	if len(remaining) != 0 {
		t.Errorf("unexpected additional prompt-failure classification(s) %v; log:\n%s", remaining, logBuf.String())
	}
}