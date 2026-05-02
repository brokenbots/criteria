# Flakey test worklog

## Status: in-progress

## Packages investigated
| Package | Method used | Finding | Fix applied | Stable? |
|---------|-------------|---------|-------------|---------|
| `internal/plugin` | `go test -race -count=3 ./...` | `TestHandshakeInfo` fails: plugin StartTimeout (2s) too tight under load when `buildNoopPlugin` runs N concurrent builds | Cache binary in TestMain; raise StartTimeout to 30s | pending |
| `internal/transport/server` | `grep time.Sleep + code review` | `TestClientHeartbeat`: fixed-sleep after cancel() is racy on loaded hosts; `TestClientDrain/ctx_cancel_unblocks_drain`: `time.Sleep(25ms)` before cancel is unnecessary | Replace fixed sleeps with deterministic wait loop (Pattern B) | pending |
| `internal/engine` | `make test-flake-watch` (count=20) | No failures detected | none needed | yes (count=20) |
| `internal/plugin` | `make test-flake-watch` (count=20) | No failures in watch target (engine+plugin packages only) | — | — |

## Run log

### 2026-05-02 — make test-flake-watch
```
go test -race -count=20 ./internal/engine/... ./internal/plugin/...
ok  github.com/brokenbots/criteria/internal/engine   91.090s
ok  github.com/brokenbots/criteria/internal/plugin  211.889s
```
PASS (both packages)

### 2026-05-02 — go test -race -count=3 -timeout=300s ./...
```
FAIL  github.com/brokenbots/criteria/internal/plugin  40.069s
--- FAIL: TestHandshakeInfo (2.49s)
    handshake_test.go:30: create plugin rpc client: timeout while waiting for plugin to start
ok    github.com/brokenbots/criteria/internal/transport/server   20.880s
ok    github.com/brokenbots/criteria/internal/engine   22.951s
```

Root cause: `buildNoopPlugin(t)` compiles the binary inside each test function using `t.TempDir()`. Under `-count=3 -race`, three builds run concurrently competing for CPU; the race detector adds overhead. After building, the plugin process itself must start within `StartTimeout: 2*time.Second`. Under load the process start (Unix socket advertisement) exceeds 2s.

### 2026-05-02 — time.Sleep grep / code review
Found timing-sensitive sleeps in `internal/transport/server/client_test.go`:
- L956: `time.Sleep(50ms)` after cancel() in TestClientHeartbeat — heartbeat goroutine may not have stopped
- L963: `time.Sleep(45ms)` to check no growth — snapshot taken before goroutine fully stopped could lead to false positive

## Notes

- `make test-flake-watch` only covers `engine` and `plugin`; transport/server tests not in that target.
- The W01 fix used `context.WithoutCancel` for plugin lifecycle. Same root class but different symptom: W01's flake was the step-deadline propagating into `Resolve`; this flake is the plugin process not starting fast enough under the test-binary-level `StartTimeout`.
- `TestClientHeartbeat` passes consistently under -count=3 on an unloaded machine but the pattern is fragile; fixing proactively.
- `TestClientDrain/ctx_cancel_unblocks_drain`: `time.Sleep(25ms)` is harmless (Drain handles already-cancelled ctx correctly) but removing it improves readability.
