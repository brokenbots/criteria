# parallel-01 — Per-iteration session isolation for parallel subworkflow steps

**Owner:** Workstream executor · **Depends on:** none · **Coordinates with:** parallel-02 (independent), parallel-04 (independent)

## Context

`parallel = [...]` on a subworkflow step fans out goroutines via
`runParallelIterations`. Each goroutine calls
`runParallelSubworkflowIteration` → `runSubworkflow` → `runWorkflowBody`
→ `initScopeAdapters(ctx, body, deps)`. The `deps` passed to every goroutine
is the **same struct**, and `deps.Sessions` is the **same parent
`*plugin.SessionManager`**.

`initScopeAdapters` calls `deps.Sessions.Open(ctx, instanceID, ...)` for each
adapter declared in the subworkflow scope. When goroutine 0 opens session
`"copilot.default"` first, goroutines 1…N−1 hit the early-exit guard in
`sessions.go`:

```go
if _, exists := m.sessions[name]; exists {
    m.mu.Unlock()
    return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
}
```

The `ErrSessionAlreadyOpen` error is deliberately swallowed in
`lifecycle.go:initScopeAdapters` to support sequential subworkflows that
re-declare a parent-scope adapter. As a result, goroutines 1…N−1 silently
reuse the session opened by goroutine 0. All concurrent `Execute` calls on that
session serialize behind the adapter's internal mutex (e.g. Copilot's
`s.execMu.Lock()`), producing wall-clock time ≈ N × single-execution time —
no actual concurrency.

**Fix:** give each goroutine its own fresh `*plugin.SessionManager` created
from a shared `Loader`. Sessions are scoped, isolated, and torn down by
`runWorkflowBody`'s existing `defer tearDownScopeAdapters`. The `Loader` is
already on the `Engine` struct (`e.loader plugin.Loader`) but is not present
in `Deps`; it must be added so that `runParallelSubworkflowIteration` can call
`plugin.NewSessionManager(deps.Loader)`.

## Prerequisites

- `make test` passes on `main` (baseline green).

## In scope

### Step 1 — Add `Loader` to the `Deps` struct

**File:** `internal/engine/node.go`

Add the `Loader` field to `Deps` after `Sessions`:

```go
// Deps carries interpreter runtime dependencies shared by node implementations.
type Deps struct {
    Sessions            *plugin.SessionManager
    Loader              plugin.Loader          // ← add
    Sink                Sink
    SubWorkflowResolver SubWorkflowResolver
    BranchScheduler     BranchScheduler
}
```

The import for `"github.com/brokenbots/criteria/internal/plugin"` is already
present in this file.

---

### Step 2 — Wire `Loader` into `buildDeps`

**File:** `internal/engine/engine.go`

In `buildDeps` (line ~434), add `Loader: e.loader`:

```go
func (e *Engine) buildDeps(sessions *plugin.SessionManager) Deps {
    return Deps{
        Sessions:            sessions,
        Loader:              e.loader,  // ← add
        Sink:                e.sink,
        SubWorkflowResolver: e.subWorkflowResolver,
        BranchScheduler:     e.branchScheduler,
    }
}
```

---

### Step 3 — Create a per-iteration `SessionManager` for subworkflow iterations

**File:** `internal/engine/parallel_iteration.go`

Replace the body of `runParallelSubworkflowIteration` (currently passes
`deps` unchanged to `runSubworkflow`) with an isolated `iterDeps`:

```go
func (n *stepNode) runParallelSubworkflowIteration(ctx context.Context, st *RunState, deps Deps) (outcome string, outputs map[string]string, err error) {
    swNode, ok := n.graph.Subworkflows[n.step.SubworkflowRef]
    if !ok {
        return "", nil, fmt.Errorf("step %q: subworkflow %q not found", n.step.Name, n.step.SubworkflowRef)
    }

    var stepInput map[string]cty.Value
    if len(n.step.InputExprs) > 0 {
        evalOpts := workflow.DefaultFunctionOptions(st.WorkflowDir)
        stepInput, err = workflow.ResolveInputExprsAsCty(n.step.InputExprs, st.Vars, evalOpts)
        if err != nil {
            return "", nil, fmt.Errorf("step %q: input expression error: %w", n.step.Name, err)
        }
    }

    // Per-iteration session isolation: each parallel goroutine receives its own
    // SessionManager so that initScopeAdapters inside runWorkflowBody opens
    // fresh adapter sessions rather than colliding on the parent scope's sessions.
    // runWorkflowBody's deferred tearDownScopeAdapters closes and kills all
    // sessions it opened, so no explicit Shutdown is needed here.
    iterDeps := deps
    iterDeps.Sessions = plugin.NewSessionManager(deps.Loader)

    swOutputs, runErr := runSubworkflow(ctx, swNode, st, stepInput, iterDeps)
    if runErr != nil {
        return "failure", nil, runErr
    }

    stringOutputs, renderErr := ctyOutputsToStrings(n.step.Name, swOutputs)
    if renderErr != nil {
        return "", nil, renderErr
    }
    return "success", stringOutputs, nil
}
```

The `plugin` package import is already present in `parallel_iteration.go`.

Key invariants:
- `iterDeps.Sink` still points to the `lockedSink` wrapper from
  `evaluateParallel`, so log serialization is preserved.
- `iterDeps.Loader` is the shared parent loader — plugin process lifecycle
  is already managed per-`Kill()` call inside `SessionManager.Close`.
- `tearDownScopeAdapters` (deferred inside `runWorkflowBody`) closes every
  session opened by `initScopeAdapters` using `iterDeps.Sessions` — the
  per-iteration manager — so sessions are cleaned up before the goroutine exits.
- The parent `deps.Sessions` is never modified.

---

### Step 4 — Tests

**File:** `internal/engine/parallel_iteration_test.go` (new or existing)

Add a test that exercises a parallel subworkflow step where the subworkflow
declares an adapter with a per-session mutex (simulating a stateful adapter):

```
TestParallelSubworkflow_IsolatedSessions_ConcurrentExecution
```

Acceptance criteria for this test:
1. N parallel iterations (N ≥ 3) of a subworkflow that each runs one adapter
   step complete in **≤ 2 × single-execution wall time** (not N×).
2. Each iteration receives a distinct adapter session (verifiable by counting
   `OpenSession` calls on a test adapter — should be N, not 1).
3. The test passes under `-race`.

Use a test adapter that records call counts in an atomic counter and introduces
a brief sleep in `Execute` to make serialization detectable via elapsed time.

Also update any existing parallel iteration tests in the file that construct
`Deps{}` without a `Loader` field — those tests will fail to compile after
Step 1. Pass `nil` for `Loader` where the test only exercises the adapter
path (adapter sessions are already open, no `NewSessionManager` needed).

---

## Behavior change

**Yes.** Parallel subworkflow iterations that declare adapters will now open
and close their own adapter sessions per-iteration rather than silently sharing
the parent session. Each adapter receives N separate `OpenSession` /
`Execute` / `CloseSession` triples instead of 1 `OpenSession` + N `Execute`
calls on the same session.

Workflows that relied (accidentally) on the shared session being preserved
across iterations will behave differently. In practice this was never
intentional — the W19 design assumed isolation.

## Reuse

- `plugin.NewSessionManager(loader)` — already exists in `internal/plugin/sessions.go`.
- The `iterDeps := deps; iterDeps.X = Y` copy pattern already appears in the
  engine for other `Deps` overrides.
- `tearDownScopeAdapters` already handles full session lifecycle — no new
  teardown code needed.

## Out of scope

- Adapter-step parallel correctness — that is parallel-02.
- Sink fan-in throughput optimisation — that is parallel-03.
- Shared variable write semantics documentation — that is parallel-04.
- Any changes to `initScopeAdapters` or the `ErrSessionAlreadyOpen` swallow
  logic — that swallow is still correct for sequential subworkflow re-declaration.
- Plugin lifecycle changes (loader Shutdown semantics, process pooling).

## Files this workstream may modify

- `internal/engine/node.go`
- `internal/engine/engine.go`
- `internal/engine/parallel_iteration.go`
- `internal/engine/parallel_iteration_test.go` (or whichever file holds
  the engine parallel tests)

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, `sdk/CHANGELOG.md`,
or any other workstream file.

## Tasks

- [ ] Add `Loader plugin.Loader` field to `Deps` in `internal/engine/node.go`
- [ ] Wire `Loader: e.loader` into `buildDeps` in `internal/engine/engine.go`
- [ ] Replace body of `runParallelSubworkflowIteration` to use per-iteration `SessionManager`
- [ ] Fix any compilation failures in existing engine tests that construct `Deps{}` directly
- [ ] Write `TestParallelSubworkflow_IsolatedSessions_ConcurrentExecution` test
- [ ] Run `go test -race -count=5 ./internal/engine/...` and confirm pass
- [ ] Run `make test` and confirm full suite green

## Exit criteria

- `go test -race -count=5 ./internal/engine/...` passes with no race conditions.
- `TestParallelSubworkflow_IsolatedSessions_ConcurrentExecution`: N=3 iterations
  complete in ≤ 2× single-iteration wall time; `OpenSession` call count = 3.
- `make test` passes.
- No changes outside the files listed above.
