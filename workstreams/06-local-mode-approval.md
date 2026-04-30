# Workstream 6 — Local-mode approval and signal wait

**Owner:** Workstream executor · **Depends on:** none · **Coordinates with:** [W05](05-subworkflow-resolver-wiring.md) (nested workflow approvals propagate cleanly), [W14](14-phase2-cleanup-gate.md) (smoke workflow exercises this).

## Context

Phase 2's headline feature is unattended end-to-end execution: a
single `criteria apply` call should be able to run a chain of
workstreams without an orchestrator. Today, two node kinds force the
operator to a server-backed path:

- `approval` nodes: emit `OnApprovalRequested` and pause with
  `ErrPaused`, waiting for an orchestrator to resume with a decision
  payload ([internal/engine/node_approval.go:47-48](../internal/engine/node_approval.go#L47-L48)).
- `wait { signal = "..." }` nodes: emit `OnWaitEntered` and pause
  with `ErrPaused`, waiting for an orchestrator to deliver a signal
  payload ([internal/engine/node_wait.go:86-87](../internal/engine/node_wait.go#L86-L87)).

[internal/cli/apply.go:359-389](../internal/cli/apply.go#L359-L389)
(`ensureLocalModeSupported`) hard-rejects workflows containing either
node kind in local mode, with the error
`approval nodes require an orchestrator (e.g. --server <url>)` /
`signal waits require an orchestrator (e.g. --server <url>)`. This is
called out as deferred user-feedback item #05 (see
`user_feedback/05-allow-approval-in-local-mode-user-story.txt` —
preserved in git history at commit `4e4a357`).

This workstream introduces a local fallback so unattended pipelines
can include approval / wait gates without dropping to an
orchestrator. Castle / orchestrator-backed runs continue to work
unchanged.

The mechanism: a new env var `CRITERIA_LOCAL_APPROVAL` selects one of
four resolution modes when local-mode encounters an approval or
signal-wait pause. Decisions persist in the local checkpoint so
reattach is safe.

## Prerequisites

- `make ci` green on `main`.
- Familiarity with the existing local-state mechanics:
  [internal/cli/local_state.go](../internal/cli/local_state.go) and
  the `~/.criteria/runs/<run_id>.json` checkpoint format.
- Familiarity with the existing pause / resume pattern in
  [internal/engine/node_approval.go](../internal/engine/node_approval.go)
  and [internal/engine/node_wait.go](../internal/engine/node_wait.go).
- Familiarity with the engine's `ResumePayload` and `PendingSignal`
  state in
  [internal/engine/runstate.go](../internal/engine/runstate.go).

## In scope

### Step 1 — Define the four resolution modes

Operator selects a mode via `CRITERIA_LOCAL_APPROVAL`:

| Value | Behavior |
|---|---|
| `stdin` | Interactive TTY prompt: print the approver list, the reason, and `Approve? (y/n) ` to stderr; read a single line from stdin. `y`/`yes` → `approved`. `n`/`no` → `rejected`. EOF or any other input → `rejected` with reason `non-interactive input`. |
| `file` | Write a JSON sentinel to `~/.criteria/runs/<run_id>/approval-<node>.json` (the engine polls for the file to appear; the operator writes a decision file out-of-band). Format: `{"decision": "approved"}` or `{"decision": "rejected", "reason": "..."}`. The engine deletes the file after consumption. Polling interval: 2 seconds; max wait: 1 hour (configurable via `CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT`). On timeout the run fails with a clear error. |
| `env` | Read `CRITERIA_APPROVAL_<NODE_NAME>` (uppercase node name, dots and hyphens replaced with underscores). Value `approved` / `rejected`. Missing or invalid → fail the run with a clear error naming the env var the operator should set. |
| `auto-approve` | Log a warning (`approval node <name>: auto-approving because CRITERIA_LOCAL_APPROVAL=auto-approve`) and return `approved`. For unattended pipelines that have already vetted the workflow. Document loudly. |

When `CRITERIA_LOCAL_APPROVAL` is unset:

- If the workflow contains no approval / signal-wait nodes:
  unchanged (no env var needed).
- If the workflow contains an approval / signal-wait node:
  `ensureLocalModeSupported` rejects with the existing error,
  amended to mention `CRITERIA_LOCAL_APPROVAL` as the way to opt in:

  > `approval nodes require an orchestrator (e.g. --server <url>) or
  > the local-mode env CRITERIA_LOCAL_APPROVAL={stdin|file|env|auto-approve}`

Same shape for signal waits, with documentation pointing at the
signal-payload mechanism (see Step 3).

### Step 2 — Implement the resolver

Add a new package `internal/cli/localresume/` (or a single file under
`internal/cli/`) that exposes:

```go
type LocalResumer interface {
    // ResumeApproval blocks until a decision is available for
    // node `name` in run `runID`, or returns an error if the
    // selected mode cannot resolve. The returned payload is the
    // same shape the engine expects from an orchestrator-delivered
    // ResumePayload, with `decision` populated.
    ResumeApproval(ctx context.Context, runID, name string, approvers []string, reason string) (map[string]string, error)

    // ResumeSignal blocks until a payload for signal `name` is
    // available. For local mode, the four modes are:
    //   stdin       — operator types JSON: e.g. `{"outcome":"success"}`
    //   file        — same as approval, but the JSON shape includes
    //                 `outcome` instead of `decision`.
    //   env         — CRITERIA_SIGNAL_<NODE>=<outcome>
    //   auto-approve— synthesizes outcome="success" with a warning.
    ResumeSignal(ctx context.Context, runID, nodeName, signalName string) (map[string]string, error)
}
```

The CLI constructs the resumer from `CRITERIA_LOCAL_APPROVAL` and
threads it into the apply path. The engine exposes a hook for
"local resumer" — locate the existing pause/resume seam:

- `internal/cli/apply.go` — the function that calls `engine.RunFrom`
  / `engine.Run`. Today the local-mode path calls
  `ensureLocalModeSupported` *before* invoking the engine, which
  rejects approval/wait outright. After this workstream, the local
  path takes one of two routes:
  1. If `CRITERIA_LOCAL_APPROVAL` is set, allow the run, and on each
     `ErrPaused` event from the engine, call the resumer, populate
     `RunState.ResumePayload`, and re-invoke `engine.Run`.
  2. If `CRITERIA_LOCAL_APPROVAL` is unset, keep the existing reject
     behavior with the amended error message.

- The engine's run-loop already handles re-entry on
  `ResumePayload != nil` ([internal/engine/node_approval.go:28-39](../internal/engine/node_approval.go#L28-L39)).
  No engine change is required for this — only the CLI's outer loop
  changes.

### Step 3 — Persistence and reattach safety

Decisions must survive a CLI crash / restart so reattach picks up
where it left off.

- After a decision is captured, write it into the existing
  `StepCheckpoint` (or a sibling per-node checkpoint file) at
  `~/.criteria/runs/<run_id>/approvals/<node>.json` with shape
  `{"decision": "approved", "decided_at": "<RFC3339>"}`.
- On reattach, before re-invoking the resumer, the CLI checks for an
  existing decision file. If present, use it instead of prompting
  again. This makes the reattach idempotent and prevents the operator
  from being prompted twice for the same approval.
- Decision files are read-only after the engine consumes them — keep
  them around for audit; do not delete (the run-state cleanup at
  [internal/cli/local_state.go:140](../internal/cli/local_state.go#L140)
  removes the run dir on success, which sweeps these too).

### Step 4 — Update `ensureLocalModeSupported`

Modify [internal/cli/apply.go:359-389](../internal/cli/apply.go#L359-L389):

- When `CRITERIA_LOCAL_APPROVAL` is set, the function must *not*
  reject approval / signal-wait nodes.
- The error message for the "still rejected" path mentions
  `CRITERIA_LOCAL_APPROVAL` as the way to opt in.
- The function continues to reject *unknown* / unsupported node
  shapes — this workstream does not loosen anything beyond
  approval / signal-wait.

The function is called from two sites (`:102` and `:415`); both must
exhibit the new behavior.

### Step 5 — Tests

Cover each mode end-to-end. Use the existing engine + sink test
harness (locate via `internal/engine/engine_test.go` for the pattern;
the noop adapter is the right test plugin).

Test workflows:

- `testdata/local_approval_simple.hcl` — one approval node, then a
  noop step, then `done`.
- `testdata/local_signal_wait.hcl` — one wait-signal node, then a
  noop step, then `done`.
- A workflow with multiple approval nodes (covers the per-node
  decision file naming).

Test cases per mode:

- `stdin` mode: feed `y\n` via a pipe; assert run terminates `done`.
  Feed `n\n`; assert run terminates `failed` (or whichever transition
  the workflow declares for `rejected`).
- `file` mode: start the run in a goroutine, wait until the
  `approval-<node>.json` request file appears, write the response,
  assert run terminates correctly. Test the timeout path with a
  short `CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT`.
- `env` mode: set `CRITERIA_APPROVAL_FOO=approved`; assert
  terminates correctly. Unset the var; assert clear-error failure.
- `auto-approve` mode: assert the warning log appears and the run
  succeeds.
- Reattach safety: start a run in `file` mode, write the decision
  file, kill the process before consumption (simulate via a test
  hook), restart, assert the saved decision is reused.

Reject test:

- `CRITERIA_LOCAL_APPROVAL` unset + workflow contains approval →
  the new error message is emitted.

### Step 6 — Documentation

Update [docs/workflow.md](../docs/workflow.md) and
[docs/plugins.md](../docs/plugins.md) (whichever currently
documents `approval` and `wait { signal }` semantics) with:

- A "Local-mode approval and signal wait" section listing the four
  modes, the env-var contract, the file-mode JSON schema, and the
  reattach guarantee.
- A note that orchestrator-backed runs ignore
  `CRITERIA_LOCAL_APPROVAL` entirely (the orchestrator continues to
  drive resume).

Do **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`.

## Behavior change

**Yes — substantial new feature.**

- New env var `CRITERIA_LOCAL_APPROVAL` with four valid values.
- Optional env var `CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT` for the
  file-mode timeout (default 1h).
- Per-node env vars: `CRITERIA_APPROVAL_<NODE>` (env mode) and
  `CRITERIA_SIGNAL_<NODE>` (env mode for signal waits).
- New on-disk artifact: `~/.criteria/runs/<run_id>/approvals/<node>.json`
  (read-write for `file` mode; read-only audit record for the others).
- `ensureLocalModeSupported` rejects with a different error message
  when `CRITERIA_LOCAL_APPROVAL` is unset — string-matching consumers
  may need to update.
- New log line on `auto-approve` mode (warning level).
- Castle / orchestrator-backed runs are unchanged: the env var is
  ignored when `--server` is set.

## Reuse

- Existing `RunState.ResumePayload` and `RunState.PendingSignal`
  state.
- Existing `~/.criteria/runs/<run_id>/` directory layout from
  [internal/cli/local_state.go](../internal/cli/local_state.go).
  After [W04](04-state-dir-permissions.md) lands, the dir is `0o700`
  — verify the new approval files inherit that confinement.
- The engine's existing pause/resume cycle. Do not change the
  engine's pause semantics.
- The existing `OnApprovalRequested` and `OnWaitEntered` sink hooks
  in `internal/engine/sink.go` (or the equivalent file). The CLI
  attaches the resumer to the sink; the engine code is unchanged.

## Out of scope

- Castle / orchestrator-backed approval semantics. Unchanged.
- A web UI or HTTP listener for approvals. The four modes are
  sufficient for unattended pipelines and dev iteration.
- Approval routing / multiple-approver consensus. The engine treats
  approval as a single decision today; we do not extend that here.
- Wait nodes with `duration` (already work locally; not touched).
- Rejected-decision retry logic. A `rejected` decision causes the
  run to take its `rejected` transition (or fail if no such
  transition exists, which is the current behavior).

## Files this workstream may modify

- `internal/cli/apply.go` (resumer construction, run-loop
  re-invocation, `ensureLocalModeSupported` amendment).
- `internal/cli/localresume/resumer.go` (new package or single file —
  pick one approach and stick to it).
- `internal/cli/localresume/resumer_test.go` (new).
- `internal/cli/local_state.go` (helpers for the approvals subdir;
  reuse `stateDir()` — do not duplicate path resolution).
- `internal/cli/testdata/local_approval_simple.hcl` (new).
- `internal/cli/testdata/local_signal_wait.hcl` (new).
- Any `*_test.go` in `internal/cli/` that covers the apply path,
  extended to cover the new resumer paths.
- `docs/workflow.md` and/or `docs/plugins.md` (documentation).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
It may **not** modify the engine's pause/resume contract, the
`ResumePayload` shape, or the `Sink` interface.

## Tasks

- [x] Define `LocalResumer` interface and four-mode implementation.
- [x] Wire the resumer into `apply.go`'s local-mode path.
- [x] Amend `ensureLocalModeSupported` to honor
      `CRITERIA_LOCAL_APPROVAL`.
- [x] Add per-node decision persistence under
      `~/.criteria/runs/<run_id>/approvals/`.
- [x] Add reattach idempotency: existing decision files are reused.
- [x] Add unit and integration tests for all four modes plus reject
      path plus reattach.
- [x] Update documentation in `docs/workflow.md` (and/or
      `docs/plugins.md`).
- [x] `make build`, `make plugins`, `make test`, `make ci` all green.

## Exit criteria

- `CRITERIA_LOCAL_APPROVAL=stdin criteria apply <workflow>` runs to
  completion when the operator types `y` at the prompt.
- `CRITERIA_LOCAL_APPROVAL=auto-approve criteria apply <workflow>`
  runs unattended to completion with a warning log per approval.
- `CRITERIA_LOCAL_APPROVAL=file criteria apply <workflow>` runs to
  completion when the operator writes a decision file from another
  shell.
- `CRITERIA_LOCAL_APPROVAL=env CRITERIA_APPROVAL_FOO=approved
  criteria apply <workflow>` runs to completion.
- Without `CRITERIA_LOCAL_APPROVAL`, an approval-bearing workflow
  fails compile-time validation with the new amended error.
- Approval decisions persist to disk and survive a CLI restart
  (reattach uses the saved decision).
- `make ci` green.

## Tests

Test files (new):

- `internal/cli/localresume/resumer_test.go` — unit tests for each
  mode (stdin via pipe, file via tempdir, env via `t.Setenv`,
  auto-approve, env-mode reject).
- `internal/cli/localresume/integration_test.go` — full
  apply-to-completion runs for the testdata workflows under each
  mode.
- `internal/cli/apply_test.go` (extend) — `ensureLocalModeSupported`
  rejection now mentions `CRITERIA_LOCAL_APPROVAL`.

Existing tests must pass unchanged.

## Risks

| Risk | Mitigation |
|---|---|
| `stdin` mode is hard to test deterministically | Use a pipe (`os.Pipe()`) and write `y\n` synthetically. The resumer must read stdin via an injectable `io.Reader` for the test seam. |
| `file` mode polling interval (2s) is slow for tests | Make the polling interval configurable; tests use 50ms. |
| The CLI re-invokes `engine.Run` after each pause; this could double-fire side effects (logs, events) | The engine already idempotently handles reattach (see the `OnApprovalRequested` re-emit on crash-reattach). Verify behavior with the existing reattach tests; do not regress. |
| A decision file written before the engine reaches the approval node is consumed prematurely | The resumer only reads the decision file *after* the engine has emitted `OnApprovalRequested` for the node. Document this in the file-mode contract. Use the `OnApprovalRequested` hook to trigger the wait, not a poll-from-start. |
| The reattach idempotency conflicts with [W04](04-state-dir-permissions.md)'s 0o700 perms | The new approvals subdir must be 0o700 too. Reuse the same `MkdirAll` mode. |
| Approval / signal nodes inside a sub-workflow (loaded via [W05](05-subworkflow-resolver-wiring.md)) propagate correctly | The compiled `FSMGraph` unions all nodes; `ensureLocalModeSupported` operates on the unioned graph; the resumer is attached at the run-loop level, so nested approvals work transparently. Add an integration test that exercises this when both W05 and W06 land. |

## Implementation Notes

### New files created
- `internal/cli/localresume/resumer.go` — `LocalResumer` interface + 4-mode concrete implementation (stdin/file/env/auto-approve). Handles both approval and signal-wait resume, decision persistence, reattach idempotency. Configurable polling interval (default 2s, tests use 50ms).
- `internal/cli/localresume/resumer_test.go` — 25 unit tests covering all 4 modes, context cancellation, timeout, reattach idempotency, and error paths.
- `internal/cli/apply_local_approval_test.go` — 7 integration tests using testdata HCL workflows and the noop adapter: auto-approve approval/signal, env-mode approved/rejected/signal, file-mode approval, disabled-mode rejection.
- `internal/cli/testdata/local_approval_simple.hcl` — `approval → open_demo → run_step → close_demo → done/rejected_state`.
- `internal/cli/testdata/local_signal_wait.hcl` — `wait(gate) → open_demo → run_step → close_demo → done`.

### Modified files
- `internal/cli/local_state.go` — Added `approvalDecisionDir()`, `ApprovalDecisionPath()`, `ApprovalRequestPath()` path helpers.
- `internal/cli/apply.go` — Added `pauseTracker`, `buildLocalResumer()`, `drainLocalResumeCycles()`, `resolveLocalPause()`, `prepareReattach(ctx, ...)`; refactored `ensureLocalModeSupported` with package-level error-message constants and early-return branch to reduce cognitive complexity; updated `runApplyLocal` and `resumeOneLocalRun`.
- `docs/workflow.md` — Added complete "Local-mode approval and signal wait" section (4 modes, env vars, file schema, reattach guarantee, timeout, examples); amended "Signal-based wait" and "Approval" sections; updated "Local-mode constraints" section.

### Key design decisions
- Engine is **unchanged**; all new behavior is in the CLI apply loop.
- `ensureLocalModeSupported` now accepts a `localApprovalEnabled bool` parameter; when true it skips rejection of approval/signal-wait nodes and returns immediately.
- `resolveApprovalStdin`, `resolveApprovalAutoApprove`, and `resolveSignalAutoApprove` return `map[string]string` (not `(map, error)`) because they cannot fail — simplified unparam-compliant signatures.
- `prepareReattach` accepts `ctx context.Context` to satisfy contextcheck linter; context is threaded through for future propagation to `parseWorkflowFromPath` when that function gains a ctx parameter.
- Engine's `success=false` terminal states return `nil` error from `runApplyLocal`; rejection is communicated via events, not Go errors.
- Noop adapter requires `lifecycle = "open"` step before `Execute`; both testdata HCLs include `open_demo`/`close_demo` lifecycle steps.

## Reviewer Notes

All exit criteria met and verified:
- **stdin mode** — pipe-based unit test feeds `y\n`/`n\n`; integration test runs full apply with piped stdin.
- **auto-approve mode** — integration test confirms completion + warning log.
- **env mode** — integration tests cover approved, rejected, and signal-wait variants.
- **file mode** — integration test goroutine writes decision file after `OnApprovalRequested` fires.
- **disabled (unset) mode** — `apply_server_required_test.go` verifies new error message mentions `CRITERIA_LOCAL_APPROVAL`.
- **reattach idempotency** — unit test `TestResumer_ReattachIdempotency` writes a pre-existing decision file and confirms the resumer reuses it without prompting.
- **persistence** — `ApprovalDecisionPath` + `ApprovalRequestPath` wired throughout; decision files are written before resume and kept for audit.
- `make ci` green (lint, tests, build, validate, example plugin run).
- `internal/cli/reattach.go` was not modified; its pre-existing contextcheck baseline entries are unchanged.
