# WS22 — End-to-end remote demo runbook + CI smoke test

**Phase:** Adapter v2 · **Track:** Remote · **Owner:** Workstream executor · **Depends on:** [WS20](WS20-remote-environment-and-shim.md), [WS21](WS21-sdk-serveremote.md). · **Unblocks:** [WS40](WS40-v2-release-gate.md) verification gate 3.

## Context

`README.md` D57.3, D44-reachability. Ships a documented, reproducible runbook for deploying a remote adapter and a CI smoke test that exercises the end-to-end flow.

The runbook intentionally uses **two backends**: one Kubernetes deployment as the reference (since k8s is the most common production target), and one Docker Compose deployment (for local trial without a cluster). **criteria itself contains no k8s code** — the runbook invokes `kubectl` externally.

## Prerequisites

WS20, WS21 merged. CI environment has Docker + `kind` (for the k8s smoke test).

## In scope

### Step 1 — Runbook document

Create `docs/adapter-remote-deployment.md`:

1. **Concepts** section explaining the phone-home model, the shim, identity verification.
2. **k8s deployment** walkthrough with sample manifests (in `docs/examples/k8s-remote-adapter/`):
   - `deployment.yaml` running `greeter` adapter with `serveRemote(...)` and host address from a ConfigMap.
   - `secret.yaml` carrying the bearer token.
   - mTLS certificate generation via `cfssl` or `cert-manager`.
3. **Docker Compose** walkthrough (in `docs/examples/compose-remote-adapter/`) for local trial:
   - `docker-compose.yml` with one service running the adapter, one running criteria with the workflow.
4. **Troubleshooting** section: common firewall / reachability issues, certificate problems, identity-mismatch debugging.

### Step 2 — CI smoke test

`internal/ci/smoke/remote_adapter_test.go`:

1. Build the `greeter` adapter binary for `linux/amd64` (from WS30 once landed; until then, an in-tree fixture adapter).
2. Start a `kind` cluster.
3. Apply the k8s manifests from `docs/examples/k8s-remote-adapter/` pointed at a host that's `host.docker.internal:7778`.
4. Start criteria with a fixture workflow.
5. Wait for adapter phone-home.
6. Run the workflow.
7. Assert success.
8. Kill the adapter pod mid-execution; assert crash policy kicks in; bring it back; verify resume.
9. Tear down `kind`.

Time budget: <5 minutes per CI run. Gated by `CRITERIA_REMOTE_E2E=1` so it's not run on every PR (only on tagged releases and weekly cron).

### Step 3 — Smoke-test fixture adapter

A tiny `criteria-adapter-remote-smoke` Go adapter in `internal/ci/smoke/testdata/` that:

- Reads `serveRemote` config from env vars (so the k8s ConfigMap can configure it).
- Implements `execute` by echoing input back as output (`echo` semantics).
- Used only for the smoke test.

### Step 4 — Tests

- The smoke test itself is the test.
- Validate runbook examples compile / lint (a small CI step that does `kubectl apply --dry-run=client -f docs/examples/k8s-remote-adapter/`).

## Out of scope

- ECS / Cloud Run / Lambda deployment guides — left as community contributions (D44-launch is explicit that launch is not criteria's problem).
- A reusable Terraform module — not in v1; a doc pointer to existing k8s manifests is enough.

## Behavior change

**No host behavior change** — pure documentation + CI test addition.

## Tests required

- Smoke test passes in CI when `CRITERIA_REMOTE_E2E=1`.
- Manifest dry-runs clean.

## Exit criteria

- Runbook published in `docs/`.
- Smoke test green in the gated lane.

## Files this workstream may modify

- `docs/adapter-remote-deployment.md` *(new)*.
- `docs/examples/k8s-remote-adapter/*.yaml` *(new)*.
- `docs/examples/compose-remote-adapter/*` *(new)*.
- `internal/ci/smoke/remote_adapter_test.go` *(new)*.
- `internal/ci/smoke/testdata/criteria-adapter-remote-smoke/` *(new)*.
- `.github/workflows/remote-e2e.yml` *(new)* — gated by tag/cron.

## Files this workstream may NOT edit

- WS20/WS21 territory.
- Other workstream files.
