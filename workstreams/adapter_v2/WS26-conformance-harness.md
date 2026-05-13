# WS26 — Cross-language conformance harness

**Phase:** Adapter v2 · **Track:** SDK · **Owner:** Workstream executor · **Depends on:** [WS23](WS23-typescript-sdk-v2.md), [WS24](WS24-python-sdk-v2.md), [WS25](WS25-go-sdk-v1.md). · **Unblocks:** [WS40](WS40-v2-release-gate.md) verification gate 1.

## Context

`README.md` D57.1. Conformance suite at `internal/adapter/conformance/` (WS03 ported its 11 sub-tests to v2). This workstream **expands** the suite to cover every v2 RPC and drives it against all three SDKs on all supported platforms. Coordinates with the existing `test-01-adapter-conformance-expansion.md` workstream (which lives outside this phase and is already merged or actively in flight — confirm with maintainers before kickoff).

## Prerequisites

WS23, WS24, WS25 merged or at least at RC tag.

## In scope

### Step 1 — Conformance test matrix

Define the matrix in `internal/adapter/conformance/matrix.yaml`:

```yaml
sdks:
  - go
  - typescript
  - python
platforms:
  - linux/amd64
  - linux/arm64
  - darwin/arm64
suites:
  - happy
  - outcomes
  - lifecycle
  - permissions     # NEW (bidi)
  - logging         # NEW (dedicated Log stream)
  - pause_resume    # NEW
  - snapshot_restore # NEW
  - inspect         # NEW
  - secrets         # NEW
  - sensitive_output # NEW
  - heartbeats      # NEW
  - chunking        # NEW
```

### Step 2 — Reference adapters per SDK

A `criteria-adapter-conformance-target-{go,ts,python}` adapter exists in each SDK repo (or in a single `criteria-adapter-conformance-targets` repo — coordinate with WS27 if added to starters). Each implements the suite's expected behavior under each test (e.g., emit N events, request specific permissions, snapshot/restore state, etc.).

### Step 3 — Sub-test implementations

Each suite gets its own file under `internal/adapter/conformance/`:

- `conformance_permissions.go` — sends N concurrent permission requests; asserts decisions arrive correctly; asserts audit log entries; asserts deny-with-error.
- `conformance_logging.go` — adapter emits 100 log lines and 10 events; asserts ordering at host display; asserts heartbeats land.
- `conformance_pause_resume.go` — pauses mid-execution; asserts adapter stalls; resumes; asserts continuation matches.
- `conformance_snapshot_restore.go` — snapshots after N events; restores; asserts permission state replays; asserts secret re-resolution.
- `conformance_inspect.go` — Inspect returns sensible structured state during execution.
- `conformance_secrets.go` — adapter declares a secret; host provides via the secret channel; assert adapter reads via `secrets.Get`; assert process env does not contain the secret.
- `conformance_sensitive_output.go` — adapter emits a sensitive output; assert it's redacted in host logs; assert taint propagates.
- `conformance_heartbeats.go` — adapter stalls Log stream; assert heartbeat-stall crash detection.
- `conformance_chunking.go` — adapter emits a 16-MiB event; assert chunk reassembly is correct.

### Step 4 — CI matrix execution

GitHub Actions workflow `.github/workflows/conformance.yml`:

- 3 SDKs × 3 platforms = 9 jobs.
- Each job downloads the SDK-specific reference adapter binary (from the corresponding SDK CI artifact), runs the conformance suite, uploads the report.
- Failure on any job blocks merge.

### Step 5 — Tests

The conformance suite **is** the tests. Add a meta-test that ensures every suite in `matrix.yaml` has a corresponding file.

## Out of scope

- Adapter migrations using the conformance results — WS30–WS36 are validated by passing it.
- The release-gate roll-up — WS40.

## Behavior change

**No host behavior change.** New tests.

## Tests required

- All 9 matrix cells green.

## Exit criteria

- All conformance suites pass for all SDKs on all platforms.
- CI workflow gates merges.

## Files this workstream may modify

- `internal/adapter/conformance/*.go` *(new suite files + matrix.yaml)*.
- `.github/workflows/conformance.yml` *(new)*.
- Reference target adapters in each SDK repo.

## Files this workstream may NOT edit

- SDK source — additions to SDK repos go through WS23/WS24/WS25.
- Other workstream files.
