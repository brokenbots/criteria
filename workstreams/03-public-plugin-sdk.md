# Workstream 3 — Public plugin SDK

**Owner:** Engine agent · **Depends on:** none · **Unblocks:** [W06](06-third-party-plugin-example.md), [W08](08-phase0-cleanup-gate.md).

## Context

Today's adapter plugins import `github.com/brokenbots/overseer/internal/plugin`
(see [cmd/overseer-adapter-noop/main.go](../cmd/overseer-adapter-noop/main.go),
[cmd/overseer-adapter-copilot/main.go](../cmd/overseer-adapter-copilot/main.go)).
Go's `internal/` rule keeps that import legal **only because the plugin
binaries live in this same module**. A third party who wants to write
their own adapter cannot.

`docs/plugins.md` currently advises external authors to import that
package, which won't compile for them. The split-era reviewer notes
called this out as deferred work (W08 reviewer, "extract
`overseer-plugin-sdk`").

This workstream extracts a small, public package that an external
plugin author can import. It does **not** re-architect plugins; the
goal is the minimum surface that makes external authoring possible.

## Prerequisites

- `make build`, `make test`, `make lint-imports` green on `main`.
- The `cmd/overseer-adapter-*` directories successfully consume the
  current internal package (status quo).

## In scope

### Step 1 — Choose the package shape

Pick one:

- **Sub-package of `sdk/`** — e.g. `github.com/brokenbots/overseer/sdk/pluginhost`.
  Lives in the published SDK sub-module. Single tag covers SDK +
  pluginhost; importers use the same `sdk` versioning. Recommended.
- **New top-level public package** — e.g. `github.com/brokenbots/overseer/pluginsdk`.
  Independent from `sdk/`. More explicit, more cost; only worth it
  if the plugin contract wants to evolve independently of the
  orchestrator-side SDK.

Document the choice in a short `// Package …` comment header on the
new package, plus an ADR-0002 if the choice is non-obvious.

### Step 2 — Define the public surface

The minimum:

- `Serve(p Plugin)` — entrypoint that mirrors today's
  `internal/plugin.Serve` but is callable from anywhere.
- `Plugin` interface — the adapter contract (name, version, session
  lifecycle, execute streaming, permit, close).
- `HandshakeConfig` — re-exported from the host so plugins agree on
  the magic cookie.
- Types/constants for log levels and permission decisions if needed.

Out: storage, run-state machines, anything specific to a particular
adapter (those stay where they are).

### Step 3 — Move or thin-wrap

Two viable shapes:

- **Move.** Relocate `internal/plugin/serve.go` and friends into the
  new public package. The `internal/plugin` package becomes a thin
  re-export for the bundled adapters' convenience (or goes away
  entirely if migration is clean).
- **Thin-wrap.** The new public package contains forwarding
  declarations to `internal/plugin`. Cheap, but creates a duplicated
  surface and a future maintenance trap.

Prefer the move. Update all bundled adapter `main.go` files to
import the new path. `make lint-imports` rules update if the
boundary moves.

### Step 4 — Doc and rename clean-up

Update `docs/plugins.md` to point at the new import path and remove
the misleading `internal/plugin` advice.

If the new package goes under `sdk/`, confirm the `make lint-imports`
rule "internal/ must not import sdk top-level" still works. (`sdk/pluginhost`
is a non-pb sdk package, so the existing rule excludes it from
`internal/`. The bundled adapters live under `cmd/`, not `internal/`,
so they are unaffected.)

### Step 5 — Test the boundary

Add a small integration test that exercises the public API the same
way an external author would: build a tiny in-tree fixture plugin
that imports only the new public package and the generated
`sdk/pb/overseer/v1`. Run it through the existing adapter
conformance harness ([internal/adapter/conformance/](../internal/adapter/conformance/))
to prove the public surface is sufficient.

## Out of scope

- Re-architecting the plugin protocol (any wire-level change is its
  own workstream and likely a breaking SDK bump).
- A multi-language plugin SDK (this workstream is Go-only).
- Sandbox / permission model evolution — that overlaps with [W04](04-shell-adapter-sandbox.md)
  but is not coupled to plugin-author ergonomics.
- Publishing a separate Docker image, npm package, etc.

## Files this workstream may modify

- New package directory (e.g. `sdk/pluginhost/` or `pluginsdk/`).
- `internal/plugin/*.go` (move/thin-wrap).
- `cmd/overseer-adapter-*/main.go` (import path swap).
- `docs/plugins.md`.
- `tools/import-lint/main.go` and tests, if the boundary rules
  change.
- `Makefile` (if a new test target is added).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
or other workstream files.

## Tasks

- [ ] Pick the package shape (Step 1).
- [ ] Define the public surface (Step 2).
- [ ] Move (or thin-wrap) the implementation (Step 3).
- [ ] Update bundled adapters and `docs/plugins.md`.
- [ ] Update `tools/import-lint/` if the boundary moves.
- [ ] Add a fixture plugin under
      `cmd/overseer-adapter-*/testfixtures/...` that imports only
      the new public surface; wire through the adapter conformance
      harness.

## Exit criteria

- A non-internal package exists; an external module could import it
  with no `internal/...` reach-through.
- All three bundled adapters compile against the new public path.
- `make build && make test && make test-conformance && make lint-imports`
  all green.
- A fixture plugin built only against the public API passes the
  adapter conformance harness.
- `docs/plugins.md` describes the public path, not `internal/plugin`.

## Tests

- Existing adapter conformance harness covers the wire contract.
- New fixture plugin proves the public API is sufficient (golden
  signal that the package shape is right).

## Risks

| Risk | Mitigation |
|---|---|
| Moving `internal/plugin` breaks an unforeseen import elsewhere | `go build ./...` plus `make lint-imports` catches it; if a non-cmd consumer reaches into `internal/plugin`, decide per-case whether to lift it into the public package or refactor the consumer. |
| Public surface is wrong on first cut and locks in poor shape | Mark the package `v0.x` in its doc comment; commit to one breaking-change window per minor release until external use shows up. |
| Conflict with [W04](04-shell-adapter-sandbox.md) sandbox plumbing | W04 stays inside the shell adapter; the plugin SDK is the host-side handshake/transport. They don't collide. If they do during execution, sequence W03 before W04. |
