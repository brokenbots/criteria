//go:build conformancefail

package conformance

import (
	"context"
	"errors"
	"testing"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// noLogStreamHandle is a minimal in-process adapter handle that does NOT
// implement adapterhost.LogStreamStarter. It is otherwise well-behaved, so
// the only v2 suite that skips is heartbeats. Because heartbeats is a required
// suite, runV2Suites must fail with "required conformance suite ... was skipped".
type noLogStreamHandle struct {
	sessions map[string]struct{}
}

func (h *noLogStreamHandle) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "nologstream", Version: "0.1.0"}, nil
}

func (h *noLogStreamHandle) OpenSession(_ context.Context, id string, _, _ map[string]string) error {
	if h.sessions == nil {
		h.sessions = make(map[string]struct{})
	}
	h.sessions[id] = struct{}{}
	return nil
}

func (h *noLogStreamHandle) Execute(_ context.Context, sessionID string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	if _, ok := h.sessions[sessionID]; !ok {
		return adapter.Result{}, errors.New("session is not open")
	}
	return adapter.Result{Outcome: "success"}, nil
}

func (h *noLogStreamHandle) CloseSession(_ context.Context, id string) error {
	delete(h.sessions, id)
	return nil
}

func (h *noLogStreamHandle) Kill() {}

func (h *noLogStreamHandle) Pause(context.Context, string) error  { return nil }
func (h *noLogStreamHandle) Resume(context.Context, string) error { return nil }
func (h *noLogStreamHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (h *noLogStreamHandle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (h *noLogStreamHandle) Restore(context.Context, string, []byte, uint32) error { return nil }

type noLogStreamLoader struct{ h adapterhost.Handle }

func (l *noLogStreamLoader) Resolve(context.Context, string) (adapterhost.Handle, error) {
	return l.h, nil
}
func (l *noLogStreamLoader) Shutdown(context.Context) error { return nil }

// TestRequiredSuiteSkipFailsRun exercises runV2Suites with a handle that does
// not implement LogStreamStarter. The heartbeats suite skips, and because it
// is required the parent test must fatalf.
func TestRequiredSuiteSkipFailsRun(t *testing.T) {
	h := &noLogStreamHandle{}
	ldr := &noLogStreamLoader{h: h}
	info := adapterhost.Info{Name: "nologstream"}
	runV2Suites(t, "nologstream", ldr, &Options{AllowedOutcomes: []string{"success"}}, &info)
}
