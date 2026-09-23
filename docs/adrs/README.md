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

[template]: https://github.com/joelparkerhenderson/architecture-decision-record
