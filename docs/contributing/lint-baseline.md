# Lint Baseline — Burn-Down Contract

This document explains how the lint baseline works, how to remove entries from
it, and why `make lint-go` is a hard PR gate.

## What is `.golangci.baseline.yml`?

`.golangci.baseline.yml` is a generated suppression file that quarantines
pre-existing lint findings on day one. Running `golangci-lint` against the
current `main` found ~230 issues — mostly long functions (`funlen`/`gocyclo`),
missing GoDoc (`revive`), and import formatting (`goimports`). Rather than
blocking every PR until all 230 are fixed, the baseline file suppresses them so
the lint job is green immediately. Each subsequent workstream removes the
suppressions it has already fixed.

The key insight: the baseline is **not a permanent allowlist**. It is a
punch-list. Every entry is annotated with the workstream that will remove it,
for example:

```yaml
    - path: internal/engine/engine.go
      linters:
        - funlen
      text: 'Function ''runLoop''' # W03: refactor runLoop
```

## How is the merged config assembled?

`golangci-lint` v1 does not support multiple config files natively. The
`lint-go` Makefile target assembles a temporary `.golangci.merged.yml` at
build time:

```sh
cat .golangci.yml > .golangci.merged.yml
tail -n +3 .golangci.baseline.yml >> .golangci.merged.yml
```

`.golangci.yml` ends with `issues.exclude-rules:` as the last section. The
`tail -n +3` strips the `issues:` and `exclude-rules:` header lines from the
baseline file and appends the baseline entries directly into that list. The
merged file is deleted after `golangci-lint` exits.

**Never commit `.golangci.merged.yml`** — it is listed in `.gitignore`.

## How is the linter invoked?

The linter is pinned via the Go 1.24+ `tool` directive in the root module's
`go.mod`:

```
tool github.com/golangci/golangci-lint/cmd/golangci-lint
```

Always invoke it through `go tool golangci-lint` (or `make lint-go`), never
through a globally-installed binary. This guarantees every contributor and the
CI runner use exactly the same version (v1.64.8 at time of writing).

In a Go workspace, `go tool golangci-lint` is accessible from any workspace
module directory because the tool is registered in the root module.

## The burn-down rule

**A workstream that touches a file with a baseline suppression must remove the
suppression as part of its diff.**

Concretely:
1. When a workstream refactors a function that has a `funlen` or `gocyclo`
   baseline entry, it must delete that entry from `.golangci.baseline.yml`.
2. When a workstream adds GoDoc to an exported symbol, it must delete the
   corresponding `revive` entry.
3. When a workstream reformats a file (e.g., via `goimports`), it must delete
   the `goimports` entry.

The reviewer enforces this. A PR that fixes the underlying issue but leaves the
baseline entry should not be merged.

## W01 snapshot (mechanical burn-down)

W01 removed mechanical suppressions (`gofmt`, `goimports`, `unused`) and moved
proto-name `revive` suppressions for `sdk/events.go` and
`sdk/payloads_step.go` to file-level `//nolint:revive` with wire-compatibility
justification.

| Snapshot | Total | W03 | W04 | W06 | W10 |
|---|---:|---:|---:|---:|---:|
| Before W01 | 240 | 42 | 133 | 54 | 11 |
| After W01 | 117 | 42 | 38 | 29 | 8 |

Residual baseline by linter after W01:

| Linter | Count |
|---|---:|
| `funlen` | 30 |
| `gocritic` | 25 |
| `gocognit` | 18 |
| `gocyclo` | 13 |
| `contextcheck` | 9 |
| `errcheck` | 9 |
| `revive` | 9 |
| `staticcheck` | 3 |
| `nilerr` | 1 |

**Adding new suppressions** (e.g., for a legitimately complex function that
cannot be simplified) requires:
- A workstream-pointer comment naming who removes it.
- An explicit justification in the PR description.

## The merge gate

`make lint-go` must exit 0 on every PR. There is no `--allow-failure` mode and
no way to skip it: the CI job runs `make lint-go` after `make lint-imports` and
before `make build`.

If you introduce a new lint violation, you have two options:
1. Fix the underlying issue (preferred).
2. Add a suppression entry to `.golangci.baseline.yml` with a workstream-pointer
   comment and a justification comment in the PR.

## Regenerating the baseline

If a workstream makes changes that cause entirely new findings (e.g., a new
linter is enabled), regenerate the baseline:

```sh
# 1. Capture findings for all three modules.
go tool golangci-lint run --out-format=json ./... > /tmp/r.json
(cd sdk      && go tool golangci-lint run --out-format=json ./... > /tmp/s.json)
(cd workflow && go tool golangci-lint run --out-format=json ./... > /tmp/w.json)

# 2. Merge and generate.
python3 -c "
import json
all = []
for f in ['/tmp/r.json', '/tmp/s.json', '/tmp/w.json']:
    all.extend((json.load(open(f)).get('Issues') or []))
json.dump({'Issues': all, 'Report': {}}, open('/tmp/all.json', 'w'))
"
go run ./tools/lint-baseline -in /tmp/all.json -out .golangci.baseline.yml

# 3. Verify lint-go is green.
make lint-go
```

Note: golangci-lint's internal issue ordering can cause suppressing one issue to
reveal another. If `make lint-go` still fails after the first generation, repeat
the capture+generate cycle using the merged config until the run is stable.

## Linters and their owning workstreams

| Linter | Workstream |
|--------|-----------|
| `funlen`, `gocyclo`, `gocognit` | W03 — god-function refactor |
| `revive`, `gocritic` (style/doc) | W06 — coverage, bench, godoc |
| Everything else | W04 — split oversized files / general hygiene |
