# Adapter Plan — Comprehensive Design

> **Status:** In active planning. This document is built incrementally across a multi-turn session. Decisions are locked as we go; open questions are tracked at the bottom.

---

## Context

### Why we're doing this

The current adapter implementation in `criteria` works but is awkward to develop against and unfriendly to end users:

- **Install friction.** Users must clone an adapter source repo, build the binary, and copy it to `~/.criteria/plugins/`. There is no `criteria adapter pull <ref>`, no version selector, no caching, no manifest discovery.
- **No integrity or version guarantees.** `go-plugin` supports hash validation, but it is unused. Workflows cannot pin an adapter version. There is no lockfile, so the same workflow can produce different behavior on different machines — a blocker for enterprise use.
- **Protocol grew ad hoc.** The current 5 RPCs (`Info`, `OpenSession`, `Execute`, `Permit`, `CloseSession`) cover the happy path but were not designed for state transfer, pause/resume, structured inspection, or future remote execution. Output schema is absent. The protocol needs a deliberate review against the workloads we expect (long-running agents, multi-turn, tool use, large payloads, remote execution).
- **Weak sandboxing.** Adapters are plain subprocesses with no isolation primitives. They inherit the parent environment, share filesystem, and receive secrets as plain map values that can leak into logs. Only the builtin shell adapter hardens itself.
- **Mixed terminology.** "Plugin" and "adapter" are both used in code and docs — directory `internal/plugin/`, proto service `AdapterPluginService`, binary prefix `criteria-adapter-`, doc titled `docs/plugins.md`. Users see "adapter" in HCL; developers see "plugin" in the SDK. This will be unified.
- **Developer friction.** SDK exists for TypeScript and Python, but complex adapters (Claude, OpenAI, Codex) reimplement the same patterns: session state maps, outcome validation loops, permission correlation. Build/distribute story is hand-rolled per adapter — no shared CI scaffolding, no OCI publishing, no starter template.

### Intended outcome

A redesigned adapter system that is:

- **Easy to pull and use** — primary user path is the workflow team's `criteria pull <workflow_ref>`, which transitively pulls every adapter the workflow needs. Direct adapter management is available via `criteria adapter pull <ref>` with optional version. Adapters are auto-pulled during workflow compile if missing, cached locally, and same workflow runs identically anywhere.
- **Verifiable** — digest-pinned, signature-verified, recorded in a lockfile alongside the workflow.
- **Easy to develop** — SDKs in multiple languages that handle transport, session state, outcome validation, permission correlation, and packaging. A starter template + CI scripts that publish to any OCI registry with near-zero developer friction.
- **Decentralized** — no required central registry. Production distribution uses OCI (any OCI-compliant registry — GHCR, ECR, GAR, Harbor, self-hosted). Development distribution allows URL-based zip via go-getter for fast iteration.
- **Sandboxed** — strong process isolation with clear secret-passing semantics, secret redaction in logs, and a well-defined permission model.
- **Extensible** — protocol designed for state transfer, pause/resume, inspection, and remote execution from day one, even if not all are implemented in v1.
- **Consistently named** — single term ("adapter"), used uniformly across code, docs, CLI, and UI.

---

## Current state (mapped from `/Users/dave/Projects/criteria` and related repos)

### Host (criteria)
- **Protocol:** HashiCorp `go-plugin`, gRPC transport, protocol v1, magic cookie `CRITERIA_PLUGIN`. Proto at `proto/criteria/v1/adapter_plugin.proto`. Service `AdapterPluginService` with 5 RPCs.
- **Discovery:** `internal/plugin/discovery.go` — `$CRITERIA_PLUGINS` or `~/.criteria/plugins/criteria-adapter-<name>`. Does not consult PATH. No version concept.
- **Lifecycle:** `internal/plugin/loader.go` + `sessions.go` — `exec.Command(path)` with 30s start timeout. Session opens lazily on first step; closes at workflow end. Crash policy: `fail` / `respawn` / `abort_run`.
- **Workflow coupling:** HCL `adapter "<type>" "<name>" { config { ... } }`. Step references via `adapter.<type>.<name>`. Config constant-folded into FSM at compile time. No versioning, no hashing, no manifest tracking.
- **Sandbox:** None at the host layer. Subprocess inherits env. Only the builtin shell adapter applies env allowlist, PATH sanitization, timeouts, output capture limits, working-dir confinement.
- **Secrets:** Passed as plain `map[string]string` in `OpenSessionRequest.config`. No redaction, no separate secret channel, no rotation hooks.
- **State / pause / resume / inspection:** None in the protocol. Sessions are ephemeral.

### CLI / workflow surface
- **Framework:** Cobra. Verbs: `compile`, `plan`, `apply`, `run`, `validate`, `status`, `stop`. No `pull`/`install`/`add`.
- **State dir:** `~/.criteria/` (override `CRITERIA_STATE_DIR`), perms `0o700`.
- **Lockfile:** Does not exist.
- **go-getter:** Not imported. Mentioned in plan as future workflow-pulling layer.
- **OCI client:** Not imported. No oras-go, no containerd, no docker SDK in tree.
- **Compilation:** HCL → FSM graph with constant-folded config; `FSMGraph.Adapters` keyed by `"<type>.<name>"`.

### SDKs
- **TypeScript** (`criteria-typescript-adapter-sdk`): `serve({ name, version, capabilities, configSchema, inputSchema, execute, ... })`. Bun `--compile` produces a single self-contained binary (~50–80 MB). Multi-arch Makefile targets exist (linux x64/arm64, darwin arm64). OCI analysis docs sketched, not built.
- **Python** (`criteria-python-adapter-sdk`): Same shape, async. Nuitka `--onefile --standalone` for single-binary distribution. No OCI scaffolding.
- **Both SDKs** share the same gRPC proto contract and handshake. Consistent across languages at the transport layer.
- **Gaps:** Session state stores, outcome validation loop, permission correlation, schema generation from native types (e.g., Zod → schema), retry/error helpers, capability registry — all reimplemented per adapter.

### Existing adapters
- `criteria-typescript-adapter-greeter` — minimal example (~40 LOC).
- `criteria-typescript-adapter-claude`, `claude-agent`, `codex`, `openai` — production-grade, 300–400 LOC each, reimplementing common patterns.

### Terminology distribution
- "plugin" referenced in ~182 files (host internals, CLI, SDK directory paths).
- "adapter" referenced in ~282 files (workflow DSL, docs body, user-facing API).
- Hybrid in places: `AdapterPluginService`, `internal/plugin/` directory managing things called adapters, `PluginName = "adapter"` constant.

---

## Goals (locked)

1. **End-user pull experience.** Primary user path: workflow pull (`criteria pull <workflow_ref>`, owned by the workflow team) transitively pulls every adapter the workflow references. Direct adapter management via `criteria adapter pull <ref>` with optional version. Workflow compile auto-pulls missing adapters into a local cache. Same workflow produces identical runtime behavior anywhere.
2. **Integrity and version pinning.** Every adapter referenced in a workflow is pinned by digest in a workflow-local lockfile, signature-verified at install time, integrity-checked at load time.
3. **Per-workflow lockfile.** Terraform-style `.criteria.lock.hcl` lives next to the workflow and is committed to VCS. No central lock authority — matches the decentralization goal.
4. **Multi-language adapter SDKs** that handle transport, session state, outcome validation, permission correlation, packaging, and publishing. Starter template + CI scripts so a new adapter is one fork away from a published OCI artifact.
5. **Decentralized distribution.** Any OCI-compliant registry works (GHCR, ECR, GAR, Harbor, self-hosted). URL-based zip via go-getter as a secondary path for development. No required central registry.
6. **Stronger sandboxing.** OS-native isolation primitives on Linux and macOS (no Windows host support — Windows users run via WSL2). Container-based isolation as an opt-in path when a clean implementation is available.
7. **Extensible protocol (v2).** Designed from day one for state transfer, pause/resume, inspection, output schema, and remote execution. Clean break — no v1 wire compatibility.
8. **Working remote adapter transport in v1.** One concrete remote transport ships and runs end-to-end (not just protocol-compatible scaffolding).
9. **Unified terminology.** Single term used uniformly across code, CLI, docs, UI.

## Non-goals (locked)

- **Native Windows host support.** Windows users run criteria inside WSL2. No Windows-native sandboxing (no AppContainer, no job objects).
- **Central registry / discovery service.** Out of scope for this release. Discovery is by URI; users supply references explicitly.
- **In-process / dynamic-library adapters.** Adapters remain out-of-process subprocesses (or remote endpoints).
- **Backward compatibility with v1 adapter protocol.** Existing adapters (claude, claude-agent, codex, openai, copilot, greeter, shell) are migrated as part of this release. No host-side v1 shim.

## Design decisions (locked)

### Scope
- **D1.** Everything in the goals list ships in v1, including one working remote adapter transport.
- **D2.** No backward compatibility with protocol v1. Hard cut to protocol v2. All existing adapters are migrated to the new SDK before the release ships; v1 host code paths are deleted, not deprecated.

### Sandbox
- **D3.** Linux: OS-native primitives (user/mount/net/pid namespaces + seccomp + landlock). macOS: `sandbox-exec` profiles. Windows: not supported on the host; recommend WSL2.
- **D4.** Container-based execution (`docker run` / OCI runtime) is opt-in per adapter declaration, used when an OCI runtime is available and the adapter benefits from heavier isolation. Container mode is the same OCI artifact already used for distribution — no separate image is built for runtime.

### Lockfile
- **D5.** Per-workflow `.criteria.lock.hcl` sitting next to the workflow file(s). Committed to VCS. Records: full adapter ref, resolved digest, signature info, SDK protocol version, source URL. Updated by `criteria adapter pull` and an explicit `criteria adapter lock` verb.

### Terminology
- **D6.** "Adapter" everywhere. Renames performed as part of v2:
  - `internal/plugin/` → `internal/adapter/`
  - `proto/criteria/v1/adapter_plugin.proto` → `proto/criteria/v2/adapter.proto`
  - `AdapterPluginService` → `AdapterService`
  - `PluginName` constant → `AdapterName`
  - SDK package paths and exported symbols updated to match.
  - `docs/plugins.md` → `docs/adapters.md`.

### Reference format and distribution
- **D7.** Canonical reference is a full OCI ref: `<registry>/<org>/<name>:<tag>` or `@sha256:<digest>`. Examples: `ghcr.io/criteria-adapters/claude:1.2.3`, `ghcr.io/acme/internal-adapter@sha256:abc...`.
- **D8.** Short aliases supported via configuration (global at `~/.criteria/config.hcl`, per-workflow via a `registry` block in workflow HCL). `criteria adapter pull claude:1.2.3` looks up `claude → ghcr.io/criteria-adapters/claude` and resolves. If the input parses as a full OCI ref, alias lookup is skipped.
- **D9.** Secondary distribution path: URL-based zip via go-getter (`https://`, `git::`, `file://`, etc.). Used for development and quick iteration. URL-zip artifacts are still digest-pinned in the lockfile. Production deployments are expected to use OCI; URL-zip is not a production recommendation.

### OCI artifact shape (default: light artifact, optional: full image)
- **D10.** **Default published artifact: OCI artifact (ORAS-style), not a runnable image.** Custom mediaType: `application/vnd.criteria.adapter.v1+json` for the config blob; `application/vnd.criteria.adapter.binary.v1` for per-platform binary blobs. Every adapter publishes this.
- **D11.** Each adapter version publishes a multi-platform OCI index pointing at:
  - One binary blob per supported platform (linux/amd64, linux/arm64, darwin/arm64, plus the SDK's "common" supported set).
  - One `adapter.yaml` manifest blob (and an OCI annotation mirroring key fields for fast inspection without blob pull).
  - Cosign signature(s) attached as referrers.
- **D12.** **Optional second publish: full runnable container image.** Adapters with heavier runtime dependencies (interpreters, system libraries that can't trivially be bundled into a single binary) or adapters intended to run independently in Kubernetes / ECS may opt in to also publishing a runnable container image alongside the OCI artifact. Default is artifact-only — flexibility without imposing dev cost on the common case.
  - **D12a. Image build and naming.** Built from a Dockerfile in the adapter repo (one is generated by the SDK starter and committed; the developer can replace it). Pushed to the same registry under a sibling tag (`<name>:<version>-image`) and signed independently with cosign.
  - **D12b. Discovery.** `adapter.yaml` carries an optional `container_image: { ref: "ghcr.io/org/name:v1.2.3-image", digest: "sha256:..." }` block when an image was published. Host reads it at pull time.
  - **D12c. Host runtime selection.** No silent fallbacks — the host fails closed when an adapter cannot serve the requested runtime.
    1. If `environment.runtime ∈ {docker, podman}` **and** `adapter.yaml.container_image` is present: `docker run <image>` directly. Canonical container path.
    2. If `environment.runtime` is set **but no image was published**: **fail closed.** Error message:
       ```
       Error: adapter <ref> does not publish a container image; cannot run under environment.runtime = "<runtime>".
       Ask the publisher to enable image publishing, or change the environment to runtime = "none".
       Publisher: <adapter.yaml.source_url>
       ```
    3. If `environment.runtime = "none"` (default): subprocess mode using the artifact binary. The runnable image, if any, is not pulled.
  - **D12c-alt. Platform mismatch error** (same error pattern as D12c.2). If the host's `GOOS/GOARCH` is not in the adapter's published platform set (D11), pull fails with:
    ```
    Error: adapter <ref> does not support <goos>/<goarch>. Supported platforms: <list>.
    Ask the publisher to add this platform, or use a different adapter.
    Publisher: <adapter.yaml.source_url>
    ```
    Detected at pull time so the failure surfaces well before `criteria apply`. No fallback (no cross-arch emulation, no host-side build).
  - **D12d. Publish action.** Reusable composite action (WS28) takes a `with_image: bool` input (default `false`). When true: builds + signs + pushes the runnable image and updates `adapter.yaml` with the `container_image` block.
  - **D12e. Policy guidance** (documented, not enforced): pure-binary adapters (claude, openai, greeter, codex, copilot, claude-agent, shell) ship artifact-only. Adapters that bundle an interpreter or non-bundlable system deps, or those intended to run as standalone container workloads (e.g., a Python adapter doing CV/ML with cuDNN), ship both. Guidance lives in `docs/adapters.md` and the starter README.

### Manifest source of truth
- **D13.** Adapter metadata is **code-declared via the SDK `serve()` config** and stays the single source of truth for developers. Fields:
  - `name`, `version`, `description`
  - `capabilities`
  - `config_schema`, `input_schema`, `output_schema`
  - `secrets` (declared secrets — see D19)
  - `permissions` (declared permissions)
  - `platforms` (list of supported `GOOS/GOARCH` tuples)
  - `sdk_protocol_version`
  - `container_image` (optional, populated when published with image mode — see D12b)
  - **`source_url`** *(required)* — public URL of the adapter's source repository / issue tracker. Quoted verbatim in user-facing error messages (D12c.2, D12c-alt) so users can find the publisher when something is wrong. SDK enforces presence at `--emit-manifest` time.
- **D14.** Build step extracts the manifest by running the adapter binary once with `--emit-manifest` (or a dedicated SDK API), writes `adapter.yaml`, and embeds it both as an OCI artifact blob and as OCI annotations on the index for fast metadata reads.
- **D15.** At pull time, the host reads `adapter.yaml` from the OCI artifact (no need to launch the adapter for discovery). At first run, the host calls `Info()` and verifies the runtime response matches the static manifest — any divergence fails the pull / aborts the run with a clear error.

### Signing and trust
- **D16.** Default signing path is **cosign keyless** via sigstore (OIDC identity from CI: GitHub Actions, GitLab CI, etc.). Signatures attached as OCI referrers per the cosign convention. Verification policy allows configurable trusted-issuer + subject-pattern rules.
- **D17.** Power users may sign with explicit cosign keys (ed25519 / ECDSA). The lockfile records whichever identity signed each pinned digest: either `keyless: {issuer, subject}` or `key: {algo, fingerprint}`.
- **D18.** Development opt-out: `criteria adapter pull --allow-unsigned` and a workflow-level `verification = "off" | "warn" | "strict"` setting (default `strict`). The lockfile clearly records that an unsigned artifact was pulled so accidental promotion to a strict project fails loudly. CI defaults to `strict`.

### Secrets
- **D19.** **Separate secret channel** — defined precisely below. Adapters declare required secrets in the manifest (e.g., `secrets: [{ name: "ANTHROPIC_API_KEY", description, required }]`). Host resolves values from a configurable provider stack (env vars, file, OS keychain, vault, sops; pluggable). Values are passed to the adapter via a dedicated `secrets` field in `OpenSession` — never via the `config` field. The adapter's process environment is scrubbed by the sandbox (D29 / D32) so accidental `process.env.X` reads return undefined.

  **What "separate channel" means (concretely):** four mechanical separations stacked on top of the same wire transport — not a separate socket.
  1. **Distinct protobuf fields.** `OpenSessionRequest` carries both `config: map<string, string>` and `secrets: map<string, string>` as different fields with different field numbers. `ExecuteRequest` similarly carries both `input` and `secret_inputs`. The wire is the same (local UDS gRPC, or mTLS gRPC for remote); the **schema** isolates them.
  2. **Declarative sensitivity tag on the proto.** A custom protobuf field option `(criteria.sensitive) = true` is applied to every secret-carrying field. Generated code, the redaction registry, the protobuf reflection used by debug/audit tooling, and the host log pipeline all consult this option and either mask the value or refuse to serialize it. Sensitivity is structural — log lines that dump the request message cannot leak secrets even when written carelessly.
  3. **Distinct SDK API surfaces.** `sdk.config.get("X")` and `sdk.secrets.get("X")` (D69) are different functions backed by different maps in different memory locations. There is no `sdk.input.get(...)` that returns a secret. An adapter author cannot read a secret through a non-secret-aware code path even by accident.
  4. **Distinct host-side pipeline.** Code that handles `config` writes it to FSM nodes, prints it in plan output, logs it freely. Code that handles `secrets` (a) loads values from providers at session open, (b) registers values with the redaction registry before any cross the wire, (c) writes only origin references (`var.api_key`, `env:OTHER`) to FSM, checkpoint, lockfile, and audit log, and (d) re-resolves from the origin on resume (D67).

  **Transport security around the channel:**
  - Local: UDS gRPC, socket file at `0o600` in a host-only temp dir; process-to-process; OS is the isolation primitive. No encryption needed.
  - Remote: mTLS gRPC over HTTP/2 (D41). Entire connection is encrypted; the field-level secret distinction still applies on top.

  **Why not a separate socket?** Two connections share the same processes with the same memory access. They add lifecycle and reconnect complexity but do not reduce the attack surface. The schema-level separation (fields + sensitivity tag) prevents the realistic leak vectors — accidental log dumps, marshalling into checkpoint files, naive serialization — which a second socket would not address.
- **D20.** **Automatic log redaction.** The host registers each secret value with the log pipeline at session open. Any log line passing through host log handling (workflow log, run log, audit log, terminal renderer) is scanned and masked before display/persistence. SDK provides a redaction-aware logger so adapter-side logs also flow through the masker.
- **D21.** Secrets are **never** persisted: not in the lockfile, not in the compiled FSM, not in checkpoint files. Only references (provider URI + key, or workflow origin like `var.api_key`) are persisted where needed for re-resolution on resume.

### Secret tagging at the workflow level (unifies D19 across the whole pipeline)
The model in D19 covers secrets flowing **into** an adapter. The workflow language additionally lets users tag any value as secret so it stays protected as it flows **between** steps, **into** adapters, and **out of** the system. The flag propagates transitively (taint), is enforced by the compiler, and is consistent with the host's secret channel.

- **D61.** **`secret = true` flag on `variable` blocks.** A workflow variable marked secret is sourced like any other variable (CLI `--var`, `--var-file`, `CRITERIA_VAR_*` env), but the value is treated as tainted from the moment it enters the workflow: never logged, never written to plan output, never appears in lockfile or checkpoint. Only the origin reference (e.g., `var.api_key`) is persisted. On resume, the value is re-resolved from the origin; if the origin is gone, the run fails with a clear "missing secret <name>" message.
- **D62.** **`secret = true` flag on `shared_variable` blocks.** Same semantics for cross-step shared state. Reads taint the consumer; writes must be sourced from a secret-tagged value or a literal that the compiler then promotes.
- **D63.** **`sensitive: true` flag on output_schema fields.** Adapter declares which outputs are secret at the protocol layer:
  ```yaml
  output_schema:
    fields:
      token:      { type: string, sensitive: true }
      expires_in: { type: number }
  ```
  When the adapter returns `token`, the host registers the value with the redaction registry and marks any `step.X.outputs.token` reference as tainted at compile time.
- **D64.** **Adapter `secrets { ... }` block satisfaction.** A workflow can satisfy an adapter's declared secrets from three sources:
  ```hcl
  adapter "anthropic" "default" {
    secrets {
      ANTHROPIC_API_KEY = var.api_key                          # workflow-tagged variable
      VAULT_TOKEN       = step.vault_fetch.outputs.token        # sensitive output
      OTHER             = "env:OTHER_SECRET"                    # provider-stack reference
    }
  }
  ```
  All three flow through the secret channel into `OpenSession.secrets`. None ever appears in `config`.
- **D65.** **Taint propagation rule.** Once a value is tagged secret (origin: secret variable, sensitive output, or adapter-declared secret resolved from the provider stack), every downstream value derived from it is also tainted. The compiler refuses to interpolate a tainted value into a `config` map, an `input` map, a log/template string, or any other non-secret-channel destination. Attempting it is a compile error with a hint: *"value `var.api_key` is marked secret; bind it via `adapter.X.secrets { ... }` or `step.X.secret_inputs { ... }` instead."*
- **D66.** **Step-level secret inputs.** Steps gain a `secret_inputs { ... }` block parallel to `input { ... }`. Inputs flow to the adapter via a dedicated secret-input field in `ExecuteRequest` (mirroring the OpenSession secrets channel). Tainted values can only be bound into `secret_inputs`, not `input`.
- **D67.** **Persistence and resume.** Persisted state stores only origin references: `var.api_key`, `step.vault_fetch.outputs.token`, `env:OTHER_SECRET`. On `Restore` (D25) or resume from checkpoint, the host re-resolves each tainted value from its origin and re-registers it with the redaction registry before the adapter session resumes. If a tainted variable's origin is unresolvable on the resume host, the resume fails with a clear missing-secret message.
- **D68.** **Log redaction registry covers all tainted values**, not just adapter-declared ones. Same mechanism as D20.

### SDK dev/test loop for secret-handling adapters
- **D69.** **No env-var fallback in the SDK.** `sdk.secrets.get("NAME")` only consults the secrets map provided by the host in `OpenSession`. An adapter running without a host has no driver (nothing calls `Execute`) so the env-var fallback would weaken security in a code path that doesn't exist in practice.
- **D70.** **Each SDK ships a test-host harness** that exercises the real wire protocol with explicit-mock secrets:
  - TypeScript: `import { TestHost } from '@criteria/adapter-sdk/testing'` — programmatic API and a CLI (`criteria-ts-adapter-test`) accepting a YAML test file.
  - Python: `from criteria_adapter_sdk.testing import TestHost` — same shape; `criteria-py-adapter-test` CLI.
  - Go: `import "github.com/brokenbots/criteria-go-adapter-sdk/testhost"` — same shape; `criteria-go-adapter-test` CLI.
  Test files declare config, secrets, inputs, expected outcomes/events. The harness spawns the real adapter binary via go-plugin handshake; secrets are passed only via the dedicated channel.
- **D71.** **Library mode for unit tests** (optional, per-SDK). Each SDK exposes the adapter's `execute` handler as a directly-callable function for fast unit tests of business logic, without spawning a subprocess or doing IPC. Secrets are explicit function parameters. Does not exercise the wire protocol — paired with D70 harness tests for full coverage.

### Channel separation: variables vs. secrets vs. shell-outs
The environment block carries two distinct kinds of data that flow to adapters in different ways. Conflating them is the source of the leakage we are trying to eliminate, so the design is explicit:

- **D72.** **`environment.variables` flow as process environment variables on the adapter.** This is the existing v0.3 behavior and remains the case in v2. These are non-sensitive — `CI=true`, `LOG_LEVEL=debug`, `TZ=UTC`, etc. The compiler rejects any attempt to interpolate a tainted (secret) value into `environment.variables` (D65).
- **D73.** **`environment.secrets` (the provider configuration) only resolves values; values flow exclusively via the dedicated secret channel.** The host:
  1. Resolves each declared secret via the configured provider (env, file, vault, sops, keychain).
  2. Passes resolved values via `OpenSession.secrets` (D19) and/or `ExecuteRequest.secret_inputs` (D66).
  3. **Never** sets a secret as a process env var on the adapter.
  4. Scrubs the adapter's process env at sandbox setup (D29 / D32) so even host-inherited variables with secret-looking names are removed unless explicitly listed in `environment.variables`.
- **D74.** **Adapter responsibility when shelling out.** Because secrets are not in the adapter's process env, an adapter that exec's a child program that itself reads `os.environ["FOO_API_KEY"]` (e.g., the official `openai` CLI, `gh`, `aws`) must **explicitly** pass the secret into the child's env when constructing the subprocess call. This is intentional — it forces the adapter author to make a deliberate decision about which secrets cross the process boundary into which child. We document this prominently: every SDK's adapter-author guide opens with a section titled *"Shelling out to a child program: passing secrets safely."*
- **D75.** **SDK helpers for redaction-safe spawning.** Each SDK ships a `secrets.spawnEnv(...)` (or equivalent) helper that:
  - Takes an explicit list of secret names the adapter wants to forward (e.g., `["ANTHROPIC_API_KEY"]`).
  - Returns an env map suitable for passing into a subprocess spawn API (`child_process.spawn`, `subprocess.Popen`, `exec.Command`).
  - Re-registers the values with the SDK's redaction layer so any output the child emits and the adapter forwards to the host (via stdout capture, log streams) is still redacted.
  - Refuses to expose a secret the adapter didn't declare in its manifest (defense in depth — a typo in the secret name can't accidentally leak an unrelated value).

  Example (TypeScript):
  ```ts
  const env = await secrets.spawnEnv(["ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"]);
  const child = spawn("openai", ["chat", "completions", "create", ...args], { env, ... });
  ```
  Example (Go):
  ```go
  env, err := secrets.SpawnEnv(ctx, "ANTHROPIC_API_KEY")
  cmd := exec.CommandContext(ctx, "openai", args...)
  cmd.Env = env
  ```

### Protocol v2 surface (locked at the goal level)
v2 includes the **full** feature set:
- **D22.** `output_schema` field on `InfoResponse` (parallel to `config_schema` and `input_schema`).
- **D23.** Dedicated log stream channel separate from semantic Execute events; Execute events become purely structured (no interleaved stdout/stderr lines).
- **D24.** Bidirectional permission stream replacing the unary `Permit` callback: adapter can ask many questions in flight without per-question RPC roundtrips. Integration with the FSM:
  - **The bidi stream is below the FSM level.** The FSM only transitions on step outcomes (unchanged from v1). Permissions are an intra-step interaction between the adapter and the host. There is no new system component; what handles the stream is a small piece of code inside the existing session object.
  - **Concrete implementation:** a `PermissionState` field on the existing `Session` struct in `internal/adapter/sessions.go` (renamed from `internal/plugin/sessions.go` per WS01). It holds an in-memory `map[request_id]requestState` plus references to the policy evaluator (the `allow_tools` glob matcher, extended with env-block policy fields per D37) and the audit log writer. A single goroutine — spawned by the session, lifetime bounded by the session — reads `PermissionRequest` messages from the bidi stream, calls `policy.Evaluate(req)`, writes the decision back on the stream, and appends an audit entry. This runs concurrently with the `Execute` goroutine, exactly as the current `Permit` callback handler does, but without per-question round-trips. **Same process, same package, ~150 LOC of new code; not a service, not a sidecar.**
  - **Audit entry per decision:** `(session_id, request_id, tool, args_digest, decision, reason, evaluated_at)` appended to the existing run audit log file at `~/.criteria/runs/<run-id>/audit.log`.
  - **Snapshot/Restore behavior (D25).** When `Snapshot()` is called the host marshals the `PermissionState` map and a recent-decisions window into the snapshot blob alongside the adapter's opaque bytes — just a `proto.Marshal` on the struct. On `Restore()`: previously-answered requests are re-answered from the audit record (deterministic replay); unanswered requests are re-presented to policy evaluation. From the adapter's perspective the stream simply keeps producing answers — no protocol change to handle resume.
  - **Pause/Resume behavior (D25).** Pause cancels the goroutine's context; Resume restarts it from the persisted `PermissionState`. The adapter sees a long wait.
  - **Concurrency model.** Parallel steps each have their own session with its own `PermissionState`. The audit log is process-wide and serialized via the existing audit writer. Policy evaluation is per-request and stateless (modulo explicit rate-limit policies), so concurrency is naturally safe.
  - **Edge cases.** Permission denied → adapter decides locally how to react and emits a step outcome (e.g., `permission_denied`); the FSM transitions on that outcome with no new workflow-level machinery for permissions. Unanswered requests at session close are audit-logged as `session_closed_with_pending: N`.
- **D25.** Lifecycle ops: `Pause(session)`, `Resume(session)`, `Snapshot(session) → opaque bytes`, `Restore(session, bytes) → session_id`. Snapshot/Restore is the durable persistence story for long-running agent sessions across host restarts and remote handoffs.
- **D26.** `Inspect(session) → structured state` for operators and UIs (read-only view of session state, current step, pending permissions, last activity).
- **D27.** Message framing tuned for remote transports — chunked messages over a defined max size so payloads can flow across HTTP/2/WebSocket without head-of-line blocking; explicit ack/heartbeat at the protocol layer so disconnects are detectable independent of transport.

### Sandbox isolation — cross-platform model
Three layers of isolation are available, applied in this priority order based on environment-block policy:
1. **Host-native primary** (per OS): the strongest sandbox the host can apply in-process without external tools. Linux = namespaces/landlock/seccomp; macOS = `sandbox-exec`.
2. **Per-OS soft alternative**: an externally-installed tool the host can defer to when present and opted-in. Available on Linux (bubblewrap); **not available on macOS** — there is no third-party tool with bubblewrap-like reach that's widely installed, so no soft alternative exists on macOS.
3. **Container mode** (cross-platform): `environment.runtime = "docker" | "podman"` per D12c. Works identically on Linux (native Docker/podman) and macOS (via Docker Desktop, Colima, Lima, podman-machine — all expose the same `docker` / `podman` CLI). This is the consistent cross-platform "stronger than host-native" path.

Per-OS implementation details below.

### Sandbox implementation (Linux)
- **D28.** **No cgo anywhere in the criteria core binary.** Constraint: criteria ships as a single statically linkable Go binary across Linux/macOS.
- **D29.** **Host-native primary (Linux):** sandbox setup happens **in-process** in the criteria host (no shipped helper binary). Approach: fork+exec with `syscall.SysProcAttr.Cloneflags` for namespaces (CLONE_NEWUSER / NEWNS / NEWPID / NEWNET / NEWIPC / NEWUTS), pure-Go landlock via `github.com/landlock-lsm/go-landlock` (syscall-based, no cgo), pure-Go seccomp via `github.com/elastic/go-seccomp-bpf` or equivalent.
- **D30.** **Per-OS soft alternative (Linux):** bubblewrap (`bwrap`) is supported as a soft optional dependency. If `bwrap` is on PATH and the environment opts in, the host uses bubblewrap instead of in-process namespacing. Useful for users who already trust their distro's bubblewrap policies. The bubblewrap path is documented but never required. **No macOS equivalent exists** — see the cross-platform model above; macOS users who want a stronger sandbox than `sandbox-exec` provides use container mode.
- **D31.** Capability degradation: when a primitive is unavailable (older kernel without landlock, etc.), the host logs which protections were skipped and continues unless the environment declares `sandbox = "strict"`, in which case it fails.

### Sandbox implementation (macOS)
- **D32.** **Host-native primary (macOS):** auto-generated `sandbox-exec` profile rendered per session from the merger of (a) adapter-manifest declared hints and (b) the environment block's policy. Profile is written to a temp file (`$TMPDIR/criteria-sb-<session>.sb`), passed via `/usr/bin/sandbox-exec -f <profile> <adapter-binary>`, and deleted after exit.
- **D33.** Acknowledged: Apple has deprecated `sandbox-exec`, but it is the only host-native option available without third-party tools. We treat it as best-effort macOS isolation and document the limitation. **No per-OS soft alternative** is supported on macOS (D30 explains why); the cross-platform escape hatch is container mode (D12c).
- **D34.** macOS without `sandbox-exec` (e.g., a future macOS that removes it) falls back to process hardening + env scrub + working-dir confinement + secret redaction, with the same degradation rules as Linux (`environment.sandbox = "strict"` fails closed). At that point the recommended path becomes container mode for users wanting real isolation.

### Environment block as the sandbox/policy boundary
- **D35.** **Environment keeps the two-label HCL form**: `environment "<type>" "<name>" { ... }`. The **type** label is an extensible enum that selects the host's runtime path (`shell`, `sandbox`, `container`, with `vm` / `firecracker` / etc. as future additions); the **name** distinguishes multiple environments of the same type (`environment "container" "dev"`, `environment "container" "prod"`). v0.3 only registered `shell`; v2 adds `sandbox` and `container` and treats the type list as extensible going forward. Realizes the Phase 4 work flagged in `workflow/compile_environments.go` and `architecture_notes.md`.
- **D36.** **Adapter manifest declares hints**, not policy: required network destinations, filesystem reach (paths the adapter expects to read/write), required secrets, CPU/memory hints, required capabilities, and an optional `compatible_environments` field. Hints are advisory for policy fields (used to fill unset fields under permissive mode per D37 rule 2) and authoritative for compatibility — see D40-compat.

  **`compatible_environments` defaults to "any."** Most adapters are portable across all environment types and should not need to enumerate types — they should not need to be republished when a new environment type (e.g., a future `vm`) is added. The field is therefore optional:
  - **Absent (the common case)** → adapter is compatible with every registered environment type, including types added later.
  - **Present as a list** → adapter is compatible only with the listed types. Use this only when there's a real constraint (e.g., an adapter that requires a docker socket: `compatible_environments: ["container"]`; an adapter that won't work without sandbox-exec features: `compatible_environments: ["sandbox"]`).
  - **Present as `["*"]`** → explicit form of "any"; equivalent to absent. Accepted for clarity but not required.

  We do **not** offer an `incompatible_environments` deny list — the allow-list form (with default = any) covers the cases cleanly and avoids two ways to express the same thing.
- **D37.** **Environment block grants policy.** The resolution rule for each policy field, per session:
  1. **Field explicitly set in the environment block** (including explicit-empty / `"none"`) — environment is authoritative; the adapter's declared hint for this field is ignored.
  2. **Field unset in the environment block** — the adapter's declared hint (D36) provides the value as a default. The hint *is* the default when policy is silent.
  3. **`policy_mode = "strict"` on the environment** — flips rule 2: unset fields default to deny-all (empty allow lists, no network, no extra filesystem reach, no extra resources beyond builtin baselines). Adapter hints are never trusted as defaults under strict mode. Strict mode is the opt-in for zero-trust / enterprise deployments.

  The environment block expresses:
  - `policy_mode = "permissive" | "strict"` (default `"permissive"` — hints fill unset fields)
  - `sandbox = "strict" | "permissive" | "off"`
  - `filesystem { read = [...], write = [...] }`
  - `network { allow = [...] }` (host:port list, `"any"`, or `"none"`). Unset → adapter's `network` hint applies in permissive mode; deny-all in strict mode. Explicit `"none"` → always deny.
  - `secrets { provider = "env" | "file:..." | "keychain" | "vault:..." | "sops:..." ; allow = [...] }`
  - `resources { cpu = "...", memory = "...", timeout = "..." }`
  - `os = "linux" | "darwin"` (optional gate so e.g. a `prod` environment only applies on Linux)
  - For container-mode: `runtime = "docker" | "podman" | "none"` and runtime-specific options.

  **Example.** Adapter declares `network: ["api.anthropic.com:443"]` in its hints.
  - Environment has no `network` block → permissive mode default → allow `api.anthropic.com:443`.
  - Environment has `network { allow = ["api.openai.com:443"] }` → environment wins; only `api.openai.com:443` is allowed; the adapter's request to `api.anthropic.com` fails at first connect, clearly.
  - Environment has `network { allow = [] }` or `network = "none"` → explicit deny; adapter fails clearly.
  - Environment has `policy_mode = "strict"` and no `network` block → strict default → deny; adapter fails clearly.
- **D38.** **Multiple environments coexist; selection is per-adapter (or per-step) via HCL expressions over variables and locals.** There is no workflow-level `workflow { environment = ... }` selector — that approach was too coarse. Each adapter or step references its environment by bareword traversal of `<type>.<name>`; the reference can be a literal or a conditional expression. Example:

  ```hcl
  variable "deploy_env" {
    type    = string
    default = "dev"
  }

  environment "container" "dev_copilot" {
    policy_mode = "permissive"
    runtime     = "docker"
    network { allow = ["api.github.com:443"] }
    secrets  { provider = "env" }
  }

  environment "container" "prod_copilot" {
    policy_mode = "strict"
    runtime     = "docker"
    network { allow = ["api.github.com:443"] }
    secrets  { provider = "vault:secret/copilot" }
    resources { cpu = "2", memory = "1Gi", timeout = "5m" }
  }

  adapter "copilot" "default" {
    environment = var.deploy_env == "prod" ? container.prod_copilot : container.dev_copilot
  }
  ```

  Dev/prod switching is done via `criteria apply --var deploy_env=prod` (or via the variables file). Different adapters in the same workflow can resolve to different environments — and to environments of *different types* (e.g., a long-running agent on `container`, a quick query adapter on `sandbox`). Different steps within an adapter session can override (the existing precedence rule from v0.3: step `environment` attr > adapter `environment` attr — preserved).
- **D39.** **Type registry is extensible and code-backed.** The host registers an environment-type handler for each of `shell`, `sandbox`, `container`, with the type registry deliberately open for future additions (`vm`, `firecracker`, etc.). Each handler knows how to:
  - Validate the fields its type supports (e.g., `runtime = "docker" | "podman"` is meaningful for `container`, an error for `shell`).
  - Apply the policy when launching an adapter session of that type.
  - Report what kind of isolation it provides (so D40-compat can validate adapter compatibility).

  All policy fields from D37 (`policy_mode`, `sandbox`, `filesystem`, `network`, `secrets`, `resources`, `os`, plus type-specific extras like `runtime` for `container`, and the existing `variables` env-var injection from v0.3) are available; which subset is meaningful is determined by the type handler.
- **D40-compat.** **Adapter↔environment compatibility is validated at compile time, but only when the adapter has declared a constraint.** If the adapter's manifest omits `compatible_environments` (or sets it to `["*"]`), every environment type is acceptable and no compatibility check runs. If a list is present, every `adapter.X.Y.environment = <type>.<name>` reference is checked: if the resolved environment's type is not in the list, compile fails with a clear error pointing at both the adapter manifest and the environment declaration. Example error: *"adapter `criteria-adapter-foo` declares `compatible_environments: [container]`; cannot bind to `shell.default` (type `shell`). Either change `adapter.foo.default.environment` to a `container` environment or use a different adapter."*

### Forward-extensibility of the environment model

These properties are committed by the v2 design — they make it cheap to add new environment types and new host OSes later without breaking changes:

- **D40-extensible.** **The environment type label is an unrestricted string at the HCL grammar level.** Adding `vm`, `firecracker`, `kata`, `appcontainer`, etc. requires zero grammar changes. The type registry is the gatekeeper.
- **D40-typedecl.** **Each type handler advertises its OS support.** Every registered type's handler declares `supported_oses` (e.g., `["linux"]`, `["linux", "darwin"]`, `["windows"]`). The registry refuses to instantiate a type on a non-supported host with a clear error: *"environment type `<type>` is not supported on `<host_os>` — supported OSes for this type: <list>."* No runtime crashes deep inside a handler.
- **D40-osfield.** **`environment.os` is enforced at compile time.** A workflow declaring `os = "darwin"` fails on a Linux host with a clear error. The valid set is an open enum (`"linux"`, `"darwin"` for v1; `"windows"` added the day we lift the Windows non-goal — purely additive).
- **D40-orthogonal.** **Platform (binary OS+arch) and environment type (isolation kind) are orthogonal dimensions** and validated independently: D11 + D12c-alt check the binary; D40-compat checks the type; D40-typedecl checks the type-on-OS fit. The three checks together prevent any combination from silently producing a broken session.
- **D40-windows.** **Adding Windows later is a well-scoped checklist, not a redesign.** When the Windows non-goal is lifted: (a) add `"windows"` to the OS validator and to lockfile platform validation, (b) build the criteria host binary for `windows/amd64` (already pure-Go with no cgo per D28, so essentially trivial), (c) implement Windows-specific environment-type handlers (`appcontainer`, `jobobject`, etc.) or extend the existing `sandbox` handler with a Windows backend, (d) extend each SDK's release matrix (Bun, Nuitka, Go) to produce `windows/amd64` binaries. None of this requires a v2 protocol or grammar change.

### Remote adapter execution (reverse phone-home; adapter launch is not criteria's problem)

**Framing decision.** Remote adapter execution is achieved by **the adapter dialing into the host**, not by the host reaching out to start anything. criteria does not contain ECS, k8s, or SSH client code. The adapter is started however the user wants (k8s Deployment, ECS service, systemd unit, manual exec) and uses an SDK helper to phone home to the criteria host. The host has a small shim that accepts those inbound connections and presents them to the session layer as if they were local adapters.

```
host_criteria  ← (held HTTP/2 mTLS) ←  remote adapter (with sdk.serveRemote)
   ↑                                          ↓
  adapter_shim (local face: UDS gRPC          (started however the user
   to the host session layer)                  wants; criteria is not involved)
```

- **D40.** **No host-level `Transport` abstraction.** The host always speaks local UDS gRPC to its session layer. The "remote" connection is a separate mTLS HTTP/2 endpoint, terminated by a small shim that exposes a local UDS to the host session layer. The two halves are bridged inside the shim. No host code outside the shim is remote-aware.
- **D41.** **`remote` environment type.** Registered alongside `shell`, `sandbox`, `container`. Configures **only the host's listener and authentication policy** — not how to launch anything. Fields:
  - `listen_address` — host bind address for inbound adapter connections (e.g., `"0.0.0.0:7778"`, `"127.0.0.1:7778"`, or `"unix:/run/criteria-remote.sock"` for SSH/socat-forwarded scenarios).
  - `mtls { server_cert, server_key, client_ca, client_identity_pattern }` — mTLS auth for inbound connections; `client_identity_pattern` is a regex that the connecting client's certificate CN/SAN must match.
  - `accept_token` — optional bearer token an adapter must present on connect (in addition to mTLS).
  - `accept_digest_from = lockfile` (default) — adapter's reported digest at handshake must match the lockfile entry for this `adapter.X.Y`. Forgers can't impersonate an adapter even if they have a valid mTLS cert.
  - Standard policy fields (`policy_mode`, `network`, `filesystem`, `secrets`, `resources`) — **advisory only** for `remote` environments; the host can't enforce them on a process it didn't launch. The compiler emits a warning when these are set, an error in `policy_mode = "strict"` mode.

  **`remote` is the only backend in v1**; no ECS / k8s / SSH backends in criteria. Users who want adapters in those runtimes deploy them in those runtimes (via their normal tooling) and have them dial home.
- **D42.** **SDK gains a `serveRemote` mode.** Each SDK adds, alongside the existing `serve({...})`:

  ```ts
  serveRemote({
    host: "wss://criteria.example.com:7778",   // or grpcs://
    mtls: { client_cert, client_key, ca_bundle },
    accept_token: process.env.CRITERIA_REMOTE_TOKEN,
    identity: { name: "claude", version: "1.2.3", digest: "sha256:..." },
    // …the same adapter handler config as serve()
  });
  ```

  Behavior: dial out to `host`, complete the auth + identity handshake, then sit on the held connection serving `Info` / `OpenSession` / `Execute` / etc. exactly as `serve(...)` would over UDS. From the adapter author's perspective, `serve` vs `serveRemote` is one function-name change — everything else is the same. The OCI artifact is unchanged; the launcher script / container entrypoint chooses which mode to invoke.
- **D43.** **Host shim behavior** (per session):
  1. Workflow compile detects a `remote` environment reference; the shim listener is registered as part of the workflow's bring-up. If no remote environment is referenced, the listener is never started (compile-time folded).
  2. At workflow start, the shim begins listening on the configured address.
  3. Adapter connects out to the shim with mTLS + token + identity. Shim verifies the client cert, the token, and that the reported identity's digest matches the lockfile.
  4. Shim creates a local UDS socket and configures a go-plugin client in **`Reattach` mode** against that socket. The session layer (loader / discovery / sessions code) consumes it like a local adapter.
  5. Shim goroutine bridges the UDS socket and the held HTTP/2 connection — protocol bytes flow through unchanged.
  6. On session close, shim closes the UDS and the inbound HTTP/2; adapter sees the disconnect and either exits or waits for a new host connection (per SDK config).
- **D44-launch.** **Adapter launch is explicitly not criteria's problem.** Users start their remote adapter however they normally run long-running services — k8s Deployment, ECS service, systemd unit, `docker run -d`, `./adapter --remote=...` from a shell. criteria provides no tooling here. The starter-template repos (WS27) ship example k8s manifests / Dockerfiles / systemd units alongside the local-mode entrypoint so adapter authors have copy-pasteable starting points, but these are documentation, not infrastructure.
- **D44-reachability.** **Reachability is the user's problem.** The remote adapter must be able to reach the host's `listen_address`. For server-deployed criteria with a stable address, this is normal. For "laptop with workflow, adapter in some cloud" scenarios, the user must arrange reachability themselves (Tailscale, ngrok, a corporate VPN, a public host:port). criteria does not bundle a rendezvous service or a tunnel. Documented as an explicit limitation with pointers to common solutions.
- **D44-isolation.** **Host-side sandbox primitives (D29 / D32) do not apply to `remote` environments** — the host is not launching the adapter, so namespaces / landlock / seccomp / sandbox-exec are out of scope. The remote runtime (k8s SecurityContext, ECS task isolation, the OS the adapter runs on, etc.) is responsible for whatever isolation it provides. The environment block's `network`, `filesystem`, `resources` fields are advisory-only for `remote`.
- **D44-windows.** **`remote` works on Windows hosts the day Windows host support is added** without protocol or grammar changes. The shim is pure-Go; `supported_oses = ["linux", "darwin", "windows"]` from day one (even though `"windows"` isn't an accepted host OS yet under D40-osfield).
- **D44-rotation.** **Lifecycle is workflow-relative.** Shim listens from workflow start. Adapter may connect at any time before the host first invokes it (the FSM / engine will block on `OpenSession` until a matching adapter has connected, with a configurable timeout). Once connected, the connection is held until the workflow ends. If the connection drops mid-execution, the existing crash-policy machinery (`fail` / `respawn` / `abort_run` from v1, expanded for v2) decides what to do — there is no new "remote crash" concept.

### SDK matrix
- **D44.** v1 ships three SDKs:
  - **TypeScript** — refactor of existing `criteria-typescript-adapter-sdk`, Bun-compiled single binary, builds for linux/{amd64,arm64} and darwin/arm64.
  - **Python** — refactor of existing `criteria-python-adapter-sdk`, Nuitka-compiled single binary, same platform set.
  - **Go** — new SDK, native Go binary, same platform set. Lower friction for host-language developers; also lets us dogfood the v2 protocol from the host repo.
- **D45.** Each SDK uplift adds: session-state store helper, outcome-validation helper, permission-correlation helper, schema generation helpers (Zod-to-schema in TS, Pydantic-to-schema in Python, struct-tags in Go), redaction-aware logger, manifest extractor (`--emit-manifest`), and a `serve(...)` API consistent across languages.

### Starter templates and CI
- **D46.** Each SDK has a public GitHub repo template: `criteria-adapter-starter-typescript`, `criteria-adapter-starter-python`, `criteria-adapter-starter-go`. `gh repo create --template ...` produces a working hello-world adapter.
- **D47.** Each starter includes a GitHub Actions workflow that, on tag push:
  1. Builds multi-arch binaries.
  2. Runs the adapter once with `--emit-manifest` and validates schema.
  3. Constructs an OCI artifact via `oras` (per D10/D11) with binaries, manifest, annotations.
  4. Cosign-keyless-signs via sigstore (OIDC from the action token).
  5. Pushes to a registry of the developer's choice (parameterized; defaults to GHCR with `${{ github.repository_owner }}`).
- **D48.** GitLab CI and a "registry-agnostic" Makefile-only path are also shipped for users not on GitHub. The reference action and its scripts are factored into a reusable composite action / shared library.

### CLI surface
- **D49.** Adapter-specific commands live under a single `criteria adapter` command group, since the workflow team's `criteria pull <workflow_ref>` is the primary user entry point and pulls adapters transitively. Direct adapter management is an operator/dev concern.
- **D50.** Verbs under `criteria adapter`:
  - `criteria adapter pull <ref>` — pull a specific adapter, update lockfile.
  - `criteria adapter lock` — re-resolve all adapters referenced by workflows in the current directory and rewrite lockfile.
  - `criteria adapter publish <path>` — dev convenience for pushing a locally-built adapter to a registry (mirrors what CI does).
  - `criteria adapter list` — list cached adapters with versions and digests.
  - `criteria adapter info <ref>` — show manifest from cache (or pull and show).
  - `criteria adapter where <ref>` — print the on-disk binary path (useful for debugging, IDE integration).
  - `criteria adapter remove <ref>` — evict from cache.
  - `criteria adapter dev <path>` — load a local-built adapter binary for development, bypassing cache and lockfile; rejected if workflow `verification = "strict"`.
- **D51.** `criteria compile` auto-pulls any missing adapters that are pinned in `.criteria.lock.hcl`. If a workflow references an adapter not in the lockfile, compile fails with a hint to run `criteria adapter lock`.
- **D52.** When the workflow team's `criteria pull <workflow_ref>` pulls a workflow, the pulled artifact's `.criteria.lock.hcl` is the authoritative manifest of adapters to transitively pull. Workflow pull invokes the adapter cache for each pinned entry, reusing existing OCI cache layers when present.

### End-state repo independence
- **D58.** **No project may unilaterally change the adapter ecosystem.** By the close of this work the following repos exist as independent units, each with its own release cadence, versioning, and ownership:
  - `criteria` — host / engine / CLI.
  - `criteria-adapter-proto` *(new, extracted in WS41)* — `.proto` files and generated bindings for Go, TypeScript, and Python. Single source of truth for the wire contract. All consumers (host + every SDK) take this as a versioned dependency.
  - `criteria-go-adapter-sdk` *(new, WS25)*, `criteria-typescript-adapter-sdk` *(existing)*, `criteria-python-adapter-sdk` *(existing)* — one SDK per language, each consuming `criteria-adapter-proto`.
  - `criteria-adapter-starter-{typescript,python,go}` *(new, WS27)* — GitHub template repos.
  - `criteria-adapter-shell` *(new, WS42, extracted from `internal/builtin/shell/`)*, `criteria-adapter-greeter`, `criteria-adapter-claude`, `criteria-adapter-claude-agent`, `criteria-adapter-codex`, `criteria-adapter-openai`, `criteria-adapter-copilot` — one adapter per repo.
- **D59.** **Proto governance.** Changes to the adapter wire contract require a release of `criteria-adapter-proto`. Host and SDKs upgrade in lockstep across a proto bump. This makes wire-protocol changes deliberate, reviewable, and discoverable; no single project can drift the contract.
- **D60.** **Distribution channels for the proto package**:
  - Go: `github.com/brokenbots/criteria-adapter-proto` Go module.
  - TypeScript: `@criteria/adapter-proto` published to npm (or GHCR npm).
  - Python: `criteria-adapter-proto` published to PyPI.
  Each language target is built and published from the proto repo's CI on every tagged release.

### Cache layout
- **D53.** Local cache uses an **OCI image-spec-compliant layout** at `~/.criteria/cache/oci/`. Structure follows the OCI Image Layout spec:
  ```
  ~/.criteria/cache/oci/
    oci-layout                 # spec marker
    index.json                 # references all cached refs
    blobs/sha256/<digest>      # binaries, manifest blobs, signatures
  ```
- **D54.** Benefits: `oras` and other OCI tools can inspect and manipulate the cache directly (debugging, mirroring, offline transfer); refs are content-addressed so duplicates are de-duped; eviction is straightforward GC over `index.json`.
- **D55.** Cache is shared across all workflows on the host. Eviction is by explicit `criteria adapter remove <ref>`, by `criteria adapter prune --older-than` / `--max-size`, and by global config (`cache.max_size`, `cache.gc_interval`).

### Migration
- **D56.** All seven existing adapters (`greeter`, `shell`, `claude`, `claude-agent`, `codex`, `openai`, `copilot`) are migrated to protocol v2 as a **blocking precondition** for the v2 release. v1 host code paths are deleted only after the seven adapters run on v2 in CI. Migration order is loosely: `greeter` (sanity-check the new SDK), `shell` (in-tree builtin), then the four external production adapters; `copilot` last because it has the richest permission model.

### Verification gates
- **D57.** Four-stage release gate for v2:
  1. **Protocol conformance suite** — exercises every v2 RPC across all three SDKs on every supported platform. Builds on and replaces the existing conformance harness at `internal/adapter/conformance/`.
  2. **Adapter migration in CI** — all seven migrated adapters run representative workflows in criteria CI, with lockfile + signature + sandbox + secrets all exercised on each run.
  3. **Remote transport end-to-end** — a documented runbook + CI smoke test launches one adapter on a remote host via mTLS gRPC and runs a workflow against it.
  4. **Publishing-flow gate** — the three starter-template repos build, sign, and publish to a CI-owned GHCR org on every PR merge. Failure here blocks release.

---

## Workstreams

The team works workstreams **in order**. Each workstream is sized to a **single PR**. Foundational items come first, higher-level items later, adapter migrations and CI scaffolding at the top of the stack. Individual workstream files (one per WS) will be authored in the criteria project's `workstreams/` directory using its established format.

### Foundation (must land before anything else)

- **WS01 — Terminology unification.** Rename `internal/plugin/` → `internal/adapter/`; rename `AdapterPluginService` → `AdapterService`; rename `PluginName` → `AdapterName`; retitle `docs/plugins.md` → `docs/adapters.md`; update all comments, log lines, and identifiers. Code-only, no behavior change. Establishes consistent terminology for everything that follows.
- **WS02 — Protocol v2 proto + Go bindings.** Author `proto/criteria/v2/adapter.proto` with all RPCs from D22–D27: `Info` (with `output_schema`), `OpenSession` (with `secrets` map), `Execute` (semantic events only, no log lines), `Log` (server-stream, dedicated), `Permissions` (bidirectional stream replacing `Permit`), `Pause`, `Resume`, `Snapshot`, `Restore`, `Inspect`, `CloseSession`. Generate Go bindings. No host integration yet — proto + types + unit tests only.
- **WS03 — Host adapter wire wired to v2.** Refactor the existing go-plugin-based host code to speak the v2 wire format over UDS gRPC (the only host-level wire — there is no separate transport abstraction; remote execution is handled by the `remote` environment per D40–D43, not by a host-level transport). Replace v1 call sites in the host with v2 calls. Delete v1 proto and v1 code paths. Expose a small `LocalSocketDialer` helper that opens a go-plugin client in `Reattach` mode against a given local socket path — this is reused by the `remote` environment handler (WS20).

### Distribution + integrity

- **WS04 — OCI cache layout.** Implement OCI-image-spec-compliant cache at `~/.criteria/cache/oci/` (D53–D55). Use `oras-go` (pure Go). Provide `Pull(ref) → digest`, `Resolve(ref) → digest`, `Open(digest) → fs.FS` APIs. Tests against a local OCI registry (`ghcr.io/oras-project/registry`) and an on-disk OCI layout fixture.
- **WS05 — Adapter manifest format.** Define `adapter.yaml` schema (D13–D15): name, version, capabilities, config/input/output schemas, declared secrets, declared permissions, platforms, SDK protocol version. Implement OCI annotation mirror for fast inspection. Implement runtime verification (`Info()` response vs static manifest).
- **WS06 — Cosign signing and verification.** Integrate `sigstore-go` for keyless verification (D16–D18). Support explicit key verification. Implement `verification = "strict" | "warn" | "off"` policy. Lockfile records signer identity. `--allow-unsigned` development flag.
- **WS07 — Lockfile.** Define `.criteria.lock.hcl` grammar: per-adapter entries with full OCI ref, resolved digest, signer identity, SDK protocol version, source URL, transport. Implement read/write/diff helpers. Lockfile lives next to workflow files and is read by the compiler.
- **WS08 — `criteria adapter` CLI group.** Cobra subcommand with verbs from D50: `pull`, `lock`, `publish`, `list`, `info`, `where`, `remove`, `prune`, `dev`. Wires WS04–WS07 to user-facing flows. Includes compile-time auto-pull (D51) and transitive-pull contract for workflow pulls (D52).

### Security and isolation

- **WS09 — Environment block extension + secret-taint compiler.** Keep the existing two-label HCL form `environment "<type>" "<name>"` (D35) — the type label is an unrestricted string at the grammar level (D40-extensible). Extend the type registry beyond `shell` to add `sandbox` and `container` (D39), with the registry deliberately open for future additions. Each registered type has a code-backed handler that validates its supported fields, applies its policy at session launch, reports its isolation kind, **and advertises `supported_oses` so the registry can refuse incompatible host/type combinations with a clear error (D40-typedecl)**. Add policy fields per D37: `policy_mode`, `sandbox`, `filesystem`, `network`, `secrets`, `resources`, `os`, plus type-specific extras (e.g., `runtime` for `container`). Enforce `environment.os` at compile time against host OS (D40-osfield) — open enum so `"windows"` can be added later. Implement the field-resolution rule (D37 rules 1–3): hint defaults when unset in permissive mode, explicit policy wins, strict mode denies by default. Implement adapter↔environment compatibility validation at compile time (D40-compat) using `compatible_environments` from the adapter manifest (default = any per D36). Adapter/step `environment = <expr>` references accept HCL expressions over variables and locals (D38) so dev/prod switching is just normal HCL plumbing — no workflow-level selector. **Also lands the workflow-level secret-tagging surface (D61–D67):** `secret = true` on `variable` and `shared_variable`, `secret_inputs` step block parallel to `input`, taint propagation in the compiler (D65), compile errors for tainted-value-into-non-secret-channel attempts, and persistence of origin references only (D67).
- **WS10 — Linux sandbox.** In-process pure-Go isolation (D28–D31): namespaces via `syscall.SysProcAttr.Cloneflags`, landlock via `github.com/landlock-lsm/go-landlock`, seccomp via pure-Go BPF helpers. Bubblewrap soft-dependency path when `bwrap` is on PATH and environment opts in. Capability-degradation logic + `sandbox = "strict"` fail-closed.
- **WS11 — macOS sandbox.** Auto-generated `sandbox-exec` SBPL profile rendered per session from adapter hints + environment policy (D32–D34). Profile written to temp, applied via `/usr/bin/sandbox-exec -f <profile>`, deleted on exit. Fallback to process hardening when sandbox-exec is unavailable.
- **WS12 — Container-mode runtime.** Implement the container-mode runtime selection logic from D12c: when `environment.runtime ∈ {docker, podman}` and the adapter has published a runnable image (`adapter.yaml.container_image` present), invoke `docker run <image>` directly with the appropriate auth/socket plumbing; when no image is published, fall back to wrapping the artifact binary in a host-provided minimal rootfs. Cgroup limits, network mode, mount specifications driven by the environment block. Log the chosen path clearly so users can tell which one ran.
- **WS13 — Secret channel + redaction registry.** Implement `secrets` map in `OpenSession` (D19) and a parallel `secret_inputs` field in `ExecuteRequest` (D66) — both separate from `config`/`input`. Provider stack: env / file / OS keychain / vault / sops; pluggable. Host log pipeline registers values from **both** adapter-declared secrets (D19) and workflow-tagged values (D68) for masking. Redaction-aware logger in host. No persistence of plaintext (D21, D67); resume re-resolves from origin references and re-registers before the session resumes.

### Protocol v2 feature surface

- **WS14 — Output schema (with sensitive fields).** Wire `output_schema` through Info → compile-time validation of step output usage. Update the FSM compiler to validate `steps.X.outputs.Y` references against the adapter's declared output schema. Honor the `sensitive: true` field flag (D63): outputs marked sensitive automatically taint downstream references and are registered with the redaction registry at runtime when emitted.
- **WS15 — Dedicated log channel.** Implement the `Log` server-stream RPC and separate log routing from `Execute` event stream. Update host event consumer to merge log+execute streams by timestamp for display.
- **WS16 — Bidirectional permission stream.** Replace unary `Permit` with `Permissions` bidi stream. Add a `PermissionState` field to the existing `Session` struct in `internal/adapter/sessions.go` and a session-bounded goroutine that reads from the stream, calls the existing policy evaluator (extended with env-block policy per D37), writes the decision back, and appends to the run audit log (D24). Queue + recent-decisions window marshalled into snapshot blobs via proto; restored deterministically. Pause cancels the goroutine context; Resume restarts from the persisted state. Unanswered requests at session close are audit-logged. **Same process, same package — not a new service.** No FSM-transition changes — permissions remain below the FSM level; the FSM still transitions only on step outcomes.
- **WS17 — Pause / Resume / Inspect.** Implement the three lifecycle ops on host + SDK base classes. Hook Pause/Resume into engine cancellation and run-resumption flow. `Inspect` returns structured state for operators and UIs.
- **WS18 — Snapshot / Restore.** Opaque-blob session snapshot and restore. Host persists snapshots under `~/.criteria/runs/<run-id>/snapshots/<session>/<seq>.bin`. Each snapshot bundles the adapter's opaque session state **and** the host's permission-handler queue + decision log for that session (per D24). Engine-level integration for resuming a paused workflow against a restored adapter session, including deterministic replay of previously-answered permission requests from the audit record.
- **WS19 — Remote-friendly framing.** Chunking for messages above a defined max size; explicit heartbeat/ack at the protocol layer. Independent of transport, but a prerequisite for WS21.

### Remote adapter execution (reverse phone-home)

- **WS20 — `remote` environment type + host shim.** Implement the `remote` environment type in the type registry (D41) with the listener + mTLS + token + lockfile-digest verification + advisory-policy fields. Implement the host shim (D43): mTLS HTTP/2 listener; per-connection bridge that creates a local UDS, configures a go-plugin client in `Reattach` mode against it, and proxies bytes between the UDS and the held HTTP/2 connection. Compile-time folding so the listener isn't started for workflows that don't reference a `remote` environment. Wire-up so the existing crash-policy machinery handles disconnect/reconnect (D44-rotation).
- **WS21 — SDK `serveRemote` mode across all three SDKs.** Add the `serveRemote({ host, mtls, accept_token, identity, ... })` entrypoint to the TypeScript, Python, and Go SDKs (D42). Same handler config as `serve(...)`; the difference is dial-out + auth + identity handshake. Identity handshake includes the adapter's manifest digest so the host can verify it matches the lockfile. Documentation in each SDK README and starter template (WS27) showing example k8s Deployment manifests, Dockerfiles, and systemd units for adapter authors who want to provide deployment guidance to their users.
- **WS22 — End-to-end remote demo runbook + CI smoke test.** Documented runbook for deploying a remote adapter (k8s Deployment example for the reference; ECS example as a documentation supplement). CI smoke test (D57.3 / WS38): spin up a remote adapter in a separate container on the CI host, have it phone home to the test criteria instance over mTLS, run a representative workflow, kill the remote process mid-execution to exercise crash-policy recovery. Note that **criteria itself contains no ECS or k8s code** — the demo invokes those tools externally (e.g., the CI workflow uses `kubectl apply`, not criteria).

### SDKs

- **WS23 — TypeScript SDK v2.** Refactor `criteria-typescript-adapter-sdk` against protocol v2. Add helpers: `SessionStore`, `OutcomeValidator`, `PermissionCorrelator`, `RedactingLogger`, `SchemaFromZod`, `secrets.get("NAME")` (D69), **`secrets.spawnEnv([...])` redaction-safe subprocess env helper (D75)**, `--emit-manifest` mode. Ship `TestHost` programmatic API + `criteria-ts-adapter-test` CLI (D70) and the optional library-mode entry (D71). README opens with the "Shelling out: passing secrets safely" section (D74). Maintain Bun-compile-to-single-binary build.
- **WS24 — Python SDK v2.** Same shape for `criteria-python-adapter-sdk`. Async-first. Pydantic-to-schema. `secrets.get("NAME")` and `secrets.spawn_env([...])` (D69, D75). Test-host harness (D70–D71). Same README opener (D74). Nuitka single-binary build.
- **WS25 — Go SDK v1.0.** New repo `criteria-go-adapter-sdk`. Same `serve(...)` API shape as TS/Python. struct-tag-based schema generation. `secrets.Get("NAME")` and `secrets.SpawnEnv(ctx, ...)` (D69, D75). Test-host harness (D70–D71). Same README opener (D74). Native Go binary.
- **WS26 — Cross-language SDK conformance harness.** Test driver that exercises every protocol v2 RPC against each SDK on each platform. Lives in criteria's `internal/adapter/conformance/` so the suite gates SDK changes (replaces and extends current harness; coordinates with existing `test-01` workstream).

### CI scaffolding and distribution

- **WS27 — Starter repos.** Three GitHub template repos: `criteria-adapter-starter-typescript`, `criteria-adapter-starter-python`, `criteria-adapter-starter-go`. Each is a working hello-world adapter against the relevant SDK; `gh repo create --template` produces a build-able new adapter (D46). Each starter ships with: a working `serve(...)` adapter, a CI workflow consuming the WS28 publish action with `with_image: false` by default, and a commented `Dockerfile` (D12a) showing how to opt into image publishing by flipping the workflow input to `true`.
- **WS28 — Reusable publish action.** Composite GitHub Action `criteria/publish-adapter` with two modes governed by a `with_image: bool` input (default `false`):
  - **Artifact mode** (always runs): multi-arch build → manifest emit → OCI artifact construction via `oras` → cosign keyless sign → push to registry (D47).
  - **Image mode** (when `with_image: true`, per D12d): additionally builds the Dockerfile in the adapter repo into a runnable container image, signs it independently with cosign, pushes under `<name>:<version>-image`, and updates the published `adapter.yaml` with the `container_image` block (D12b).
  - Used by all three starters and by adapter-migration WSes.
- **WS29 — GitLab CI + Makefile-only paths.** Equivalent pipelines for users not on GitHub (D48). Documented as supported paths in adapter-author docs.

### Adapter migrations (blocking precondition)

All adapter-migration workstreams must replace any `process.env.X` (or equivalent) reads with `sdk.secrets.get("X")` (D69) and declare the corresponding entries in the adapter manifest's `secrets:` list. The adapter binary's process environment is scrubbed by the sandbox, so any missed migration will fail loudly at first run.

- **WS30 — Migrate `greeter`.** Smallest adapter; sanity-checks SDK ergonomics and the publish action. Lands in `criteria-typescript-adapter-greeter` against TS SDK v2. No secrets to migrate.
- **WS31 — Migrate `shell` to v2 (still in-tree).** Migrate `internal/builtin/shell/` to protocol v2 against the Go SDK (consumed as a local module). Stays in-tree for this WS — extraction to its own repo happens in WS42.
- **WS32 — Migrate `claude`.** Reference TS production adapter against v2. Demonstrates session state helper, outcome validator, redacting logger.
- **WS33 — Migrate `claude-agent`.** Demonstrates permission correlator with the new bidi permission stream.
- **WS34 — Migrate `codex`.** Demonstrates Zod schema generation. Verifies edge cases around streaming.
- **WS35 — Migrate `openai`.** Verifies multi-provider patterns; second TS production adapter.
- **WS36 — Migrate `copilot`.** Last; richest permission model. Final stress test for the protocol and SDK helpers.

### Release gate

- **WS37 — v1 protocol code removal.** Now that all seven adapters run on v2, delete v1 host code paths, v1 proto, v1 conformance fixtures. Confirm no `criteria-adapter-*` v1 references remain.
- **WS38 — Remote transport end-to-end demo.** Documented runbook + CI smoke test (D57.3). Launches an adapter on a remote host via mTLS gRPC, runs a representative workflow, captures logs and metrics.
- **WS39 — Documentation refresh.** Rewrite `docs/adapters.md`, author migration guide for adapter developers, document the security model, document the environment block extensions, document the lockfile, document the CLI, document remote adapters.
- **WS40 — v2 release gate.** Stand up the four verification gates from D57. Tag release.

### End-state independence (final step — D58–D60)

- **WS41 — Extract `criteria-adapter-proto` to its own repo.** Move `proto/criteria/v2/` out of the criteria repo into a new standalone repo `criteria-adapter-proto`. Set up CI to build and publish language packages on every tagged release: Go module (`github.com/brokenbots/criteria-adapter-proto`), npm (`@criteria/adapter-proto`), PyPI (`criteria-adapter-proto`). Switch host and all three SDKs to depend on the published packages. Delete the in-tree proto. After this WS, the wire contract is governed by an independent repo and no consumer can change it unilaterally.
- **WS42 — Extract `criteria-adapter-shell` to its own repo.** Move `internal/builtin/shell/` out of the criteria repo to a new standalone repo `criteria-adapter-shell`. Adopt the standard adapter build pipeline (multi-arch binary, manifest, cosign-keyless-signed OCI artifact published to GHCR via the WS28 publish action). Update criteria to remove the builtin shortcut path and load `shell` like any other adapter (with a baked-in default registry ref). After this WS, criteria's host code has zero in-tree adapter implementations.
- **WS43 — Independence verification.** Confirm the end state: criteria repo contains only host/engine/CLI code (no adapter implementations, no proto sources). All three SDKs are in their own repos consuming `criteria-adapter-proto` as a versioned dependency. All seven adapters are in their own repos. The published proto package version pinned in each consumer is documented in a `DEPENDENCIES.md` table maintained by the proto repo's release process. End-to-end smoke test: `criteria pull <workflow_ref>` from a clean machine successfully pulls a workflow whose `.criteria.lock.hcl` references adapters built from each of the three SDKs, and the workflow runs to completion.

---

## Verification

End-to-end checks gated by **WS40**:

1. **Conformance suite** runs every v2 RPC against TS, Python, and Go SDKs on linux/{amd64,arm64} and darwin/arm64. Run command (from criteria repo):
   ```sh
   go test -race ./internal/adapter/conformance/...
   ```

2. **All seven migrated adapters** run their representative workflows in criteria CI on every PR:
   - `greeter` — minimal smoke test.
   - `shell` — builtin, exercises sandbox.
   - `claude`, `claude-agent`, `codex`, `openai` — exercise secrets channel, redaction, output schema, session state.
   - `copilot` — exercises bidirectional permission stream.

3. **Lockfile + signature + sandbox + secrets** all exercised on every CI run:
   - Workflows include `.criteria.lock.hcl` with cosign-keyless-signed digests.
   - Verification mode `strict`.
   - Environment block grants different policies per workflow to exercise allow/deny paths.

4. **Remote transport demo** runs in CI as a smoke test:
   - One adapter is launched in a separate container on the CI host.
   - mTLS handshake completes.
   - A workflow runs end-to-end against the remote adapter.
   - Heartbeat-loss recovery exercised by killing the remote process mid-execution.

5. **Publishing flow** runs on every PR to each starter-template repo:
   - Build → manifest emit → OCI artifact construction → cosign keyless sign → push to GHCR.
   - The published artifact is then pulled by criteria CI and run through the conformance suite.

6. **Manual demo**: `criteria pull <workflow_ref>` from a fresh machine resolves the workflow's lockfile, pulls and verifies all referenced adapters, runs the workflow successfully, and `criteria adapter list` shows the cached adapters with digests and signers.

---

## Critical files (touched by this work)

### Host (criteria)
- `proto/criteria/v2/adapter.proto` *(new in WS02; moved out of repo in WS41)*
- `internal/adapter/` *(renamed from `internal/plugin/`)* — discovery, loader, sessions, local UDS gRPC wire
- `internal/adapter/environment/` *(new)* — registered environment type handlers
  - `shell/` — variables injection (existing v0.3 behavior, kept)
  - `sandbox/{linux,darwin,common}.go` *(new)* — OS-native sandbox primitives
  - `container/` *(new)* — docker/podman wrapping
  - `remote/` *(new)* — `remote` environment type: shim listener + mTLS server + lockfile-digest verifier + per-connection UDS bridge using `Reattach` mode. No ECS / k8s / SSH client code; the user starts the remote adapter out-of-band, the adapter dials in via `sdk.serveRemote`.
- `internal/adapter/oci/` *(new)* — oras-go-based pull, cache, verify
- `internal/adapter/secrets/` *(new)* — provider stack, redaction registry
- `internal/cli/adapter_*.go` *(new)* — pull/lock/publish/list/info/where/remove/dev (all under `criteria adapter` group)
- `workflow/schema.go` — extend `EnvironmentSpec` and `AdapterDeclSpec` with v2 fields
- `workflow/compile_environments.go` — type registry, policy field validation
- `workflow/lockfile.go` *(new)* — `.criteria.lock.hcl` read/write/diff
- `internal/builtin/shell/` *(migrated to v2 in WS31; deleted in WS42 when shell becomes an external adapter)*
- `internal/adapter/conformance/` — expanded suite covering v2 RPCs across SDKs
- `docs/adapters.md` *(renamed from `docs/plugins.md`)*
- `go.mod` — consumes `github.com/brokenbots/criteria-adapter-proto` after WS41

### Adapter wire contract (independent repo, created in WS41)
- `criteria-adapter-proto` *(new)* — `.proto` sources, generated bindings, multi-language CI publishing pipeline.
  - Go module: `github.com/brokenbots/criteria-adapter-proto`
  - npm: `@criteria/adapter-proto`
  - PyPI: `criteria-adapter-proto`

### SDKs (separate repos)
- `criteria-typescript-adapter-sdk` *(existing)* — v2 uplift; new helpers; `--emit-manifest`; consumes `@criteria/adapter-proto` after WS41
- `criteria-python-adapter-sdk` *(existing)* — v2 uplift; consumes `criteria-adapter-proto` PyPI package after WS41
- `criteria-go-adapter-sdk` *(new, WS25)* — consumes `github.com/brokenbots/criteria-adapter-proto` after WS41
- `criteria-adapter-starter-{typescript,python,go}` *(new, WS27)* — GitHub template repos
- `criteria/publish-adapter` *(new, WS28)* — reusable composite GitHub Action (shared by all starters and adapter repos)

### Adapter repos (each independent, one per adapter)
- `criteria-adapter-shell` *(new in WS42, extracted from `internal/builtin/shell/`)*
- `criteria-typescript-adapter-greeter` *(existing, migrated in WS30)*
- `criteria-typescript-adapter-claude` *(existing, migrated in WS32)*
- `criteria-typescript-adapter-claude-agent` *(existing, migrated in WS33)*
- `criteria-typescript-adapter-codex` *(existing, migrated in WS34)*
- `criteria-typescript-adapter-openai` *(existing, migrated in WS35)*
- `criteria-adapter-copilot` *(existing; verify SDK language before WS36)*

---

## Open questions / parking lot

These remain for resolution during workstream authoring, not now:

- **Output schema shape**: free-form JSON Schema, or a constrained type-vocabulary mirroring `config_schema`/`input_schema`? Probably mirror the existing schema to keep consistency. Decide in WS05/WS14.
- **Lockfile drift detection**: when a workflow is edited to reference a new adapter or version, what's the exact error mode? Soft warning on compile vs. hard failure? Pin to WS07.
- **Snapshot/restore portability**: are session snapshots portable across host architectures? Probably not in v1 — record the snapshot host's arch in metadata and refuse mismatched restores. Decide in WS18.
- **Bubblewrap policy mapping**: how environment-block policy fields map to `bwrap` flags. Decide in WS10.
- **Cosign keyless TUF root refresh policy**: pinned root vs. auto-refresh. Decide in WS06.
- **Copilot adapter language**: confirm whether `copilot` is TS or another language — affects which SDK migration covers WS36. Verify before kickoff.

## Workstreams

*(populated near the end, once decisions are locked)*

## Verification

*(populated near the end)*

---

## Open questions / parking lot

- Release scope: which of the nine goals are v1 must-have vs. v2 / scaffold-only?
- Terminology lock: confirm "adapter" everywhere (likely yes, since users see it in HCL).
- Sandbox baseline for v1: subprocess hardening + namespaces, full container, seccomp/landlock, or WASM?
- Distribution: OCI as required path for production. URL-zip via go-getter for dev. Anything else? Git refs? Local path?
- Lockfile scope: per-workflow file (terraform-style `.criteria.lock.hcl`)? Project-level? Both?
- SDK language priorities beyond TS and Python: Go? Rust? Others?
- Backward compatibility: clean break to protocol v2 with shim, or maintain v1 wire compat?
- Remote adapters: protocol-only scaffold in v1, or one working transport (e.g., HTTP/2 over TLS)?
