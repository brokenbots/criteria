# WS13 — Secret channel + provider stack + redaction registry

**Phase:** Adapter v2 · **Track:** Security · **Owner:** Workstream executor · **Depends on:** [WS02](WS02-protocol-v2-proto.md), [WS09](WS09-environment-block-and-secret-taint.md). · **Unblocks:** every adapter migration WS that uses secrets.

## Context

`README.md` D19–D21 and the explicit definition of "separate channel" in D19: same wire, distinct proto fields (with `(criteria.sensitive) = true`), distinct SDK API, distinct host pipeline. This WS implements:

1. The provider stack that resolves secret values from env / file / OS keychain / vault / sops.
2. Wire-up of `OpenSession.secrets` and `ExecuteRequest.secret_inputs` population at session/step time.
3. The host-side redaction registry that masks values everywhere they would be logged.
4. The taint-origin re-resolution for resume (D67).

WS09 already added the `OriginRef` type and the workflow-level taint compiler. This WS provides the runtime providers and the redaction pipeline.

## Prerequisites

WS02 (proto with sensitive fields), WS09 (environment block with `secrets { provider = ... }` parsing + `OriginRef` type).

## In scope

### Step 1 — Provider interface

`internal/adapter/secrets/provider.go`:

```go
type Provider interface {
    Name() string                                            // "env", "file", "keychain", "vault", "sops"
    Resolve(ctx context.Context, ref OriginRef) (string, error)
    // CanResolve returns true if this provider can handle the given reference kind/URI.
    CanResolve(ref OriginRef) bool
}
```

`OriginRef` (from WS09):

```go
type OriginRef struct {
    Kind string  // "env" | "file" | "keychain" | "vault" | "sops" | "var" | "shared_var" | "step_output"
    Ref  string  // e.g., "ANTHROPIC_API_KEY", "/run/secrets/key", "vault:secret/app/key#api_key"
}
```

### Step 2 — Concrete providers

- `internal/adapter/secrets/provider_env.go` — reads `os.Getenv(ref.Ref)`. Strips trailing newlines.
- `internal/adapter/secrets/provider_file.go` — `os.ReadFile(ref.Ref)`. Path-confines to a configurable root (defaults to user home; configurable via environment block).
- `internal/adapter/secrets/provider_keychain.go` — uses `github.com/keybase/go-keychain` on darwin and `secret-tool` shell-out on Linux. Falls back to file/env when keychain unavailable.
- `internal/adapter/secrets/provider_vault.go` — Vault KV v2 client using `github.com/hashicorp/vault/api`. Auth via configured method (token, AppRole, JWT).
- `internal/adapter/secrets/provider_sops.go` — invokes `sops --decrypt` on a sops-encrypted file. `getsops/sops` Go SDK preferred over shell-out.

Each provider has tests with a fake backend.

### Step 3 — Stack assembly

`internal/adapter/secrets/stack.go`:

```go
type Stack struct { providers []Provider }

func StackFromEnvironment(env *workflow.EnvironmentNode) (*Stack, error)

// Resolve walks the stack in order. First provider that CanResolve wins.
func (s *Stack) Resolve(ctx context.Context, ref OriginRef) (string, error)
```

The environment block's `secrets { provider = "vault:..." }` selects the active provider; other providers are available for fallback via a `secrets { fallback = ["env"] }` list.

### Step 4 — Session-open population

In `internal/adapter/sessions.go` (modify the WS03-introduced OpenSession path):

```go
// Build OpenSessionRequest.secrets:
for _, decl := range manifest.Secrets {
    ref := bindingFor(decl.Name, adapter, env)  // OriginRef from the workflow's adapter.secrets {} binding
    val, err := stack.Resolve(ctx, ref)
    if err != nil && decl.Required {
        return fmt.Errorf("required secret %q not resolvable: %w", decl.Name, err)
    }
    req.Secrets[decl.Name] = val
    redaction.Register(val)  // see Step 5
}
```

### Step 5 — Redaction registry

`internal/adapter/secrets/redaction.go`:

```go
type Registry struct {
    mu    sync.RWMutex
    values map[string]struct{}  // raw values; lookup by string match
}

func (r *Registry) Register(value string)
func (r *Registry) Redact(in string) string  // replace every registered value with "[REDACTED]"
func (r *Registry) Wrap(w io.Writer) io.Writer  // streaming wrapper
```

Wired into:

- Host log pipeline (`internal/log/`).
- Run audit log writer.
- Terminal renderer.
- Plan output writer.

Any byte stream emitted by the host or relayed from the adapter passes through `Registry.Wrap(...)` before display/persistence.

### Step 6 — Step-level secret inputs

Same pattern at `ExecuteRequest` construction:

```go
for _, binding := range step.SecretInputs {
    ref := binding.Origin  // OriginRef
    val, err := stack.Resolve(ctx, ref)
    if err != nil { ... }
    req.SecretInputs[binding.Name] = val
    redaction.Register(val)
}
```

### Step 7 — Persistence and resume

When the host persists a session checkpoint (`Snapshot()` in WS18), the secrets section stores `map<string, OriginRef>` not values. On `Restore()`, the host re-runs the resolve loop and re-registers values with redaction before the adapter's session resumes.

This WS lands the read/write hooks. The actual `Snapshot/Restore` RPC handling is WS18.

### Step 8 — Tests

- `provider_env_test.go`, `provider_file_test.go`, etc. — each provider with fake backends.
- `stack_test.go` — ordering, fallback, error paths.
- `redaction_test.go` — register/redact, streaming wrapper byte-correctness over chunk boundaries.
- Session integration test: workflow with a secret-tagged variable; adapter declares the secret; assert (a) the secret reaches the adapter via the secret channel, (b) any host log line containing the value is redacted, (c) checkpoint file contains only the origin ref.

## Out of scope

- The taint compiler — WS09.
- The SDK's `secrets.get(...)` and `secrets.spawnEnv(...)` adapter-side helpers — WS23–WS25.
- Snapshot/Restore RPC — WS18.

## Reuse pointers

- WS09's `OriginRef` and resolved binding tables.
- WS02's `OpenSessionRequest.secrets` / `ExecuteRequest.secret_inputs` proto fields.
- Existing log pipeline in `internal/log/`.

## Behavior change

**Yes** — secrets now flow over a dedicated channel and are auto-redacted in logs. Adapters that previously read `process.env.X` directly (v1 pattern) will see `undefined` — this is intentional, and the corresponding migration WS for each adapter (WS30–WS36) rewrites them.

## Tests required

- All `internal/adapter/secrets/*_test.go` pass.
- Integration test demonstrates end-to-end masking.

## Exit criteria

- Provider stack composes correctly per environment block.
- Redaction registry masks all log surfaces.
- Persistence stores origin refs only.

## Files this workstream may modify

- `internal/adapter/secrets/*.go` *(new)*.
- `internal/adapter/sessions.go` — populate secrets at OpenSession/Execute.
- `internal/log/` — install redaction wrapper.

## Files this workstream may NOT edit

- `workflow/compile_taint.go` — WS09's territory.
- SDK repos — WS23–WS25.
- Other workstream files.
