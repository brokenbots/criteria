# ADR-0005 — Workflow source model, fetch/cache, and provenance

**Status:** Accepted

**Date:** 2026-09-18

**Deciders:** Project lead (this repo) — normative plan CRI-214 (sections 3.1–3.8 and the section 6 working defaults)

**Workstream:** M0.1 (CRI-215) — first milestone of the Workflow URL Loading and
Operator Routing series (CRI-215..CRI-251). This ADR is the M0 contract of that
series and blocks every later ticket in it; nothing downstream lands before it
merges.

---

## Context

A criteria run today obtains its entry workflow one of two ways: a local
workflow directory handed to the CLI (`criteria apply <dir>`), or a workflow
baked into a derived image (the `criteria/runtime` image bakes the binary and
bundled adapters on an Alpine base; derived images may bake workflows and
custom adapters — see [runtime/docker.md](../runtime/docker.md)). Remote
*subworkflow* sources already exist as lock-time machinery, not as a
run-admission path: `criteria adapter lock` recursively materializes remote
git/archive subworkflow trees through an in-CLI fetcher
([internal/cli/subworkflow_fetch.go](../../internal/cli/subworkflow_fetch.go))
behind an unexported `workflowFetcher` interface
([internal/cli/adapter_lock_recursive.go](../../internal/cli/adapter_lock_recursive.go)),
caches them under `cache/workflows/<slug>/<resolved>` and records a
`workflow_ref` pin (`workflow/lockfile.LockedWorkflowRef`) in the parent's
`.criteria.lock.hcl`. The compiler itself still accepts local paths only
([workflow.md → Source schemes](../workflow.md#source-schemes)).

Three gaps block the series:

1. **No source model for the run itself.** The operator-facing assignment
   surface (`SubmitWorkflowAssignment` in
   [proto/criteria/v1/server.proto](../../proto/criteria/v1/server.proto))
   carries raw workflow source text; there is no normative answer to how a run
   obtains its workflow from a URL, how that composes with images, or which
   failure modes reject the run.
2. **No cache lifecycle.** `cache/workflows/` accumulates `<slug>/<version>`
   trees with no index and no sweep; the OCI adapter cache, by contrast, has an
   `index.json` reachability model and explicit GC
   (`criteria adapter prune`, [internal/adapter/oci/gc.go](../../internal/adapter/oci/gc.go)).
3. **No provenance and no pin enforcement for the entry workflow.** Adapter
   pins are enforced from the lockfile, but a run's workflow-source origin —
   where it came from, what exact content, fetched when, under which image —
   is recorded nowhere, and an expected pin on fetched workflow content is not
   checked before execution.

The normative plan (CRI-214) settled these questions in its sections 3.1–3.8
and closed three formerly-OPEN working defaults in its section 6. This ADR
records them as the binding decisions of the series.

## Decision

Decisions D1–D8 map to plan sections 3.1–3.8; decisions D9–D11 record the
three formerly-OPEN working defaults (plan section 6) as decided, each with
its rationale. A later ticket in the series may not contradict any of them
without superseding this ADR.

| Plan section | Topic | Decision |
|---|---|---|
| §3.1 | Source model — three modes | D1 |
| §3.2 | Mode selection and declaration | D2 |
| §3.3 | Fetch mechanism — go-getter behind `workflowFetcher` | D3 |
| §3.4 | Cache layout — `cache/workflows/<slug>/<version>` | D4 |
| §3.5 | Cache index file | D5 |
| §3.6 | Provenance via `RunMetadata` | D6 |
| §3.7 | Expected-pin enforcement fails closed | D7 |
| §3.8 | Validation posture for fetched workflows | D8 |
| §6 (former OPEN) | "In Review" counts as clean | D9 |
| §6 (former OPEN) | Triage-class concurrency | D10 |
| §6 (former OPEN) | Orphan sweep cadence | D11 |

### D1 (§3.1) — Source model: exactly three modes

Every run's workflow source is exactly one of:

- **image-only** — the workflow HCL is baked into the container image. Nothing
  is fetched at run time. The trust anchor is the image itself (reference +
  digest); the run is as trustworthy as the image it starts from.
- **url-only** — the workflow is fetched from a URL at run admission and
  executed on the minimal base image (`criteria/runtime` with binary and
  bundled adapters, no baked workflow). The trust anchor is fetch integrity:
  resolved immutable version, content digest, lockfile pins, and validation.
- **url+image** — the URL is the content and the image is the process. The
  image supplies the runtime and adapter set; it never supplies workflow
  content, and the URL never supplies a process. The image operator's trust
  anchor governs execution tooling; the fetch trust anchor governs content.

Exactly one mode applies per run, and the mode is part of the run's recorded
provenance (D6). The three modes are the whole model: a run cannot be
"mostly image with a URL patch" — a derived image that carries workflow
content is image-only, and its content changes only by shipping a new image.

**Rationale.** Two orthogonal inputs (content, process) with three
meaningful combinations is the minimum model that covers the deployment
shapes the series must serve: sealed/air-gapped environments (image-only),
operator-submitted URLs against a shared minimal runtime (url-only), and
pinned runtime + pinned content (url+image). Anything finer-grained (per-file
mixing of baked and fetched content) would make provenance ambiguous and pin
enforcement ill-defined.

### D2 (§3.2) — Mode selection and declaration

The mode is **declared, not inferred**. A run specification (CLI invocation
or workflow assignment) states its source inputs explicitly:

- URL present, no image override → **url-only** on the minimal base image.
- URL and image both declared → **url+image**.
- No URL → **image-only**; the run uses the image-baked workflow. If a run
  that is intended image-only carries a URL, admission rejects it (fail
  closed) rather than silently fetching.

An underspecified or contradictory declaration (e.g. neither baked workflow
nor URL available in the image) is rejected at admission — the assignment
ends in the existing `REJECTED` state ("invalid source") and no run is
created. Mode selection never guesses.

**Rationale.** Inferred modes are how provenance goes stale: if the runtime
silently falls back between baked and fetched content, the recorded origin no
longer describes what actually ran. Explicit declaration keeps D6's record
authoritative and gives the fail-closed posture of D7 a well-defined trigger.

### D3 (§3.3) — Fetch mechanism: go-getter behind `workflowFetcher`

Remote workflow fetching adopts
[hashicorp/go-getter](https://github.com/hashicorp/go-getter) as the transport
layer, used **behind the existing `workflowFetcher` abstraction**. The seam is
the current interface — `Fetch(ctx, callerDir, source) (dir, *LockedWorkflowRef,
error)` in
[internal/cli/adapter_lock_recursive.go](../../internal/cli/adapter_lock_recursive.go) —
and it is the only surface call sites see; the hand-rolled git/HTTP archive
paths inside `defaultWorkflowFetcher` are replaced by go-getter
implementations of the same contract. No caller learns that go-getter exists.

The getter registry is a fixed, compile-time allowlist of audited built-ins:
git, HTTP(S) (including archive forms), and file. No custom getters, no
scheme plugins, and no transport outside the allowlist. Local path sources
continue to resolve before the fetcher (the existing
`LocalSubWorkflowResolver` path) and never enter go-getter, so local-source
semantics and path confinement are unchanged. Materialized trees pass through
the same cache layout (D4) and validation pipeline (D8) as today; extraction
stays path-confined, and untrusted-content handling remains host-enforced.

**Rationale.** go-getter removes the bespoke transport code (git
ls-remote/clone plumbing, archive download/extraction) that each new scheme
would otherwise grow by hand, while the interface keeps the blast radius of
the dependency inside one package. The allowlist matters more than the
dependency: the fetcher must never grow a scheme because go-getter happens to
support it — widening the allowlist is a deliberate, reviewed decision.

**Dependency note.** go-getter enters the root module's go.mod when the
implementation milestone lands (this ticket is zero-code). At pin time it
must pass the existing vulnerability gates (`make vuln-scan`, `make
vulncheck`) and the import-boundary lint (`make lint-imports`); the import
lives behind the CLI-side fetcher, not in `workflow/` or `sdk/`.

### D4 (§3.4) — Cache layout

Fetched workflow sources are cached under `$CRITERIA_HOME/cache/workflows/<slug>/<version>/`:

- `<slug>` — a sanitized source identity (the existing `slugify` rules over
  the source URL), stable across fetches of the same source.
- `<version>` — the immutable resolved version: a git commit SHA for git
  sources, or the `sha256:` content digest for archive sources.

The layout is rooted under `CRITERIA_HOME` via
`internal/dirs.CacheWorkflows` (already defined); `CRITERIA_STATE_DIR` and the
legacy `~/.criteria` detection follow the same resolution as every other
criteria path. Trees are written atomically — materialize into a temp
directory, then rename into place — so concurrent runs fetching the same
version are idempotent (first rename wins, losers adopt the winner's tree),
matching the existing fetcher behavior.

**Rationale.** The two-level layout keeps the identity→content split that
makes sweeping (D11) and provenance (D6) decidable from the filesystem alone:
`<slug>` names where content came from, `<version>` names exactly which
content. Version keys are content-derived (SHA or digest), so a cached tree
can never silently change under a recorded pin.

### D5 (§3.5) — Cache index file

Alongside the version trees, `cache/workflows/index.json` records one row per
cached `<slug>/<version>`: source URL, kind (git/archive), resolved ref or
digest, fetch timestamp, on-disk size, last-used timestamp, and the registry
review state (D9). The index mirrors the provenance-annotation pattern of the
OCI adapter cache (`reference`/`source_url` annotations there; explicit fields
here).

The index is **registry bookkeeping, not the run-time authority**. Run-time
authority stays with the lockfile pins (D7); the index exists so that review
state (D9), orphan detection (D11), and provenance reporting (D6) have a
single queryable source. Writes are atomic (temp file + rename); a missing or
corrupt index degrades gracefully — cached trees remain usable for pinned
runs, and the sweep conservatively treats unindexed trees as orphans only
when their version directory is also unreferenced by any known lockfile.

**Rationale.** Without an index, every lifecycle question (what is cached,
when was it fetched, is it reviewed, is it orphaned) degenerates into
filesystem heuristics. The OCI cache already demonstrates the
index-as-reachability model in this codebase; the workflow cache adopts the
same shape rather than inventing a third scheme.

### D6 (§3.6) — Provenance recorded via `RunMetadata`

Every run that loads any non-image-only workflow source records a
**`RunMetadata`** record at admission. The record captures, at minimum:

- the resolved mode (D1) and, for url modes, every source URL fetched
  (entry workflow and each remote subworkflow);
- the resolved immutable version per source (commit SHA / content digest) and
  the kind it was resolved from;
- the fetch timestamp and the image reference + digest the run executes on;
- the lockfile pins in effect (`workflow_ref` and adapter pins from
  `.criteria.lock.hcl`);
- the validation verdict for fetched content (D8) and the review state
  observed at admission (D9).

Image-only runs record the image reference and digest (their only provenance
inputs). Implementation lands with the run-admission milestones of the
series — the surface is a run-scoped metadata record, not per-step event
traffic — but the field set above is fixed here as the contract.

**Rationale.** Operator routing means runs are submitted by parties other
than the executing agent; "what workflow actually ran, from where, on what
image" must be answerable per run after the fact, without re-deriving it from
cache state that sweeping (D11) may have removed. Recording provenance once
at admission — when fetch, pins, and validation are all in scope — is the
single point where the record can be complete and authoritative.

### D7 (§3.7) — Expected-pin enforcement fails closed

When a run carries an expected pin for fetched workflow content — a
`workflow_ref` pin in `.criteria.lock.hcl`, or an operator-declared expected
ref/digest — the fetched content **must** match it. A mismatch, an expected
pin whose content cannot be resolved, or a fetched tree that cannot be
verified against its pin refuses the run **before execution**. Pin
enforcement never degrades to warn-and-continue, for the entry workflow and
for every fetched subworkflow alike.

**Rationale.** The lockfile already has this posture for adapters — a digest
mismatch fails the run; the workflow source deserves no weaker treatment. The
whole value of pinning is that a recorded version is a promise; admitting a
run on a broken promise converts the lockfile into documentation. Fail-closed
is also what makes D9 and D10 safe: the permissive review/concurrency
policies lean entirely on pin and validation checks being absolute.

### D8 (§3.8) — Validation posture for fetched workflows

Fetched workflows are validated by the **identical compile-time pipeline as
local workflows** — parse, compile, and full schema/type validation — after
fetch and before any execution. There is no reduced validation tier for
remote content, no preview mode that skips validation, and no
"run-if-parseable" shortcut. Validation failure refuses admission (fail
closed) and is recorded in `RunMetadata` (D6).

Fetched content is untrusted input for the whole pipeline: extraction is
path-confined, fetched files are never auto-executed, and adapter trust
continues to come exclusively from the lockfile/signing machinery — fetching
a workflow never confers trust on the adapters it names.

**Rationale.** Local and fetched content differ in *transport*, not in the
trust the engine may place in them once admitted; two validation bars would
guarantee that the weaker one becomes the de facto bar. Keeping one bar also
keeps error quality uniform: an invalid workflow fails the same way wherever
it came from.

### D9 (§6, formerly OPEN) — "In Review" counts as clean

The index (D5) records a **review state per workflow source** — a registry
field maintained by the series' operator-review flow, deliberately local to
criteria (it is a review *of the source*, not a mirror of a VCS PR status).
A source whose review state is **"In Review"** — under active operator
review — counts as **clean**: it is admissible without additional gating,
and review-in-progress is not a fail-closed condition.

**Rationale.** Review is an active, accountable operator action, and
exercising the workflow is part of how review happens; blocking runs while a
source is in review would deadlock the review process it exists to serve.
The fail-closed posture is reserved for the two conditions that indicate the
*content itself* is wrong — pin mismatch (D7) and validation failure (D8) —
so the policy stays simple: review state governs *eligibility classes* (this
decision and D10), content checks govern *admission*.

What does **not** count as clean: a first-seen source with no registry state
(it runs in the triage class, D10), and any source that failed pin or
validation checks (D7/D8) regardless of review state.

### D10 (§6, formerly OPEN) — Triage-class concurrency

Sources with no recorded review state — never-reviewed, first-seen content —
run in the **triage class**: a global concurrency cap of **1** (single-flight
across all triage-class runs), operator-overridable upward via
configuration. The cap never applies to clean or In-Review sources (D9),
which run with normal concurrency.

**Rationale.** The triage class exists to bound the blast radius of content
nobody has reviewed yet, while still allowing the one concurrent execution
that triage itself requires. Single-flight is the most restrictive defensible
default: it costs nothing until a second unreviewed source wants to run at
the same moment, and raising the knob is an explicit, conscious operator
decision rather than a silent default. A per-source cap was rejected: it
would let N unproven sources execute in parallel, which is precisely the
scenario the class exists to contain.

### D11 (§6, formerly OPEN) — Orphan sweep cadence

The workflow cache sweep removes **orphans** — `<slug>/<version>` trees
referenced by neither the index (D5) nor any lockfile pin visible to the
sweep — on a **24-hour default cadence**, and on demand. Sweeping is never
performed inline on the run path. The sweep is conservative under
uncertainty: when reachability cannot be established (missing index, unread
lockfile), the tree is retained. The 24h cadence is a default, operator-tunable
like D10's cap.

**Rationale.** Inline sweeping would couple run latency to garbage
collection and race with concurrent runs reading cached trees; deferring to a
cadence keeps the run path deterministic. The OCI adapter cache already
separates GC from execution (`criteria adapter prune`); the workflow cache
follows the same separation, with a cadence defaulting the "when" so that
unattended installs do not accumulate unbounded state. Conservatism under
uncertainty keeps a sweep from deleting a tree a pinned run is about to
need — deleting slowly is recoverable, deleting wrongly is not.

## Consequences

**Unblocked by this ADR.** M1.1 (CRI-216) implements D3+D4+D5+D8 (go-getter
behind the fetcher seam, cache layout, index, single validation bar); M2.1
(CRI-223) implements D6+D7 (`RunMetadata` at admission, fail-closed expected
pins) and builds operator routing on the D1/D2 modes; D9–D11 land with the
registry and cache-lifecycle milestones of the series. Every later ticket
implements against these decisions; deviations require superseding this ADR.

**Adopted.** The three-mode model gives every run a precise, recordable
origin; the `workflowFetcher` seam keeps go-getter contained to one package
with an audited getter allowlist; the `<slug>/<version>` + index layout makes
review state, sweeping, and provenance decidable rather than heuristic; and
fail-closed pins give the lockfile authority over workflow content, not just
adapters.

**Costs and limitations.**

- go-getter is a new dependency (lands at M1.1): it must pass the vuln gates
  at pin time, and the getter allowlist is a standing maintenance obligation
  — new schemes are deliberate decisions, not emergent capabilities.
- The index adds a second on-disk source of truth next to the lockfile. The
  split of responsibilities is fixed: lockfile = run-time pin authority,
  index = registry/GC/review bookkeeping. Corrupt-index behavior (D5) is
  deliberately degrade-safe.
- The triage-class concurrency cap (D10) is a throughput restriction on
  unreviewed content; operators with heavy triage workloads must raise it
  explicitly.
- The sweep cadence (D11) trades disk retention between sweeps for run-path
  determinism; very cache-sensitive environments tune the cadence down and
  invoke on-demand sweeps in maintenance windows.
- Image-only provenance is only as strong as the image reference + digest
  (D6); content-addressable workflow attestation inside images is out of
  scope for this ADR.

## Related

- [ADR-0003](ADR-0003-conformance-scope.md) — conformance scope; fetched-source
  behavior is exercised by host tests once the implementing milestones land.
- [ADR-0004](ADR-0004-adapter-tools.md) — adapter-as-tool contract; adapter
  trust boundaries referenced by D8 are unchanged by this ADR.
- [internal/cli/subworkflow_fetch.go](../../internal/cli/subworkflow_fetch.go) —
  the existing fetcher implementation D3 replaces the transports of.
- [internal/cli/adapter_lock_recursive.go](../../internal/cli/adapter_lock_recursive.go) —
  the `workflowFetcher` interface D3 binds go-getter behind.
- [workflow/lockfile](../../workflow/lockfile/types.go) — `LockedWorkflowRef`
  (`workflow_ref` pins), the expected-pin surface D7 enforces.
- [internal/dirs/dirs.go](../../internal/dirs/dirs.go) — `CacheWorkflows`, the
  `CRITERIA_HOME` root D4 uses.
- [internal/adapter/oci/gc.go](../../internal/adapter/oci/gc.go) — the
  index-reachability GC model D5/D11 mirror; `criteria adapter prune`.
- [proto/criteria/v1/server.proto](../../proto/criteria/v1/server.proto) —
  `SubmitWorkflowAssignment`, the admission surface D2's rejection path uses.
- [runtime/docker.md](../runtime/docker.md) — the `criteria/runtime` minimal
  base image and image-baking model behind D1's modes.

## Sign-off

| Role | Reviewer | Status | Date |
|---|---|---|---|
| Project lead (this repo) | Dave Sanderson (normative plan CRI-214, sections 3.1–3.8 and section 6 defaults) | Accepted | 2026-09-18 |