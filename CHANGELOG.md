# Changelog

All notable changes to Criteria are recorded here.

## [v0.1.0] — 2026-04-27

### Headline: project renamed from Overseer to Criteria

Phase 0 of the post-separation cleanup closes with this release. The
key change is an ADR-0001-driven full brand rename. Everything visible
to users — binary names, env vars, module path, proto package, state
directory — has moved from the `overseer` namespace to `criteria`.

### New module path

```
github.com/brokenbots/criteria
```

All three Go modules (`root`, `sdk/`, `workflow/`) updated.

### Binary names

| Old | New |
|-----|-----|
| `overseer` | `criteria` |
| `overseer-adapter-noop` | `criteria-adapter-noop` |
| `overseer-adapter-copilot` | `criteria-adapter-copilot` |
| `overseer-adapter-mcp` | `criteria-adapter-mcp` |

### Environment variables

All `OVERSEER_*` variables renamed to `CRITERIA_*`:

| Old | New |
|-----|-----|
| `OVERSEER_PLUGINS` | `CRITERIA_PLUGINS` |
| `OVERSEER_PLUGIN` (handshake cookie) | `CRITERIA_PLUGIN` |
| `OVERSEER_COPILOT_BIN` | `CRITERIA_COPILOT_BIN` |
| `OVERSEER_SERVER` | `CRITERIA_SERVER` |
| `OVERSEER_RUN_ID` | `CRITERIA_RUN_ID` |
| `OVERSEER_EVENTS_FILE` | `CRITERIA_EVENTS_FILE` |
| `OVERSEER_STATE_DIR` | `CRITERIA_STATE_DIR` |

### State directory

Local run state moved from `~/.overseer/` to `~/.criteria/`.

To migrate existing run state:

```sh
mv ~/.overseer ~/.criteria
```

### Proto package

```
overseer.v1  →  criteria.v1
```

Generated Go bindings: `sdk/pb/criteria/v1/`.
Connect service name: `criteria.v1.CriteriaService`.

### What else shipped in Phase 0

- **W02** — README and CONTRIBUTING rewrites for general-purpose audience.
- **W03** — Public plugin-author SDK (`sdk/pluginhost`) extracted from
  `internal/plugin/`; external authors no longer need to import internal packages.
- **W04** — Shell adapter sandboxing: configurable allow/deny list, PATH
  restriction, and env-var filtering.
- **W05** — Copilot adapter E2E suite in the default test lane.
- **W06** — Standalone third-party plugin example (`examples/plugins/greeter`)
  demonstrating the `sdk/pluginhost` public API.
- **W07** — LICENSE (MIT), SECURITY.md, CODEOWNERS, issue/PR templates,
  Dependabot config.

### Install

```sh
go install github.com/brokenbots/criteria/cmd/criteria@v0.1.0
```

Requires Go 1.26 or later. Note: the GitHub repository rename from
`brokenbots/overseer` to `brokenbots/criteria` is a pending operator
action; GitHub serves redirects from the old URL.

### SDK conformance note

The in-repo conformance suite (`sdk/conformance`) passes against the
in-memory reference Subject. Cross-repo conformance against the
orchestrator-side implementation is transiently affected until the
orchestrator's paired rename PR lands — tracked separately and does
not block this release.

[v0.1.0]: https://github.com/brokenbots/criteria/releases/tag/v0.1.0
