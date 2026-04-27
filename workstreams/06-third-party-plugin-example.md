# Workstream 6 — Third-party plugin example

**Owner:** Doc / engine agent · **Depends on:** [W03](03-public-plugin-sdk.md) · **Unblocks:** [W08](08-phase0-cleanup-gate.md).

## Context

Once [W03](03-public-plugin-sdk.md) lands a public plugin-author SDK,
the next missing piece is proof: an example plugin that lives outside
this repo's module, imports only the public SDK and the generated
proto bindings, and runs against `overseer apply`. Without this, the
"third-party plugins are possible" story is theoretical.

The split-era reviewer notes called this out as deferred work (W08
reviewer, "third-party 'hello world' overseer plugin example").

This workstream produces a small example repo (or example directory
that *could* become its own repo) that demonstrates the full path:
clone, build, install into `~/.overseer/plugins/`, run a workflow
that uses it, observe expected output.

## Prerequisites

- [W03](03-public-plugin-sdk.md) merged with the public SDK
  available at a stable import path.
- `make plugins` builds the bundled adapters successfully.

## In scope

### Step 1 — Pick the form

Two viable shapes:

- **Sibling repo** at e.g. `github.com/brokenbots/overseer-example-plugin-greeter`.
  Most realistic — proves the import works from outside this module
  with no replace directive. More overhead (separate repo, separate
  CI).
- **In-tree example directory** at e.g. `examples/plugins/greeter/`
  with its own `go.mod` so it imports the public SDK as an external
  module (using a `replace` directive only for local development).
  Less overhead, but an importer with a sharp eye sees the
  `replace` and questions whether the example is honest.

Recommend the in-tree directory with **no `replace` directive in the
committed `go.mod`** — the example pins the published SDK version
explicitly. A local-dev `go.work` file (gitignored) lets contributors
test against unreleased SDK changes; the committed example always
builds against a real published tag.

### Step 2 — Build the example

`examples/plugins/greeter/`:

- `go.mod` declaring its own module path and depending on
  `github.com/brokenbots/overseer/sdk@<latest>` (the public plugin
  SDK package from W03).
- `main.go` — a small adapter that takes a `name` input and returns
  `"hello, <name>"`.
- `README.md` — install + run instructions, written for a developer
  who has never seen this repo.
- A workflow file under `examples/plugins/greeter/example.hcl` that
  uses the adapter.

### Step 3 — Wire into CI

Add a `make example-plugin` target that:

- Builds the greeter plugin into the example's `bin/`.
- Copies it to a temp `OVERSEER_PLUGINS` dir.
- Runs `overseer apply` against `example.hcl`.
- Asserts the run completes and produces expected output.

CI runs `make example-plugin` after `make build`. Failure means the
public plugin SDK regressed in a way that broke an external consumer —
exactly the signal this workstream exists to catch.

### Step 4 — Document

Update `docs/plugins.md` to reference the greeter example as the
canonical "minimum third-party plugin". Replace any older inline
sample code with a pointer.

## Out of scope

- Authoring a sibling repo. The in-tree directory is enough proof.
  Spawning a real sibling repo can happen later if external authors
  want a starter template.
- Demonstrating advanced plugin features (sessions, streaming
  responses, permission negotiation). The greeter is intentionally
  minimal.
- Multi-language plugin examples. Go-only.

## Files this workstream may modify

- `examples/plugins/greeter/` (new directory).
- `Makefile` (new `example-plugin` target).
- `.github/workflows/ci.yml` (new step running `make example-plugin`).
- `docs/plugins.md` (pointer update).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
or other workstream files.

## Tasks

- [ ] Pick the form (in-tree directory recommended).
- [ ] Author the greeter `main.go`, `go.mod`, `README.md`, `example.hcl`.
- [ ] Add `make example-plugin` target.
- [ ] Wire into CI.
- [ ] Update `docs/plugins.md`.
- [ ] Verify `make example-plugin` exits 0 against the published SDK
      version (or against the in-tree SDK if no published version
      yet, with a forward-pointer comment).

## Exit criteria

- `examples/plugins/greeter/` exists and builds with no `replace`
  directive in its committed `go.mod` (or, if the published SDK
  version doesn't yet exist, a documented temporary `replace`
  with a follow-up to remove it after [W08](08-phase0-cleanup-gate.md)
  cuts the first tag).
- `make example-plugin` runs end-to-end and asserts output.
- CI gates `make example-plugin` on every PR.
- `docs/plugins.md` points at the example.

## Tests

- The `make example-plugin` end-to-end check is the test.
- A regression here is a regression in the public plugin SDK
  contract (the W03 deliverable).

## Risks

| Risk | Mitigation |
|---|---|
| Example go.mod pins a specific SDK version that lags master | Acceptable; bumping the pin is one PR. The CI gate catches breakage early; the cost is one bump per minor SDK release. |
| Example becomes an unmaintained drift point as the SDK evolves | The CI gate is the maintenance forcing function. If the example fails to build, it's blocking; that means it gets fixed. |
| In-tree example with `replace` masks real external-author breakage | Hard rule: no `replace` in the committed `go.mod` once W08 cuts a tag. Until then, document the temporary `replace` with an explicit follow-up issue. |
| The example's HCL accidentally exercises non-public engine behavior | Keep the example small and read-only against the SDK contract. If the engine internals leak through, that's a W03 bug, not a W06 bug — file accordingly. |
