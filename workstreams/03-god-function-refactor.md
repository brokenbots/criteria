# Workstream 3 — God-function refactor

**Owner:** Workstream executor · **Depends on:** [W01](01-flaky-test-fix.md), [W02](02-golangci-lint-adoption.md) · **Unblocks:** [W08](08-for-each-multistep.md) (which lands on top of the refactored `runLoop`).

## Context

The Phase 0 tech evaluation flagged four functions exceeding the
50-line target — collectively the largest contributors to the
`gocyclo`/`funlen`/`gocognit` baseline that [W02](02-golangci-lint-adoption.md)
quarantines. Each has 6+ levels of conditional nesting, mixes
unrelated concerns, and is not testable in isolation:

| Function | File | Lines | Tech-eval estimate |
|---|---|---|---|
| `resumeOneRun` | [internal/cli/reattach.go:40](../internal/cli/reattach.go) | 194 | gocyclo > 20 |
| `Execute` (copilotPlugin) | [cmd/criteria-adapter-copilot/copilot.go:186](../cmd/criteria-adapter-copilot/copilot.go) | 154 | gocyclo > 18 |
| `runLoop` (Engine) | [internal/engine/engine.go:144](../internal/engine/engine.go) | 113 | gocyclo > 15 |
| `runApplyServer` | [internal/cli/apply.go:150](../internal/cli/apply.go) | 106 | gocyclo > 12 |

This workstream is **pure refactor**. No behavior change, no new
features, no new tests for new behavior. Lock-in is the existing
test suite plus the deterministic `make test` from
[W01](01-flaky-test-fix.md). Each refactor is judged by:

- All extracted functions ≤ 50 lines (the [W02](02-golangci-lint-adoption.md)
  `funlen` threshold) and ≤ 15 cyclomatic / 20 cognitive
  complexity.
- The matching entries in `.golangci.baseline.yml` are deleted in
  the same diff that performs the extraction.
- `make test`, `make ci`, `make lint-go` green.
- `git diff` on the touched files shows logical extraction, not
  reshuffled lines: each helper has a single job, takes a
  named-typed parameter set (no opaque `any`), and returns a
  named-typed result.

The four refactors are listed below in **dependency order**. Land
them as separate commits within this workstream so a regression
bisects to the correct extraction.

## Prerequisites

- [W01](01-flaky-test-fix.md) and [W02](02-golangci-lint-adoption.md)
  merged. `make test` is deterministic; `.golangci.baseline.yml`
  exists and `make lint-go` is green.
- `make ci` green on `main`.

## In scope

### Step 1 — Refactor `resumeOneRun` ([internal/cli/reattach.go:40](../internal/cli/reattach.go))

The 194-line function is the highest-value extraction. Target
shape (function names are mandatory; bodies illustrative):

```go
func resumeOneRun(ctx context.Context, log *slog.Logger, cp *StepCheckpoint, opts servertrans.Options) {
    log = log.With("run_id", cp.RunID, "step", cp.CurrentStep)
    rc, err := buildRecoveryClient(ctx, log, cp, opts)
    if err != nil {
        return // buildRecoveryClient logs and clears the checkpoint
    }
    defer rc.Close()

    resp, err := attemptReattach(ctx, log, rc, cp)
    if err != nil || resp == nil {
        return
    }

    graph, err := loadCheckpointWorkflow(log, cp)
    if err != nil {
        return
    }

    if resp.Status == "paused" {
        resumePausedRun(ctx, log, rc, cp, graph, resp)
        return
    }
    resumeActiveRun(ctx, log, rc, cp, graph, resp)
}
```

Extracted helpers (each ≤ 50 lines, single concern):

- `buildRecoveryClient(ctx, log, cp, opts) (*recoveryClient, error)` —
  credential validation + `servertrans.NewClient` + `SetCredentials`.
  Logs and removes the checkpoint on every failure path so the
  caller can `return` cleanly.
- `attemptReattach(ctx, log, rc, cp) (*ReattachResponse, error)` —
  the `ReattachRun` RPC + the `CanResume` short-circuit.
- `loadCheckpointWorkflow(log, cp) (*workflow.Graph, error)` —
  `parseWorkflowFromPath` wrapper that handles the
  abandon-checkpoint-on-failure case.
- `resumePausedRun(ctx, log, rc, cp, graph, resp)` — the
  `WithPendingSignal` re-entry path for `paused` status.
- `resumeActiveRun(ctx, log, rc, cp, graph, resp)` — the normal
  resume path.
- `recoveryClient` is a small wrapper (or a type alias of the
  existing client type) that bundles credentials + a `Close`. If
  the existing client type already has the right shape, alias it
  and skip introducing a new type.

The "log and remove checkpoint" pattern repeats; encapsulate in
`abandonCheckpoint(log, cp, reason string, err error)` that logs
at the appropriate level and calls `RemoveStepCheckpoint`.

### Step 2 — Refactor `copilotPlugin.Execute` ([cmd/criteria-adapter-copilot/copilot.go:186](../cmd/criteria-adapter-copilot/copilot.go))

The 154-line `Execute` mixes session-state setup, event-handler
registration, model selection, and the main wait loop. Target
shape:

```go
func (p *copilotPlugin) Execute(ctx context.Context, req *pb.ExecuteRequest, sink pluginhost.ExecuteEventSender) error {
    s, prompt, maxTurns, err := p.prepareExecute(req)
    if err != nil {
        return err
    }

    s.execMu.Lock()
    defer s.execMu.Unlock()

    cleanup := s.beginExecution(sink)
    defer cleanup()

    state := newTurnState(maxTurns)
    unsubscribe := s.session.On(state.handleEvent(sink))
    defer unsubscribe()

    if err := applyRequestModel(ctx, s.session, req.GetConfig()); err != nil {
        return err
    }

    if _, err := s.session.Send(ctx, copilot.MessageOptions{Prompt: prompt}); err != nil {
        return fmt.Errorf("copilot: send prompt: %w", err)
    }

    return state.awaitOutcome(ctx, sink)
}
```

Extracted helpers:

- `(p *copilotPlugin).prepareExecute(req) (*sessionState, string, int, error)` —
  session lookup, prompt extraction, `max_turns` parsing.
- `(s *sessionState).beginExecution(sink) (cleanup func())` — the
  active/activeCh/sink bookkeeping that currently lives in the body
  with manual `defer`.
- `turnState` (new struct) holds `finalContent`, `assistantTurns`,
  `turnDone`, `errCh`, `maxTurns`. Methods: `handleEvent(sink)
  func(copilot.SessionEvent)` (the current 60-line switch),
  `awaitOutcome(ctx, sink) error` (the current `for { select }`
  block).
- `applyRequestModel(ctx, session, cfg map[string]string) error` —
  the per-request `SetModel` path (currently lines 305–313). This
  helper is also reused by [W09](09-copilot-agent-defaults.md) when
  fixing the `reasoning_effort`-without-`model` drop.

The `handleEvent` switch is the largest single block; if it still
exceeds 50 lines after extraction, split per-event-type handlers
(`handleAssistantMessage`, `handleToolRequest`, `handleSessionIdle`)
on `turnState`.

### Step 3 — Refactor `Engine.runLoop` ([internal/engine/engine.go:144](../internal/engine/engine.go))

The 113-line `runLoop` mixes vars seeding, state construction, the
node-eval loop, the `_continue` interception for `for_each`, and
pause handling. Target shape:

```go
func (e *Engine) runLoop(ctx context.Context, sessions *plugin.SessionManager, current string, firstStepAttempt int) error {
    vars := e.seedRunVars()
    st := &RunState{
        Current:          current,
        Vars:             vars,
        PendingSignal:    e.pendingSignal,
        ResumePayload:    e.resumePayload,
        Iter:             e.resumedIter,
        firstStep:        true,
        firstStepAttempt: firstStepAttempt,
    }
    deps := e.buildDeps(sessions)

    for {
        node, err := nodeFor(e.graph, st.Current)
        if err != nil {
            e.sink.OnRunFailed(err.Error(), st.Current)
            return err
        }
        next, err := node.Evaluate(ctx, st, deps)
        if err != nil {
            return e.handleEvalError(st, err)
        }
        next = e.interceptForEachContinue(st, next)
        if done, err := e.advanceOrTerminate(st, next); done {
            return err
        }
    }
}
```

Extracted helpers (all on `*Engine`):

- `seedRunVars() map[string]cty.Value` — the
  `SeedVarsFromGraph`/`resumedVars`/`varOverrides` block plus the
  `OnVariableSet` emission for fresh runs.
- `buildDeps(sessions) Deps` — trivial, but isolates the `Deps`
  construction from the loop body.
- `interceptForEachContinue(st, next) string` — the `_continue`
  interception logic. **Important:** [W08](08-for-each-multistep.md)
  changes the semantics of this helper, so keep its signature
  narrow and the body well-named so W08 has an isolated edit.
- `advanceOrTerminate(st, next) (done bool, err error)` — the
  terminal-state check + `st.Current = next` + pause/resume
  bookkeeping currently woven through the loop.
- `handleEvalError(st, err) error` — the `ErrPaused` handling
  plus generic error propagation.

Preserve every existing event emission (`OnVariableSet`, `OnRunFailed`,
etc.) byte-for-byte: the event stream is contract-visible to the
SDK and a regression here breaks downstream consumers.

### Step 4 — Refactor `runApplyServer` ([internal/cli/apply.go:150](../internal/cli/apply.go))

The 106-line function bundles compile, client setup, sink
construction, run start, and a checkpoint-write closure. Target
shape:

```go
func runApplyServer(ctx context.Context, opts applyOptions) error {
    runCtx, cancelRun := context.WithCancel(ctx)
    defer cancelRun()

    log := newApplyLogger()
    src, graph, loader, err := compileForExecution(runCtx, opts.workflowPath, log)
    if err != nil {
        return err
    }
    defer loader.Shutdown(context.Background())

    client, runID, err := setupServerRun(runCtx, log, graph, src, opts.serverURL, opts.name, applyClientOptions(opts), cancelRun)
    if err != nil {
        return err
    }
    defer client.Close()

    sink := buildServerSink(client, runID, graph, opts.workflowPath, opts.serverURL, log)
    state := newLocalRunState(runID, graph, opts.workflowPath, opts.serverURL, client)

    return executeServerRun(runCtx, log, loader, sink, state, graph, opts)
}
```

Extracted helpers:

- `applyClientOptions(opts) servertrans.Options` — the seven-field
  `clientOpts` struct construction.
- `buildServerSink(client, runID, graph, path, serverURL, log) *run.Sink` —
  including the `CheckpointFn` closure (which itself becomes a
  small named function `writeRunCheckpoint(...)` that the closure
  delegates to).
- `newLocalRunState(...)` — the `localRunState` struct construction.
- `executeServerRun(ctx, log, loader, sink, state, graph, opts) error` —
  the actual run execution loop currently inlined after sink
  construction.

`newApplyLogger` is trivial but isolates the logger configuration
so test code can swap it.

### Step 5 — Burn down baseline entries

For each of the four refactors, in the same commit:

- Delete the corresponding `funlen`/`gocyclo`/`gocognit` entries in
  `.golangci.baseline.yml`.
- Run `make lint-go`; it must exit 0 without those entries.
- If `make lint-go` reports a finding on the new helper, fix the
  helper in the same commit (do not re-add a baseline entry).

Reviewer rejects the workstream if `.golangci.baseline.yml` retains
any of the four function-level entries pointed at W03.

## Out of scope

- Changing observable behavior of any of the four functions.
  Identical event streams, identical error messages, identical
  exit codes.
- Adding new tests for new behavior. The existing tests (post-W01)
  are the lock-in. If a refactor genuinely cannot be locked in by
  existing tests, that is a coverage gap and goes to
  [W06](06-coverage-bench-godoc.md), not this workstream.
- Changing the public SDK contract or the proto wire format.
- Splitting files. File splits are [W04](04-split-oversized-files.md);
  this workstream stays within the existing files.
- Fixing the `reasoning_effort`-without-`model` bug in
  `applyRequestModel`. That is [W09](09-copilot-agent-defaults.md);
  this workstream extracts the helper unchanged.

## Files this workstream may modify

- `internal/cli/reattach.go`
- `internal/cli/reattach_test.go` (only if existing tests need
  updates to compile against extracted helpers)
- `internal/cli/apply.go`
- `internal/cli/apply_test.go` (same caveat)
- `internal/engine/engine.go`
- `internal/engine/engine_test.go` (same caveat)
- `cmd/criteria-adapter-copilot/copilot.go`
- `cmd/criteria-adapter-copilot/copilot_internal_test.go` (same caveat)
- `.golangci.baseline.yml` (delete W03-pointed entries only)

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream
file. It may **not** edit unrelated source files. If a refactor
exposes a bug in adjacent code, file an `[ARCH-REVIEW]` note in
this workstream's reviewer log rather than fixing the adjacent
file.

## Tasks

- [ ] Refactor `resumeOneRun` per Step 1; commit independently.
- [ ] Refactor `copilotPlugin.Execute` per Step 2; commit
      independently.
- [ ] Refactor `Engine.runLoop` per Step 3; commit independently.
- [ ] Refactor `runApplyServer` per Step 4; commit independently.
- [ ] Delete the matching `.golangci.baseline.yml` entries in each
      commit.
- [ ] `make ci` green on the final commit.
- [ ] `go test -race -count=10 ./...` green across all three
      modules (catches refactor-induced races).
- [ ] CLI smoke: `./bin/criteria apply examples/hello.hcl` exits 0.

## Exit criteria

- All four named functions are ≤ 50 lines and ≤ 15 cyclomatic /
  20 cognitive complexity.
- `make lint-go` exits 0 with the four function-level baseline
  entries deleted.
- `make ci` green; `go test -race -count=10 ./...` green.
- The Copilot adapter conformance suite
  (`make test-conformance` and `cmd/criteria-adapter-copilot/conformance_test.go`)
  passes — proves the `Execute` refactor preserved the contract.
- The example workflows under `examples/` continue to validate
  (`make validate`).
- No new functions added by this workstream exceed the funlen /
  gocyclo / gocognit thresholds.
- `git log --stat` shows four extraction commits, each with a
  clear, narrowly-scoped diff.

## Tests

This workstream **adds no new tests**. Lock-in:

- The existing engine, plugin, and CLI test packages.
- The Copilot adapter internal test
  ([cmd/criteria-adapter-copilot/copilot_internal_test.go](../cmd/criteria-adapter-copilot/copilot_internal_test.go))
  and conformance test
  ([cmd/criteria-adapter-copilot/conformance_test.go](../cmd/criteria-adapter-copilot/conformance_test.go)).
- `make validate` against the full `examples/` corpus.
- The CLI smoke target.

If lock-in is insufficient for a specific refactor, do **not**
write a new behavior test in this workstream — escalate to
[W06](06-coverage-bench-godoc.md) and pause that refactor until
W06 lands the missing coverage.

## Risks

| Risk | Mitigation |
|---|---|
| Refactor changes observable behavior in a way the test suite doesn't catch | Run the example workflows end-to-end before declaring done; cross-check the ND-JSON event stream from a sample run pre- and post-refactor with `diff` — they should match modulo timestamps. Document the comparison in reviewer notes. |
| Extracted helpers leak into other packages and become a public API by accident | Helpers stay unexported (`lowerCamelCase`) and live in the same package as the original function. No new exports. |
| `runLoop` extraction collides with W08's planned `for_each` semantics change | Step 3 explicitly preserves `interceptForEachContinue` as a single, narrowly-named helper so W08 has an isolated edit point. W08's reviewer notes must reference this helper by name. |
| Copilot `Execute` refactor introduces a new race condition | `go test -race -count=10 ./cmd/criteria-adapter-copilot/...` is part of exit criteria. The `goleak` verification from W01 carries forward. |
| The four extractions land as one giant commit, defeating bisect | Exit criteria requires four separate commits. Reviewer rejects bundle commits. |
| A refactor exposes a real latent bug | Fix it in the same workstream **only if** the fix is mechanical (≤ 5 lines, no new behavior). Anything larger is `[ARCH-REVIEW]` material; the refactor proceeds with the bug preserved (with a comment), and the bug becomes a forward-pointer for a follow-up. |
| Refactor kicks the `gocognit` threshold up rather than down due to extracted-helper indirection | The `gocognit` threshold is 20 in `.golangci.yml`. If a helper hits it, restructure further before declaring done. Do not raise the threshold. |
