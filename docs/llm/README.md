# Criteria LLM Prompt Pack

## How to assemble the prompt

Concatenate `docs/LANGUAGE-SPEC.md` with the 8 pattern files to produce a
complete LLM authoring system prompt:

```bash
cat docs/LANGUAGE-SPEC.md docs/llm/0*.md > prompt.md
```

**System prompt = `docs/LANGUAGE-SPEC.md` + the 8 pattern files concatenated in order.**

Total token budget: ~12,000 tokens (≈8,000 for the spec + ≈4,000 for the pack).
For smaller-context models, drop individual patterns; each file is self-contained.

## Pattern index

| # | Pattern | When to use it |
|---|---|---|
| 01 | Linear pipeline | Sequential steps, no branching. |
| 02 | Branching switch | One-of-N routing on a captured value. |
| 03 | Sequential iteration | Apply one step to each element of a list, in order. |
| 04 | Concurrent iteration | Same as 03, but all iterations run in parallel. |
| 05 | Subworkflow call | Delegate to a reusable child workflow module. |
| 06 | Human-in-the-loop | Pause for an external signal or human approval. |
| 07 | Mutable shared state | Two or more steps read and write a common variable. |
| 08 | File-driven prompts | Load step inputs from files at runtime using `file()`. |

## Maintenance

Each pattern's HCL is also under `examples/llm-pack/`; `make validate` compiles all of them on every CI run.
