# WS43 — Independence verification

**Phase:** Adapter v2 · **Track:** End-state independence · **Owner:** Workstream executor · **Depends on:** [WS41](WS41-extract-adapter-proto-repo.md), [WS42](WS42-extract-shell-adapter.md). · **Unblocks:** end of phase.

## Context

`README.md` D58–D60. After this WS, the end state is verified:

- **criteria** repo: host / engine / CLI only. No adapter implementations. No proto sources.
- **criteria-adapter-proto** repo: wire contract + bindings, multi-language published.
- **Three SDKs** in their own repos: `criteria-typescript-adapter-sdk`, `criteria-python-adapter-sdk`, `criteria-go-adapter-sdk` — each consuming `criteria-adapter-proto` as a versioned dependency.
- **Seven adapters** in their own repos: `criteria-typescript-adapter-greeter`, `-claude`, `-claude-agent`, `-codex`, `-openai`, `criteria-adapter-shell` (new), `criteria-adapter-copilot`.
- **DEPENDENCIES.md** in the proto repo tracks consumer pin versions.

## Prerequisites

WS41, WS42 merged.

## In scope

### Step 1 — Audit the criteria repo

```sh
! find internal/builtin -type d -name '*adapter*' -not -empty
! find proto/ -type f
! grep -rn 'github.com/brokenbots/criteria/proto' --include='*.go' .   # should reference the external proto module only
```

The first must find nothing (or only `noop` test fixture if any). The second must find nothing (proto is external). The third must reference `criteria-adapter-proto` not the in-tree path.

### Step 2 — Audit consumer repos

For each SDK + each adapter, verify their `go.mod`/`package.json`/`pyproject.toml` consumes the published `criteria-adapter-proto` package, not a vendored copy.

### Step 3 — Smoke test the full chain

A test that:

1. Clones a fresh criteria release on a clean machine.
2. Runs `criteria pull <workflow-fixture-ref>` where the fixture references all three SDK families (one TS adapter, one Python adapter, one Go adapter).
3. The workflow pull transitively pulls all three adapter artifacts from their respective repos' GHCR registries.
4. `criteria apply` runs the workflow.
5. All three adapters' steps complete successfully.

This is the canonical "the user experience works end-to-end across the independent repos" demonstration.

### Step 4 — Documentation finalization

- The proto repo's README documents the governance model: changes require a release of the proto repo; consumers upgrade by bumping their pinned version.
- DEPENDENCIES.md table populated with current pin versions of each known consumer.
- A "verifying independence" section in `docs/release-process.md` (criteria) documenting how to re-run this WS's audits.

## Out of scope

- Any code changes — pure audit + docs.

## Behavior change

**N/A — audit + verification.**

## Tests required

- Audits pass.
- Smoke test passes.

## Exit criteria

- All three audits clean.
- Smoke test green.
- DEPENDENCIES.md populated.

## Files this workstream may modify

- `docs/release-process.md` in criteria.
- `DEPENDENCIES.md` in the proto repo.
- `README.md` in the proto repo (governance section).

## Files this workstream may NOT edit

- Source code (audit only).
- Other workstream files.
