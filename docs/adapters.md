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
- **Binding into an adapter.** Satisfy declared secrets from a workflow variable,
  a sensitive step output, or a provider reference:

  ```hcl
  adapter "anthropic" "default" {
    source  = "ghcr.io/your-org/criteria-adapter-anthropic"
    version = "0.5.0"
    secrets {
      ANTHROPIC_API_KEY = var.api_key                    # secret-tagged variable
      VAULT_TOKEN       = step.vault_fetch.outputs.token  # sensitive output
      OTHER             = "env:OTHER_SECRET"              # provider reference
    }
  }
  ```

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
  network  { allow = ["api.anthropic.com:443"] }
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
`network { allow }` (host:port list, `"any"`, or `"none"`), `secrets { provider,
allow }`, `resources { cpu, memory, timeout }`, `os` (compile-time host gate),
and type-specific extras such as `runtime` for `container`. Compatibility between
an adapter and an environment type is checked at compile time only when the
adapter declares a `compatible_environments` constraint.

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
- The adapter calls the SDK's `serveRemote(...)` (one function-name change from
  `serve(...)`): dial out over mTLS gRPC, complete the auth + identity handshake,
  then serve `Info`/`OpenSession`/`Execute`/… on the held connection. Available
  in all three SDKs.
- A small host-side shim bridges the inbound mTLS connection to a local UDS so
  the session layer treats it like any local adapter; no other host code is
  remote-aware.
- Launch and reachability are yours to arrange. Copy-pasteable k8s `Deployment`
  and `docker-compose` examples live under [`docs/examples/`](examples/); see
  [docs/adapter-remote-deployment.md](adapter-remote-deployment.md) for the full
  deployment guide.

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
  (extended by environment policy fields), with one audit entry per decision at
`$CRITERIA_HOME/runs/<run-id>/audit.log` (default
`~/.local/criteria/runs/<run-id>/audit.log`).

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
