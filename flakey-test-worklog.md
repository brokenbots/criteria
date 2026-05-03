# Flakey test worklog

## Status: stability-gate-met

## Packages investigated
| Package | Method used | Finding | Fix applied | Stable? |
|---------|-------------|---------|-------------|---------|
| `internal/plugin` | `go test -race -count=3 ./...` | `TestHandshakeInfo`: `buildNoopPlugin(t)` compiled binary per-test via `t.TempDir()`; under `-race -count=3` parallel packages, N concurrent builds + race overhead caused plugin process to miss the 2s `StartTimeout` | Moved build to `TestMain` (package-level `testNoopPluginBin`); raised `StartTimeout` 2s→30s; same caching applied to `buildPublicSDKFixture` via `sync.Once` | yes (count=20, all modules) |
| `internal/plugin` (conformance) | `go test -race -count=20 ./...` | `TestPublicSDKFixtureConformance`: `loader.go` `StartTimeout: 5s` too tight under full `./...` `-race -count=20` load; plugin process exceeded 5s startup time; `conformance.go` also used 5s context which expired before startup completed | Raised `StartTimeout` in `loader.go` 5s→30s; raised context timeouts in `conformance.go` 5s→30s; updated `handshake_test.go` comment (loader.go now also uses 30s) | yes (count=20, all modules) |
| `internal/cli/localresume` | `go test -race -count=20 ./...` | `TestFileMode_Approval_WritesAndConsumes`: `pollForFile` failed immediately on JSON decode error when file was caught mid-write (TOCTOU race: `os.WriteFile` truncates then writes; poller read truncated empty file) | Made `pollForFile` retry on JSON decode errors (continue to next tick); track last decode error and report it if deadline fires; tightened `TestFileMode_InvalidJSON` timeout to 200ms to keep test fast | yes (count=20, all modules) |
| `internal/transport/server` | `grep time.Sleep` + code review | `TestClientHeartbeat`: fixed `time.Sleep(50ms)` after `cancel()` — goroutine may not have stopped yet on loaded hosts; `TestClientDrain/ctx_cancel_unblocks_drain`: unnecessary `time.Sleep(25ms)` before `cancel()` | Replaced fixed sleep with `waitForCond` polling loop (Pattern B); removed unnecessary pre-cancel sleep | yes (count=20, all modules) |
| `internal/engine` | `make test-flake-watch` (count=20 ×3) | No failures | none needed | yes (count=20 ×3) |
| `internal/cli` | `go test -race -count=20 ./...` | `time.Sleep` calls are all inside polling loops with hard deadlines — not racy | none needed | yes (count=20) |

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

### 2026-05-02 — go test -race -count=20 ./... (root, post reviewer fix + pollForFile fix)
```
ok  github.com/brokenbots/criteria/cmd/criteria-adapter-copilot          6.626s
ok  github.com/brokenbots/criteria/cmd/criteria-adapter-copilot/testfixtures/fake-copilot  2.261s
ok  github.com/brokenbots/criteria/cmd/criteria-adapter-mcp              5.514s
ok  github.com/brokenbots/criteria/cmd/criteria-adapter-mcp/mcpclient    2.937s
ok  github.com/brokenbots/criteria/cmd/criteria-adapter-noop            36.368s
ok  github.com/brokenbots/criteria/events                                3.216s
ok  github.com/brokenbots/criteria/internal/adapter/conformance         52.518s
ok  github.com/brokenbots/criteria/internal/adapters/shell              41.845s
ok  github.com/brokenbots/criteria/internal/cli                        479.021s
ok  github.com/brokenbots/criteria/internal/cli/localresume             20.486s
ok  github.com/brokenbots/criteria/internal/engine                     116.220s
ok  github.com/brokenbots/criteria/internal/plugin                     120.493s
ok  github.com/brokenbots/criteria/internal/run                          4.352s
ok  github.com/brokenbots/criteria/internal/transport/server           113.896s
ok  github.com/brokenbots/criteria/tools/import-lint                    52.689s
ok  github.com/brokenbots/criteria/tools/lint-baseline                   3.730s
```
PASS — all root-module packages at count=20 -race ✓

### 2026-05-02 — sdk/ and workflow/ modules (count=20 -race)
```
ok  github.com/brokenbots/criteria/sdk                  1.300s
ok  github.com/brokenbots/criteria/sdk/conformance     14.790s
ok  github.com/brokenbots/criteria/sdk/pluginhost       1.751s
ok  github.com/brokenbots/criteria/workflow             3.301s
```
PASS — all non-root modules at count=20 -race ✓

### 2026-05-02 — make ci (final stability gate)
All targets pass: build, test, lint-imports, lint-go, lint-baseline-check, validate, example-plugin ✓

## Notes

- The W01 fix used `context.WithoutCancel` to decouple plugin lifecycle from step-deadline context. This flake is in the same root class (CPU pressure during parallel `./...` runs) but a different symptom: the test itself was adding build-time contention by compiling a fresh binary per test call.
- `StartTimeout` in `loader.go` was raised from 5s to 30s. This aligns with the test-side 30s used in `handshake_test.go`. The test comment was updated to reflect that both now use 30s; the rationale is CPU pressure under `-race -count=20` parallel package load rather than matching a specific production constant.
- `TestClientHeartbeat` and `TestClientDrain` fixes are proactive (both passed under count=10); the `waitForCond` pattern eliminates the fragility class entirely.
- `publicsdk_conformance_test.go` uses `package plugin_test` (no TestMain access), so a `sync.Once` package-level var is the correct caching idiom there.
- `pollForFile` TOCTOU fix: `os.WriteFile` on POSIX is not atomic (truncate then write). A poller that reads mid-write sees an empty file and gets "unexpected end of JSON input". The fix retries on decode errors — subsequent polls pick up the complete file. The `TestFileMode_InvalidJSON` test was updated to use a shorter `FileTimeout` (200ms) so it stays fast now that decode errors trigger retry rather than immediate failure.
