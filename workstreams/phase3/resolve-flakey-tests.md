# Workstream — Resolve Flakey Tests

**Owner:** Workstream executor 

## Purpose

Eliminate non-deterministic test failures without changing any observable behavior, public API contracts, or SDK/proto wire shapes. Both test code and production code are in scope — production code may be changed **only** when a test is flaky because the production code itself has a race or timing assumption baked in.

---

## Before you start — read the branch notes

Before touching any code, locate and read `flakey-test-worklog.md` at the repo root on the **current branch**. This file is the handoff log for this workstream. It records which packages have already been investigated, which fixes were attempted, and which are still open. If the file does not yet exist, create it with the header shown in the Worklog section below and record your starting state.  

Also read the current workstream files in workstreams/ the last active file will generally have some details on what flakey tests they may have rut into.

Do **not** write notes into this file (`resolve-flakey-tests.md`). All investigation notes, run logs, and per-fix decisions go in `flakey-test-worklog.md` on the working branch.

---

## Constraints

- **No behavior changes.** Every fix must be behaviorally equivalent. If a production-code change is needed, it must be observable-output-neutral and must not alter any public function signature, interface, proto field, or SDK type.
- **No API contract changes.** The `sdk/` module's exported surface and the Connect/proto wire format are frozen for this workstream.
- **No new test skips.** Do not use `t.Skip` as a fix. A skipped test is not a resolved test.
- **No relaxed assertions.** Do not widen expected values or remove assertion calls to make a test pass.
- **Stay on the current branch.** Continue from wherever the PR branch is. Do not open a new branch.
- **Merge the PR when done.** After all identified flakey tests are confirmed stable (see the stability gate below), merge the open PR.

---

## How to find flakey tests

Work through each method in order. Record findings in `flakey-test-worklog.md` as you go.

### 1. Run the existing flake-watch target

```sh
make test-flake-watch
```

This reruns the historically-flaky packages `./internal/engine/...` and `./internal/plugin/...` with `-count=20 -race`. Any non-deterministic failure will surface here within a few runs. If it fails, that package is the first to fix.

### 2. Broad high-repetition race run across all modules

```sh
go test -race -count=10 -timeout=300s ./...
cd sdk      && go test -race -count=10 -timeout=120s ./...
cd workflow && go test -race -count=10 -timeout=120s ./...
```

This is slower but catches races in packages not covered by `test-flake-watch`. Run this before and after every fix to confirm the fix holds.

### 3. Identify `time.Sleep`-dependent tests

```sh
grep -rn "time\.Sleep" --include="*_test.go" .
```

Every `time.Sleep` in a test is a candidate for flakiness on slower or heavily-loaded machines. Review each call site and decide whether it can be replaced with a channel-based synchronization or a polling helper with a deterministic timeout (see patterns below).

### 4. Identify goroutine-leak candidates

Packages that use `goleak` (`internal/engine`, `internal/plugin`, `internal/transport/server`) will fail if a goroutine is left running after a test. Check:

```sh
go test -race -v -run . ./internal/engine/... ./internal/plugin/... ./internal/transport/server/... 2>&1 | grep -E "FAIL|leak|goroutine"
```

### 5. Check the CI run on the PR

Read the most recent CI run attached to the open PR. Any test that is marked failed-but-passed-on-retry is a confirmed flakey test. Prioritize these.

---

## Common patterns and canonical fixes

Apply the simplest fix that resolves the non-determinism. Choose from the patterns below; do not invent new synchronization idioms.

### Pattern A — Replace `time.Sleep` with channel or condition synchronization

When a test sleeps to wait for a goroutine to reach a certain state, replace the sleep with a channel that the code closes or sends on when it reaches that state. If the production code does not already expose such a signal, add an unexported test-hook field (e.g., `readyCh chan struct{}`) and set it only when a test wires it in via a constructor option or by direct struct assignment in an `_test.go` file.

```go
// Before (flakey)
go startWorker()
time.Sleep(30 * time.Millisecond)
doAssert()

// After
readyCh := make(chan struct{})
go startWorker(readyCh)
<-readyCh
doAssert()
```

### Pattern B — Replace `time.Sleep` with `require.Eventually` / polling loop

When the sleep is waiting for a side-effect to propagate (e.g., a metric counter to tick, a file to appear), replace it with a tight poll with a hard deadline:

```go
require.Eventually(t, func() bool {
    return checkCondition()
}, 2*time.Second, 5*time.Millisecond)
```

Use a generous wall-clock deadline (at least 2 s) with a tight poll interval (≤ 10 ms). This keeps tests fast on fast machines and reliable on slow ones.

### Pattern C — Goroutine leak — ensure shutdown is called in cleanup

If `goleak` reports a leaked goroutine, trace it back to the goroutine's start site. In almost all cases the fix is to register a `t.Cleanup` that calls the appropriate shutdown method:

```go
svc := newService(...)
t.Cleanup(func() { svc.Shutdown(context.Background()) })
```

Ensure `Shutdown` (or `Close`) drains internal goroutines before returning. If it does not, that is a production-code bug eligible for a non-behavior-changing fix (e.g., add a `sync.WaitGroup` drain at the end of `Shutdown`).

### Pattern D — Race on shared state — narrow the critical section

If `-race` reports a data race, the fix must be a proper synchronization primitive (mutex, atomic, channel). Do not widen a lock to cover code that does not need protection — narrow the critical section to exactly the shared variable. Do not add `//nolint:race` or `//nolint:datarace` suppressions.

### Pattern E — Test ordering dependency — use `t.Parallel()` and clean up globals

If a test relies on global state set by another test, either isolate the global behind a constructor that tests can override, or add `t.Parallel()` and ensure each test gets its own instance. Never rely on test execution order.

---

## Stability gate (required before merging)

A flakey test is **resolved** when all of the following are true:

1. `make test-flake-watch` exits 0 three consecutive times in a clean shell.
2. `go test -race -count=20 ./...` (all modules) exits 0 at least once.
3. `make ci` exits 0 on the branch.

Record the output of these three runs in `flakey-test-worklog.md` before requesting or performing a merge.

---

## Branch and PR workflow

1. Continue commits on the existing PR branch. Do not squash in-progress commits until the stability gate is met.
2. Commit message format: `fix(tests): <short description of what was non-deterministic>` — keep it one line.
3. Once the stability gate is met, merge the PR (do not squash across packages — keep logical commits together).
4. After merge, verify `make ci` is green on `main`.

---

## Worklog file

All notes go in `flakey-test-worklog.md` at the repo root on the working branch. The file is not committed to `main` as a permanent artifact — it lives on the branch for reviewer context and is merged with it. Use this header structure:

```markdown
# Flakey test worklog

## Status: <in-progress | stability-gate-met | merged>

## Packages investigated
| Package | Method used | Finding | Fix applied | Stable? |
|---------|-------------|---------|-------------|---------|

## Run log
### <date> — <command run>
<paste trimmed output here>

## Notes
<free-form observations, dead ends, open questions>
```

Update the `Status` field and the table as you work. Each row in the table corresponds to one package. The `Stable?` column must say `yes (count=20)` before the package is considered done.

## Reviewer Notes

### Review 2026-05-02 — changes-requested

#### Summary
The flake fixes themselves are directionally correct: caching the plugin test binaries removes the repeated `go build` contention that triggered `TestHandshakeInfo`, and the server-client tests replaced fixed sleeps with synchronization that is less scheduler-sensitive. Repo validation is green on my pass. I am still requesting changes because the workstream's required stability gate is not yet evidenced in `flakey-test-worklog.md`: the recorded all-module race run is `-count=10`, not the required `-count=20`, and the log does not show the separate `sdk/` and `workflow/` module runs required by this repository's multi-module layout. No security issues were identified in the code changes reviewed here.

#### Plan Adherence
- `make test-flake-watch`: worklog records three consecutive clean runs, and I reproduced one passing run locally.
- Broad high-repetition race runs: partially met. The worklog records a root-module `go test -race -count=10 -timeout=300s ./...` pass, but the workstream's stability gate requires a `-count=20` all-module race pass and recorded output. In this repository, `sdk/` and `workflow/` are separate modules, so they need their own recorded runs unless an equivalent command demonstrably covered them.
- `time.Sleep` review and goroutine-leak hardening: implemented in `internal/transport/server/client_test.go`, with stronger synchronization than before.
- Worklog maintenance: mostly good, but `Status: stability-gate-met` is premature until the exact gate evidence is recorded.

#### Required Remediations
- **blocker** — `flakey-test-worklog.md:3`, `flakey-test-worklog.md:51-63`, `workstreams/phase3/resolve-flakey-tests.md:134-138`: the recorded stability evidence does not satisfy the stated acceptance bar. The log shows `go test -race -count=10 -timeout=300s ./...`, while the workstream requires a `-count=20` all-module race pass and recorded output before merge. Because this repo uses separate root, `sdk/`, and `workflow/` modules, the current log also does not prove the non-root modules were exercised at the required threshold. **Acceptance:** rerun the exact stability-gate race coverage at the required threshold, record the command output in `flakey-test-worklog.md`, and do not claim `stability-gate-met` until that evidence is present. If you use per-module commands, include root, `sdk/`, and `workflow/` explicitly.
- **nit** — `internal/plugin/handshake_test.go:22-24`, `flakey-test-worklog.md:67`, evidence `internal/plugin/loader.go:128`: the comment/note says the 30 s timeout "matches production loader.go", but production `loader.go` uses 5 s. The larger test timeout may still be reasonable, but the rationale is currently inaccurate. **Acceptance:** update the code comment and worklog note so they accurately describe the relationship to production and why the larger test-only timeout is intentional.

#### Test Intent Assessment
The strongest coverage in this change is around the actual flake mechanisms: moving the noop plugin build into `TestMain` and caching the public SDK fixture behind `sync.Once` directly removes the parallel-build contention that made startup timing nondeterministic, and the heartbeat/drain tests now assert quiescence instead of relying on fixed sleeps. What is still missing is test intent at the workstream level: the submitted evidence does not yet prove the required `-count=20` all-module stability gate, so the workstream cannot be accepted even though the focused tests and my validation runs are green.

#### Validation Performed
- `make test-flake-watch` — passed (`internal/engine/...` and `internal/plugin/...` at `-count=20 -race`).
- `go test -race -count=1 -timeout=300s ./... && (cd sdk && go test -race -count=1 -timeout=120s ./...) && (cd workflow && go test -race -count=1 -timeout=120s ./...)` — passed.
- `make ci` — passed.
