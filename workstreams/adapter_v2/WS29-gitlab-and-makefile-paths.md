# WS29 — GitLab CI + registry-agnostic Makefile equivalents

**Phase:** Adapter v2 · **Track:** CI scaffolding · **Owner:** Workstream executor · **Depends on:** [WS28](WS28-reusable-publish-action.md). · **Unblocks:** adoption by non-GitHub users.

## Context

`README.md` D48. GitHub Actions is not the only CI users have. Ship equivalent paths for GitLab CI and a portable Makefile-only flow. Same outputs (multi-arch builds, OCI artifact, cosign-keyless signature). Lives in each starter repo and is documented in `docs/adapters.md` (WS39).

## Prerequisites

WS28 merged (its underlying scripts are reusable).

## In scope

### Step 1 — GitLab CI template

Each starter repo (WS27) ships `.gitlab-ci.yml.example`:

```yaml
publish-adapter:
  stage: publish
  image: registry.gitlab.com/criteria/publish-adapter:v1   # mirrored from GH
  rules:
    - if: $CI_COMMIT_TAG
  script:
    - publish-adapter \
        --sdk=typescript \
        --registry=$CI_REGISTRY \
        --signing-mode=keyless \
        --platforms=linux/amd64,linux/arm64,darwin/arm64
  id_tokens:
    SIGSTORE_ID_TOKEN:
      aud: sigstore
```

A small container image `criteria/publish-adapter` (also published from WS28) is the runtime; same scripts as the composite action.

### Step 2 — Makefile-only path

Each starter has a `publish` make target that does the same steps locally (no CI required):

```make
publish:
	$(MAKE) build-linux-amd64 build-linux-arm64 build-darwin-arm64
	./scripts/emit-manifest.sh
	./scripts/oras-push.sh
	./scripts/cosign-sign.sh
```

Cosign-keyless from a developer machine works via interactive OIDC (browser-based device flow). Documented in the starter README.

### Step 3 — Container image for the action's runtime

A `criteria/publish-adapter` container image (Alpine + Go + Node + Python + bun + nuitka + oras + cosign). Built on every WS28 release. Mirrored to multiple registries (GHCR + GitLab.com Container Registry + Docker Hub).

### Step 4 — Tests

- Verify the GitLab template lints (using `gitlab-ci-lint`).
- Verify the Makefile path produces equivalent artifacts as the GH action on a local machine.

## Out of scope

- Other CI systems (Buildkite, CircleCI, Jenkins) — documented as "use the Makefile path or contribute a template."

## Behavior change

**N/A — new files in starter repos.**

## Tests required

- Lint + smoke as above.

## Exit criteria

- Each starter repo has a working GitLab template + Makefile path.
- The runtime container image is published.

## Files this workstream may modify

- `.gitlab-ci.yml.example` and `Makefile` in each starter repo.
- A new `criteria/publish-adapter-image` repo (or sub-directory of `publish-adapter`) for the container.

## Files this workstream may NOT edit

- Other workstream files.
