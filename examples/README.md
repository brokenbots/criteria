# Examples

Standalone workflow files that demonstrate Criteria features. All examples
are validated by `make validate` and can be run locally with the `noop` or
`shell` adapter unless noted otherwise.

## Running an example

```sh
criteria apply examples/<name>.hcl
```

## Example index

| File | Description |
|------|-------------|
| [`hello.hcl`](hello.hcl) | Minimal single-step workflow — smoke test baseline. |
| [`demo_tour_local.hcl`](demo_tour_local.hcl) | Demonstrates variables, for_each, wait (duration), and branch without requiring a server. |
| [`build_and_test.hcl`](build_and_test.hcl) | Build-and-test pipeline with shell steps and retry policy. |
| [`file_function.hcl`](file_function.hcl) | Uses the `file()` expression function to read content from a local file. |
| [`for_each_review_loop.hcl`](for_each_review_loop.hcl) | **Multi-step for_each iteration body**: `execute → review → cleanup → _continue`. Canonical example for W08 multi-step iteration. Uses the `noop` adapter. |
| [`perf_1000_logs.hcl`](perf_1000_logs.hcl) | Performance fixture — runs 1000 no-op steps to benchmark step throughput. |
| [`workstream_review_loop.hcl`](workstream_review_loop.hcl) | Two-agent executor/reviewer loop for workstream files. Requires the `copilot` adapter. |

## Multi-step for_each (featured example)

`for_each_review_loop.hcl` is the canonical example for the W08 multi-step
iteration feature. It shows a loop whose body spans three steps:

```
execute → review → cleanup → _continue
```

All three steps have access to `each.value` and `each.index`. See the
[for_each documentation](../docs/workflow.md#for-each) for details on
iteration body semantics and `each.*` lifetime.
