# Examples

Standalone workflows validated by `make validate`. Adapters referenced by
`source` are pulled and pinned on `criteria adapter lock`; the in-tree `noop`
fixture and the `plugins/greeter` adapter run without a registry.

Run one with:

```sh
criteria apply examples/<name>/<file>.hcl
```

| Example | Demonstrates |
|---|---|
| [`hello/hello.hcl`](hello/hello.hcl) | Minimal single-step workflow (smoke-test baseline). |
| [`tour/tour.hcl`](tour/tour.hcl) | Variables, `for_each` iteration, `parallel` fan-out, a duration `wait`, a `switch`, and a top-level `output` — in one workflow. |
| [`subworkflow/parent.hcl`](subworkflow/parent.hcl) | A parent workflow invoking a sub-workflow via `target = subworkflow.<name>` (multi-file). |
| [`build_and_test/build_and_test.hcl`](build_and_test/build_and_test.hcl) | Linear shell build → test pipeline with a retry policy. |
| [`copilot_planning_then_execution/`](copilot_planning_then_execution/copilot_planning_then_execution.hcl) | Two-phase agent workflow (plan, then execute) using the `copilot` adapter. |
| [`plugins/greeter/`](plugins/greeter/) | A minimal adapter implementation plus a workflow that runs it (`make example-plugin`). |
| [`llm-pack/`](llm-pack/) | Prompt-pack patterns surfaced by `criteria spec --with-patterns`. |
