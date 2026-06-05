# ADR-0003 — Conformance scope: host + imported SDK, not every SDK

**Status:** Accepted

**Date:** 2026-06-05

---

## Context

Protocol v2 ships three adapter SDKs (Go, TypeScript, Python), each in its own
repository. An early design (D57.1, WS26) proposed a single cross-language
**conformance matrix** in the `criteria` repo: a CI job (`conformance.yml`) that,
for each `sdk ∈ {go, typescript, python}` × `platform ∈ {linux/amd64,
linux/arm64, darwin/arm64}`, downloaded that SDK's "reference adapter" binary and
ran the host conformance suite against it.

That model does not scale and puts the responsibility in the wrong place:

- The host repo becomes a bottleneck coupled to **every** SDK's release cadence:
  a conformance run cannot go green until all three external SDKs publish a
  reference adapter. In practice the matrix sat permanently red behind an
  `exit 1` placeholder ("Download reference adapter … not yet wired").
- It conflates two different questions. Whether the **TypeScript** or **Python**
  SDK correctly implements the wire is *that SDK's* problem, testable in *that
  SDK's* repo against the shared proto package. The `criteria` repo only needs to
  know that **its own host implementation** and **the SDK it actually imports**
  (the in-tree Go `sdk/adapterhost`) agree on the wire.

## Decision

Conformance testing in the `criteria` repo is scoped to **the host side plus the
single SDK this project imports**, and to **proto compatibility**. It does not
validate other-language SDKs.

Concretely, the in-repo coverage is:

1. **Host ↔ imported-Go-SDK conformance** — `TestNoopAdapterConformance`
   ([internal/adapter/conformance/noop_adapter_test.go](../../internal/adapter/conformance/noop_adapter_test.go))
   builds the in-tree adapter on `sdk/adapterhost` and runs the host conformance
   suite against it as a real subprocess. Runs under the normal `go test ./...`
   in the `unit-tests` CI job.
2. **SDK in-memory conformance** — `make test-conformance` runs
   `sdk/conformance` against an in-memory Subject.
3. **Proto compatibility** — the required `proto-drift` CI gate
   (`make proto-check-drift`) ensures the generated bindings match the `.proto`
   sources, so host and SDK cannot silently diverge on the wire.

The cross-language matrix workflow (`.github/workflows/conformance.yml`) is
**removed**. `TestExternalAdapterConformance` is **retained** as an opt-in tool
(driven by `CRITERIA_CONFORMANCE_ADAPTER`) for ad-hoc validation of an arbitrary
adapter binary, but it is not wired into a CI matrix and skips when the env var
is unset.

Each SDK repository owns its own conformance: it depends on the published proto
package (see [WS41](../../workstreams/adapter_v2/WS41-extract-adapter-proto-repo.md))
and runs the conformance contract against itself in its own CI.

## Consequences

- The conformance gate is **self-contained and green from in-repo work** — it no
  longer blocks on external SDK reference adapters being published. This removes
  the "publish reference adapters per SDK" item from the host repo's critical
  path.
- Responsibility is correctly distributed: the host proves host-side correctness
  and proto compatibility; each SDK proves its own conformance against the shared
  contract. Adding a fourth SDK later does not touch the host repo's CI.
- The proto package (WS41) becomes the single shared artifact that lets each SDK
  self-test against the same contract the host validates against — reinforcing
  the proto-as-source-of-truth governance (D59).
- **Trade-off:** the host repo no longer detects a TypeScript/Python regression
  directly. That is intentional — it is the SDK's responsibility, caught in the
  SDK's own CI against the versioned proto package. An end-to-end cross-SDK
  smoke can still live in a dedicated integration lane (e.g. the remote-e2e or a
  release gate) without coupling routine host CI to every SDK.

## Related

- D57.1, WS26 — the original cross-SDK conformance matrix proposal (superseded by this ADR).
- WS41 — proto extraction; each SDK self-tests against the published package.
- `proto-drift` CI gate — proto compatibility.
