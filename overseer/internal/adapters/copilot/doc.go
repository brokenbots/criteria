// Package copilot implements the `copilot` adapter built on the official
// github.com/github/copilot-sdk/go SDK.
//
// Phase 0 design notes:
//   - One copilot.Client per Overseer process; one Session per step.
//   - Permission handler approves all (every request structured-logged via
//     the EventSink).
//   - Outcome is determined by parsing the agent's final assistant message
//     for a trailing line of the form `RESULT: <outcome>`. The workflow's
//     prompt is responsible for instructing the model to emit this. If no
//     RESULT line is found the outcome defaults to `needs_review`.
//
// IMPORTANT: this file is a working scaffold. The SDK import is wired but
// guarded by a build tag so the rest of the codebase compiles for developers
// who do not have the GitHub Copilot CLI installed locally. Build the real
// adapter with `go build -tags copilot ./...` once `copilot` is on PATH.
//
// The non-tagged stub returns a clear error so workflows that reference the
// adapter fail fast in dev environments.
package copilot

const Name = "copilot"

// resultPrefix is the conventional marker the agent must emit to signal the
// final outcome of the step.
const resultPrefix = "RESULT:"
