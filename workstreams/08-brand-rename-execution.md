# Workstream 8 — Brand rename execution

**Owner:** Rename agent (or human committer) · **Depends on:** [W01](01-naming-convention-review.md)–[W07](07-repo-hygiene.md) · **Unblocks:** [W09](09-phase0-cleanup-gate.md).

## Context

[W01](01-naming-convention-review.md) accepted ADR-0001
([docs/adrs/ADR-0001-naming-convention.md](../docs/adrs/ADR-0001-naming-convention.md)),
which adopts the **Branded House** option with `criteria` as the
top-level brand. The ADR placed the rename itself behind a separate
"Brand rename execution" workstream. This is that workstream.

The ADR's "Legacy-name eradication" row is the contract: every textual
occurrence of `overseer`, `overlord`, `castle`, and `parapet`
(case-insensitive) is removed from the repository, except for an
explicit historical-context allowlist. The merge gate is the
`git grep` command in the ADR's "Rename-phase merge gate" section.
This workstream executes the rename, drives that gate to zero, and
hands off to [W09](09-phase0-cleanup-gate.md) for phase close-out.

The rename in this repo proceeds unilaterally. The paired PR in the
overlord repo (renaming its consumer of the proto package, env vars,
and Go module path) is coordinated separately and is not gated by
this workstream's merge — conformance against an unrenamed overlord
will fail transiently until that paired PR lands. That breakage is
acknowledged and accepted; the rename window per the ADR is "now"
precisely because the only consumer is the overlord team.

## Prerequisites

- [W01](01-naming-convention-review.md)–[W07](07-repo-hygiene.md)
  merged on `main`. Their exit criteria are verified.
- ADR-0001 in `Accepted` state.
- `make build`, `make test`, `make test-conformance`,
  `make lint-imports`, `make validate`, `make proto-check-drift` all
  green on `main`.
- Paired-PR coordination with the overlord-repo maintainer is open;
  the overlord-side rename is owned by that maintainer but the proto
  package and module-path changes here are visible to them before
  this lands.
- A working `buf` toolchain (the rename touches generated bindings).

## In scope

The rename touches roughly 170 files. The order below is chosen so
the compiler / `buf` / `go mod tidy` flag mistakes early. Follow it
unless a step is plainly independent.

### Step 1 — Pre-flight snapshot

- [ ] Branch from `main`.
- [ ] Record the baseline:
      `git grep -i -c -E 'overseer|overlord|castle|parapet' | wc -l`
      (file count) and the same without `-c | wc -l` (occurrence
      count). The merge-gate command will drive both to zero outside
      the allowlist.
- [ ] Confirm `git status` is clean and that `make ci` (or the
      equivalent build+test set) passes from `main`.

### Step 2 — Go module path

- [ ] `go.mod` (root): `module github.com/brokenbots/overseer` →
      `module github.com/brokenbots/criteria`.
- [ ] `sdk/go.mod`: same prefix change.
- [ ] `workflow/go.mod`: same.
- [ ] Update every `import "github.com/brokenbots/overseer/..."`
      across the tree to `criteria`.
- [ ] `go work sync` then `go mod tidy` in each module
      (`./`, `sdk/`, `workflow/`).
- [ ] `examples/plugins/greeter/go.mod` (third-party plugin example)
      and any other nested module updated for `replace` / `require`
      lines that reference the old module path.

### Step 3 — Proto sources

- [ ] `proto/overseer/v1/` → `proto/criteria/v1/` (`git mv` the
      directory).
- [ ] Within that directory: `overseer.proto` → `criteria.proto`;
      `castle.proto` → `server.proto`; `events.proto` and
      `adapter_plugin.proto` keep their filenames.
- [ ] `package overseer.v1;` → `package criteria.v1;` in every
      `.proto` file.
- [ ] `option go_package = "...overseer/v1;overseerv1";` →
      `"...criteria/v1;criteriav1";` (or the equivalent style this
      repo uses — check `proto/overseer/v1/*.proto` for the exact
      form before editing).
- [ ] Service rename: `OverseerService` → `CriteriaService`;
      `CastleService` → `ServerService`. RPC names that embed brand
      words (`RegisterOverseer`, `OverseerHeartbeat`, etc.) get the
      same treatment — flag each one in the diff and rename
      consistently.
- [ ] `buf.yaml`: `name: buf.build/brokenbots/overseer` →
      `name: buf.build/brokenbots/criteria`. Comments referencing
      "Overseer" → "Criteria" or rephrase to remove the brand-word.

### Step 4 — Generated bindings

- [ ] `make proto` regenerates into `sdk/pb/criteria/v1/...` based
      on the renamed proto sources and `paths=source_relative`.
- [ ] `git rm -r sdk/pb/overseer/` once the new tree is in place
      and contains the regenerated output.
- [ ] Connect-Go bindings: directory and file names follow the proto
      file names: `sdk/pb/criteria/v1/criteriav1connect/{criteria,server,adapter_plugin}.connect.go`.
- [ ] `make proto-check-drift` clean.

### Step 5 — Command directories and Makefile

- [ ] `cmd/overseer/` → `cmd/criteria/` (`git mv`).
- [ ] `cmd/overseer-adapter-copilot/` → `cmd/criteria-adapter-copilot/`.
- [ ] `cmd/overseer-adapter-mcp/` → `cmd/criteria-adapter-mcp/`.
- [ ] `cmd/overseer-adapter-noop/` → `cmd/criteria-adapter-noop/`.
- [ ] `Makefile`: `bin/overseer` → `bin/criteria`; `bin/overseer-adapter-*`
      → `bin/criteria-adapter-*`; `./cmd/overseer-adapter-*` glob →
      `./cmd/criteria-adapter-*`; comments and `@echo` strings
      retoned. Re-check `make build`, `make plugins`,
      `make example-plugin` after edits.
- [ ] `.gitignore`: any `bin/overseer*` patterns updated.

### Step 6 — Internal package renames

- [ ] `internal/transport/castle/` → `internal/transport/server/`
      (`git mv`). Update the package declaration and every importer.
- [ ] Spot-rename other `internal/...` packages whose directory or
      file names embed brand words (none expected by ADR Appendix A,
      but verify with `git ls-files internal/ | grep -iE
      'overseer|overlord|castle|parapet'`).

### Step 7 — Source identifier sweep

The compiler is the oracle for this step. After Steps 2–6 the build
will fail with a list of unresolved references; resolve them by
renaming identifiers in line with the brand:

- [ ] Struct, field, method, constant, and variable names that embed
      `Overseer`, `Overlord`, `Castle`, or `Parapet` get renamed to
      `Criteria` / `Orchestrator` / `Server` / `UI` (or to a
      descriptive name where the brand was the only signal).
- [ ] Log messages, error strings, comments, and docstrings that
      mention any of the four legacy names get rewritten. Many of
      these are user-visible (CLI help text, `--help` output, error
      surfaces) — rewrite them to the new brand verbatim, do not
      leave them as a trailing TODO.
- [ ] `make build`, `make plugins`, `make test -race ./...`,
      `make test-conformance` green at the end of this step.

### Step 8 — Environment variables

- [ ] All 15 `OVERSEER_*` env vars renamed to `CRITERIA_*`. The
      castle-coupled variants pick up the server rename in the same
      pass:
      - `OVERSEER_CASTLE_URL` → `CRITERIA_SERVER_URL`
      - `OVERSEER_CASTLE_CODEC` → `CRITERIA_SERVER_CODEC`
      - `OVERSEER_CASTLE_TLS` → `CRITERIA_SERVER_TLS`
      - `OVERSEER_TLS_*` → `CRITERIA_TLS_*`
      - `OVERSEER_PLUGINS`, `OVERSEER_PLUGIN`, `OVERSEER_COPILOT_*`,
        `OVERSEER_WORKFLOW`, `OVERSEER_NAME`, `OVERSEER_LOG_LEVEL`,
        `OVERSEER_STATE_DIR`, `OVERSEER_OUTPUT` → `CRITERIA_*`
        equivalents.
- [ ] No compatibility shim. Hard cutover. ADR-0001 leaves the
      shim-vs-cutover call to this workstream; the consumer set is
      one team, the renaming is mechanical, and a shim doubles the
      surface area for tests. Mention the cutover prominently in the
      release notes ([W09](09-phase0-cleanup-gate.md) authors them).
- [ ] Confirm with `grep -rn 'OVERSEER_' --include='*.go'
      --include='*.md' --include='*.proto' --include='*.hcl'
      --include='Makefile'` that no `OVERSEER_*` references remain.

### Step 9 — Default state directory

- [ ] `~/.overseer/` references → `~/.criteria/` across code, docs,
      and CLI help text. The plugin-discovery search path
      (`~/.overseer/plugins/` → `~/.criteria/plugins/`) is part of
      this.
- [ ] No automatic migration. A one-line README/CHANGELOG note tells
      operators to `mv ~/.overseer ~/.criteria` if they have local
      state to preserve. Internal-only consumers; first-run code
      complexity is not justified.

### Step 10 — Examples, fixtures, golden test data

- [ ] `examples/*.hcl`: any reference to `overseer`/etc. (binary
      name, env var, narrative comment) updated. Check
      `examples/demo_tour_local.hcl`,
      `examples/workstream_review_loop.hcl` specifically — they
      carry the densest narrative.
- [ ] `examples/plugins/greeter/`: README, `go.mod`, `main.go`,
      `example.hcl` updated for the new module path and binary
      naming (`overseer-adapter-greeter` → `criteria-adapter-greeter`).
- [ ] `internal/cli/testdata/plan/*.golden` regenerated: these
      golden files embed binary names and env-var names. Run the
      relevant test with `-update` (or the project's golden-update
      flag) and inspect the diff before committing — golden updates
      should match the rename pattern and nothing else.
- [ ] `internal/cli/testdata/compile/`: same treatment for any
      golden compile output.
- [ ] All `*_test.go` files referencing brand strings updated.

### Step 11 — Documentation prose

- [ ] `README.md` — rebrand and tone pass. Coordinate with the W02
      rewrite (which already ran with the old brand): replace
      "overseer" with "criteria", rephrase any "Castle" → "server",
      "overlord" → "orchestrator". The ADR-0001 link stays.
- [ ] `CONTRIBUTING.md` — same.
- [ ] `AGENTS.md` — same. Cross-repo references to
      `github.com/brokenbots/overlord` become references to its
      renamed counterpart (coordinate with the overlord maintainer
      for the final repo URL; until they confirm, link the issue
      tracking the rename).
- [ ] `SECURITY.md` — rebrand.
- [ ] `docs/workflow.md`, `docs/plugins.md` — rebrand.
- [ ] `PLAN.md` — rebrand. (W09's coordination-set edits supersede
      structural changes; this step is mechanical text only.)

### Step 12 — `.github/` and CI

- [ ] `.github/workflows/ci.yml`: matrix entries, job names, cache
      keys, artifact names referencing `overseer` → `criteria`.
      Re-run the CI lane locally (`make ci`) after edits.
- [ ] `.github/CODEOWNERS`: paths use the new directory names
      (`/proto/criteria/`, `/sdk/pb/criteria/`, etc.).
- [ ] `.github/ISSUE_TEMPLATE/bug_report.md` — version line
      `overseer --version` → `criteria --version`; brand prose
      retoned.
- [ ] `.github/ISSUE_TEMPLATE/config.yml` and the PR template —
      brand strings updated.
- [ ] `.github/agents/workstream-executor.agent.md` and
      `.github/agents/workstream-reviewer.agent.md` — any pinned
      examples or path references updated. The directive set itself
      stays unchanged unless a directive embeds a brand word as
      load-bearing content.

### Step 13 — Cross-repo coordination artifacts

- [ ] AGENTS.md "high-value files" pointers and the "talking to a
      Castle-compatible orchestrator" / "Castle-compatible" phrasing
      retoned ("server-compatible orchestrator" or simply
      "orchestrator").
- [ ] If the overlord repo is itself being renamed in lockstep,
      update the URL in AGENTS.md and README to the new repo URL.
      If the overlord rename lands later, leave a note in
      `docs/adrs/ADR-0001-naming-convention.md` Sign-off section
      ("overlord-side rename pending — link will update at <PR>").

### Step 14 — Run the merge gate

The ADR's gate is the contract:

```sh
git grep -i -E 'overseer|overlord|castle|parapet' \
  -- ':!docs/adrs/ADR-0001-naming-convention.md' \
     ':!CHANGELOG.md' \
     ':!workstreams/0[1-9]-*.md' \
     ':!workstreams/archived/'
```

- [ ] Output is empty. Anything that surfaces is one of:
      - a missed rename — fix it;
      - intentional historical narrative in a workstream file
        (allowlist already covers `workstreams/0[1-9]-*.md` and
        `workstreams/archived/`);
      - a release-notes line in `CHANGELOG.md` (if W07 introduced
        one) — allowlisted;
      - an ADR-0001 audit-trail line — allowlisted.
- [ ] If the rename surfaces a file the allowlist needs to grow to
      cover (e.g. a migration-notes doc, a deprecation example),
      add it to the gate command above with a one-line
      justification in this workstream's reviewer notes. Do not
      expand the allowlist silently.
- [ ] `make ci` (or full lane: `make build plugins proto
      proto-lint proto-check-drift test test-conformance
      lint-imports validate example-plugin`) green.

### Step 15 — Repo rename (operator action)

The GitHub repo rename is a Settings action by the org owner; the
executor cannot perform it. Either path is acceptable:

- **Rename now.** Owner renames `brokenbots/overseer` →
  `brokenbots/criteria`. GitHub serves redirects for the old URL
  but `go install` consumers must update the import path. Push the
  W08 PR after the rename so the new module path resolves on first
  fetch.
- **Defer to W09.** Land W08 with the new module path; rename the
  repo as part of W09's tag/publish step. Module path resolution
  fails between merge and rename — acceptable for an internal
  consumer set.

Whichever path is chosen, document the operator step inline so
[W09](09-phase0-cleanup-gate.md) can verify it landed.

## Out of scope

- Tagging `v0.1.0` and archiving Phase 0 workstream files. That is
  [W09](09-phase0-cleanup-gate.md).
- Authoring the CHANGELOG entry for the rename. The CHANGELOG is on
  the W07/W09 axis; this workstream's reviewer notes are the source
  material from which W09 drafts the entry.
- Renaming the overlord repo or its internals. That repo's rename is
  owned by its maintainer; this workstream coordinates timing only.
- Rewriting docs *content* beyond the rebrand sweep. Substantive doc
  rewrites belong in W02 (already shipped) or in a Phase 1 doc
  workstream.
- Adding a deprecated-env-var compatibility shim. Step 8 explicitly
  rejects it; revisit only if a downstream consumer surfaces a
  blocker.

## Files this workstream may modify

This workstream modifies essentially every file in the repository.
The "files NOT to modify" set still applies in spirit — coordination
documents (`README.md`, `PLAN.md`, `AGENTS.md`,
`workstreams/README.md`) get the mechanical rebrand sweep here, but
their structural edits (Phase-0-closed footer, archived-workstream
links, status snapshot updates) are reserved for W09.

Explicit allowlist of files that **keep** legacy-brand text after
this workstream:

- `docs/adrs/ADR-0001-naming-convention.md` — ADR audit trail.
- `CHANGELOG.md` — release notes line for the rename (if present).
- `workstreams/0[1-9]-*.md` — historical narrative for Phase 0.
- `workstreams/archived/**` — historical workstream files (W09
  archives Phase 0 here).
- `.git/**` — git history, by definition out of scope for textual
  rewriting.

## Tasks

- [ ] Pre-flight snapshot recorded.
- [ ] Steps 2–13 executed in order, with `make ci` green at the end.
- [ ] Merge-gate command (Step 14) returns zero matches outside the
      allowlist.
- [ ] CLI smoke: `./bin/criteria apply examples/hello.hcl
      --events-file /tmp/events.ndjson` exits 0.
- [ ] `make example-plugin` green.
- [ ] Reviewer notes capture: (a) the diff size, (b) any allowlist
      additions with justifications, (c) the operator step for the
      GitHub repo rename (done now or deferred to W09), (d) the
      paired-PR status in the overlord repo.

## Exit criteria

- Every checkbox above ticked on the W08 branch.
- `git grep -i -E 'overseer|overlord|castle|parapet'` outside the
  allowlist returns zero.
- `make build && make plugins && make test && make test-conformance
  && make lint-imports && make validate && make proto-check-drift &&
  make example-plugin` all green.
- Generated bindings live under `sdk/pb/criteria/v1/`; `sdk/pb/overseer/`
  no longer exists.
- Module paths in `go.mod`, `sdk/go.mod`, `workflow/go.mod` all
  rooted at `github.com/brokenbots/criteria`.
- `cmd/criteria/`, `cmd/criteria-adapter-{copilot,mcp,noop}/` exist;
  `cmd/overseer*/` no longer exist.
- Reviewer notes record the post-rename state of the four
  coordination files (their structural close-out happens in W09; the
  rebrand sweep happens here).

## Tests

This workstream introduces no new tests. The validation signal is:

- The full `make ci` lane stays green across the rename.
- Golden files regenerated cleanly — diffs are rename-shaped, not
  behavioural.
- The conformance suite continues to pass against the in-memory
  Subject (cross-repo conformance against the unrenamed overlord
  will fail until its paired PR lands; that breakage is documented,
  not blocking).

## Risks

| Risk | Mitigation |
|---|---|
| Wire-compat break: proto package change is incompatible with the unrenamed overlord | Expected and accepted per ADR-0001. The paired PR in the overlord repo lands in lockstep; conformance is transiently red between merges. |
| `go install github.com/brokenbots/overseer/...` breaks for any external consumer | The ADR explicitly accepts this for pre-1.0 internal-consumer-only state. README documents the new path. |
| Golden test data updates accidentally absorb behavioural changes alongside rename changes | Inspect each golden diff. A rename-only diff is mechanical (same shape, brand words swapped). Anything else is rejected and re-investigated. |
| Repo rename happens before code lands → temporary 404 on the old URL for active clones | GitHub serves redirects for renamed repos; affected only if a contributor's local clone is mid-rebase. Communicate the rename in advance. |
| Allowlist creeps to hide missed renames | The merge-gate command lives in this workstream and in ADR-0001. Each allowlist addition requires a one-line justification in reviewer notes; reviewer rejects unsupported additions. |
| Cross-repo references in AGENTS.md break when overlord rename lags | If the overlord rename lands later, the AGENTS.md link points to the GitHub-redirect path; refresh in W09 or a Phase 1 doc pass. |
| Env-var hard cutover surprises a stale local config | Release notes (W09) call this out prominently. The cutover is mechanical and reversible by export-renaming. |
| `make ci` becomes the only signal — a rename mistake that compiles but breaks at runtime ships through | Run the CLI smoke explicitly (Tasks list) and re-run `make example-plugin` end-to-end. The example plugin exercises the binary name, env var, and state dir on a real path. |
