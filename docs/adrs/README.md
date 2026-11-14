# Architecture Decision Records

Lightweight ADRs capture decisions whose rationale would otherwise live
only in PR descriptions, chat history, or contributor heads. Format
follows the [lightweight ADR template][template]: Status, Context,
Decision, Consequences.

An ADR is not merged in `Proposed` state. The reviewers listed in the
ADR's sign-off section flip it to `Accepted` once the decision is
final.

## Index

| ADR | Title | Status |
|---|---|---|
| [ADR-0001](ADR-0001-naming-convention.md) | Naming convention — adopt `criteria` as the top-level brand | Accepted |
| [ADR-0002](ADR-0002-while-step-iteration.md) | `while` step iteration modifier | Accepted |
| [ADR-0003](ADR-0003-conformance-scope.md) | Conformance scope — host + imported SDK, not every SDK | Accepted |
| [ADR-0004](ADR-0004-adapter-tools.md) | Adapter-as-tool contract — grammar, naming, return semantics, cycle policy, wire decision | Accepted |
| [ADR-0005](ADR-0005-workflow-source-model.md) | Workflow source model, fetch/cache, and provenance | Accepted |
| [ADR-0006](ADR-0006-sendprompt-injection.md) | SendPrompt: mid-turn agent message injection; redirect deferred | Accepted |
| [ADR-0007](ADR-0007-peer-execution.md) | Peer execution: criteria-to-criteria remote adapters (staged A→B) | Proposed |
| [ADR-0008](ADR-0008-criteria-as-adapter.md) | Criteria-as-adapter — a criteria instance serving the adapter v2 surface | Proposed |
| [ADR-0009](ADR-0009-subworkflow-overrides.md) | Subworkflow environment/adapter overrides — pass-in/pinning model | Proposed |
| [ADR-0010](ADR-0010-kintsugi-split.md) | Kintsugi split — Parapet standalone repo + castle-side proto/SDK extraction | Accepted |
| [ADR-0012](ADR-0012-interactive-shell.md) | Interactive criteria shell — one `criteria shell` verb surface over LocalControlService (human REPL + AI-harness NDJSON; free graph edits under linkage validation) | Accepted |
| [ADR-0013](ADR-0013-decision-adapter.md) | decision adapter — the System One decision-model family behind one adapter (one `/v1/systemone` wire for TypeSafe Jev cloud + local Ollama; strict question contract, fail-closed step outcome mapping) | Accepted |
| [ADR-0014](ADR-0014-environment-host-of-record.md) | The environment as host-of-record: one session/ownership seam | Accepted |

[template]: https://github.com/joelparkerhenderson/architecture-decision-record
