//go:build !copilot

package copilot

import (
	"context"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/workflow"
)

// Adapter is the no-op stub used when the SDK is not compiled in.
// Build with `-tags copilot` to enable the real adapter.
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return Name }

func (a *Adapter) Execute(ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	sink.Log("agent", []byte("copilot adapter is not enabled in this build (rebuild with `-tags copilot`)\n"))
	return adapter.Result{Outcome: "failure"}, ErrNotEnabled
}
