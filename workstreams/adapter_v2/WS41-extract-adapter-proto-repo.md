# WS41 — Extract adapter wire contract to `criteria-adapter-proto` repo

**Phase:** Adapter v2 · **Track:** End-state independence · **Owner:** Workstream executor (creates new repo) · **Depends on:** [WS40](WS40-v2-release-gate.md) (v2 shipped). · **Unblocks:** [WS43](WS43-independence-verification.md).

## Context

`README.md` D58–D60. The proto + bindings live in their own repo so no single project can unilaterally change the wire. Multi-language publishing (Go module, npm package, PyPI package) is part of this repo's CI.

## Prerequisites

WS40 (v2 release tagged; the proto we're extracting is stable).

## In scope

### Step 1 — Create the repo

`criteria-adapter-proto` under the brokenbots org. Apache-2 license. Standard repo hygiene.

### Step 2 — Move proto sources

Copy `proto/criteria/v2/*.proto` and helper Go code (`chunking.go`) into the new repo with history (git filter-repo to preserve commits touching these files). New repo's directory layout:

```
proto/                       # .proto sources
  options.proto
  adapter.proto
gen/
  go/                        # generated Go bindings
  ts/                        # generated TypeScript types
  python/                    # generated Python types
internal/
  chunking.go                # helper, exported as a Go subpackage
.github/workflows/
  publish.yml                # multi-language publish on tag
```

### Step 3 — Multi-language publishing

CI workflow that, on tag push:

- **Go**: `go mod tidy`; tag triggers Go module proxy index update at `github.com/brokenbots/criteria-adapter-proto`.
- **npm**: `npm publish` to `@criteria/adapter-proto`.
- **PyPI**: `python -m build` + `twine upload` to `criteria-adapter-proto`.

### Step 4 — Update consumers

In each consumer repo (criteria, criteria-go-adapter-sdk, criteria-typescript-adapter-sdk, criteria-python-adapter-sdk):

- Replace vendored `.proto` files / bindings with a versioned dependency on the new package.
- For criteria: `go get github.com/brokenbots/criteria-adapter-proto@v1.0.0` and delete `proto/criteria/v2/`.
- For the TS SDK: `bun add @criteria/adapter-proto@^1.0.0` and delete vendored TS types.
- For the Python SDK: `pip add criteria-adapter-proto==1.0.0` and delete vendored Python types.

Run each repo's CI; coordinate four PRs landing together.

### Step 5 — Versioning policy

Document in the new repo's README: SemVer for the proto package. Breaking changes (field removals, type changes) require a major bump. Additive changes (new RPCs, new optional fields) are minor. Patch is bug fixes in generated code.

### Step 6 — DEPENDENCIES.md

A table maintained in the proto repo that lists each known consumer's pinned proto version. Updated by consumers as they upgrade.

## Out of scope

- Shell adapter extraction — WS42.
- Final independence verification — WS43.

## Behavior change

**No wire-protocol behavior change.** Consumers see a versioned external dependency instead of a vendored one.

## Tests required

- Each consumer repo passes its own CI after the swap.
- Multi-language publish workflow runs successfully on a tag in the new repo.

## Exit criteria

- `criteria-adapter-proto` repo exists with multi-language CI.
- All four consumer repos consume the published package.
- `proto/criteria/v2/` deleted from the criteria monorepo.

## Files this workstream may modify

- The new `criteria-adapter-proto` repo.
- Each consumer repo's package config + import paths.
- `proto/criteria/v2/` (deleted from criteria).

## Files this workstream may NOT edit

- The wire contract semantics (no field/RPC changes in this WS).
- Other workstream files.
