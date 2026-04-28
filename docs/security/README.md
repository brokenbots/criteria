# Security Documentation

This directory contains the Criteria security documentation.

| Document | Description |
|---|---|
| [shell-adapter-threat-model.md](shell-adapter-threat-model.md) | Threat model for the `shell` adapter: trust boundaries, attacker capabilities, defender goals, mitigation table, and Phase 2 deferred work. |

## Living documents

Treat every document here as living. When a workstream touches the shell adapter
(or any adapter covered by a threat model), the threat model must be updated in
the same pull request. This contract is enforced at review time, not by tooling.
