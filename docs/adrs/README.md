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

> **Pending — no record yet.** [ADR-0013] (decision adapter: System One
> decision-model calls with a strict question contract and fail-closed step
> outcome mapping) is not written yet — its record tracks Kanboard KB-199, and
> this index only lists ADRs that exist, never a row for an unwritten one.
> Until the record lands, the contract it will capture is documented in the
> adapter's own repo: the
> [`criteria-adapter-decision` README](https://github.com/brokenbots/criteria-adapter-decision)
> (full adapter contract) and its
> [backend guide](https://github.com/brokenbots/criteria-adapter-decision/blob/main/docs/backends.md).

[template]: https://github.com/joelparkerhenderson/architecture-decision-record
