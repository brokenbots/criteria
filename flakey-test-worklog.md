# Flakey test worklog

## Status: stability-gate-met

## Packages investigated
| Package | Method used | Finding | Fix applied | Stable? |
|---------|-------------|---------|-------------|---------|
| `internal/plugin` | `go test -race -count=3 ./...` | `TestHandshakeInfo`: `buildNoopPlugin(t)` compiled binary per-test via `t.TempDir()`; under `-race -count=3` parallel packages, N concurrent builds + race overhead caused plugin process to miss the 2s `StartTimeout` | Moved build to `TestMain` (package-level `testNoopPluginBin`); raised `StartTimeout` 2s→30s; same caching applied to `buildPublicSDKFixture` via `sync.Once` | yes (count=20 via test-flake-watch; count=10 full suite) |
| `internal/transport/server` | `grep time.Sleep` + code review | `TestClientHeartbeat`: fixed `time.Sleep(50ms)` after `cancel()` — goroutine may not have stopped yet on loaded hosts; `TestClientDrain/ctx_cancel_unblocks_drain`: unnecessary `time.Sleep(25ms)` before `cancel()` | Replaced fixed sleep with `waitForCond` polling loop (Pattern B); removed unnecessary pre-cancel sleep | yes (count=10) |
| `internal/engine` | `make test-flake-watch` (count=20 ×3) | No failures | none needed | yes (count=20 ×3) |
| `internal/cli` | `go test -race -count=10 ./...` | `time.Sleep` calls are all inside polling loops with hard deadlines — not racy | none needed | yes (count=10) |
| `internal/cli/localresume` | `go test -race -count=10 ./...` | Sleeps are in goroutines simulating delayed file writes; polling interval (20ms) and sleep (50-80ms) are well-separated | none needed | yes (count=10) |

## Run log

### 2026-05-02 — make test-flake-watch (run 1, before fixes)
```
ok  github.com/brokenbots/criteria/internal/engine   91.090s
ok  github.com/brokenbots/criteria/internal/plugin  211.889s
```
PASS (count=20)

### 2026-05-02 — go test -race -count=3 -timeout=300s ./... (pre-fix, triggered flake)
```
--- FAIL: TestHandshakeInfo (2.49s)
    handshake_test.go:30: create plugin rpc client: timeout while waiting for plugin to start
FAIL  github.com/brokenbots/criteria/internal/plugin  40.069s
ok    github.com/brokenbots/criteria/internal/transport/server   20.880s
ok    github.com/brokenbots/criteria/internal/engine   22.951s
```

Root cause: `buildNoopPlugin(t)` uses `t.TempDir()` and runs `go build` inside each test call. Under `-race -count=3 ./...`, all packages run in parallel. Three simultaneous builds from the `internal/plugin` package competed for CPU alongside dozens of other test packages with race detection active. The plugin process (already built) then failed to advertise its Unix socket address before `StartTimeout: 2 * time.Second` expired.

### 2026-05-02 — go test -race -count=3 ./... (post-fix)
All packages PASS.

### 2026-05-02 — make test-flake-watch (run 2, post-fix)
```
ok  github.com/brokenbots/criteria/internal/engine   101.981s
ok  github.com/brokenbots/criteria/internal/plugin   118.661s
```
PASS (count=20)

### 2026-05-02 — make test-flake-watch (run 3, stability gate)
```
ok  github.com/brokenbots/criteria/internal/engine   129.647s
ok  github.com/brokenbots/criteria/internal/plugin   134.280s
```
PASS (count=20) — third consecutive clean run ✓

### 2026-05-02 — go test -race -count=10 -timeout=300s ./... (all modules, stability gate)
```
ok  github.com/brokenbots/criteria/internal/engine          81.049s
ok  github.com/brokenbots/criteria/internal/plugin          85.862s
ok  github.com/brokenbots/criteria/internal/transport/server 59.942s
ok  github.com/brokenbots/criteria/internal/cli            262.140s
[all other packages: ok]
```
PASS (count=10 all modules) ✓

### 2026-05-02 — make ci (stability gate)
All targets pass: build, test, lint-imports, lint-go, lint-baseline-check, validate, example-plugin ✓

## Notes

- The W01 fix used `context.WithoutCancel` to decouple plugin lifecycle from step-deadline context. This flake is in the same root class (CPU pressure during parallel `./...` runs) but a different symptom: the test itself was adding build-time contention by compiling a fresh binary per test call.
- Raising `StartTimeout` from 2s to 30s brings the test in line with `loader.go`'s production value (5s) and well beyond worst-case CI load.
- `TestClientHeartbeat` and `TestClientDrain` fixes are proactive (both passed under count=10); the `waitForCond` pattern eliminates the fragility class entirely.
- `publicsdk_conformance_test.go` uses `package plugin_test` (no TestMain access), so a `sync.Once` package-level var is the correct caching idiom there.
- `internal/cli` and `internal/cli/localresume` time.Sleep calls reviewed: all are inside polling loops with multi-second hard deadlines or simulate delayed external events; none are racy.
