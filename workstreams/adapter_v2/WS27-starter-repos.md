# WS27 — Starter GitHub template repos (TS / Python / Go)

**Phase:** Adapter v2 · **Track:** CI scaffolding · **Owner:** Workstream executor · **Depends on:** [WS23](WS23-typescript-sdk-v2.md), [WS24](WS24-python-sdk-v2.md), [WS25](WS25-go-sdk-v1.md), [WS28](WS28-reusable-publish-action.md). · **Unblocks:** [WS40](WS40-v2-release-gate.md) verification gate 4.

## Context

`README.md` D46. Three GitHub template repos, each producing a working hello-world adapter when a user clicks "Use this template" or runs `gh repo create --template`. Each ships a CI workflow consuming the WS28 publish action with `with_image: false` by default, plus a commented `Dockerfile` showing how to opt in to image publishing.

## Prerequisites

WS23–WS25 (SDKs exist and have RCs published).
WS28 (the reusable publish action exists).

## In scope

### Step 1 — `criteria-adapter-starter-typescript`

New repo template containing:

- `package.json` with `@criteria/adapter-sdk` dep.
- `index.ts` — minimal hello-world `serve(...)` with a `greet` outcome.
- `tsconfig.json`, `bun.lockb`.
- `.github/workflows/publish.yml` invoking `criteria/publish-adapter@v1` with `with_image: false`.
- `Dockerfile` (commented "uncomment to enable container image publishing").
- `examples/k8s/`, `examples/docker-compose/`, `examples/systemd/` — remote-adapter deployment examples (from WS21's docs).
- `README.md` with quickstart: clone → edit `index.ts` → push tag → adapter is published.

### Step 2 — `criteria-adapter-starter-python`

Same shape using `criteria-adapter-sdk` PyPI package. `main.py` entrypoint, `pyproject.toml`, Nuitka build script in Makefile.

### Step 3 — `criteria-adapter-starter-go`

Same shape using `github.com/brokenbots/criteria-go-adapter-sdk`. `main.go` entrypoint.

### Step 4 — Template-repo configuration

Each repo has `template: true` set in GitHub repo settings. README links to a hosted documentation site (deferred — for now, link to the SDK README in its own repo).

### Step 5 — Validation in CI

A meta-CI test (in this WS itself) that periodically: creates a fresh repo from each template, pushes a tag, validates the publish workflow succeeds and the artifact is signed and pullable.

## Out of scope

- The publish action itself — WS28.
- GitLab CI templates — WS29.

## Behavior change

**N/A — new template repos.**

## Tests required

- Manually verified template instantiation succeeds and publishes a working adapter for each language.

## Exit criteria

- Three template repos exist and pass meta-CI on a tagged commit.

## Files this workstream may modify

- Everything in the three new template repos.

## Files this workstream may NOT edit

- The criteria monorepo.
- The SDK repos.
- Other workstream files.
