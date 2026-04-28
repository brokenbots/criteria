# Performance Baseline — v0.2.0

Captured on Apple M3 Max (arm64/darwin) with `make bench` (`-benchtime=3s`).
These numbers establish a regression baseline; significant regressions should
be investigated before merging.

## Workflow compile (`workflow/`)

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkCompile_Hello` | 70,956 | 108,178 | 942 |
| `BenchmarkCompile_Perf1000Logs` | 80,585 | 109,502 | 956 |
| `BenchmarkCompile_WorkstreamLoop` | 1,658,581 | 1,757,809 | 13,902 |

`BenchmarkCompile_WorkstreamLoop` exercises the larger workstream-loop fixture
(complex HCL with many nodes) and is expected to be an order of magnitude
slower than a simple hello-world compile.

## Engine run (`internal/engine/`)

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkEngineRun_10Steps` | 12,687 | 19,896 | 268 |
| `BenchmarkEngineRun_100Steps` | 136,371 | 189,817 | 2,608 |
| `BenchmarkEngineRun_1000Steps` | 1,488,535 | 1,889,036 | 26,008 |

Allocation growth is approximately linear in step count (~26 allocs/step),
which is expected for the current per-node allocation model.

## Plugin execution (`internal/plugin/`)

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkBuiltinPlugin_Execute` (shell/`true`) | 17,875,147 | 81,028 | 110 |
| `BenchmarkBuiltinPlugin_Info` | 255.7 | 928 | 4 |
| `BenchmarkLoaderResolveBuiltin` | 44.46 | 80 | 2 |

`BenchmarkBuiltinPlugin_Execute` spawns a real subprocess (`/usr/bin/true`)
each iteration; the ~18 ms cost is dominated by OS process-spawn latency and
is expected to dwarf all other contributors.

## Reproduction

```sh
make bench
```

To run a single benchmark group:

```sh
go test -run='^$' -bench=BenchmarkCompile -benchmem ./workflow/...
go test -run='^$' -bench=BenchmarkEngine  -benchmem ./internal/engine/...
go test -run='^$' -bench=BenchmarkBuiltin -benchmem ./internal/plugin/...
```
