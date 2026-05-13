# WS09 — Environment block extension + secret-taint compiler

**Phase:** Adapter v2 · **Track:** Security · **Owner:** Workstream executor · **Depends on:** [WS02](WS02-protocol-v2-proto.md), [WS05](WS05-adapter-manifest.md), [WS07](WS07-lockfile.md). · **Unblocks:** [WS10](WS10-linux-sandbox.md), [WS11](WS11-macos-sandbox.md), [WS12](WS12-container-runtime.md), [WS13](WS13-secrets-channel-redaction.md), [WS20](WS20-remote-environment-and-shim.md).

## Context

`README.md` D35–D40 + D61–D67. This workstream is two interlocking pieces:

1. **Environment block extension.** Keep the existing two-label HCL form `environment "<type>" "<name>"`. Extend the type registry beyond `shell` to add `sandbox`, `container`, and `remote` (handler skeletons here — actual isolation behavior in WS10/11/12; remote shim in WS20). Each handler advertises `supported_oses`. Add policy fields per D37 with the three-rule field-resolution semantics. Implement adapter↔environment compatibility validation (D40-compat) using the manifest's `compatible_environments` (default = any per D36).

2. **Secret taint compiler.** Add `secret = true` to `variable` and `shared_variable` blocks; add `secret_inputs` step block parallel to `input`; implement taint propagation in the compiler so any secret-tagged value can only flow through secret channels.

These two land together because the environment block carries `secrets { provider = ... }` config that the taint compiler needs to honor, and because both touch the same compile-time HCL pipeline.

## Prerequisites

- WS02 (proto v2 with `secrets` and `secret_inputs` fields).
- WS05 (manifest types — the compiler reads `manifest.Manifest` to enforce compatibility).
- WS07 (lockfile + adapter resolution at compile time — provides the manifest).

## In scope

### Step 1 — Type registry

`internal/adapter/environment/registry.go`:

```go
type Handler interface {
    Type() string                                 // "shell" | "sandbox" | "container" | "remote"
    SupportedOSes() []string                      // ["linux"], ["linux","darwin"], etc.
    ValidateFields(body hcl.Body) hcl.Diagnostics
    Prepare(ctx PrepareContext) (Prepared, error) // called at session-open; returns whatever the loader needs
    IsolationKind() IsolationKind                 // for D40-compat reporting
}

var DefaultRegistry = NewRegistry(
    &shell.Handler{},
    &sandbox.Handler{},      // skeleton; WS10 + WS11 fill in
    &container.Handler{},    // skeleton; WS12 fills in
    &remote.Handler{},       // skeleton; WS20 fills in
)
```

This WS lands the registry and the `shell` handler (which mirrors v0.3's existing behavior plus new policy fields). It lands skeletons for the other three so the compiler can reference them.

### Step 2 — Environment HCL schema

In `workflow/schema.go`:

```go
type EnvironmentSpec struct {
    Type   string   `hcl:",label"`
    Name   string   `hcl:",label"`
    Body   hcl.Body `hcl:",remain"`
}

// After the type handler validates and partially decodes Body, the
// concrete EnvironmentNode carries the typed policy fields:
type EnvironmentNode struct {
    Type      string
    Name      string
    PolicyMode    string                 // "permissive" (default) | "strict"
    OS            string                 // "" (any) | "linux" | "darwin"
    Variables     map[string]string      // existing v0.3 behavior
    Filesystem    *FilesystemPolicy
    Network       *NetworkPolicy
    Secrets       *SecretsPolicy
    Resources     *ResourcesPolicy
    TypeSpecific  map[string]cty.Value   // e.g., runtime="docker" for container; mtls{} for remote
}
```

`shell`-type accepts only `variables` + `policy_mode` + `os`. `sandbox` adds `filesystem`/`network`/`resources` (no `runtime`). `container` adds `runtime` + `image` overrides. `remote` adds `listen_address`/`mtls`/`accept_token`. Each type's handler validates its accepted set and rejects unknown fields with helpful diagnostics.

### Step 3 — Field resolution (the three rules)

`workflow/compile_environments.go` — rewrite the existing function to apply D37's rules:

1. If a field is set in the environment block → use the environment's value.
2. If unset and `policy_mode = "permissive"` → use the adapter's manifest hint (D36).
3. If unset and `policy_mode = "strict"` → deny / empty / default-deny.

Return a `ResolvedPolicy` per (adapter, environment) pair, cached on the FSM graph.

### Step 4 — Compatibility check (D40-compat)

For every `adapter.X.Y.environment = <type>.<name>` reference:

```go
// in workflow/compile_steps_adapter_ref.go (or equivalent)
if mft.CompatibleEnvironments != nil && !contains(mft.CompatibleEnvironments, env.Type) && !contains(mft.CompatibleEnvironments, "*") {
    return diag(...,
        "adapter %q declares compatible_environments: %v; cannot bind to %s.%s (type %s)",
        adapterRef, mft.CompatibleEnvironments, env.Type, env.Name, env.Type)
}
```

Default = any (manifest field absent) → no check runs.

### Step 5 — OS gate (D40-osfield)

If `environment.os` is set and does not match the host's GOOS, fail at compile with a clear message and the list of supported OSes.

### Step 6 — Secret taint extensions

Add to HCL grammar:

- `variable` block: `secret = true` boolean.
- `shared_variable` block (whichever current name applies): `secret = true`.
- `step` block: a new `secret_inputs { … }` block parallel to `input { … }`.
- `adapter` block: a new `secrets { NAME = <expr> }` block.

### Step 7 — Taint propagation pass

`workflow/compile_taint.go` (new):

```go
// TaintPass walks every cty value-producing node in the FSM. Nodes are
// marked tainted if:
//
//   - they reference a variable/shared_variable with secret = true
//   - they reference a step output declared with sensitive: true in the
//     adapter's output_schema
//   - they reference an adapter's secrets { ... } block entry
//
// The pass propagates taint transitively. Any tainted value used outside
// a "secret channel" destination (config map, log/template string,
// non-secret_inputs binding, lockfile field) is a hard compile error.
func TaintPass(graph *FSMGraph) hcl.Diagnostics
```

### Step 8 — Compile-error messages

When the taint pass detects a bad flow, the diagnostic must:

- Point at the source line of the offending expression.
- Name the tainted origin (e.g., `var.api_key`, `step.vault_fetch.outputs.token`).
- Suggest the fix: *"bind it via `adapter.X.secrets { ... }` or `step.X.secret_inputs { ... }` instead."*

### Step 9 — Persistence of origin references only (D67)

`internal/state/` (or wherever run state is persisted) — when serializing FSM state, secret-tagged values are recorded as `OriginRef{kind, ref}` not raw values. On resume, the secrets package (WS13) re-resolves.

This WS lands the `OriginRef` type + the marshal/unmarshal hooks; WS13 wires the re-resolution provider.

### Step 10 — Tests

- HCL parsing: every new field on every block type.
- Type registry: each registered handler validates its fields; unknown fields produce diagnostics with file:line.
- Field resolution: table-driven over (adapter hint, environment value, policy_mode) combinations; verify the three-rule outcome.
- Compatibility check: positive + negative cases per env type.
- OS gate: positive + negative.
- Taint pass: every flow rule has positive + negative tests.
- Existing `compile_environments_test.go` updated for new behavior.

## Out of scope

- Linux sandbox primitives — WS10.
- macOS sandbox-exec profile rendering — WS11.
- Container-mode launch — WS12.
- Remote shim — WS20.
- Secrets provider stack + redaction registry — WS13.

## Reuse pointers

- `hashicorp/hcl/v2` and `gohcl` for decode.
- Existing v0.3 environment-parsing in `workflow/compile_environments.go` — heavily rewritten but the variable-injection logic is kept verbatim for the `shell` type.
- Manifest types from WS05 (`manifest.Manifest`).
- Lockfile types from WS07.

## Behavior change

**Yes — language-level additions.**

- HCL: `secret = true` on variables; `secret_inputs` step block; `secrets {…}` adapter block; expanded environment fields.
- Compile errors when tainted values cross into non-secret channels.
- Environment type registry rejects unknown types and incompatible fields.
- Default `policy_mode` is `permissive`; strict mode opt-in.

## Tests required

- `workflow/compile_environments_test.go`, `workflow/compile_taint_test.go`, `workflow/compile_steps_adapter_ref_test.go` etc.
- Fixture workflows under `workflow/testdata/v2/` exercising every new HCL surface.
- `make ci` green.

## Exit criteria

- HCL parses every new block; rejects malformed ones with file:line diagnostics.
- Taint compiler enforces the rules in `README.md` D61–D67.
- Existing fixtures still compile (with minimal edits to add lockfile + verification = "off" where needed).

## Files this workstream may modify

- `workflow/schema.go`, `workflow/compile_*.go`, `workflow/compile_taint.go` *(new)*.
- `internal/adapter/environment/registry.go` *(new)* + per-type skeleton files.
- `internal/state/origin_ref.go` *(new)*.

## Files this workstream may NOT edit

- `internal/adapter/environment/sandbox/*` beyond skeleton — WS10/WS11.
- `internal/adapter/environment/container/*` beyond skeleton — WS12.
- `internal/adapter/environment/remote/*` beyond skeleton — WS20.
- `internal/adapter/secrets/` — WS13.
- `proto/criteria/v2/` — WS02.
