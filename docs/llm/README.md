# Criteria LLM Prompt Pack

## How to assemble the prompt

Combine the language spec with the eight pattern files to build a complete LLM authoring system prompt:

```
System prompt = docs/LANGUAGE-SPEC.md + the 8 pattern files concatenated in order.
Total token budget: ~12,000 tokens (8,000 for the spec + ~4,000 for the pack).
```

```bash
cat docs/LANGUAGE-SPEC.md docs/llm/0*.md > prompt.md
```

## Pattern index

| # | Pattern | When to use it |
|---|---|---|
| 01 | Linear pipeline | Sequential steps, no branching. |
| 02 | Branching switch | One-of-N routing on a captured value. |
| 03 | Sequential iteration | Run one step per item in a list, in order. |
| 04 | Concurrent iteration | Run one step per item in parallel with a concurrency cap. |
| 05 | Subworkflow call | Reuse a workflow as a callable unit; pass inputs and capture outputs. |
| 06 | Human-in-the-loop | Pause for human approval or wait for an external signal. |
| 07 | Mutable shared state | Accumulate or update a value across multiple steps. |
| 08 | File-driven prompts | Iterate over a file list and process each file in a step. |

## Maintenance

Each pattern's HCL is also under `examples/llm-pack/`; `make validate` compiles all of them on every CI run.
