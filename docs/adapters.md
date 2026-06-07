# Adapters

This is the reference for running adapter-backed workflows with Criteria and for
authoring your own adapters. For the workflow language itself (variables, step
outputs, branching, iteration, wait nodes, approval gates) see
[workflow.md](workflow.md).

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
  identity, and makes the workflow reproduce identically anywhere.
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

<!-- validator: skip: illustrative excerpt only -->
```hcl
workflow "agent_hello" {
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
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}

state "done"   { terminal = true }
state "failed" { terminal = true; success = false }
```

- The first label is the adapter **type**, the second an instance **name**; steps
  bind via `target = adapter.<type>.<name>` (a traversal, not a string).
- The engine opens the session before the step runs and closes it afterward — no
  explicit open/close steps.

### 2. Pin adapters in the lockfile

```bash
criteria adapter lock        # resolve every referenced adapter, pin digests
```

This writes `.criteria.lock.hcl` next to the workflow. Commit it. From then on
the workflow resolves to the exact pinned digests; `criteria adapter lock
--upgrade` re-resolves to the latest matching versions.

If a workflow references OCI adapters but no lockfile exists, compilation tells
you to run `criteria adapter lock`. Missing adapters are pulled into the local
cache (`~/.criteria/cache/oci`) automatically during compile.

### 3. Manage the local cache directly (optional)

| Command | Purpose |
|---|---|
| `criteria adapter pull <ref>` | Fetch an artifact into the cache (verifies signature). `--allow-unsigned` to skip; `--registry <alias>` for short-name resolution. |
| `criteria adapter list` | List cached (`--installed`) or workflow-referenced (`--referenced`) adapters. |
| `criteria adapter info <name>` | Print the cached manifest and verified signer identity. |
| `criteria adapter where <name>` | Print the on-disk binary path for this platform. |
| `criteria adapter remove <name>` | Remove an adapter from the cache (`--prune` to GC blobs). |
| `criteria adapter prune` | Reclaim cache space (`--older-than 30d`, `--max-size <bytes>`). |
| `criteria adapter dev <binary>` | Register a local binary as an adapter, skipping lockfile + signature checks — the fast inner-loop path. |

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
  public keys they trust in a **trust config** — a global `~/.criteria/trust.hcl`
  and/or a `trust.hcl` beside the workflow (their union is used), or ad-hoc
  `--trusted-key <pem>` on `pull`/`lock`:

  ```hcl
  # ~/.criteria/trust.hcl
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
  via TUF and cached at `~/.criteria/cache/sigstore/`; clear that directory to
  refresh) and a Rekor entry created while online at signing time. Fully
  air-gapped consumers use explicit-key mode or `--allow-unsigned`.

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

  <!-- validator: skip: illustrative excerpt only -->
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

<!-- validator: skip: illustrative excerpt only -->
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
- Launch and reachability are yours to arrange. The starter repos ship
  copy-pasteable k8s `Deployment`, `docker-compose`, and `systemd` examples under
  `examples/remote/`. See [docs/adapter-remote-deployment.md](adapter-remote-deployment.md)
  for the full deployment guide.

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
  `~/.criteria/runs/<run-id>/audit.log`.

## Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| `workflow uses OCI adapter references but .criteria.lock.hcl is missing` | Run `criteria adapter lock`, then commit the lockfile. |
| Pull fails: *adapter does not support `<goos>/<goarch>`* | The publisher didn't build your platform. Ask them to add it, or use a different adapter (no cross-arch emulation). |
| Pull fails: *does not publish a container image; cannot run under runtime = "…"* | The adapter is artifact-only. Set `environment.runtime = "none"`, or ask the publisher to publish an image. |
| Signature verification failed at pull | The artifact is unsigned or the signer is outside the trust policy. Fix the publisher's signing, adjust the trust policy, or (dev only) `--allow-unsigned` / `verification = "warn"`. |
| Compile error: *value `var.x` is marked secret* | A tainted value was used in `config`/`input`/a string. Bind it via `adapter.X.secrets { … }` or `step.X.secret_input { … }`. |
| Adapter's child process can't see a secret | Secrets aren't in the process env by design. Pass them explicitly via the SDK's `secrets.spawnEnv([...])` helper. |
| Sandbox protections "skipped" in logs | A primitive is unavailable on this host/kernel. Acceptable under `permissive`; set `sandbox = "strict"` to fail closed instead. |

For upgrading an existing project from v0.3, see
[adapter-v2-migration.md](adapter-v2-migration.md).
