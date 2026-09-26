# Adapters

This is the reference for running adapter-backed workflows with Criteria and for
authoring your own adapters. For the workflow language itself (variables, step
outputs, branching, iteration, wait nodes, approval gates) see
[workflow.md](workflow.md).

> **Status.** The adapter protocol (v2) and Go SDK are recently reworked and need
> broad testing; only the `copilot` and `shell` adapters have real use. The
> TypeScript/Python SDKs and the `sandbox`/`container`/`remote` environments are
> lightly tested at best. This document describes the intended model; see
> [README → Component status](../README.md#component-status) for what is exercised
> today.

## Concepts

- **Adapter** — an out-of-process program that performs work for a workflow step
  (an LLM agent, a shell runner, an API client). The host speaks a versioned
  gRPC protocol (v2) to it over a local transport; the adapter stays outside the
  Criteria process boundary, so its failures are isolated from the engine.
- **OCI artifact** — the distribution unit. An adapter version is published as a
  multi-platform OCI artifact (per-platform binary blobs + an `adapter.yaml`
  manifest), to any OCI-compliant registry (GHCR, ECR, GAR, Harbor, self-hosted).
  There is no central registry; you reference adapters by their registry URL.
- **Manifest (`adapter.yaml`)** — code-declared metadata the adapter emits about
  itself: name, version, capabilities, config/input/output schemas, declared
  secrets, supported platforms, and an optional container-image reference. The
  host reads it at pull time without launching the adapter.
- **Lockfile (`.criteria.lock.hcl`)** — a Terraform-style file committed next to
  your workflow that pins every referenced adapter by digest, records the signer
  identity, and (for fetched subworkflows) pins the resolved commit/archive
  identifier. Each workflow directory owns its own lockfile; there is no
  aggregate root lockfile.
- **Signing** — published artifacts are signed with cosign. The default CI path
  is **keyless** (Sigstore/Fulcio, no long-lived keys); explicit Ed25519 keys are
  also supported. The host verifies signatures at pull time against a
  configurable trust policy.
- **Environment** — the sandbox/policy boundary a step's adapter runs under
  (`shell`, `sandbox`, `container`, `remote`). Declared in HCL and bound to
  adapters or steps.

## Quickstart

### 1. Reference an adapter and run a workflow

Declare an adapter by its OCI reference and bind steps to it:

```hcl
workflow {
  name          = "agent_hello"
  version       = "1"
  initial_state = "ask"
  target_state  = "done"
}

adapter "claude" "assistant" {
  source  = "ghcr.io/your-org/criteria-adapter-claude" # repo path, version-decoupled
  version = "0.5.0"                                     # semver: "1.2.3", "^1.2", "latest"
  config {
    max_turns = 4
  }
}

step "ask" {
  target = adapter.claude.assistant
  input {
    prompt = "Summarize the repository's README in two sentences."
  }
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
```

- The first label is the adapter **type**, the second an instance **name**; steps
  bind via `target = adapter.<type>.<name>` (a traversal, not a string).
- Adapters have a two-phase lifecycle (see [Adapter session lifecycle](#adapter-session-lifecycle)):
  every adapter is **verified** eagerly at run start, but its working-directory
  **session binding** is deferred until the first step that actually targets it.
- The engine closes sessions automatically when the scope ends — no explicit
  close step is required.

### 2. Pin adapters in the lockfile

```bash
criteria adapter lock        # resolve every referenced adapter, pin digests
```

`criteria adapter lock` is recursive by default. It walks every workflow
directory reachable from the root — local subworkflows and fetched workflow
references alike — and writes a `.criteria.lock.hcl` in **each** directory. Each
lockfile covers only the adapters declared in that directory, so any workflow
directory remains independently shippable and carries the pins its author tested
against. There is no aggregate root lockfile that overrides subworkflow pins.

Use `--no-recursive` to lock only the named directory, preserving the previous
single-directory behaviour. A second recursive run touches no file when the tree
is already up to date.

`criteria adapter lock --upgrade` re-resolves version constraints and accepts
digest drift under immutable version pins. It also recurses; plain `lock` does
not accept drift but still re-fetches and re-verifies every pinned digest.

If a workflow references OCI adapters but no lockfile entry exists, `validate`
and `apply` fail with an error naming the workflow directory and adapter and
directing you to run `criteria adapter lock`.

#### Fetched workflow references

A `subworkflow` whose `source` is a git ref or archive is fetched into the local
cache and pinned in the **parent** lockfile as a `workflow_ref` block:

```hcl
workflow_ref {
  source       = "git::https://github.com/example/criteria-workflows?ref=main"
  resolved_ref = "sha256:abc123..."  # resolved commit SHA or archive digest
}
```

The source reference is recorded as written; the `resolved_ref` is the immutable
identifier (commit SHA for git, content digest for archives). Subsequent runs
fetch by that resolved identifier, so a mutable branch is pinned at lock time and
the fetched tree is reproducible.

A fetched workflow must ship a complete lockfile covering its own adapters. The
three states are defined:

- **Complete lockfile** — the recorded pins are used as-is. Adapters are pulled
  by digest; signature verification follows the local trust policy. Declared
  version constraints are not re-resolved.
- **No lockfile** — the run fails, naming the fetched workflow and stating that
  `criteria adapter lock` against it will generate the pins.
- **Partial or stale lockfile** — treated as missing for the uncovered adapters;
  the run fails naming each unpinned adapter. A lockfile entry whose version no
  longer satisfies the declared constraint is also rejected.

#### Local inventory provenance

Every adapter pulled through Criteria records its original `reference` and
`source_url` as OCI index annotations. `criteria adapter list` shows these
alongside the digest, so cached entries can be traced back to where they came
from. Entries that predate this feature are labelled `(unattributed)` and can be
cleared with `criteria adapter prune --unattributed-only`.

### 3. Manage the local cache directly (optional)

| Command | Purpose |
|---|---|
| `criteria adapter pull <ref>` | Fetch an artifact into the cache (verifies signature). `--allow-unsigned` to skip; `--registry <alias>` for short-name resolution. |
| `criteria adapter list` | List cached (`--installed`) or workflow-referenced (`--referenced`) adapters. |
| `criteria adapter info <name>` | Print the cached manifest and verified signer identity. |
| `criteria adapter where <name>` | Print the on-disk binary path for this platform. |
| `criteria adapter remove <name>` | Remove an adapter from the cache (`--prune` to GC blobs). |
| `criteria adapter prune` | Reclaim cache space (`--older-than 30d`, `--max-size <bytes>`). `--unattributed-only` clears entries that lack provenance annotations. |
| `criteria adapter dev <binary>` | Register a local binary as an adapter, skipping lockfile + signature checks — the fast inner-loop path. |

## Adapter session lifecycle

Adapter provisioning is split into three phases so that the whole workflow
tree is reproducible at compile time, fail-fast verification runs before any
step executes, and session binding stays lazy per scope.

### Phase 1: compile-time resolution

`criteria compile` and `criteria apply` build the entire FSM graph before the
run starts, recursively compiling every transitive subworkflow. For every
workflow directory reachable from the root, the compiler reads that directory's
`.criteria.lock.hcl` and merges all adapter pins into a single in-memory pin set
carried on the compiled graph. A subworkflow's own lockfile remains the authority
for its adapters on disk; the merge is performed once in memory so the whole
tree has one resolved view.

The content of every `file()` reference in adapter `config { }` blocks is also
read at compile time and cached in the graph. Runtime `var.*` references in
config are preserved and re-evaluated at scope entry, but the static content of
`file()`-referenced assets is immutable for the duration of the run.

After `apply` begins, deleting, modifying, or replacing any lockfile,
`.chcl`/`.hcl` file, or `file()`-referenced asset in the workflow tree has no
effect on the in-flight run.

### Phase 2: eager verification at apply start

Before the first workflow step executes, the engine verifies **every** adapter
declared anywhere in the compiled graph — including adapters in subworkflows the
run has not yet reached — using the same merged pin set that `validate` and
`apply` setup checked. It is not possible for the startup coverage gate and the
engine to disagree on which adapters are pinned.

Verification resolves the adapter binary or OCI artifact, checks the
signature/digest against the lockfile and trust policy, performs the protocol
`Info` handshake, validates the resolved `config` block against the adapter's
manifest schema, checks that required secrets are present, and validates sandbox
primitive availability and strict-mode policy failures for adapters bound to a
`sandbox` environment (missing landlock/seccomp/cgroup primitives, or a
strict-mode policy that cannot be satisfied on the host). A missing or
unverifiable adapter in a subworkflow fails the run at startup, before any step
executes.

This phase runs in a neutral working directory, so a missing or not-yet-created
`working_directory` does **not** cause a failure.

### Phase 3: lazy session binding at scope entry

When a step first targets an adapter in a scope, the engine opens a session for
that scope:

- the adapter process is launched in its resolved `working_directory`;
- `OpenSession` is called with the resolved config and secrets;
- the per-session permission and log streams are started;
- side-effecting sandbox setup runs: transient cgroup directories are created,
  and the sandboxed process is launched with the resolved `working_directory`
  as its cwd (for example via the bubblewrap `--chdir` option).

If the resolved working directory is missing at this point, the bind fails and
produces an error that names the adapter, the step, and the directory. Because
binding only happens for adapters that are actually reached, an adapter
declared in a branch that is never taken is verified but never bound. Similarly,
subworkflow adapter sessions are opened when the subworkflow is entered and
torn down when it exits; they are not held open for the whole run.

### What is rejected eagerly vs. deferred

Rejected before the run starts:

- any missing or incomplete lockfile entry for an OCI-backed adapter;
- any verification failure in phase 2;
- a `working_directory` path that contains `..`;
- a `working_directory` that falls outside the configured allowed roots (when
  any are configured);
- a `sandbox` adapter whose `policy_mode = "strict"` references a host primitive
  (landlock, seccomp, cgroupv2) that is unavailable on the current host.

Deferred to first use in a scope:

- a `working_directory` that simply does not exist yet. This is the case a
  bootstrap step is allowed to fix by creating the directory before the first
  adapter step that uses it;
- the actual sandbox environment setup, including transient cgroup directory
  creation and the sandboxed process chdir to the resolved `working_directory`;
- runtime `var.*` resolution in adapter `config { }` blocks, so `--var` overrides
  and directories created by earlier steps still bind at scope entry.

## Authoring an adapter

Start from a template rather than wiring the protocol by hand:

- [`criteria-adapter-starter-typescript`](https://github.com/brokenbots/criteria-adapter-starter-typescript)
- [`criteria-adapter-starter-python`](https://github.com/brokenbots/criteria-adapter-starter-python)
- [`criteria-adapter-starter-go`](https://github.com/brokenbots/criteria-adapter-starter-go)

`gh repo create --template …` (or "Use this template") gives a buildable
hello-world adapter with a publish workflow, a commented Dockerfile, and remote
deployment examples. Each SDK exposes the same `serve({...})` shape — a
config/input/output schema plus an `execute` handler — and helpers for session
state, outcome validation, permission correlation, a redaction-aware logger, and
manifest emission (`--emit-manifest`).

| Language | SDK | Single-binary build |
|---|---|---|
| TypeScript | [`@criteria/adapter-sdk`](https://github.com/brokenbots/criteria-typescript-adapter-sdk) | Bun `--compile` |
| Python | [`criteria-python-adapter-sdk`](https://github.com/brokenbots/criteria-python-adapter-sdk) | Nuitka `--onefile` |
| Go | [`criteria-go-adapter-sdk`](https://github.com/brokenbots/criteria-go-adapter-sdk) | `go build` |

### Publishing

Building is the adapter's own job (its toolchain); publishing is uniform. The
[`brokenbots/publish-adapter`](https://github.com/brokenbots/publish-adapter)
action wraps `criteria adapter publish`: emit manifest → validate → construct
the OCI artifact → cosign-sign → push. The starters ship three equivalent paths:

- **GitHub Actions** — push a `v*` tag; `publish.yml` signs **keyless** via the
  job's OIDC identity (`id-token: write`).
- **GitLab CI** — `.gitlab-ci.yml.example`, signing keyless via GitLab
  `id_tokens`.
- **Local / other CI** — `make publish REGISTRY=…`, calling
  `criteria adapter publish out/adapter --registry <ref>` (add `--keyless` in CI,
  `--sign-key <key>` for explicit-key signing, or publish unsigned for local
  experiments).

To also ship a runnable container image (for `environment.runtime = "docker"`),
build and push the image from your own CI, then record it with
`criteria adapter publish … --image <ref>` (or the action's `image:` input). The
publish step does not build images — it records the already-pushed image's
digest in the manifest. See [Environments → container](#container) and
[docs/runtime/docker.md](runtime/docker.md).

### Signing and trust

The model is **"the lockfile is the trust anchor"**: `criteria adapter lock`
verifies the artifact's signature and pins the signer (key fingerprint, or
keyless issuer + subject); `pull`/`compile`/`apply` then re-verify against that
pin on every run. A changed signer surfaces as a `SignerChanged` lockfile diff.

- **Keyless (default in CI, public).** `criteria adapter publish --keyless`
  obtains an ephemeral key, has Fulcio certify it against the workflow's OIDC
  identity, **records the signature in the Rekor transparency log**, and attaches
  the resulting Sigstore bundle (certificate + inclusion proof) as an OCI
  referrer. The Rekor entry is what keeps the signature verifiable after the
  ~10-minute Fulcio certificate expires — the verifier checks the certificate at
  the log timestamp, not at verification time. Token resolution order:
  `--identity-token`, then `SIGSTORE_ID_TOKEN`, then the ambient GitHub Actions
  provider. Override the log with `--rekor-url` (default the public Sigstore
  Rekor). By default any subject from a well-known CI OIDC issuer (e.g. GitHub
  Actions) is accepted at first lock and then pinned, so **an adapter signed by
  its own repo's CI verifies with no per-consumer configuration**.
- **Explicit key (enterprise, offline).** `--sign-key <pem>` signs with an
  Ed25519 key; the lockfile records the key fingerprint. Consumers declare which
  public keys they trust in a **trust config** — a global file under
  `$CRITERIA_HOME/trust.hcl` (default `~/.local/criteria/trust.hcl`;
  `CRITERIA_STATE_DIR` is a deprecated alias) and/or a `trust.hcl` beside the
  workflow (their union is used), or ad-hoc `--trusted-key <pem>` on `pull`/`lock`:

  ```hcl
  # ~/.local/criteria/trust.hcl
  trusted_key {
    key = <<-EOT
    -----BEGIN PUBLIC KEY-----
    ...
    -----END PUBLIC KEY-----
    EOT
  }
  trusted_key { path = "keys/team.pem" }  # path is relative to this file
  ```

  Generate a key pair with, e.g., `openssl genpkey -algorithm ed25519`. Key mode
  verifies fully offline (no Fulcio, Rekor, or TUF).
- **Verification posture.** The workflow-level setting
  `verification = "strict" | "warn" | "off"` controls failure handling. The CLI
  override `--allow-unsigned` (or `CRITERIA_ALLOW_UNSIGNED=1`) skips verification
  for a single invocation; it is available on `pull`, `lock`, `compile`, and
  `apply` for local development and CI. Precedence: `--allow-unsigned` > env >
  workflow `verification` > the built-in default. During the signing-completion
  transition the effective default is `warn` (log, don't fail) so legacy/unsigned
  artifacts don't break `lock`/`apply`; it returns to `strict` once keyless
  verification is confirmed in CI.
- **TUF / air-gapped.** Keyless verification needs the Sigstore TUF root (fetched
  via TUF and cached at `$CRITERIA_HOME/cache/sigstore/`, default
  `~/.local/criteria/cache/sigstore/`; clear that directory to refresh) and a
  Rekor entry created while online at signing time. Fully air-gapped consumers
  use explicit-key mode or `--allow-unsigned`.

## Adapter tools

An adapter may present **tools** — named operations that other adapters call
mid-execution and receive results from inline. A tool call is **not a step**:
the callee never enters the FSM, is never outcome-routed, and its result is
data returned to the caller. The workflow-grammar view (HCL forms,
compile-time validation) lives in
[workflow.md → Adapter tools](workflow.md#adapter-tools) and
[LANGUAGE-SPEC.md → Adapter tools](LANGUAGE-SPEC.md#adapter-tools); the
normative wire contract is
[ADR-0004 — Adapter-as-tool contract](adrs/ADR-0004-adapter-tools.md). This
section is the adapter-author reference: declaring a tool surface, granting
and consuming calls on the caller side, how a call travels the wire, and how
the host secures it.

### Authoring a callable adapter

A callable adapter declares the tools it presents on its adapter declaration:

```hcl
adapter "git" "repo" {
  source  = "ghcr.io/your-org/criteria-adapter-git"
  version = "0.3.0"

  tool "git_status" {}
  tool "git_diff" {}
}
```

- **Static tool blocks.** `tool "<name>" { }` declares, at configuration
  time, a stable named tool. Names are unique within the adapter; the block
  body takes no attributes today and is reserved for future use. Static
  declarations take precedence at compile time over every other tool
  source: once static blocks exist, the caller's `tools` entry naming an
  undeclared tool is rejected with a compile error, even when
  `dynamic_tools = true` is also set. At run time a static+dynamic callee
  is lenient — `staticToolErrorCode` short-circuits on
  `dynamic_tools = true`, so the call is dispatched (gated only by
  `allow_tools`) and only the callee itself can report `unknown_tool` via
  the reserved `call_error` output.
- **Dynamic tools.** `dynamic_tools = true` admits a tool surface discovered
  at run time — the MCP adapter populates its surface from the MCP server's
  `tools/list` at `OpenSession` and reports it in `InfoResponse.tools`. The
  compile-time name check is lenient for dynamic surfaces (the surface is
  not knowable ahead of the run); every call is gated by `allow_tools` at
  call time instead. Static blocks and `dynamic_tools = true` may combine —
  the blocks name the stable surface, and the flag additionally admits
  runtime-discovered tools.
- **Handshake-reported tools.** An adapter that declares neither static
  blocks nor `dynamic_tools = true` may still report a runtime tool surface
  in its `Info` handshake (`InfoResponse.tools`); the compiler checks
  callers' `tools` entries against it when the handshake is available. An
  adapter that presents none of the three has no tool surface, and
  references naming its tools are rejected.
- **Output schemas.** A successful tool result carries the callee's
  `ExecuteResult` outcome plus its typed outputs, JSON-encoded the same way
  step outputs are. Declare the output schema in your manifest (the
  `serve({...})` schema) so the host decodes outputs with the same typing as
  ordinary step outputs, and mark outputs that carry sensitive values
  `sensitive: true`: those are registered with the run's redaction registry
  and masked on every host surface (see [Security model](#security-model)).

The callee implements nothing tool-specific beyond the declaration: the host
runs a tool call as a nested `Execute` against the callee's ordinary
session, with the call arguments rendered as the callee's input keys and
validated against its declared input schema.

### Consuming tools

A caller grants calls with a step-level `tools` list — bare traversals in
the target-naming form `adapter.<type>.<name>.tools[.<tool>]`:

```hcl
workflow {
  name          = "review_pipeline"
  version       = "1"
  initial_state = "review"
  target_state  = "done"
}

state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}

permissions {
  allow_tools = ["adapter.git.*.tools.git_*"]
}

adapter "git" "repo" {
  source  = "ghcr.io/your-org/criteria-adapter-git"
  version = "0.3.0"
  tool "git_status" {}
  tool "git_diff" {}
}

adapter "copilot" "assistant" {
  source = "ghcr.io/brokenbots/criteria-adapter-copilot"
}

step "review" {
  target = adapter.copilot.assistant
  tools  = [adapter.git.repo.tools.git_status,
            adapter.git.repo.tools.git_diff]
  input { prompt = "Review the working tree." }
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}
```

Grant semantics:

- Entries are **literals, not glob patterns**. A bare `…tools` entry grants
  the instance's **entire tool surface** (it covers any named call to that
  instance); `…tools.<tool>` grants **exactly one** tool.
- Every entry **grants the call**: entries are unioned into the step's
  effective allow set, keyed by the target string.
  [LANGUAGE-SPEC](LANGUAGE-SPEC.md#adapter-tools) also specifies the same
  list shape on the `adapter` declaration (configuration level), where it
  applies to every step that targets the instance; step-level lists union
  onto it.
- The compiler **flags** pointless entries — a callee that presents no tool
  surface, a caller that lacks the `adapter_tools` capability, duplicate
  entries — and warns on call-graph cycles. It does not derive runtime
  behavior from the list: runtime gating happens on the actual permission
  surface, per call.

`allow_tools` interplay: the target string doubles as the
permission-surface target. When an adapter attempts a call, the host matches
the effective `allow_tools` patterns — the union of the workflow
`permissions.allow_tools` and the step's `allow_tools` — against the **full
target string** with Go `path/filepath.Match` semantics:

| `allow_tools` pattern | Matches |
|---|---|
| `adapter.git.repo.tools` | the bare surface grant string only (anchored exact match) |
| `adapter.git.repo.tools.git_status` | exactly that call (every dot must be present) |
| `adapter.*.tools` | every instance's bare tool surface |
| `adapter.git.*.tools.git_*` | every `git_*` tool of any `git` instance |

- Dots are literal characters (not wildcards and not separators); there is
  no `**` recursive syntax; `*` matches any run of non-`/` characters, so it
  may span dots but never crosses `/`.
- Matching is anchored full-string matching, and the first matching pattern
  wins; an empty or absent list denies all tool requests.
- A glob that should cover every tool call of an instance is spelled
  `adapter.<type>.<name>.tools.*` — a bare `…tools` *pattern* matches only
  bare-form strings, because matching is anchored and exact.

Capability: the calling adapter must declare the `adapter_tools` capability
string in `InfoResponse.capabilities`. The host gates **per call**, not at
session open: a call attempted by an adapter that never declared the
capability is answered with the typed `capability_missing` failure, and the
compiler warns when a `tools` entry sits on a step whose target adapter
lacks the capability. Against a host that predates adapter tools, a granted
call comes back as a bare allow-grant with no result, which the caller
surfaces as the typed `host_unsupported` failure; a denied call returns
`cancel` as usual.

### The call/return lifecycle

A tool call **is** a gated permission request with a payload (ADR-0004 §8).
The calling adapter emits a `permission.request` AdapterEvent on the Execute
stream, in map form:

| Field | Meaning |
|---|---|
| `kind` | `"adapter_tool"` — distinguishes tool calls from other permission requests |
| `request_id` | correlation id, minted by the calling adapter |
| `target` | the full target string `adapter.<type>.<name>.tools[.<tool>]` |
| `tool` | the tool name only (no adapter prefix) |
| `args` | JSON object of typed call arguments |
| `args_digest` | `sha256(canonical_json(args))`, for audit and correlation |

The host gates the call in a fixed order before anything runs:

1. **Capability** — the caller session must have declared `adapter_tools`
   (typed `capability_missing`).
2. **Target shape** — the strict `adapter.<type>.<name>.tools[.<tool>]` form
   (typed `malformed_target`).
3. **Pause gate** — while a pause is draining in-flight calls, new calls are
   refused typed `paused` before the policy runs (ADR-0004 §11); the caller
   sees the typed failure and may re-issue the call once the run resumes.
4. **Permission policy** — the step's `tools` grants are checked first
   (literals), then the effective `allow_tools` globs on the full target
   string. A deny takes the existing permission-deny path unchanged: the
   host answers `PermissionEvent.cancel`.
5. **Graph validation** — the callee must be declared in the workflow, and a
   named call must exist on a callee that declares a static surface (typed
   `unknown_adapter` / `unknown_tool`).
6. **Self-call rejection** — a call to the calling adapter's own instance is
   rejected typed `self_call`.
7. **Call-chain checks** — the call may not re-enter an adapter already on
   the call chain (typed `cycle_detected`) and may not exceed
   `policy.max_tool_depth`, default 8 (typed `depth_exceeded`); each
   rejection is audited and the run continues.
8. **Argument validation** — call arguments are validated against the
   callee's input schema inside dispatch, after every gate above (typed
   `invalid_args`). Because this runs inside `dispatchNestedToolCall`, a
   self-call, cyclic call, or depth-exceeding call carrying malformed args
   returns the gate code (`self_call` / `cycle_detected` /
   `depth_exceeded`), not `invalid_args`.
9. **Nested execution** — an allowed call runs the callee in its own
   session and replies with the callee's result.

Every gate decision is audited — one audit entry per decision, attributed to
the call's own nesting layer, at
`$CRITERIA_HOME/runs/<run-id>/audit.log` (default
`~/.local/criteria/runs/<run-id>/audit.log`) — so caller-layer and
callee-side decisions are separately distinguishable.

What the callee sees: its **own session**, resolved through the ordinary
lazy-bind path, governed by its **own environment** and its **own
`allow_tools`** — the declaring workflow's `permissions.allow_tools`, never
the caller's step grants. Its outputs are decoded against its own output
schema, typed exactly as step outputs, and its own `on_crash` governs its
session: an `abort_run` crash in the callee's session aborts the run (the
caller still receives the typed `callee_crash` result).

What the caller sees: a typed `PermissionEvent.tool_call_result` on its
Permissions stream, correlated by `request_id` — the callee's outcome plus
JSON-encoded typed outputs on success, or a typed `call_error` code on
failure (`callee_crash`, `callee_timeout`, `canceled`, and the gate codes
above; a callee may also report its own typed rejection — e.g. `unknown_tool`
for a call outside its discovered surface — via the reserved `call_error`
output). The caller's own outcome routing is unaffected: it completes its
step exactly as it would have without the call and routes through its
`outcome` blocks as normal — a failed tool call is data for the caller, not
a run failure. Multiple in-flight calls per caller session interleave;
replies may arrive in any order and are correlated by `request_id`.

### Message directions (for adapter authors)

The two streams have fixed directions. Implement against them exactly as
[ADR-0004 §8](adrs/ADR-0004-adapter-tools.md) specifies:

- **The call travels on the `Execute` stream** (adapter → host) as a
  `permission.request` AdapterEvent with the payload table above. The
  adapter emits it and blocks until the host answers on the Permissions
  stream.
- **The answer travels on the `Permissions` bidi stream** (host → adapter)
  as a `PermissionEvent`: `request` (allow-grant) followed by
  `tool_call_result` for a granted call; `cancel` for a denied call.
- **`PermissionDecision` is the adapter-to-host direction** and is drained
  and discarded by the host — it is an ACK channel, not a result channel.
  Never carry a tool result on it.

```text
caller adapter                host                          callee adapter
     |                         |                                |
     |  1. permission.request  |                                |
     | ---- Execute stream --> |                                |
     |    { request_id, target, tool, args, args_digest }       |
     |                         |                                |
     |                         |  2. allow_tools policy check   |
     |                         |     + audit entry              |
     |                         |                                |
     |                         |  3. nested Execute             |
     |                         | -----------------------------> |
     |                         |                                |
     |                         |  4. ExecuteResult              |
     |                         | <----------------------------- |
     |                         |    (outcome + typed outputs)   |
     |                         |                                |
     |  5. PermissionEvent     |                                |
     |     .tool_call_result   |                                |
     | <--- Permissions stream |                                |
     |    { request_id, result }                                |
```

Degradation: a granted call answered only by a bare `request` with no
subsequent `tool_call_result` means the host predates adapter tools —
surface that as the typed `host_unsupported` failure. Unknown
`PermissionEvent` oneof members are ignored by older adapters, and adapters
that never declare `adapter_tools` never initiate calls, so old adapters are
unaffected.

## Secrets

Adapters declare the secrets they need in their manifest; the host resolves and
delivers them over a **dedicated channel** that is structurally separate from
non-sensitive config, so values cannot leak through naive logging or
serialization.

- **Declared secrets.** The manifest lists `secrets: [{ name, description,
  required }]`. The host resolves each from a configured provider stack (env,
  file, OS keychain, vault, sops) and passes values only via the protocol's
  dedicated secret fields — never via `config` or `input`.
- **Workflow-level tagging.** A `variable` (or `shared_variable`) marked
  `secret = true` is tainted from the moment it enters the workflow: never
  logged, never written to plan output, lockfile, or checkpoint — only its
  origin reference is persisted, and it is re-resolved on resume.
- **Binding into an adapter.** Satisfy declared secrets from a workflow variable
  or a secret data block. The value must already be secret-tainted; a literal
  string or non-secret reference is rejected at compile time:

  ```hcl
  variable "api_key" {
    type   = string
    secret = true
  }

  data "internal" "vault_token" {
    type   = string
    secret = true
    value  = "env:VAULT_TOKEN"   # provider origin for the declared secret
  }

  adapter "anthropic" "default" {
    source  = "ghcr.io/your-org/criteria-adapter-anthropic"
    version = "0.5.0"
    secrets {
      ANTHROPIC_API_KEY = var.api_key
      VAULT_TOKEN       = data.internal.vault_token.value
    }
  }
  ```

  Provider references (`env:NAME`, `file:path`, keychain, vault, sops) are
  allowed only as origins for declared secret variables or data blocks, never
  directly inside `adapter.secrets` or `secret_input`.

- **Taint propagation.** Once a value is secret, every value derived from it is
  too. The compiler refuses to interpolate a tainted value into `config`,
  `input`, a log/template string, or any non-secret destination, with a hint to
  bind it via `secrets { … }` or a step's `secret_input { … }` instead.
- **Log redaction.** Each secret is registered with the redaction registry at
  session open; any value crossing the host log pipeline (workflow/run/audit log,
  terminal) is masked. SDKs ship a redaction-aware logger so adapter-side logs
  flow through the masker too.
- **Shelling out to a child program.** Because secrets are *not* placed in the
  adapter's process environment, an adapter that exec's a child needing a secret
  in *its* env (e.g. an upstream CLI) must pass it explicitly. Each SDK provides
  a `secrets.spawnEnv([...])` helper that returns a child env containing only the
  named, declared secrets and re-registers them for redaction. This is by design
  — it forces a deliberate decision about which secret crosses which boundary.

## Environments

The environment block is the sandbox/policy boundary. It keeps the two-label
form `environment "<type>" "<name>" { … }`: the **type** selects the runtime
isolation path; the **name** distinguishes instances. Bind an environment per
adapter (or per step) by reference:

```hcl
environment "container" "prod" {
  policy_mode = "strict"
  runtime     = "docker"
  network  { allow = ["*"] }
  secrets  { provider = "vault:secret/anthropic" }
  resources { cpu = "2", memory = "1Gi", timeout = "5m" }
}

adapter "anthropic" "default" {
  source      = "ghcr.io/your-org/criteria-adapter-anthropic"
  version     = "0.5.0"
  environment = var.deploy_env == "prod" ? container.prod : sandbox.dev
}
```

### Types

| Type | Isolation |
|---|---|
| `shell` | No added isolation; the adapter runs as a plain subprocess with env injection. The lightest path. |
| `sandbox` | OS-native isolation. **Linux:** user/mount/pid/net/IPC/UTS namespaces + landlock + seccomp (in-process, no cgo, no helper binary); `bubblewrap` is used instead when present and opted in. **macOS:** an auto-generated `sandbox-exec` profile. |
| `container` | `docker run` / `podman run` of the adapter's published runnable image (`environment.runtime = "docker" \| "podman"`). The same cross-platform "stronger than host-native" path. |
| `remote` | The adapter is not launched by the host; it dials in (phone-home). See [Remote execution](#remote-execution). |

The type label is an open enum — `vm`, `firecracker`, etc. can be added without
grammar changes; the registry gates which types a given host OS supports.

### Policy resolution

Each policy field resolves per session:

1. **Set explicitly in the environment block** → the environment is
   authoritative; the adapter's manifest hint for that field is ignored.
2. **Unset** → the adapter's manifest hint provides the default (permissive
   mode).
3. **`policy_mode = "strict"`** → unset fields default to deny-all; adapter hints
   are never trusted as defaults. This is the zero-trust/enterprise opt-in.

Fields: `policy_mode` (`permissive` default / `strict`), `sandbox`
(`strict`/`permissive`/`off`), `filesystem { read, write }`,
`network { allow }` (host:port list or `"*"`), `secrets { provider,
allow }`, `resources { cpu, memory, timeout }`, `os` (compile-time host gate),
and type-specific extras such as `runtime` for `container`. Compatibility between
an adapter and an environment type is checked at compile time only when the
adapter declares a `compatible_environments` constraint.

`network { allow }` semantics:

- Absent or empty `allow` denies outbound networking.
- `allow = ["*"]` explicitly opts in to unrestricted outbound networking.
- `"*"` must be the only entry; it cannot be combined with host:port values,
  and no other glob-like values are accepted.
- Exact host:port lists are enforced by the `sandbox` type:
  - **macOS:** resolved to IP addresses and rendered as `sandbox-exec` remote
    rules; unresolved declared hosts fail closed in strict mode.
  - **Linux:** mapped to allowed TCP ports via landlock (host-level
    restriction is not available).
- Exact host:port lists are rejected for `container` and `remote` environments
  because those backends cannot enforce per-endpoint egress; use `allow = ["*"]`
  to declare unrestricted egress intent, or omit the block to deny egress.

### Per-OS support matrix

| Capability | Linux | macOS |
|---|---|---|
| `shell` | ✅ | ✅ |
| `sandbox` host-native | ✅ namespaces + landlock + seccomp | ✅ `sandbox-exec` (best-effort; Apple-deprecated) |
| `sandbox` soft alternative | ✅ bubblewrap (opt-in) | — (use `container`) |
| `container` | ✅ docker/podman | ✅ docker/podman (Docker Desktop, Colima, Lima, podman-machine) |
| `remote` | ✅ | ✅ |

Windows is not a supported host; run Criteria under WSL2. When a sandbox
primitive is unavailable (e.g. an older kernel without landlock), the host logs
which protections were skipped and continues — unless `sandbox = "strict"`, which
fails closed.

## Remote execution

Remote adapters use a **reverse phone-home** model: the adapter dials into the
host, not the other way around. Criteria contains no k8s/ECS/SSH client code —
you start the adapter however you run any long-running service, and it connects
back.

- The `remote` environment configures only the host's inbound listener and auth
  (`listen_address`, `mtls { … }`, optional `accept_token`, and
  `accept_digest_from = lockfile` so a connecting adapter's reported digest must
  match the pinned one).
- Two ways to package the remote side:
  - **Peer mode (recommended)** — the container runs the `criteria` engine's
    `peer` subcommand as the entrypoint. It launches and supervises the adapter
    child directly, serves the full v2 contract on the phone-home connection,
    and streams typed supervision facts (spawn/exit/crash/heartbeat) back to
    the host over PeerService, so lifecycle RPCs and crash classification work
    remotely exactly as they do for local adapters.
  - **Runner mode (legacy)** — the adapter calls the SDK's `serveRemote(...)`
    (one function-name change from `serve(...)`): dial out over mTLS gRPC,
    complete the auth + identity handshake, then serve
    `Info`/`OpenSession`/`Execute`/… on the held connection. Available in all
    three SDKs; functional but deprioritized on the peer timeline.
- In runner mode a small host-side shim bridges the inbound mTLS connection to
  a local UDS so the session layer treats it like any local adapter; peer mode
  needs no bridge. No other host code is remote-aware.
- Launch and reachability are yours to arrange. Copy-pasteable k8s `Deployment`
  and `docker-compose` examples live under [`docs/examples/`](examples/); see
  [docs/adapter-remote-deployment.md](adapter-remote-deployment.md) for the full
  deployment guide, including the recommended peer-mode image and manifests.

Host-side sandbox primitives do not apply to `remote` environments (the host did
not launch the process); `network`/`filesystem`/`resources` are advisory there,
and the compiler warns (errors under `policy_mode = "strict"`).

## Lifecycle

Beyond `OpenSession`/`Execute`/`CloseSession`, protocol v2 defines lifecycle
operations the host drives on a session:

- **Pause / Resume** — suspend and resume a long-running session; the host also
  pauses the permission-handling goroutine and resumes it from persisted state.
- **Snapshot / Restore** — `Snapshot` returns opaque adapter state (plus the
  host's permission state and recent-decision window); `Restore` rehydrates it,
  re-resolving any tainted secrets from their origins first. This is the durable
  story for long-running agents across host restarts and remote handoffs.
- **Inspect** — a read-only structured view of session state (current step,
  pending permissions, last activity) for operators and UIs.

Adapters opt into these via the SDK; the shared conformance suite exercises
pause/resume, snapshot/restore, and inspect against every adapter so behavior is
uniform.

### Log stream lifetime

Protocol v2 also opens a per-session **Log stream** as soon as the session is
opened. This stream is the host's only source of adapter-level liveness
heartbeats, and it must remain open for the entire lifetime of the session:

- **The log stream must remain open for the lifetime of the session.** An
  adapter that returns from its `Log` RPC before the session is closed stops
  sending heartbeats. The host detects this as a broken contract and disarms
  stall detection for that session so it is not falsely declared crashed, but
  the adapter will fail the mandatory heartbeat conformance suite.
- **Current Go SDK transitional requirement.** In the Go SDK today the
  heartbeat ticker is scoped to the adapter's `Log` call, so a Go adapter's
  `Log` implementation must block until its context is cancelled. The intent is
  for the SDK to own stream lifetime independently in a future go-sdk update;
  until then, adapter authors must keep `Log` alive and let the SDK emit
  `Heartbeat` events at the configured interval.
- **Conformance enforces the contract.** Any adapter that declares a log stream
  must pass the `heartbeats` conformance suite, which verifies the session
  survives an idle period longer than the stall threshold and that actual
  heartbeat events were observed.

## Security model

- **Process scrub.** The sandbox setup scrubs the adapter's process environment;
  secret-looking inherited variables are removed unless explicitly listed in
  `environment.variables`. Secrets never arrive as env vars — only via the
  dedicated channel.
- **Sandbox primitives.** Per-OS isolation as in the matrix above: Linux
  namespaces + landlock + seccomp (pure-Go, no cgo); macOS `sandbox-exec`;
  container mode as the cross-platform escape hatch. Capability degradation is
  logged and fails closed only under `sandbox = "strict"`.
- **Redaction registry.** Every tainted value — adapter-declared secrets,
  secret-tagged variables, and `sensitive: true` outputs — is registered and
  masked across all host log surfaces before display or persistence. Secrets are
  never written to the lockfile, compiled FSM, or checkpoints; only origin
  references are persisted.
- **Permission stream.** Tool-permission requests flow over a bidirectional
  stream handled inside the session, evaluated against the `allow_tools` policy
  and then against the resolved environment filesystem and network policy.
  Each layer produces a distinguishable denial reason and one audit entry per
  decision at `$CRITERIA_HOME/runs/<run-id>/audit.log` (default
  `~/.local/criteria/runs/<run-id>/audit.log`).
- **Adapter tool calls.** An adapter-to-adapter tool call is a gated
  permission request plus a nested session (see
  [Adapter tools](#adapter-tools)); the call is secured per layer:
  - **Deny-by-default per layer.** The call itself is gated by the caller's
    permission policy — an empty or absent `allow_tools` list denies all
    tool requests, with the step's `tools` entries granting covered calls —
    and the callee session is separately governed by its **own** `allow_tools`
    (the declaring workflow's `permissions.allow_tools`) and its own
    environment filesystem/network policy. Neither layer inherits the
    other's grants, and every decision is audited with distinguishable
    layer attribution.
  - **Redaction of nested outputs.** The callee's outputs are decoded by the
    same path as ordinary step outputs, so `sensitive: true` fields are
    registered with the run's redaction registry, and registered values are
    masked across all host log surfaces before display or persistence —
    including the nested call's own event and audit traffic (a call's
    target/tool echo in `tool.call` / `tool.call_result` events, permission
    decision events, and audit entries is masked). `args_digest` is a
    one-way digest of the call arguments, so calls are auditable without
    carrying plaintext.
  - **Depth cap.** `policy.max_tool_depth` (default 8) bounds the tool-call
    stack, and runtime cycle detection rejects a call that would re-enter an
    adapter already on the call chain; both are typed failures returned to
    the calling adapter as the tool result. The run continues — a failed
    call is data for the caller, not a run failure — and each enforcement is
    audited.
  - **Self-call prohibition.** A call whose callee is the calling adapter's
    own instance is rejected with the typed `self_call` failure;
    same-session reentry is out of scope for v1.

## Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| `workflow uses OCI adapter references but .criteria.lock.hcl is missing` | Run `criteria adapter lock`, then commit the lockfile. |
| `validate`/`apply` reports an unpinned adapter in a subworkflow | A subworkflow lockfile is missing or incomplete. Run `criteria adapter lock <subworkflow-dir>` to regenerate that directory's pins. |
| Fetched workflow has no/incomplete lockfile | Materialise the fetched workflow (`criteria adapter lock` against it) so its adapters are pinned, or request an updated lockfile from the publisher. |
| Pull fails: *adapter does not support `<goos>/<goarch>`* | The publisher didn't build your platform. Ask them to add it, or use a different adapter (no cross-arch emulation). |
| Pull fails: *does not publish a container image; cannot run under runtime = "…"* | The adapter is artifact-only. Set `environment.runtime = "none"`, or ask the publisher to publish an image. |
| Signature verification failed at pull | The artifact is unsigned or the signer is outside the trust policy. Fix the publisher's signing, adjust the trust policy, or (dev only) `--allow-unsigned` / `verification = "warn"`. |
| Compile error: *value `var.x` is marked secret* | A tainted value was used in `config`/`input`/a string. Bind it via `adapter.X.secrets { … }` or `step.X.secret_input { … }`. |
| Adapter's child process can't see a secret | Secrets aren't in the process env by design. Pass them explicitly via the SDK's `secrets.spawnEnv([...])` helper. |
| Sandbox protections "skipped" in logs | A primitive is unavailable on this host/kernel. Acceptable under `permissive`; set `sandbox = "strict"` to fail closed instead. |

For upgrading an existing project from v0.3, see
[adapter-v2-migration.md](adapter-v2-migration.md).
