# Performance Baseline — v0.2.0

Captured on Apple M3 Max (arm64/darwin) with `make bench` (default `-benchtime`).

| | |
|---|---|
| **Hardware** | Apple M3 Max (arm64/darwin) |
| **Go version** | go1.26.2 darwin/arm64 |
| **Commit** | e890474a3146fe7e7473534b63a9f1723ff0bbda |

**Regression policy**: Regressions > 20% on any of these baselines should fail review until justified.

## Workflow compile (`workflow/`)

| Benchmark | ns/op | B/op | allocs/op | Notes |
|---|---:|---:|---:|---|
| `BenchmarkCompile_Hello` | 68,115 | 108,177 | 942 | Minimal hello workflow |
| `BenchmarkCompile_1000Steps` | 33,163,892 | 55,741,619 | 389,695 | 1 000-node sequential workflow, stresses compiler |
| `BenchmarkCompile_WorkstreamLoop` | 1,605,975 | 1,757,873 | 13,902 | Workstream-loop fixture |

`BenchmarkCompile_1000Steps` exercises 1 000 sequential HCL step nodes and is
expected to be ~500× slower than a single-step compile. The allocation delta
(389,695 vs 942) confirms the benchmark is stressing the compiler proportionally.

## Engine run (`internal/engine/`)

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkEngineRun_10Steps` | 12,325 | 19,896 | 268 |
| `BenchmarkEngineRun_100Steps` | 123,252 | 189,818 | 2,608 |
| `BenchmarkEngineRun_1000Steps` | 1,414,919 | 1,889,037 | 26,008 |

Allocation growth is approximately linear in step count (~26 allocs/step),
which is expected for the current per-node allocation model.

## Plugin execution (`internal/plugin/`)

| Benchmark | ns/op | B/op | allocs/op | Notes |
|---|---:|---:|---:|---|
| `BenchmarkBuiltinPlugin_Execute` (shell/`true`) | 11,146,722 | 81,013 | 110 | Full per-step cost: Open+Execute+Close, subprocess spawn |
| `BenchmarkPluginExecuteNoop` | 8.386 | 0 | 0 | Pure Execute dispatch with in-process noop adapter, session opened once |
| `BenchmarkBuiltinPlugin_Info` | 231.6 | 928 | 4 | |
| `BenchmarkLoaderResolveBuiltin` | 43.26 | 80 | 2 | |

`BenchmarkBuiltinPlugin_Execute` spawns a real subprocess (`/usr/bin/true`)
each iteration; the ~11 ms cost is dominated by OS process-spawn latency.
`BenchmarkPluginExecuteNoop` isolates the plugin-dispatch overhead from
subprocess cost: 8 ns/op with zero allocations.

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
