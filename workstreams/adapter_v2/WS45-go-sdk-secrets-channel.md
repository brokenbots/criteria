# WS45 — Go adapter SDK secrets channel + in-tree adapter consumption

**Phase:** Adapter v2 · **Track:** Security / bugfix · **Owner:** Workstream executor · **Depends on:** [WS13](../archived/v4/adapter-v2/WS13-secrets-channel-redaction.md) (host secret channel + redaction registry — landed). · **Unblocks:** [WS36](WS36-migrate-copilot.md) (copilot secrets migration). · **Base branch:** `adapter-v2`

> **Origin.** Discovered during the 2026-06-05 review of the remaining adapter_v2 workstreams, not in the original WS01–WS44 plan. WS13 wired the **host** side of the secret channel and the proto carries it, but the **in-tree Go adapter SDK** (`sdk/adapterhost`) never surfaced it to adapters, so no in-tree adapter consumes it. This is the Go-path analogue of the `secrets.get` / `secrets.spawnEnv` work that D69/D75 specify for the TypeScript and Python SDKs (WS23/WS24).

## Context

The wire already delivers secrets to the adapter:

- [`v2.OpenSessionRequest.Secrets`](../../sdk/pb/criteria/v2/adapter.pb.go) — `map<string,string>`, field 3 — resolved secret values for the session.
- [`v2.ExecuteRequest.SecretInputs`](../../sdk/pb/criteria/v2/adapter.pb.go) — `map<string,string>` — per-step secret inputs (D66).
- [`v2.InfoResponse.Secrets`](../../sdk/pb/criteria/v2/adapter.pb.go) — declared secret names → descriptions (the manifest declaration, D19).

The [`adapterhost.Service`](../../sdk/adapterhost/service.go) interface hands the adapter the raw request structs, so an adapter *can* read `req.GetSecrets()["NAME"]` today. Two gaps remain:

1. **No ergonomic, redaction-safe accessor.** There is no `secrets.Get(name)` / `SpawnEnv([...])` surface (D69/D75). Adapters that want a secret read the raw map and roll their own child-process env plumbing, with no redaction-registry integration on the adapter side.
2. **In-tree adapters bypass the channel entirely.** The `copilot` adapter resolves its GitHub token from process env — [`copilot.go:249-255`](../../cmd/criteria-adapter-copilot/copilot.go#L249-L255) reads `COPILOT_GITHUB_TOKEN` / `GH_TOKEN` / `GITHUB_TOKEN` via `os.Getenv`. This is exactly the pattern D69 forbids: once the sandbox scrubs the adapter process env (D29/D32), those reads return empty and the adapter silently loses auth. `provider_api_key` is likewise a `config` field, not a secret.

`shell` and `noop` legitimately consume no secrets (shell's `environment.variables` env injection is the non-secret D72 path and stays as-is). So the only in-tree adapter to migrate is `copilot`.

## Prerequisites

- WS13 merged (host secret channel + redaction registry) — done.
- `make ci` green on `adapter-v2`.

## In scope

### Step 1 — Add a secrets accessor to `adapterhost`

New file: `sdk/adapterhost/secrets.go`. Provide a small, redaction-aware surface the adapter handler can use, sourced from the request structs it already receives:

- A `Secrets` view constructed from `OpenSessionRequest.Secrets` (session-scoped) optionally merged with `ExecuteRequest.SecretInputs` (step-scoped) for the duration of an `Execute`.
- `Get(name string) (string, bool)` — returns the resolved secret; no process-env fallback (D69).
- `SpawnEnv(names ...string) ([]string, error)` (D75) — returns an `exec.Command`-ready env slice containing only the explicitly named, declared secrets; refuses names not declared in the adapter's `InfoResponse.Secrets`; registers the values with the adapter-side redaction layer so any child output the adapter forwards is masked.

Keep it minimal and Go-idiomatic; this is not a port of the full TS helper, just the two operations the in-tree path needs.

### Step 2 — Migrate `copilot` to the secrets channel

- Declare the GitHub token as a secret in the copilot manifest (`InfoResponse.Secrets`, e.g. `GITHUB_TOKEN`), so the host resolves and delivers it via `OpenSession.secrets`.
- Replace `resolveGitHubToken()`'s `os.Getenv` chain with a read from the `adapterhost` secrets accessor. Preserve the precedence order across the accepted names by resolving them from the secrets map rather than the environment.
- When copilot shells out / sets `options.GitHubToken`, source the value from the secrets accessor (and use `SpawnEnv` if it forwards into a child process).

### Step 3 — Validation

- `go test ./cmd/criteria-adapter-copilot/... ./sdk/adapterhost/...`
- Confirm via test that with the process env scrubbed (no `GH_TOKEN` set) but the secret delivered on `OpenSession.secrets`, copilot authenticates; and that with neither, it fails closed with a clear "missing secret" surface.

## Behavior change

**Yes.** Enumerated:

- Copilot no longer reads `COPILOT_GITHUB_TOKEN` / `GH_TOKEN` / `GITHUB_TOKEN` from process env. The token must be supplied via the workflow's secret channel (`adapter.copilot.default.secrets { GITHUB_TOKEN = ... }` resolving through the provider stack). This is the intended D69 end-state, but it is a breaking change for any workflow that relied on the adapter inheriting the host's `GITHUB_TOKEN` env var. Documented in the workstream's reviewer notes and surfaced as a clear missing-secret error.
- Copilot's manifest now declares `GITHUB_TOKEN` as a required secret, so `criteria` can report it at compile time when unsatisfied.

## Reuse

- [`v2.OpenSessionRequest.Secrets`](../../sdk/pb/criteria/v2/adapter.pb.go) / `ExecuteRequest.SecretInputs` / `InfoResponse.Secrets` — already generated.
- [`adapterhost.Service`](../../sdk/adapterhost/service.go) — the accessor is constructed from the request structs the interface already passes.
- Existing copilot `resolveGitHubToken()` precedence logic — port, don't rewrite.
- The host-side redaction registry from WS13 — the adapter-side helper complements it; do not reimplement host masking.

## Out of scope

- TypeScript / Python SDK `secrets.get` / `secrets.spawnEnv` — those are WS23/WS24 in their own repos.
- Host-side secret resolution, provider stack, or redaction registry — WS13, landed; do not touch.
- The external Go author-facing SDK (`criteria-go-adapter-sdk`, WS25). This WS targets only the in-tree `sdk/adapterhost` path.
- Migrating `shell` or `noop` — they consume no secrets (shell keeps `environment.variables` env injection per D72).
- `provider_api_key` redesign beyond moving the GitHub token; provider credentials for custom endpoints can follow in a later pass if needed.

## Files this workstream may modify

- New file: `sdk/adapterhost/secrets.go` (+ `secrets_test.go`).
- `cmd/criteria-adapter-copilot/copilot.go` and adjacent files for the token resolution + manifest secret declaration (+ tests).

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.
- Host-side secret/redaction code under `internal/`.
- Generated proto files.

## Tasks

- [ ] Add `sdk/adapterhost/secrets.go` with `Get` and `SpawnEnv` (Step 1).
- [ ] Unit-test the accessor: declared/undeclared name handling, no env fallback, SpawnEnv refusal of undeclared names.
- [ ] Declare `GITHUB_TOKEN` in copilot's `InfoResponse.Secrets` (Step 2).
- [ ] Replace copilot's `os.Getenv` token chain with the secrets accessor (Step 2).
- [ ] Validation incl. scrubbed-env / delivered-secret test and fail-closed test (Step 3).

## Exit criteria

- `sdk/adapterhost` exposes `Get` and `SpawnEnv`; neither falls back to process env.
- `copilot` resolves its GitHub token from the secret channel and declares it in its manifest; no `os.Getenv` secret reads remain in copilot.
- With the secret delivered via `OpenSession.secrets` and process env scrubbed, copilot authenticates; with neither, it fails closed with a clear missing-secret message.
- `go test ./cmd/criteria-adapter-copilot/... ./sdk/adapterhost/...` green; `make ci` green.

## Tests

- `TestSecrets_Get_DeclaredAndUndeclared` — `Get` returns delivered secrets; absent names report not-found; no env fallback.
- `TestSecrets_SpawnEnv_RefusesUndeclared` — `SpawnEnv` returns only declared names and errors on undeclared ones.
- `TestCopilotResolvesTokenFromSecrets` — token sourced from `OpenSession.secrets`, not env.
- `TestCopilotFailsClosedWithoutSecret` — missing token surfaces a clear error, no silent unauthenticated call.

## Risks

| Risk | Mitigation |
|---|---|
| Breaking workflows that relied on env-inherited `GITHUB_TOKEN` | Intended D69 behavior; surface a clear missing-secret error and document the migration in reviewer notes. Coordinate with WS36's reviewer log. |
| Step-scoped `SecretInputs` vs session-scoped `Secrets` precedence ambiguity | Define precedence explicitly (step overrides session) and cover it in a test. |
| Adapter-side redaction diverging from host registry | `SpawnEnv` registers with the adapter redaction layer only; the host registry (WS13) remains the source of truth for host-emitted logs. Document the boundary. |
