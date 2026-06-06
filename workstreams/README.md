# Criteria workstreams

The active phase's workstream files live at the top of this directory;
prior phases are in [`archived/`](archived/).

## Status

- **Phase 0** — post-separation cleanup — **closed 2026-04-27**. All nine
  workstreams merged; `v0.1.0` tagged. Archived under [`archived/v0/`](archived/v0/).
- **Phase 1** — stabilization + critical user fixes — **closed 2026-04-29**.
  All eleven workstreams merged; lint baseline burn-down gate clean.
  Archived under [`archived/v1/`](archived/v1/). The `v0.2.0` tag was
  documented but not pushed at this close; it ships at HEAD with the
  combined Phase 1 + Phase 2 work below.
- **Phase 2** — maintainability + unattended MVP + Docker runtime + Copilot
  tool-call finalization — **closed 2026-05-02**. Sixteen workstreams scoped,
  two cancelled (W05, W11). `v0.2.0` tagged at HEAD covering combined Phase 1
  + Phase 2 work. Archived under [`archived/v2/`](archived/v2/).
- **Phase 3** — HCL/runtime rework — **closed 2026-05-06**. All nineteen active
  workstreams merged (W20 skipped); `v0.3.0` tagged. Archived under
  [`archived/v3/`](archived/v3/). See [docs/roadmap/phase-3-summary.md](../docs/roadmap/phase-3-summary.md)
  for full outcomes.
- **v0.3.1** — post-Phase-3 bugfixes + parallel correctness — **closed
  2026-05-xx**. Eleven workstreams (6 bugfix, 4 parallel, 1 QoL). Archived
  under [`archived/v3.1/`](archived/v3.1/).
- **v0.3.2** — pre-Phase-4 feature + tech-debt prep — **closed 2026-05-13**.
  Twelve workstreams (2 doc, 5 feat, 4 tech debt, 1 test). All merged; `v0.3.2`
  tag pending. Archived under [`archived/v3.2/`](archived/v3.2/).

## Phase 2 workstreams (archived)

All Phase 2 workstream files have been moved to [`archived/v2/`](archived/v2/).
See [PLAN.md](../PLAN.md) for the project-level roadmap with per-workstream
links and outcomes.

## Phase 1 workstreams (archived)

All Phase 1 workstream files have been moved to [`archived/v1/`](archived/v1/).

## Phase 0 workstreams (archived)

All Phase 0 workstream files have been moved to [`archived/v0/`](archived/v0/).

## Phase 3 workstreams (archived)

Phase 3 closed 2026-05-06 with `v0.3.0` tagged. All workstream files have been
moved to [`archived/v3/`](archived/v3/). See
[docs/roadmap/phase-3-summary.md](../docs/roadmap/phase-3-summary.md) for the
full per-workstream outcome summary.

Post-phase documentation cleanup workstreams (also archived to `archived/v3/`):

- [doc-01](archived/v3/doc-01-docs-cleanup.md) ✅ — Docs cleanup: runtime/compiler reference and roadmap files.
- [doc-02](archived/v3/doc-02-meta-cleanup.md) ✅ — Docs cleanup: meta/index files (`README.md`, `CONTRIBUTING.md`, `PLAN.md`, `workstreams/README.md`).

## v0.3.1 workstreams (archived)

Post-Phase-3 bugfix and parallel correctness workstreams. All files moved to
[`archived/v3.1/`](archived/v3.1/).

## v0.3.2 workstreams (archived)

Pre-Phase-4 feature and tech-debt prep workstreams, closed 2026-05-13. All files
moved to [`archived/v3.2/`](archived/v3.2/).

- [doc-03](archived/v3.2/doc-03-llm-language-spec.md) ✅ — `docs/LANGUAGE-SPEC.md` + `spec-gen` tool.
- [doc-04](archived/v3.2/doc-04-llm-prompt-pack.md) ✅ — LLM prompt pack (8 curated HCL examples).
- [feat-01](archived/v3.2/feat-01-templatefile-function.md) ✅ — `templatefile(path, vars)` HCL function.
- [feat-02](archived/v3.2/feat-02-fileset-function.md) ✅ — `fileset(path, pattern)` HCL function.
- [feat-03](archived/v3.2/feat-03-hash-crypto-encoding-functions.md) ✅ — 13 hash, encoding, and dynamic HCL functions.
- [feat-04](archived/v3.2/feat-04-while-step-modifier.md) ✅ — `while` step iteration modifier.
- [feat-05](archived/v3.2/feat-05-per-line-console-output.md) ✅ — Per-line console output.
- [td-01](archived/v3.2/td-01-lint-baseline-ratchet.md) ✅ — Lint baseline ratchet 24 → 16.
- [td-02](archived/v3.2/td-02-nolint-suppression-sweep.md) ✅ — `//nolint` suppression sweep (62 → 31).
- [td-03](archived/v3.2/td-03-staticcheck-deprecated-enum.md) ✅ — Staticcheck deprecated-enum cleanup.
- [td-04](archived/v3.2/td-04-todo-closure.md) ✅ — TODO marker closure + lint-no-todos guard.
- [test-02](archived/v3.2/test-02-hcl-parsing-eval-coverage.md) ✅ — HCL parsing and eval coverage gaps.

## Phase 4 — Adapter system v2 (active)

Phase 4 opens the full adapter-system rewrite. Workstream files are in
[`adapter_v2/`](adapter_v2/). See [`adapter_v2/README.md`](adapter_v2/README.md)
for scope, goals, and workstream index.

**Mid-phase archive + review (2026-06-05).** The phase is still open. Completed in-repo
workstreams are archived to [`archived/v4/adapter-v2/`](archived/v4/adapter-v2/) to keep the
active set focused. Archiving is gated on *validated landed code*, not the plan — each
archived WS has an in-repo merge plus visible host/engine/proto code. The remaining set was
then reviewed WS-by-WS against the tree and CI (see findings below).

- **Done & archived:**
  - *Host/engine/proto/wire (merged + code-verified):* **WS01–WS20, WS22, WS26, WS31, WS37**.
    (WS37 confirmed during review — the adapter v1 protocol is fully removed; the
    `proto/criteria/v1` that remains is the unrelated server/run API.)
  - *SDK / publishing / adapter migrations (sessions 2–3, verified 2026-06-05):* **WS25** Go SDK
    (`criteria-go-adapter-sdk` v0.5.1 — extracted, switched over #228, host consumers compile),
    **WS28** publish action (`brokenbots/publish-adapter@v0.1.0` — proven against all 5 adapter
    repos; the WS27-starter-repo linkage in its exit criteria is superseded by the real adapter
    repos), **WS30 / WS32 / WS33 / WS34 / WS35** the five TS adapter migrations (greeter, claude,
    claude-agent, codex, openai — each published as a signed `v0.5.0` OCI artifact via the action,
    in its own repo; *Publish* runs green).
  - *SDK secrets / extraction / multi-language packaging (session 4, 2026-06-05; npm/PyPI
    publishes proceeding **out of band**):* **WS45** copilot secret channel + go-sdk `Secrets`
    accessor (#229, go-sdk v0.5.2); **WS36** copilot extracted + published
    (`criteria-adapter-copilot` v0.5.0, signed OCI) + removed from the monorepo (#230); **WS42**
    shell extracted + published (`criteria-adapter-shell` v0.5.0, signed OCI) + monorepo decoupled
    so no test depends on it (#231); **WS41** proto multi-language packaging (TS protobuf-es +
    Python codegen, gated `publish-langs.yml` verified in CI, SemVer policy, `DEPENDENCIES.md`);
    **WS23** TS SDK (`@criteria/adapter-sdk@0.5.0`, own repo + publish workflow); **WS24** Python
    SDK (`criteria-python-adapter-sdk@0.5.0`, own repo + tag). The npm/PyPI publishes for
    WS23/WS24/WS41 — and the consequent TS/Python SDK proto consumer-switch — are owner-token-gated
    and handled out of band; the publish workflows are wired and skip gracefully until the tokens
    (`NPM_TOKEN`+`@criteria` scope / `PYPI_API_TOKEN`) land.
  - *Remote serving + starter repos + release gates (session 5, 2026-06-06):* **WS21** serveRemote
    across all three SDKs (TS `serveRemote.ts`, Python `serve_remote` reconnect parity, Go
    `ServeRemote` reconnect + `LoadClientTLS` — each merged to its SDK `main`, with tests, README
    sections, and k8s/docker-compose/systemd examples); **WS27** the three starter template repos
    (`criteria-adapter-starter-{typescript,python,go}` — created under `brokenbots`, marked GitHub
    templates, each a build-able hello-world adapter with `publish.yml` + commented `Dockerfile` +
    `examples/remote/`); **WS38** release gates (`release-gates.yml` — Gate 3 reuses the
    `workflow_call`-able `remote-e2e.yml`; Gate 4 publishing-flow loop guarded behind
    `CRITERIA_CI_ENABLED` until the `criteria-ci` org + three `adapter-test-*` clones are
    provisioned; `docs/release-process.md` added).
  - *Publishing infra (verified DONE 2026-06-06):* **WS29** — all three starter repos ship
    `.gitlab-ci.yml.example` (keyless via GitLab `id_tokens`) + a `make publish` target, and
    `docs/adapters.md` documents the three publishing paths (GitHub Actions / GitLab CI / local
    `make publish`). The WS29 Step-3 `criteria/publish-adapter` runtime container image and the
    `scripts/*.sh` are **not built** — `criteria adapter publish` (the CLI) performs manifest
    emit → validate → OCI push → sign in one binary, so a separate runtime image is unnecessary.
  - *Signing completion — WS06 follow-up (PR #244, CI green, 2026-06-06):* **WS46** uniform
    verification override (`--allow-unsigned`/`CRITERIA_ALLOW_UNSIGNED`/workflow `verification`
    attr, `warn` transition default D-WS46-1), **WS47** explicit-key trust + lockfile-as-trust-
    anchor (`trust.hcl`, `policyForPin`/`assertSignerMatchesPin`), **WS48** keyless signing with
    a Rekor transparency-log bundle (verifiable post-cert-expiry; legacy path fails closed). The
    Step-5 flip of the transition default back to `strict` is **deferred** to a follow-up gated on
    the real-OIDC CI run.
- **Remaining in [`adapter_v2/`](adapter_v2/)** — the non-extraction backlog:
  - *Independence + hardening:* **WS43** independence verification (base `main`, post-merge),
    **WS44** CI coverage ratchet (base `main`, floors captured after WS40), **WS39** docs refresh.
  - *Release gates (see WS40 note) — all four now self-contained:* Gate 1 conformance **done**
    (rescoped, [ADR-0003](../docs/adrs/ADR-0003-conformance-scope.md)); Gate 2 in-tree adapters
    (`noop`/`mcp` + examples) covered in `ci.yml` e2e; Gate 3 **WS38** wired into
    `release-gates.yml` (reuses `remote-e2e.yml`) — **needs one `workflow_dispatch` validation
    run on the branch**; Gate 4 **rescoped to a self-contained publish→pull round-trip** (build
    in-tree `noop` → `criteria adapter publish` to an ephemeral local `registry:2` → pull back →
    verify). The `criteria-ci` org + `adapter-test-*` clones + `CRITERIA_CI_ENABLED`/
    `CRITERIA_CI_TOKEN` are **no longer needed** — the real keyless→GHCR publish is validated in
    each adapter repo's own `publish.yml`. **WS40 deliberately holds the `v0.5.0` tag + merge to
    `main`** pending out-of-band manual testing; only the Gate 3/Gate 4 validation runs remain
    before the candidate is green.
  - *Security hardening (new track, scoped 2026-06-06):* **WS49** osv-scanner vulnerability gate
    in CI, **WS50** dependency-freshness policy + automation (latest major.minor + 7-day
    supply-chain cooldown; Go tooling `go list`/`go-mod-outdated`/`gomajor` as the primary
    mechanism, Dependabot demoted to routine minor/patch), **WS51** catch-up upgrades to clear
    the backlog and flip the osv gate to blocking. WS51's actual dependency bumps are sequenced to
    run **after** the v0.5.0 candidate clears manual testing so the RC under test is not disturbed.

### Publishing + extraction progress (2026-06-05, session 2)

Worked the publishing critical path end-to-end and started the independence extraction.

**Versioning correction (important).** These artifacts are **not** v2 products — "v2" is the
*protocol* version (from the proto rework). No stable release exists, so everything is
versioned **`0.5.0`** to track the next criteria release line, not `2.0.0`.

- **WS28 — publish action: DONE.** Reusable **publish-only** composite action
  [`brokenbots/publish-adapter@v0`](https://github.com/brokenbots/publish-adapter) (tagged
  `v0.1.0`). Wraps `criteria adapter publish` (manifest emit → validate → OCI push → optional
  cosign sign). Building stays with the adapter. Self-test green against GHCR.
  - Supporting host fixes landed on `adapter-v2`: cosign signing in `criteria adapter publish`
    (#222), `adapterhost --emit-manifest` (#223), validate-before-push + noop fixture (#224).
- **WS30, WS32–WS36(TS) — adapters PUBLISHED:** greeter, claude, claude-agent, codex, openai
  each build via the action and are **published as `v0.5.0` OCI artifacts on GHCR**. Their
  `publish.yml` was rewired (build SDK sibling → build adapter → publish). First real release
  artifacts. *(Cleanup: prune the earlier `2.0.0-rc.1` test packages + the
  `criteria-adapter-selftest` package — needs `delete:packages` scope.)*
- **WS23 — TS SDK: publish-READY.** `@criteria/adapter-sdk@0.5.0` builds/tests; added manifest
  type-vocab normalization (`bool→boolean`, `list_string→array`) + an npm publish workflow
  (skips gracefully until `NPM_TOKEN` + the `@criteria` npm scope are configured — owner step).
- **WS41 — proto extraction: FOUNDATION done.** New repo
  [`criteria-adapter-proto`](https://github.com/brokenbots/criteria-adapter-proto) (`v0.5.0`):
  standalone Go module with the v2 `.proto` sources + bindings (`package criteriav2`), seeded
  from the live `sdk/pb` copy, smoke-tested. **Switchover not done** (see below).
- **WS25 — Go SDK: FOUNDATION done.** New repo
  [`criteria-go-adapter-sdk`](https://github.com/brokenbots/criteria-go-adapter-sdk) (`v0.5.0`):
  `adapterhost` extracted, builds/tests standalone against `criteria-adapter-proto`. Confirms the
  Go adapter SDK is cleanly separable (only proto + go-plugin + grpc).
- **WS24 — Python SDK: still entirely v1** (only `criteria/v1` bindings). Needs a full v2 port.

**Remaining for the extraction switchover (deliberately deferred — the risky half):**
- The in-tree proto **diverged into two copies** (`proto/criteria/v2` vs `sdk/pb/criteria/v2`);
  reconcile the helper drift (host `chunking.go` exports `SendChunks`/`AssembleChunks` the SDK
  copy lacks; divergent grpc bindings) into the proto repo before deleting in-tree.
- The in-tree `sdk/` module **conflates two SDKs**: the adapter SDK (`adapterhost`, extracted)
  and an unrelated **events/v1 server-API client** (root pkg + `pb/criteria/v1` + connectrpc,
  importing host `internal/`). Only `adapterhost` belongs in the Go adapter SDK; the rest stays
  with the host or becomes its own client package.
- `serve_remote_test.go` dropped from the Go SDK (imported host `internal/adapter/environment/remote`;
  serveRemote deferred).
- **Switchover (WS41/WS25/WS42):** repoint host consumers (`cmd/criteria-adapter-*`,
  `adapters/shell`, `internal/adapter/*`) + the Go SDK to the new modules, then **delete in-tree
  `proto/` + `sdk/`** and prove the host still builds/tests. Plus TS/Python proto packages
  (`@criteria/adapter-proto`, PyPI). Each new repo's `RECONCILE.md` has the details.

**Next planned sequence (user):** finish SDK publishing → all adapters (incl. in-branch copilot +
shell) in their own repos and published → proto switchover → then archive most remaining
workstreams and return to the release gate (WS40).

### SDK-folder disentanglement (2026-06-05, session 3)

Resolved the two in-tree SDK folders (`criteria-typescript-adapter-sdk/`,
`criteria-python-adapter-sdk/`), which were in **opposite** states. Neither was
referenced by the monorepo build; both are designed to live in their own repos (WS23/WS24).

- **TypeScript — in-tree was stale; repo is canonical.** The in-tree folder was the old
  WS21 `serveRemote`-only skeleton (`criteria-typescript-adapter-sdk@0.1.0`); the real SDK
  already ships as [`@criteria/adapter-sdk@0.5.0`](https://github.com/brokenbots/criteria-typescript-adapter-sdk)
  (tagged, published session 2). Its one unique asset — `serveRemote.ts` (the **deferred**
  WS21 remote-serve path, absent from the published `main`) — was preserved on the
  [`deferred/serve-remote`](https://github.com/brokenbots/criteria-typescript-adapter-sdk/tree/deferred/serve-remote)
  branch with a `DEFERRED.md` provenance note. In-tree folder deleted.
- **Python — in-tree was canonical; repo was a stale skeleton.** The repo
  ([`criteria-python-adapter-sdk`](https://github.com/brokenbots/criteria-python-adapter-sdk))
  was a May-6 husk predating v2; the full v2 SDK (WS24/#204) lived in-tree at the **wrong**
  version `2.0.0rc1`. Corrected to **`0.5.0`** (per the session-2 policy: v2 = protocol, not
  product; artifacts track the 0.5.0 line), seeded into the repo over the skeleton (repo
  LICENSE retained), **42 tests pass**, pushed to `main`, tagged **`v0.5.0`**. In-tree folder
  deleted.
- **Net:** all three adapter SDKs now live solely in their own repos at `0.5.0`
  (`@criteria/adapter-sdk`, `criteria-python-adapter-sdk`, `criteria-go-adapter-sdk`); the
  monorepo no longer carries SDK source. Next: proto/Go-SDK switchover.

### Proto switchover — v2 bindings now external (2026-06-05, session 3)

The adapter **protocol v2** bindings no longer live in the monorepo.

- **Divergence reconciled.** The two in-tree copies (`proto/criteria/v2`,
  `sdk/pb/criteria/v2`) were byte-identical generated bindings; only the consumed copy
  (`sdk/pb/criteria/v2`, 57 importers) mattered — the root copy had zero real Go importers.
  Their only real drift was helper code: the root copy's remote-chunk surface
  (`SendChunks`/`AssembleChunks`/`ChunkEnvelope`/…, no live consumers — deferred WS19) and the
  sdk copy's `outputs.go`. Both, plus the full v2 test suite, were folded into
  [`criteria-adapter-proto`](https://github.com/brokenbots/criteria-adapter-proto) and tagged
  **`v0.5.1`** (additive over v0.5.0).
- **Host repointed.** All 57 files now import
  `github.com/brokenbots/criteria-adapter-proto/criteria/v2` (alias `v2` preserved);
  `criteria-adapter-proto v0.5.1` added to the root + `sdk` module `go.mod`. In-tree
  `proto/criteria/v2` + `sdk/pb/criteria/v2` **deleted**; the **v1 server API**
  (`proto/criteria/v1`, `sdk/pb/criteria/v1`) **stays** in the monorepo (to be broken out
  later). Makefile `proto`/`proto-check-drift` repointed to v1; obsolete `buf.gen.v2.yaml`
  removed. All four workspace modules build; full test suite green; import boundaries OK.
- **Deferred to the Go-SDK switchover:** the `sdk/` module still conflates the adapter SDK
  (`sdk/adapterhost`, incl. an in-tree `serve_remote*` that the external go-sdk dropped) with
  the events/v1 server-API client. `go mod tidy` on `sdk/` fails because
  `sdk/adapterhost/serve_remote_test.go` imports host `internal/…/remote` — a pre-existing
  cross-dependency to untangle when `sdk/adapterhost` is repointed to `criteria-go-adapter-sdk`.

### Go-SDK switchover — adapterhost now external (2026-06-05, session 3)

The Go **adapter SDK** (`adapterhost`) no longer lives in the monorepo.

- **go-sdk repo brought current → `v0.5.1`.** Carried the clean unit tests (`serve_test`,
  `manifest_test` — proto-only deps) into
  [`criteria-go-adapter-sdk`](https://github.com/brokenbots/criteria-go-adapter-sdk) (it was
  test-free) and bumped its proto dep to `v0.5.1`. `serve_remote.go` already shipped on `main`;
  only `serve_remote_test.go` (imports host `internal/…/remote`) was preserved on the
  [`deferred/serve-remote`](https://github.com/brokenbots/criteria-go-adapter-sdk/tree/deferred/serve-remote)
  branch. `ServeRemote` has **zero in-tree callers** (truly deferred).
- **Host repointed.** All `sdk/adapterhost` importers (adapters `cmd/criteria-adapter-*`,
  `adapters/shell`, examples, conformance testfixtures) now import
  `github.com/brokenbots/criteria-go-adapter-sdk/adapterhost`; `criteria-go-adapter-sdk v0.5.1`
  added to the root + `tools` modules. In-tree `sdk/adapterhost` **deleted**.
- **import-lint updated.** The boundary rule (production `internal/` must not import the adapter
  SDK; testfixture adapter binaries may) was repointed to the external path and split into its
  own rule, since `criteria-go-adapter-sdk` no longer matches the `criteria/sdk` prefix; unit
  tests + whole-repo boundary check pass.
- **`sdk/` module after extraction.** Now holds only the **events/v1 server-API client**
  (root pkg + `pb/criteria/v1` + connectrpc + conformance). `go mod tidy` on `sdk/` succeeds
  again (the host-internal cross-dep left with the deferred test). It still requires the host
  module for `github.com/brokenbots/criteria/events` — the next conflation to untangle when the
  server API is broken out.
- All four workspace modules build; full test suite green; import boundaries OK.

### Copilot secrets + extraction (WS45, WS36 — 2026-06-05, session 4)

- **WS45 — copilot secret channel (DONE, merged #229).** Added a redaction-aware
  `adapterhost.Secrets` accessor (`Get` / `SpawnEnv` / `WithStepSecrets`) to
  `criteria-go-adapter-sdk` (**v0.5.2**) — the D69/D75 surface for Go adapters. (WS45's spec
  targeted the in-tree `sdk/adapterhost`, which no longer exists after #228; it correctly
  landed in the external SDK.) Copilot now resolves its GitHub token from the secret channel
  (declared in `InfoResponse.Secrets`), **fails closed** with a clear error when absent, and
  no longer reads `os.Getenv`. The shared conformance harness gained an `Options.Secrets`
  field; other adapters are unaffected.
- **WS36 — copilot extraction (DONE).** [`criteria-adapter-copilot`](https://github.com/brokenbots/criteria-adapter-copilot)
  repo created (`main` + tag **v0.5.0**), consuming `criteria-adapter-proto` +
  `criteria-go-adapter-sdk`, published as a signed OCI artifact via `publish-adapter`. The
  host-dependent `conformance_test.go` is preserved on the repo's `deferred/conformance`
  branch (it needs the host's internal harness). Manifest gained `source_url` + `platforms`
  (required by publish validation; copilot had never declared them since it was never
  published). The in-tree `cmd/criteria-adapter-copilot/` is removed in a follow-up PR —
  build/test/validate/spec-check all pass without it (the host-side copilot permission-alias
  policy stays). `docs/adapters.md` still uses copilot as its worked example and is left to
  the **WS39** documentation refresh.

### Shell extraction (WS42 — 2026-06-05, session 4)

- **WS42a — shell extraction (DONE).** [`criteria-adapter-shell`](https://github.com/brokenbots/criteria-adapter-shell)
  repo created (`main` + tag **v0.5.0**, flattened to `package main`), published as a signed OCI
  artifact (`ghcr.io/brokenbots/criteria-adapter-shell`); `conformance_test.go` preserved on the
  repo's `deferred/conformance` branch; manifest gained `source_url` + `platforms`.
- **WS42b — in-tree removal + test decoupling (DONE).** Per owner guidance, the monorepo must be
  **self-contained**: no test fixture may depend on the extracted shell adapter (the WS42-spec
  "pull shell + default registry ref for tests" approach was rejected — see
  [[feedback-self-contained-tests]]). Findings: nothing imported `adapters/shell`; the
  `--builtin-shell` dispatch no longer exists; removing the in-tree shell **broke no tests or
  gates** (fixtures used `"shell"` as a string with mock executors). Reworked the ~30 affected
  test files to neutral in-tree test adapters — **`noop`** for generic adapter refs, and a
  dedicated **`exec`** command-adapter (carrying the old shell input/output/policy schema) for
  the workflow-compiler tests that assert on `command`/`stdout`/`exit_code`/policy. The shell
  **environment** type (`environment "shell"`, hardcoded in `compile_environments.go`) and
  `allow_tools "shell:…"` tool grants are unchanged. Examples + `.criteria/workflows` keep using
  `adapter "shell"` — that's correct real usage of the now-published external adapter. All four
  modules build; full test suite, `make lint`, `make validate`, `make validate-self-workflows`
  green.

### Multi-language proto packaging (WS41 — 2026-06-05, session 4)

Completed the multi-language **infrastructure** in
[`criteria-adapter-proto`](https://github.com/brokenbots/criteria-adapter-proto) (the Go
switchover landed earlier via #227):

- **`buf.gen.multi.yaml`** generates TS ([protobuf-es](https://github.com/bufbuild/protobuf-es))
  + Python (protoc python/grpc) bindings from the `.proto` sources. Verified: TS compiles
  (`tsc`), Python imports + wheel builds — locally **and** in CI.
- **`npm/` (`@criteria/adapter-proto`)** + **`python/` (`criteria-adapter-proto`)** package
  manifests; generated bindings are produced at publish time, not committed (avoids drift).
- **`publish-langs.yml`**: on tag, generates + builds + publishes npm + PyPI, each **gated** on
  its credential (`NPM_TOKEN`/`PYPI_API_TOKEN`) and skipping gracefully when unset. Verified via
  `workflow_dispatch`: both jobs generated + built (`criteria_adapter_proto-0.5.1.whl`+`.tar.gz`,
  npm `tsc`) and skipped publish. Go needs no publish step (module proxy).
- **Versioning policy** (SemVer, one version across all languages) in the README; **`DEPENDENCIES.md`**
  consumer pin-table.
- **Owner-gated remainder:** the real npm/PyPI publish (needs the tokens + `@criteria` scope) and
  the TS/Python SDK consumer-switch (blocked on that publish — both SDKs bundle their own proto
  today and pass their own CI).

## Language cleanup — Terraform-shaping the HCL (archived 2026-06-05)

A focused sub-effort (WS01–WS11) that landed on `main` and merged into `adapter-v2`
(#203). All eleven workstreams complete; files archived to
[`archived/v4/language-cleanup/`](archived/v4/language-cleanup/).

## Workstream conventions

Every workstream file declares:

- **Goal**, **Prerequisites**, **In scope** (with file paths and line ranges),
  **Out of scope** (explicit "do not touch" list), **Reuse pointers** (existing
  functions/interfaces to use), **Behavior change** disclosure ("yes" or "no";
  if yes, every observable difference enumerated for the reviewer), **Tests
  required**, **Exit criteria**, and a **Files this workstream may modify**
  list.
- The "may not edit" set is restated in every workstream: `README.md`,
  `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`,
  `workstreams/README.md`, and any other workstream file. Those are the
  cleanup-gate's territory.

See [PLAN.md](../PLAN.md) for the project-level roadmap.

## Files NOT editable by workstream-executor or workstream-reviewer

The executor and reviewer agents are scoped to **the single workstream
file they are executing**. They may not edit:

- `README.md`
- `PLAN.md`
- `AGENTS.md`
- `CHANGELOG.md`
- `CONTRIBUTING.md`
- `workstreams/README.md`
- Any other workstream file in this directory

A workstream that needs changes to those files declares them in its
"Files this workstream may modify" list and must be the cleanup gate
for that phase, or it defers the edit to the cleanup gate with a
forward-pointer note in its reviewer log.

## Archived

- Phase 0 — [`archived/v0/`](archived/v0/) (closed 2026-04-27, `v0.1.0`).
- Phase 1 — [`archived/v1/`](archived/v1/) (closed 2026-04-29).
- Phase 2 — [`archived/v2/`](archived/v2/) (closed 2026-05-02, `v0.2.0`
  combined-phase tag).
- Phase 3 — [`archived/v3/`](archived/v3/) (closed 2026-05-06, `v0.3.0`).
- v0.3.1 — [`archived/v3.1/`](archived/v3.1/) (post-Phase-3 bugfixes + parallel).
- v0.3.2 — [`archived/v3.2/`](archived/v3.2/) (pre-Phase-4 feature + tech-debt prep, closed 2026-05-13).
- Phase 4 (partial) — [`archived/v4/adapter-v2/`](archived/v4/adapter-v2/) (completed
  in-repo WSes; phase still open — see the Phase 4 section above).
- Language cleanup — [`archived/v4/language-cleanup/`](archived/v4/language-cleanup/)
  (WS01–WS11, landed on `main`, merged via #203).

The pre-separation v1.x phases live in the orchestrator repo's
`workstreams/archived/`; they are not copied here.
