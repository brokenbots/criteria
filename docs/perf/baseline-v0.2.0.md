# Performance Baseline — v0.2.0

Captured on Apple M3 Max (arm64/darwin) with `make bench` (default `-benchtime`).

| | |
|---|---|
| **Hardware** | Apple M3 Max (arm64/darwin) |
| **Go version** | go1.26.2 darwin/arm64 |
| **Commit** | f857df97c66f3b7034fbcd19163b59b70817ac95 |

**Regression policy**: Regressions > 20% on any of these baselines should fail review until justified.

## Workflow compile (`workflow/`)

| Benchmark | ns/op | B/op | allocs/op | Notes |
|---|---:|---:|---:|---|
| `BenchmarkCompile_Hello` | 70,336 | 108,179 | 942 | Minimal hello workflow |
| `BenchmarkCompile_1000Steps` | 31,983,687 | 55,741,410 | 389,695 | 1 000-node sequential workflow, stresses compiler |
| `BenchmarkCompile_WorkstreamLoop` | 1,824,206 | 1,891,169 | 15,097 | Workstream-loop fixture (updated at f857df9: +2 shell steps vs original 13,902 allocs/op at e890474, +8.6%, within 20% threshold) |

`BenchmarkCompile_1000Steps` exercises 1 000 sequential HCL step nodes and is
expected to be ~500× slower than a single-step compile. The allocation delta
(389,695 vs 942) confirms the benchmark is stressing the compiler proportionally.

## Engine run (`internal/engine/`)

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkEngineRun_10Steps` | 12,442 | 19,896 | 268 |
| `BenchmarkEngineRun_100Steps` | 124,624 | 189,818 | 2,608 |
| `BenchmarkEngineRun_1000Steps` | 1,466,508 | 1,889,038 | 26,008 |

Allocation growth is approximately linear in step count (~26 allocs/step),
which is expected for the current per-node allocation model.

## Plugin execution (`internal/plugin/`)

| Benchmark | ns/op | B/op | allocs/op | Notes |
|---|---:|---:|---:|---|
| `BenchmarkBuiltinPlugin_Execute` (shell/`true`) | 22,162,986 | 81,263 | 111 | Full per-step cost: Open+Execute+Close, subprocess spawn |
| `BenchmarkPluginExecuteNoop` | 8.297 | 0 | 0 | Pure Execute dispatch with in-process noop adapter, session opened once |
| `BenchmarkBuiltinPlugin_Info` | 240.6 | 928 | 4 | |
| `BenchmarkLoaderResolveBuiltin` | 43.44 | 80 | 2 | |

`BenchmarkBuiltinPlugin_Execute` spawns a real subprocess (`/usr/bin/true`)
each iteration; the cost is dominated by OS process-spawn latency.
`BenchmarkPluginExecuteNoop` isolates the plugin-dispatch overhead from
subprocess cost: ~8 ns/op with zero allocations.

## Reproduction

```sh
make bench
```

To run a single benchmark group:

```sh
go test -run='^$' -bench=BenchmarkCompile -benchmem ./workflow/...
go test -run='^$' -bench=BenchmarkEngine  -benchmem ./internal/engine/...
go test -run='^$' -bench=Benchmark        -benchmem ./internal/plugin/...
```

## Notes on `bench` target scope

The `bench` target runs three targeted packages rather than `./...` per module.
This avoids triggering `TestMain` setup in packages like `cmd/criteria-adapter-mcp`
(which builds a test binary during TestMain) when no benchmarks exist in those packages.
The SDK and workflow modules have no benchmarks yet; they are included via targeted
`./workflow/...` invocation.
