// Package adapter defines the contract between the Overseer engine and the
// concrete code that runs a step (shell, Copilot agent, etc.).
package adapter

import (
	"context"

	"github.com/brokenbots/overseer/workflow"
)

// EventSink is what an adapter uses to stream incremental output and
// adapter-specific events back to the engine. Implementations are responsible
// for forwarding to the Castle transport.
type EventSink interface {
	// Log streams a chunk of output. `stream` is one of "stdout", "stderr",
	// "agent".
	Log(stream string, chunk []byte)

	// Adapter records a structured adapter-specific event. `kind` namespaces
	// the event (e.g. "tool.invocation"); `data` is opaque JSON-serialisable
	// payload.
	Adapter(kind string, data any)
}

// Result is what an adapter returns from Execute.
type Result struct {
	// Outcome must match one of the outcomes declared on the step in HCL.
	// On error, the engine treats the result as the conventional "failure"
	// outcome (if mapped) regardless of this value.
	Outcome string
	// Outputs carries key/value pairs captured from the step execution (W04).
	// Keys are defined by the adapter's OutputSchema. Nil means no outputs.
	Outputs map[string]string
}

// Adapter executes a single step. The engine calls Execute once per step
// invocation. Implementations must respect ctx cancellation (used for
// step timeouts and run cancellation).
type Adapter interface {
	Name() string
	// Info returns the adapter's declared configuration schema. The compiler
	// uses this to validate agent config and step input blocks at compile time.
	// Return a zero-value AdapterInfo to opt out of validation (permissive mode).
	Info() workflow.AdapterInfo
	Execute(ctx context.Context, step *workflow.StepNode, sink EventSink) (Result, error)
}
