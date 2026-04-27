# ADR-0001: Naming convention

- **Status:** Proposed
- **Date:** 2026-04-27
- **Deciders:** Project lead (this repo) + overlord-team reviewer
- **Workstream:** [W01 — Naming convention review](../../workstreams/01-naming-convention-review.md)

## Context

The current brand stack — **overseer** (executor), **overlord**
(orchestrator), **castle** (server), **parapet** (UI) — was chosen for
internal coherence as a fantasy/military metaphor. Internal adoption is
picking up and colleagues are pushing for public releases. Two
structural problems surface as the project approaches public release:

1. **Word-association risk.** Inclusive-documentation guidance (Google,
   Microsoft, Apple style guides) explicitly flags terms like
   "overseer" (US slavery connotation) and "overlord" (authoritarian)
   as terms to avoid. An offensive or charged name produces adoption
   friction independent of technical merit — see the GIMP case for the
   long tail of that friction.
2. **Mental brand tax.** Four unique, metaphor-coupled names force
   every new contributor to memorize four distinct words *and* a
   metaphor map ("castle = server, parapet = UI"). The
   "Branded House vs House of Brands" framing argues for collapsing
   this into one top-level brand with descriptive sub-component names
   — the Kubernetes (k8s, kubectl, kubelet, kube-proxy) pattern, also
   adopted by Eclipse after its alphabetical-release-name era was
   widely criticised for the same cognitive-load reason.

The window is now: while the only consumer is the overlord team, the
cost of a rename is one paired PR. Once external consumers exist
(Docker images, npm scopes, blog posts, search engine indexing) the
cost grows quickly.

This ADR is **decision and documentation only**. The rename itself,
if any, is a separate phase scheduled after this ADR reaches
`Accepted`.

## Considered options

### Option 1 — Keep current names

Document the rationale, close the door.

- **Pros:** zero migration cost; existing internal coherence preserved.
- **Cons:** "overseer" and "overlord" remain on the user-visible
  surface; mental brand tax remains for new contributors and external
  users; no opportunity to consolidate brand equity into a single
  searchable identity; the "window is now" argument applies just as
  strongly later, only at higher cost.

### Option 2 — Branded House with new top-level brand *(recommended)*

One top-level brand; descriptive component names underneath.

| Today | Proposed |
|---|---|
| overseer (executor / CLI) | `<brand>` (the CLI is the user-facing entry point; carries the bare brand name) |
| overlord (orchestrator) | `<brand>-orchestrator` (separate repo's call; this ADR coordinates) |
| castle (server) | `<brand>-server` |
| parapet (UI) | `<brand>-ui` |

- **Pros:** removes both socially charged words in one stroke;
  eliminates the metaphor-lookup tax for new contributors;
  concentrates brand equity into one searchable identity; matches
  patterns users already know from k8s, helm, terraform; leaves room
  for a memorable top-level brand without forcing the metaphor onto
  every component.
- **Cons:** loses the fantasy/military internal-coherence story
  (intentional cost); requires paired PRs across this repo and the
  overlord repo; one-time disruption for the small set of current
  internal users.

### Option 3 — Rename only the user-visible parts; keep `overseer` as Go module path

Rename binaries, env vars, brand prose; leave `github.com/brokenbots/overseer`.

- **Pros:** cheaper migration (no module-path churn for the overlord
  importer).
- **Cons:** permanent skew between marketing name and import path.
  Every Go consumer reads `import "github.com/.../overseer"` while
  the docs and CLI say `<brand>`. This is the "House of Brands"
  failure mode — the rename window is now precisely so we don't carry
  this skew for the rest of the project's life.
- **Verdict:** rejected.

### Option 4 — Keep `overseer` as the top-level brand; only descriptivize sub-components (drop castle, parapet, overlord)

Resolve the mental brand tax but leave the primary word-association
concern.

- **Pros:** narrower migration scope.
- **Cons:** "overseer" is the most flagged term in the inclusive-doc
  guidance cited above; option 4 keeps it on every binary, env var,
  proto package, and import path. This option resolves the secondary
  problem and ignores the primary one.
- **Verdict:** rejected.

## Decision

**Adopt Option 2 — Branded House. Top-level brand: `criteria`.**

The component-name pattern:

- `criteria` — the CLI binary and primary marketing surface.
- `criteria-orchestrator` — the orchestrator (formerly overlord).
- `criteria-server` — the server (formerly castle).
- `criteria-ui` — the UI (formerly parapet).
- `criteria-adapter-<plugin>` — adapter binaries (today's pattern
  carried forward unchanged in shape; only the prefix changes).

Rationale, against the Appendix B criteria:

- **Sentence-readable CLI.** `criteria apply <workflow>`,
  `criteria validate <workflow>`, `criteria compile <workflow>` all
  parse as English. For a tool whose primary surface is a daily-use
  CLI, the ergonomic win is felt every command — it is the strongest
  single argument for this name and the deciding factor over
  fanciful alternatives.
- **Semantic fit.** A workflow file is a set of criteria the run must
  meet. The metaphor is mild but it does not fight the user.
- **Cultural audit clean.** Neutral, professional, no political or
  historical baggage in any major market; no slang collisions in
  EN/ES/DE/FR/JA on a desk-research pass. Reads as something a
  regulated-industry CTO would not flag.
- **Open in the relevant ecosystem.** GitHub-org, npm-scope, and
  Docker-Hub-org checks are still required before merge (Appendix C),
  but the software-side surface is materially less crowded than it is
  for the rejected candidates.

Acknowledged cost (the common-word tradeoff):

- **Search ownability is weak.** "Criteria" is a high-volume English
  word and Criteria Corp owns the dictionary-form .com domain. SEO
  for the bare name will be a content/stars/Docker-pulls effort
  rather than a free win. This is the Chef/Puppet precedent: both
  succeeded with common-word names, but the search-ownability work
  fell to the project rather than the name.
- **Criteria Corp collision is bounded.** Criteria Corp is an HR /
  pre-employment-assessment platform serving 4,000+ customers. It
  files in USPTO class 35/45 (employment services); a workflow
  execution tool would file in class 9/42 (software). Cross-class
  registrations don't usually trigger a §2(d) refusal, so the
  collision is *SEO-real but trademark-bounded*. The in-domain
  workflow space (n8n, Camunda, Airflow, Conductor, Argo, Temporal,
  go-vela) has no "criteria"-named player; queries scoped to the
  domain (`"criteria workflow"`, `"criteria apply CLI"`) return the
  project quickly once content exists.
- **Trademark defensibility is weaker than for a fanciful mark.** A
  registration in USPTO class 9/42 is plausible because "criteria"
  is not directly descriptive of workflow execution, but the mark
  will be narrower than a coined-word mark would have been.

This cost is explicitly accepted in exchange for the daily-use CLI
ergonomics.

### Why not a conflict-cleaner alternative

A sanity-check pass considered `mandate` as a candidate with a
materially cleaner SEO and trademark profile than `criteria`. It was
rejected on **hard-gate criterion 2 (cultural audit)**: `mandate`
carries an authoritative, top-down register that triggers
psychological reactance — the same tone problem this rename is
specifically designed to escape from `overlord`. Trading "overlord"
for "mandate" would resolve the conflict-density issue while
reintroducing the original tone problem.

`criteria`, by contrast, sits at the opposite end of the agency
spectrum: evaluative rather than commanding, high-agency for the
user (the user *meets* the standard rather than having an order
*done to* them), and "almost invisible" in its neutrality. The
conflict cost of `criteria` is preferred over the tone cost of any
authoritative-register alternative. Other close-neighbour candidates
considered (Charter, Dictum, Rubric, Premise, Tenet, Axiom, Codex,
Doctrine) are recorded in Appendix C with their individual rejection
reasons.

## Consequences

### What changes when the rename phase lands

| Surface | Change |
|---|---|
| Go module path | `github.com/brokenbots/overseer` → `github.com/brokenbots/criteria` (also `/sdk`, `/workflow`) |
| Binaries | `overseer`, `overseer-adapter-{copilot,mcp,noop}` → `criteria`, `criteria-adapter-{copilot,mcp,noop}` |
| Env vars | 15 `OVERSEER_*` vars → `CRITERIA_*` (incl. `OVERSEER_CASTLE_URL` → `CRITERIA_SERVER_URL` to absorb the castle-rename in the same pass) |
| Default state dir | `~/.overseer/` → `~/.criteria/` |
| Proto package | `overseer.v1` → `criteria.v1`; `OverseerService` → `CriteriaService`; `CastleService` → `ServerService` (or similar — the rename phase finalises) |
| Docs prose | README, AGENTS, CONTRIBUTING, PLAN, docs/* — find/replace + tone pass |
| **Legacy-name eradication** | Every textual occurrence of `overseer`, `overlord`, `castle`, and `parapet` (case-insensitive, all four) is removed from the repository. This includes — but is not limited to — source code identifiers, package paths, struct/field/method names, log strings, error messages, gRPC/proto comments, HCL fixture values, golden-test data, CI workflow files, badge URLs, and AGENTS.md cross-repo references that name the overlord repo (these become references to its renamed counterpart). The only permitted residue is git history itself and this ADR file (which intentionally preserves the old names as the audit trail of the decision). |

### Rename-phase merge gate

The legacy-name eradication row above is a verifiable claim. The
rename workstream's PR must include the output of:

```sh
git grep -i -E 'overseer|overlord|castle|parapet' \
  -- ':!docs/adrs/ADR-0001-naming-convention.md' ':!CHANGELOG.md'
```

returning **zero matches** as a merge gate. Any other file that
needs to keep an old name (e.g., a release-notes section explaining
the rename, or a migration shim that reads `OVERSEER_*` env vars for
one release cycle) goes on an explicit allowlist that the reviewer
signs off on, with each entry justified inline.

The same gate runs in the overlord repo's paired PR with the four
names plus its own old name on the search list.

### What does not change

- HCL DSL keywords (`workflow`, `step`, `adapter`, `input`, `output`)
  — verified to carry no brand coupling.
- Internal Go package layout below the module root (`internal/...`)
  — stays as-is unless individual packages happen to embed brand
  names; spot fixes only.
- Project ownership, license, governance.

### What this unblocks

- **W02 — README and contributor docs.** Runs against final names
  rather than throwing the work away on rename day.
- **W07 — Repo hygiene.** Same.
- **W08 — Publishing (Docker images, releases).** First public surface
  uses the final brand from day one; no `<oldname>` package to
  deprecate later.

### What this blocks

- No public release (Docker Hub, GitHub release tags beyond
  `v0.x`-internal, blog posts, conference mentions) until the rename
  phase lands. This ADR being `Accepted` is the gate; the rename PR
  landing is the unblocker.

## Migration phase placeholder

The rename itself is **not** scheduled by this ADR. A separate
workstream (call it W?? — "Brand rename execution") owns:

- Paired PR with the overlord repo for module path, proto package,
  and env-var changes.
- Env-var compatibility shim (read `OVERSEER_*` as fallback for one
  release cycle, log a deprecation warning) — or hard cutover, the
  rename workstream decides.
- `~/.overseer/` → `~/.criteria/` migration logic on first run.
- Docs find/replace + tone pass.
- Coordinated tag and release notes across both repos.

Phase 0 explicitly does **not** carry the rename unless the workstream
graph is updated to schedule it. Default plan: W02 and W07 run with
current names; the rename workstream lands in a later phase and gets
a final find/replace pass.

## Appendix A — Naming surface inventory

Current state of every place a brand name appears in user-visible
text. This table is the source of truth for the "what changes" list
above.

| Surface | Current state | Rename impact |
|---|---|---|
| Go module path | `github.com/brokenbots/overseer` (+ `/sdk`, `/workflow`) | High |
| Binaries | `overseer`, `overseer-adapter-{copilot,mcp,noop}` | Medium |
| Env vars | 15 `OVERSEER_*` (incl. `OVERSEER_CASTLE_URL`, `OVERSEER_CASTLE_CODEC`, `OVERSEER_CASTLE_TLS`, `OVERSEER_PLUGIN[S]`, `OVERSEER_COPILOT_*`, `OVERSEER_WORKFLOW`, `OVERSEER_TLS_*`, `OVERSEER_NAME`, `OVERSEER_LOG_LEVEL`, `OVERSEER_STATE_DIR`, `OVERSEER_OUTPUT`) | Medium |
| Default state dir | `~/.overseer/` (overridable via `OVERSEER_STATE_DIR`) | Low |
| Proto package | `overseer.v1` across `proto/overseer/v1/{overseer,castle,events,adapter_plugin}.proto`; services `OverseerService`, `CastleService`, `AdapterPluginService` | High |
| Docs prose | `README.md`, `AGENTS.md`, `CONTRIBUTING.md`, `PLAN.md`, `docs/workflow.md`, `docs/plugins.md` | Low |
| HCL DSL keywords | None brand-coupled | Zero |
| TS bindings | None exist | Zero |
| Docker / goreleaser / ko.yaml | None exist | Zero |
| `parapet` usage | Zero occurrences in repo (UI not yet built) | Zero |
| Cross-repo refs | `AGENTS.md` mentions overlord (cross-repo); `PLAN.md` discusses split history | Low |

## Appendix B — Top-level brand selection criteria

Criteria are split into **hard gates** (a candidate that fails any of
these is out) and **scored factors** (weighted on the merits).

### Hard gates

1. **Brevity & pronounceability.** ≤8 characters, ≤4 syllables,
   unambiguous spelling. No typing-penalty acronyms. The CLI is the
   primary surface; this is felt every command.
2. **Cultural audit.** Global-safe, screened against EN, ES, DE, FR,
   JA at minimum. Passes the "would I pitch this to a regulated-
   industry CTO" test. No political, historical, or slang collisions
   in major markets.

### Scored factors

3. **Search ownability.** First-page Google result for the bare name
   within 6 months realistic. Generic English words (`chef`,
   `puppet`, `forge`) score *low* but are not disqualified — Chef
   and Puppet succeeded as category leaders despite the SEO penalty,
   because the project's stars/content/Docker-pulls did the
   ownability work the name didn't. Common-word names are
   acceptable when paid for with another factor (typically
   ergonomics, see factor 4a).
4a. **Registrability — domains and orgs.** `.io`, `.dev`, or `.sh`
    domain available; GitHub org available; npm scope (`@<brand>`)
    available; Docker Hub org (`<brand>`) available. All four
    checked before sign-off.
4b. **Trademark defensibility.** USPTO TESS search in classes 9 and
    42 (software). Fanciful marks score highest; suggestive marks
    are middle; descriptive marks are weakest and may face a 2(e)(1)
    refusal. A weaker mark is not disqualifying — see factor 3 — but
    the cost should be explicit in the Decision.
5. **Brand type.**
   - **5a. Sentence-readable CLI.** Names that produce English-like
     command lines (`<brand> apply`, `<brand> validate`) earn a
     premium specifically for daily-use CLI tools. This is the
     factor that makes common-word names worth their SEO cost.
   - **5b. Fanciful or repurposed-classical.** Kubernetes, Heptio,
     Terraform, Argo — these names won the trademark and ownability
     tests for free.
   - These two are alternatives, not additions. A candidate scores
     on whichever it leans toward.
6. **Subtle lineage to current metaphor (optional).** Kubernetes kept
   the "Project 7" reference in its seven-spoked logo. Nice to have,
   not load-bearing.

## Appendix C — Candidate shortlist and selection

The shortlist evolved over two passes. The initial five were filtered
against Appendix B criteria, three more candidates were added during
the discussion, and the field narrowed to **Criteria** as the chosen
brand. The full table is preserved here as the audit trail for the
decision.

| # | Candidate | Outcome | Reason |
|---|---|---|---|
| 1 | **Cadre** | Rejected | Cadre.com is an active real-estate platform; "cadre" carries party-cadre political connotation in non-US markets (criterion 2); search ownability weak |
| 2 | **Praxio** | Rejected | Multiple existing software projects already named Praxio — the "fanciful coinage = unowned" assumption did not survive verification (criterion 3 + 4a) |
| 3 | **Tessera** | Rejected | Tessera Inc. (semiconductor IP), Tessera Therapeutics, Tessera Data — crowded across software-adjacent classes (criterion 4b risk) |
| 4 | **Vela** | Rejected | Direct conflict with [go-vela](https://github.com/go-vela/server), an active workflow/CI tool in the same ecosystem (Go) and problem space — disqualifying on criterion 3 |
| 5 | **brokenbots-extension** (`bb-cli` / `brokenbots-cli`) | Rejected | "broken" reads as a defect signal in a product context; couples the project's marketability to the org name forever |
| 6 | **Herald** | Rejected | Reads as a magazine/newspaper brand, not a developer tool — fails the "feel" test for the target audience even though it scores well on the formal criteria |
| 7 | **Cleros** (Greek κλῆρος, "lot/portion") | Rejected | Strong on ownability and lineage but obscure; first-read mispronunciation risk; worse on factor 5a than Criteria |
| 8 | **Mandate** | Rejected | Materially cleaner SEO and trademark profile than Criteria — no major software-space conflict on a desk-research pass. **Fails hard-gate criterion 2 (cultural audit):** authoritative, top-down register triggers psychological reactance and reintroduces the same tone problem the rename is designed to escape from `overlord`. Considered specifically to test whether a conflict-cleaner authoritative-register alternative was worth the tone tradeoff; the answer was no |
| 9 | **Charter** | Rejected | Charter Communications dominates search across consumer markets; the search-only collision is survivable in the software class but the brand would be perpetually disambiguated against the ISP in casual conversation |
| 10 | **Dictum** | Rejected | Distinctive Latin word, probably clean field, but slightly more authoritarian than Mandate (and Mandate already fails on tone); also obscure with mispronunciation risk on first read |
| 11 | **Rubric** | Rejected | Worse conflict density than Criteria — Rubric.com is an active LangOps platform and Rubrik (alt-spelling) is a major data-protection company; both are dev-adjacent and both spellings collide in software-developer search context |
| 12 | **Premise** | Rejected | Drowned by "on-premise" / "on-premises" SEO across every adjacent product category; Premise.com (data company) compounds the search problem |
| 13 | **Tenet** | Rejected | Tenet Healthcare (giant) plus the 2020 Christopher Nolan film — search context is permanently captured by both |
| 14 | **Axiom** | Rejected | Direct dev-tools collision: Axiom.co is an active log-management/observability platform |
| 15 | **Codex** | Rejected | Direct dev-tools collision: OpenAI Codex |
| 16 | **Doctrine** | Rejected | Direct dev-tools collision: Doctrine ORM is a major PHP project; same query depth captures both |
| 17 | **Criteria** | **Selected** | Sentence-readable CLI surface (`criteria apply <workflow>`); semantic fit with workflow files as criteria; cultural audit clean (high-agency, evaluative tone — opposite end of the friction spectrum from "overlord"); software-side surface materially less crowded than #1–4 and #11–16. The Criteria Corp collision is bounded — cross-class for trademark, in-domain workflow space empty. Acknowledged cost: weaker SEO and narrower trademark mark than a fanciful name — accepted in exchange for daily-use CLI ergonomics and the anti-authoritarian tone (factor 5a + criterion 2 together beat factor 3 for this product class) |

### Pre-merge verification still required

Before this ADR flips from `Proposed` to `Accepted`, the project lead
must complete and record the results of:

- `whois criteria.io`, `whois criteria.dev`, `whois criteria.sh` —
  identify which top-level domain the project will use.
- `gh repo view brokenbots/criteria` (will fail until created) plus
  a check that no existing public GitHub org `criteria` blocks a
  future `github.com/criteria` move if the project ever leaves the
  brokenbots org.
- `npm view criteria` and `npm view @criteria/*` — establish whether
  the bare package name and scope are available.
- Docker Hub `criteria` org availability.
- USPTO TESS search in classes 9 and 42 — record any citation that
  would block registration. Descriptive-mark risk under §2(e)(1) is
  expected to be the main exposure; the Decision section already
  acknowledges a weaker mark.

These results are recorded inline below this paragraph before
sign-off; a clean sweep is not required (the cost has been accepted),
but the *actual state* must be documented so the rename phase plans
around real conflicts rather than assumed ones.

---

## Sign-off

| Role | Reviewer | Status | Date |
|---|---|---|---|
| Project lead (overseer repo) | _TBD_ | _Pending_ | _Pending_ |
| Overlord-team representative | _TBD_ | _Pending_ | _Pending_ |

This ADR may not be merged in `Proposed` state. Both sign-offs above
are required before flipping to `Accepted`. The chosen top-level
brand (`criteria`) was filled into the Decision section during drafting.
