# ADR-0010 — Kintsugi split: Parapet standalone, castle-side proto/SDK extraction

**Status:** Accepted (2026-10-03, project lead — board-decision session; the decision
predates this record, which commits it verbatim after the fact)
**Date:** 2026-10-03
**Deciders:** Project lead; board-decision session 2026-10-03 (restructures KB-119's
ruling: the coupling is killed by repo split, not by in-place tightening)
**Series:** Kanboard KB-163 (this record; un-armed — no k8s-run). Extraction and
consumption lane: KB-164 … KB-168, each card carrying its own execution step
(see Sequencing).

---

## Context

The criteria event vocabulary and the run-viewer UI live in more places than one
repo, and the seams are hand-maintained. KB-119's evidence (criteria head
99baa28, 2026-10-03) documents the coupling:

- **The event vocabulary exists in synchronizing-by-hand copies.** The engine's
  producer table (`internal/runstate/eventvocab.go`) maps each ND-JSON
  `payload_type` onto the seam's lowerCamelCase envelope type; the Kintsugi run
  viewer's data source (`dataSource.ts`, `TERMINAL_EVENT_TYPES = {runCompleted,
  runFailed}`) consumes the same vocabulary from the other side. Parity is
  enforced by nobody: drift is silent, and no conformance test runs anywhere.
- **A third copy exists on the server side.** Castle vendors its own
  `proto/criteria/v1` copy (five files, package `criteria.v1`:
  `server`/`orchestrator`/`events`/`criteria`/`peer`) plus its generated Go and
  client/SDK surface — outside the coupling KB-119 named, but of the same
  kind: a vendored duplicate of a wire family whose canonical home is a
  different repo.
- **The engine embeds the viewer binary.** `internal/runstate/viewer.go` pins
  `go:embed all:viewer/dist` — a 692K blob checked into the engine repo — kept
  in sync through a manual "checkout a recorded castle commit and rebuild"
  recipe. The engine's release flow is gated on a hand-performed cross-repo
  dance.
- **The deploy pin lives in the castle/parapet slice.** Parapet is not a
  product; it is a directory inside the castle monorepo (top-level package
  name `parapet`; `packages/run-viewer` publishing `@castle/run-viewer`),
  deployed as a pinned image by the workflow-example fleet manifest.

None of the copies is wrong today. The defect is structural: nothing makes any
of them agree tomorrow, and every change pays the cross-repo coordination cost
twice — once in human effort, once in the risk that the effort is skipped.

## Decision

Six rulings, project lead, 2026-10-03 board-decision session:

1. **Parapet extracts to a standalone repo and product, renamed Kintsugi.** It
   is removed from the castle monorepo wholesale. No more embedded-blob
   coupling: the engine stops carrying the viewer and starts consuming a
   published artifact (see Consequences).
2. **The castle-side proto/SDK surface extracts to its own repo.** Castle's
   vendored `proto/criteria/v1/*.proto` (five files, package `criteria.v1`:
   `server`/`orchestrator`/`events`/`criteria`/`peer`) plus its generated Go and
   castle's client/SDK surface move to a dedicated repository, on the pattern of
   the `criteria-adapter-proto` repo and the published adapter SDKs — the one
   move that removes the vendored server-side copy from the coupling surface
   entirely. The repo is named **`criteria-control-plane-proto`** (adapter-style
   wire-name convention: it names the domain, not a consumer).
3. **Castle repo untouched.** Only parapet extracts and renames; castle stays
   the control plane (ADR-0008 doctrine). A castle rename would churn module
   paths, CI, and deploy manifests for zero product-facing benefit.
4. **The TypeScript package scope renames `@castle/*` → `@kintsugi/*`, bundled
   with the extraction.** One clean break: the rename sweep is an explicit
   acceptance item on the extraction card and covers deep-subpath imports (e.g.
   `@castle/run-viewer/src/test/mocks`), not just published-package names.
5. **The seam vocabulary stops drifting by construction.** The extraction makes
   the shared artifact the one home for the vocabulary; criteria pins its
   producer table against it with a conformance test, and the same artifact
   becomes the viewer source of record (see Consequences).
6. **Sequencing: the record lands first; extractions execute after the v0.6.0
   mechanical wave.** Spec/ADR now, extraction cards afterwards — same repos,
   less concurrent churn. Lane graph adopted as written: engine lane through
   its cards first; the wire-lane card (KB-166) publishes the module KB-170
   gates on; the Kintsugi lane (KB-164 parallel → KB-165 → KB-167) runs as its
   own line; the disarmed validations fire once all lanes land.

### Scope notes (verified 2026-11-14)

- The deploy repin targets **`k8s/06-parapet-fleet.yaml`** — the deployed P340
  fleet line. The legacy hand-maintained manifest `k8s/06-parapet.yaml` was
  retired by KB-372 (2026-11-06; its header comment is in-tree), so the
  original body's pin site is stale: the fleet manifest carries the repin, and
  KB-165's published-artifact consumption is unchanged by the move.
- This record's PR is docs-only by construction (this ADR plus its index row).
  Dependency or toolchain changes that the CI merge gate requires are permitted
  only as their own justified commits attributed to the series card, per the
  2026-11-14 scope ruling — an acceptance criterion forbidding what CI requires
  is a defect, not a guardrail. As committed, this record needed none: the
  release-image builder already carries the exact-pin discipline this commit
  class requires (Dockerfile.runtime's `golang:1.27.4-alpine` build stage held
  in lockstep with go.mod's `toolchain go1.27.4`).

## Consequences

**The coupling class of KB-119 dies three ways.** The viewer vocabulary's
producer and consumer sides gain a shared artifact and a pinning conformance
test; the server-side vendored copy leaves the coupling surface entirely; and
the engine stops embedding a foreign build product.

- **For the engine (criteria):** `internal/runstate/viewer.go`'s 692K
  `go:embed all:viewer/dist` blob and the manual "checkout a recorded castle
  commit and rebuild" recipe are removed. Runs keep their viewer, but its
  source becomes Kintsugi's published run-viewer artifact. `eventvocab` gains a
  conformance test pinning criteria's producer table
  (`payloadTypeToSeamType` / `TerminalSeamEventTypes`) against the shared
  artifact's vocabulary — the drift KB-119 documented becomes a failing test
  instead of a silent production surprise.
- **For the control plane (castle):** the repo is untouched; its vendored
  `proto/criteria/v1` copy and client/SDK surface are extracted to
  `criteria-control-plane-proto` and consumed from there, ending the
  third-copy problem.
- **For Kintsugi:** a standalone repo and product identity. The extraction
  renames the published TS scope `@castle/*` → `@kintsugi/*` in the same break.
- **For the series:** execution is deliberately deferred past the v0.6.0
  mechanical wave; the extraction and consumption cards (KB-164 … KB-168)
  sequence against the other lanes before the disarmed validations fire.

## Sign-off

| Role | Reviewer | Status | Date |
|---|---|---|---|
| Project lead | dave | Accepted | 2026-10-03 |
