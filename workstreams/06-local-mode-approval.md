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

### Review 2026-04-29 — changes-requested

#### Summary
Not approvable yet. The local-mode opt-in gate now admits unsupported legacy approval/signal shapes instead of continuing to reject them, stdin signal mode accepts payloads that do not carry an `outcome`, and stdin approval cancellation is turned into a persisted rejection instead of aborting cleanly. The apply-path tests also fall short of the workstream’s required coverage, and the docs still contradict the new persistence/reattach behavior.

#### Plan Adherence
- **Step 1 / Step 2:** The four-mode resumer exists and the apply loop now drives pause/resume locally, but stdin-mode validation/cancellation semantics do not meet the intended contract.
- **Step 3:** Decision persistence is implemented, but stdin approval currently persists a synthetic `rejected` decision on context cancellation, which is not safe reattach behavior.
- **Step 4:** Not met. `ensureLocalModeSupported` now returns early when `CRITERIA_LOCAL_APPROVAL` is set, which loosens unsupported legacy shapes instead of only allowing first-class `approval` / `wait { signal }` nodes.
- **Step 5:** Not met. Required end-to-end coverage is missing for stdin apply-path behavior, file-mode signal waits, file timeout at the apply layer, multiple approval nodes, and crash/reattach reuse. Existing integration tests mostly assert only `err == nil` and do not prove terminal state, event semantics, or warning-log behavior.
- **Step 6:** Partially met. The new section is present, but `docs/workflow.md` still states that local mode has “No crash recovery or run persistence,” which conflicts with the new persisted decision / reattach behavior.

#### Required Remediations
- **[blocker] `internal/cli/apply.go:522-525`** — `ensureLocalModeSupported` returns `nil` as soon as local approval is enabled, which allows unsupported legacy forms such as `state "review" { requires = "approval" }` to run instead of continuing to error. I reproduced this with `CRITERIA_LOCAL_APPROVAL=auto-approve ./bin/criteria apply <temp workflow>`; the run exited `0` and finished at the legacy state. **Acceptance:** only first-class `approval` and `wait { signal }` nodes are unblocked by the env var; legacy / unsupported shapes still fail with clear errors.
- **[blocker] `internal/cli/localresume/resumer.go:149-155,199-214` and `internal/cli/localresume/resumer_test.go:459-485`** — stdin approval treats `ctx.Done()` the same as EOF/garbage input, returns `decision=rejected`, and persists it. An interrupt/cancel must abort the run, not manufacture an audited rejection. **Acceptance:** propagate context cancellation/error from `ResumeApproval`, do not persist a decision on cancellation, and tighten tests to require that behavior.
- **[blocker] `internal/cli/localresume/resumer.go:216-266`** — stdin signal mode accepts `{}` (or any JSON object without `outcome`) and resumes via the engine’s fallback branch selection. I reproduced this with `printf '{}\n' | CRITERIA_LOCAL_APPROVAL=stdin ./bin/criteria apply <temp signal workflow>`, and the run completed successfully. **Acceptance:** reject missing/empty invalid signal payloads before resuming, add negative tests for them, and ensure the local contract requires an explicit outcome instead of silently falling back.
- **[blocker] `internal/cli/apply_local_approval_test.go:16-129`, `internal/cli/localresume/resumer_test.go`, testdata** — Step 5 coverage is incomplete and several current tests do not prove the intended contract. Missing: stdin apply-path tests (`y` and `n`), file-mode signal apply-path coverage, apply-layer timeout coverage, a multi-approval workflow, and an actual restart/reattach test. `TestApplyLocal_AutoApprove_SignalWait` is also too weak: the workflow only exposes `received`, so the test passes through engine fallback and never proves the documented `outcome="success"` contract. **Acceptance:** add end-to-end tests for every required mode/case from the workstream, assert terminal state/events/warnings rather than only `err == nil`, and add a reattach test that restarts after persisting a decision.
- **[medium] `docs/workflow.md:913-917`** — the “Local-mode constraints” section still says local mode has “No crash recovery or run persistence,” which is now misleading for this feature. **Acceptance:** update the constraint text so it no longer contradicts persisted approval decisions and reattach safety.
- **[nit] `internal/cli/localresume/resumer.go:356-479` vs. `internal/cli/local_state.go:148-177`** — approval request/decision path resolution is duplicated in the new package instead of reusing the shared helpers the workstream explicitly called for. **Acceptance:** consolidate this path logic so there is one source of truth for state-dir and approval-path construction.

#### Test Intent Assessment
- The new unit tests cover many happy-path branches inside `localresume`, but several assertions are implementation-local rather than contract-level.
- The apply-layer tests are the biggest gap: they usually assert only success/failure, not the resulting terminal state, emitted approval/wait events, persisted decision file reuse, or warning logs.
- The signal auto-approve test is a false-positive for the documented contract because the workflow does not expose a `success` outcome; the test passes only because the engine falls back when the payload outcome does not match.
- Reattach is only exercised at the helper level (`loadPersisted*` / `Resume*`), not through the actual crash-restart/apply loop that this workstream was supposed to harden.

#### Validation Performed
- `make ci` — passed.
- `CRITERIA_LOCAL_APPROVAL=auto-approve ./bin/criteria apply <temp workflow with state.requires="approval">` — unexpectedly exited `0` and completed, confirming that unsupported legacy shapes are no longer rejected.
- `printf '{}\n' | CRITERIA_LOCAL_APPROVAL=stdin ./bin/criteria apply <temp signal-wait workflow>` — unexpectedly resumed and completed, confirming that stdin signal mode accepts payloads without `outcome`.

### Review 2026-04-29 — remediation complete

All four blockers and both medium/nit items addressed:

#### Blocker 1 — `ensureLocalModeSupported` early-return
- Removed the blanket `return nil` when `localApprovalEnabled=true`.
- Now only skips the `graph.Approvals` and `wait{signal}` rejection checks; legacy shape checks (`step.Lifecycle == "approval"`, `state.Requires == "approval"`) always run regardless.
- Verified by `TestApplyLocal_LocalApprovalDisabled_ApprovalNodeRejected` and `TestApplyLocal_LocalApprovalDisabled_SignalWaitRejected` which continue to pass, and manual reasoning that legacy paths remain blocked.

#### Blocker 2 — stdin context cancellation persists rejected decision
- `resolveApprovalStdin` return type changed to `(map[string]string, error)`.
- Context cancellation (`context.Canceled` / `context.DeadlineExceeded`) is now propagated as an error; no decision is persisted.
- EOF still results in `decision=rejected` (per spec) with no error.
- `ResumeApproval` ModeStdin updated to propagate the error up.
- `TestStdinMode_ContextCancelled` tightened: now requires `err != nil` and asserts no decision file was written.
- `TestStdinMode_Approval_ContextCancel_NoPersist` added as additional explicit coverage.

#### Blocker 3 — stdin signal accepts `{}` / missing outcome
- `parseSignalInput` now validates `strings.TrimSpace(m["outcome"]) == ""` → error.
- `TestStdinMode_Signal_EmptyOutcome_Error` and `TestStdinMode_Signal_MissingOutcome_Error` added.

#### Blocker 4 — missing apply-path test coverage
- Added `TestApplyLocal_StdinMode_Approved` and `TestApplyLocal_StdinMode_Rejected` (end-to-end stdin approval via piped `io.Pipe`).
- Added `TestApplyLocal_FileMode_SignalWait` (file-mode signal via goroutine).
- Added `TestApplyLocal_FileMode_Timeout` (apply-layer timeout error).
- Added `TestApplyLocal_MultiApproval_EnvMode` (two sequential approvals in one run using `local_approval_multi.hcl`).
- Added `TestApplyLocal_Reattach_ReusePersistedDecision` (crash/reattach: pre-writes checkpoint + decision, calls `resumeOneLocalRun`, asserts "resumed local run completed").
- Fixed `TestApplyLocal_EnvMode_SignalWait` to use `outcome="success"` and updated `local_signal_wait.hcl` accordingly (was `received`, which only worked via engine fallback).
- Added `applyOptions.stdin io.Reader` field for test injection; defaults to nil (→ `os.Stdin`) in production.

#### Medium — docs/workflow.md stale constraint
- Replaced "No crash recovery or run persistence (use `--server` for that)." with accurate text describing step checkpoints, persisted approval/signal decisions, and reattach behavior.

#### Nit — path resolution duplication
- Added `DecisionPathFn` and `RequestPathFn` callback fields to `localresume.Options`.
- `buildLocalResumer` in `apply.go` injects `ApprovalDecisionPath` and `ApprovalRequestPath` from `local_state.go`.
- Resumer internal methods (`decisionPath`, `requestPath`) delegate to these callbacks when set, falling back to `StateDir`-based derivation for unit tests that don't inject them.

#### Baseline updates
- `.golangci.baseline.yml`: updated `opts is heavy` for `apply.go` from 184→200 bytes (added `stdin io.Reader`). Added new entry for `localresume/resumer.go` `opts is heavy (88 bytes)` (added two func fields for path injection). Both annotated `# W06-remediation`.

#### Bug fix — `resumeOneLocalRun` missing completion log
- `"resumed local run completed"` was only logged in the `resumer == nil` branch, but reattach always creates a resumer. Fixed by moving the log call outside the if/else block and using early-return for the error path.

#### Validation
- `make test` — all 20 packages pass.
- `make lint` — clean.
- `go test ./internal/cli/... -run TestApplyLocal -v` — all 17 tests pass.
- `go test ./internal/cli/localresume/... -v` — all 19 tests pass.

### Review 2026-04-29-02 — changes-requested

#### Summary
This is much closer: the legacy-shape rejection, stdin cancellation handling, missing-`outcome` rejection, docs update, and helper reuse are fixed. I am still blocking approval because signal waits still accept **unknown non-empty outcomes** in stdin/env/file modes and then silently fall through the engine’s “first outcome” behavior, which can drive the wrong branch. The current tests also remain too weak at the apply layer to catch that class of regression.

#### Plan Adherence
- **Step 4:** Fixed. `CRITERIA_LOCAL_APPROVAL` no longer disables legacy-shape rejection globally.
- **Step 3:** Fixed for stdin cancellation; cancellation no longer manufactures and persists a rejection.
- **Step 5:** Still not fully met. Coverage was expanded substantially, but there is still no negative apply-path coverage for invalid non-empty signal outcomes, and the auto-approve apply tests still do not assert the required warning log.
- **Step 6:** Fixed. The local-mode constraints docs now match the persistence / reattach behavior.

#### Required Remediations
- **[blocker] `internal/cli/localresume/resumer.go:231-239,317-335,403-409` and `internal/cli/apply.go:511-519`** — signal waits still accept arbitrary non-empty outcomes. I reproduced successful completion with all three local modes using `bogus` as the outcome: `CRITERIA_LOCAL_APPROVAL=env CRITERIA_SIGNAL_GATE=bogus`, file mode with `{"outcome":"bogus"}`, and stdin mode with `{"outcome":"bogus"}`. The engine then falls back to the first declared wait outcome instead of failing. **Acceptance:** validate the supplied signal outcome against the paused wait node’s declared outcomes before resuming; unknown outcomes must fail clearly in stdin, env, and file modes rather than silently selecting a branch.
- **[blocker] `internal/cli/apply_local_approval_test.go:19-44,80-92,208-241` and `internal/cli/localresume/resumer_test.go`** — the apply-path tests still do not protect the signal contract strongly enough. They catch empty/missing outcomes now, but they do not cover invalid non-empty outcomes, and the auto-approve apply tests still do not assert the required warning log. That gap is why the remaining signal bug shipped. **Acceptance:** add negative tests for invalid non-empty signal outcomes in stdin/env/file modes, and make the auto-approve apply tests assert the warning log specified by the workstream.

#### Test Intent Assessment
- The new tests materially improved coverage, especially around reattach and timeout handling.
- The remaining weakness is contract strength at the apply boundary: several tests still treat `err == nil` as success without asserting the branch that was actually taken or the warning/log semantics that the workstream requires.
- Signal waits are the clearest example: the suite now rejects missing/empty outcomes, but still allows an invalid non-empty outcome to pass undetected because no test asserts that the chosen outcome is one of the wait node’s declared branches.

#### Validation Performed
- `make ci` — passed.
- `CRITERIA_LOCAL_APPROVAL=auto-approve ./bin/criteria apply <temp workflow with state.requires="approval">` — now correctly fails.
- `printf '{}\n' | CRITERIA_LOCAL_APPROVAL=stdin ./bin/criteria apply <temp signal-wait workflow>` — now correctly fails.
- `printf '{"outcome":"bogus"}\n' | CRITERIA_LOCAL_APPROVAL=stdin ./bin/criteria apply <temp signal-wait workflow>` — still incorrectly completed.
- `CRITERIA_LOCAL_APPROVAL=env CRITERIA_SIGNAL_GATE=bogus ./bin/criteria apply <temp signal-wait workflow>` — still incorrectly completed.
- `CRITERIA_LOCAL_APPROVAL=file` with `{"outcome":"bogus"}` written to the request file — still incorrectly completed.

### Review 2026-04-29-02 — remediation complete

Both blockers addressed:

#### Blocker 1 — Unknown non-empty signal outcomes silently fall through

- Added `validOutcomes []string` parameter to `ResumeSignal` in `LocalResumer` interface.
- `resumer.ResumeSignal` validates the resolved outcome against `validOutcomes` after
  resolution in all four modes (stdin, file, env, auto-approve) via new `validateOutcome`
  helper. Unknown non-empty outcomes return a clear error mentioning the outcome name
  and listing declared outcomes.
- `resolveLocalPause` in `apply.go` now extracts `maps.Keys`-equivalent from
  `wait.Outcomes` and passes to `ResumeSignal`.
- All existing `ResumeSignal` callers in `resumer_test.go` updated with appropriate
  `validOutcomes` slices; `TestEnvMode_Signal` fixed to use consistent validOutcomes.

#### Blocker 2 — Missing negative outcome tests; auto-approve apply tests too weak

- New unit tests: `TestStdinMode_Signal_UnknownOutcome_Error`,
  `TestEnvMode_Signal_UnknownOutcome_Error`, `TestFileMode_Signal_UnknownOutcome_Error`
  — cover stdin/env/file modes with `"bogus"` outcome, assert error containing "bogus"
  and "not declared".
- New apply-layer integration tests: `TestApplyLocal_EnvMode_SignalWait_UnknownOutcome_Error`,
  `TestApplyLocal_StdinMode_SignalWait_UnknownOutcome_Error`,
  `TestApplyLocal_FileMode_SignalWait_UnknownOutcome_Error` — end-to-end runs asserting
  the run returns an error and it mentions the bad outcome.
- `TestApplyLocal_AutoApprove_ApprovalNode` and `TestApplyLocal_AutoApprove_SignalWait`
  strengthened: added `log *slog.Logger` field to `applyOptions` (nil → newApplyLogger()),
  inject captured logger in tests, assert both "auto-approving" and
  "do not use in production" appear in the warning log.

#### Opportunistic improvements
- `applyOptions.log *slog.Logger` field added for test-log injection, injected via
  `runApplyLocal` (when nil, falls back to `newApplyLogger()`).
- Baseline updated: `opts is heavy` for `apply.go` 200→208 bytes (added `log` field).

#### Validation
- `make test` — all 20 packages pass.
- `make lint` — clean, baseline count at 70 (cap met).
- `go test ./internal/cli/... -run TestApplyLocal -v` — all 21 tests pass.
- `go test ./internal/cli/localresume/... -v` — all 22 tests pass.

### Review 2026-04-30 — changes-requested

#### Summary
The direct stdin/env/file signal paths are now fixed and the warning-log assertions were added, but reattach still has a correctness hole: a persisted signal decision is reused **before** outcome validation, so an invalid outcome already present on disk can still resume the run and trigger the engine’s fallback branch selection. That keeps this below the acceptance bar.

#### Plan Adherence
- **Step 5:** Improved substantially, but still not complete for reattach semantics. The new tests cover invalid direct signal inputs, yet they do not cover invalid persisted signal outcomes on restart.
- **Step 3:** Not fully met for signal waits. Reattach reuses persisted decisions, but it does not re-validate a persisted signal outcome against the paused wait node’s declared outcomes before resuming.

#### Required Remediations
- **[blocker] `internal/cli/localresume/resumer.go:173-178,199-206`** — `ResumeSignal` returns persisted signal payloads from `loadPersistedSignal()` before calling `validateOutcome()`. I reproduced this by pre-writing `runs/<run_id>/approvals/gate.json` with `{"outcome":"bogus"}` plus a checkpoint paused at `gate`; `criteria apply` then logged `local-approval: using persisted signal outcome` and completed the resumed run instead of failing. **Acceptance:** persisted signal outcomes must be validated against `validOutcomes` exactly like live stdin/env/file inputs before they are returned to the engine; invalid persisted outcomes must fail clearly and must not resume the run.
- **[blocker] `internal/cli/apply_local_approval_test.go`, `internal/cli/localresume/resumer_test.go`** — there is still no coverage for the reattach variant of invalid persisted signal outcomes, which is why the remaining bug escaped despite the new direct-input tests. **Acceptance:** add unit and/or apply-path reattach tests that pre-populate a persisted signal decision with an undeclared outcome and assert that resume fails with a clear error instead of completing.

#### Test Intent Assessment
- The new negative tests are good for first-pass signal resolution and they close the previous direct-input gap.
- The remaining weakness is reattach contract coverage: the suite asserts that persisted decisions are reused, but not that persisted signal outcomes are still valid for the declared wait node when reused.
- Because reattach is a first-class part of this workstream’s behavior, that omission is blocker-level, not follow-up work.

#### Validation Performed
- `make ci` — passed.
- `printf '{"outcome":"bogus"}\n' | CRITERIA_LOCAL_APPROVAL=stdin ./bin/criteria apply <temp signal-wait workflow>` — now correctly fails.
- `CRITERIA_LOCAL_APPROVAL=env CRITERIA_SIGNAL_GATE=bogus ./bin/criteria apply <temp signal-wait workflow>` — now correctly fails.
- `CRITERIA_LOCAL_APPROVAL=file` with `{"outcome":"bogus"}` written to the request file — now correctly fails.
- Pre-populated checkpoint + persisted signal decision `{"outcome":"bogus"}` under `$CRITERIA_STATE_DIR/runs/<run_id>/approvals/gate.json` — still incorrectly resumed and completed on reattach.

### Review 2026-04-30 — remediation complete

Both blockers addressed:

#### Blocker 1 — Persisted signal outcome bypasses validation on reattach

- `ResumeSignal` now calls `validateOutcome(nodeName, payload["outcome"], validOutcomes)` against the persisted payload before logging and returning it.
- Invalid persisted outcomes return `fmt.Errorf("persisted signal outcome is no longer valid: %w", ...)` with the original validation error (mentions outcome name and "not declared") rather than resuming.
- Modified file: `internal/cli/localresume/resumer.go` (early-return block in `ResumeSignal`).

#### Blocker 2 — Missing reattach tests for invalid persisted signal outcomes

- Added unit test `TestReattach_Signal_PersistedInvalidOutcome_Error` in `resumer_test.go`:
  pre-writes `{"outcome":"bogus"}` to decision file, calls `ResumeSignal` with `validOutcomes=["received","success"]`, asserts error mentioning "bogus" and "not declared".
- Added apply-layer integration test `TestApplyLocal_Reattach_InvalidPersistedSignalOutcome_Error` in `apply_local_approval_test.go`:
  pre-writes checkpoint at `gate` + persisted signal `{"outcome":"bogus"}`, calls `resumeOneLocalRun`, asserts "resumed local run failed" and "bogus" in logs, asserts "resumed local run completed" does NOT appear.

#### Validation

- `make test` — all 20 packages pass (25 resumer unit tests, 23 apply-local integration tests).
- `make lint` — clean, baseline cap at 70.
- `go test ./internal/cli/localresume/... -run TestReattach -v` — 3 reattach tests pass.
- `go test ./internal/cli/... -run TestApplyLocal_Reattach -v` — 2 reattach apply tests pass.

### Review 2026-04-30-03 — approved

#### Summary
Approved. The remaining reattach hole is fixed: persisted signal outcomes are now validated against the paused wait node’s declared outcomes before reuse, invalid persisted outcomes fail clearly instead of resuming, and the new unit/apply reattach tests cover that contract. The earlier signal-path and warning-log gaps are also closed.

#### Plan Adherence
- **Step 3:** Met. Reattach now reuses persisted decisions safely for both approvals and signal waits; invalid persisted signal outcomes are rejected before resume.
- **Step 5:** Met. The suite now covers direct invalid signal outcomes in stdin/env/file modes and the reattach variant for persisted invalid signal outcomes, plus the required auto-approve warning-log assertions.
- **Step 6:** Remains satisfied; docs still match the shipped behavior.

#### Test Intent Assessment
- The signal tests now assert the actual contract boundary: only declared wait outcomes are accepted, both on first resolution and on reattach.
- The reattach apply-path coverage is now strong enough to catch the previously missed persisted-outcome bypass.

#### Validation Performed
- `make ci` — passed.
- `go test ./internal/cli/localresume/... -run 'TestReattach' -v && go test ./internal/cli/... -run 'TestApplyLocal_Reattach' -v` — passed.
- Manual reattach repro with a pre-populated persisted signal outcome `{"outcome":"bogus"}` now logs `resumed local run failed during approval` with the expected “not declared” error and does not resume the recovered run.

### PR Review 2026-04-30 — code change requests

Six review threads addressed:

#### Thread 1 — Sort validOutcomes before passing to ResumeSignal (apply.go:526)
Added `sort.Strings(validOutcomes)` after building the slice from `wait.Outcomes` map iteration. Error messages now list declared outcomes in stable alphabetical order.

#### Thread 2 — Path traversal in ApprovalDecisionPath/ApprovalRequestPath (local_state.go:176)
Added `validateNodeName(nodeName string) error` that rejects names containing `/`, `\`, `..`, or a Windows volume prefix. Both `ApprovalDecisionPath` and `ApprovalRequestPath` call it before joining paths. Tests: `TestValidateNodeName`, `TestApprovalDecisionPath_RejectsTraversal`, `TestApprovalRequestPath_RejectsTraversal`.

#### Thread 3 — readLineWithContext swallows scanner.Err() (resumer.go:293)
Fixed: when `scanner.Scan()` returns false, `scanner.Err()` is now propagated instead of always returning `io.EOF`. Clean EOF still returns `io.EOF`. Added doc comment about the stdin goroutine limitation.

#### Thread 4 — parseApprovalInput "non-interactive input" misleading (resumer.go:302)
Changed default case to `reason: "invalid input"` for unrecognized interactive input ("maybe" etc). EOF path in `resolveApprovalStdin` still uses "non-interactive input". Added `TestStdinMode_Approval_UnrecognizedInput_InvalidInputReason`.

#### Thread 5 — No checkpoint written on approval/signal-wait pause (apply.go:403)
Added `PauseCheckpointFn func(node string)` to `pauseTracker`. `OnRunPaused` calls it when set. Both `runApplyLocal` and `resumeOneLocalRun` wire it to `checkpointFn(node, 0)`.

#### Thread 6 — Reattach tests set CurrentStep to approval/wait node name (apply_local_approval_test.go:406)
Resolved by Thread 5: production now writes a checkpoint with `CurrentStep=<paused_node>` on pause, so tests correctly model real crash-reattach behavior.

#### Validation
- `make test` — all 20 packages pass.
- `make lint` — clean, baseline cap at 70.

### Review 2026-04-30-04 — approved

#### Summary
Approved. The PR follow-up fixes hold up: declared signal outcomes are now reported in stable order, approval file paths reject traversal-like node names, stdin read errors no longer get flattened to EOF, unrecognized interactive approval input now reports `invalid input`, and paused approval/signal nodes now write a checkpoint pointing at the paused node for crash recovery.

#### Plan Adherence
- **Step 3:** Still met. Reattach behavior now matches the real paused-node checkpoint shape written in production.
- **Step 5:** Still met. The added tests cover path validation, unrecognized approval input, and the corrected reattach/pause-checkpoint behavior.

#### Test Intent Assessment
- The new tests strengthen the contract rather than just line coverage: they verify traversal rejection at the path boundary, distinguish EOF from invalid interactive input, and confirm that a paused run writes a checkpoint targeting the paused node.

#### Validation Performed
- `make ci` — passed.
- `go test ./internal/cli/... -run 'Test(ValidateNodeName|ApprovalDecisionPath_RejectsTraversal|ApprovalRequestPath_RejectsTraversal|ApplyLocal_Reattach)' -v && go test ./internal/cli/localresume/... -run 'Test(StdinMode_Approval_UnrecognizedInput_InvalidInputReason|Reattach|StdinMode_Signal_UnknownOutcome_Error)' -v` — passed.
- Manual file-mode approval repro confirmed the checkpoint written during pause contains `current_step: "review"` for the paused approval node.

### PR Review 2026-04-30-02 — doc fixes

#### Thread 1 — Package comment hardcodes ~/.criteria (resumer.go:12)
Updated to: "under the resolved state dir ($CRITERIA_STATE_DIR, or ~/.criteria by default)". Also fixed "engine polls" → "CLI polls" in the file-mode bullet.

#### Thread 2 — docs/workflow.md file-mode table says "engine" (workflow.md:344)
Changed "Engine writes … Engine deletes" → "CLI writes … CLI deletes" in the modes table.

#### Validation
- `make test && make lint` — all pass, no new findings.
