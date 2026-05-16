# WS31 — Migrate `shell` builtin to protocol v2 (still in-tree)

**Phase:** Adapter v2 · **Track:** Adapter migration · **Owner:** Workstream executor · **Depends on:** [WS03](WS03-host-v2-wire.md), [WS09](WS09-environment-block-and-secret-taint.md), [WS13](WS13-secrets-channel-redaction.md), [WS25](WS25-go-sdk-v1.md). · **Unblocks:** [WS37](WS37-v1-protocol-code-removal.md) (one of seven gates) and [WS42](WS42-extract-shell-adapter.md).

## Context

The `shell` adapter is the only in-tree builtin. It lives at `internal/builtin/shell/`. This WS migrates it to protocol v2 against the Go SDK (consumed as a local Go module since it's still in-tree). Stays in-tree for this WS — extraction to its own repo is WS42.

**Replace any `os.Getenv(...)` reads against the host environment with `sdk.secrets.Get(...)` (D69). The shell adapter is special — it deliberately injects controlled env vars into the child shell (the existing `environment.variables` machinery). Those continue to be non-secret env-var injection (D72); the migration only affects secrets, not the regular variables flow.**

## Prerequisites

WS03 (host wire on v2), WS09 (env block extension), WS13 (secrets channel), WS25 (Go SDK RC).

**Sequencing note (added in post-review hardening).** The README workstream list now flags that WS31 should land **with** WS03, not after WS04–WS29. The full-scope dependency on WS09/WS13/WS25 makes a single-PR co-landing impractical. Resolution: split WS31 into two PRs against this same file:

- **WS31a (lands with WS03):** minimum-viable shell migration using a vendored slice of the Go SDK that only implements `serve(...)` (no `secrets.Get`, no env-block hints yet). Existing `environment.variables` and `allow_tools` paths port over as-is. The point is to keep the in-tree shell adapter green through WS04–WS29 so CI exercises a real adapter.
- **WS31b (lands when WS09/WS13/WS25 are ready):** the full migration described in the rest of this file — `secrets.Get`, env-block hints, Go SDK as a real dependency.

Without WS31a, the only v2-speaking adapter during WS04–WS29 is the `noop` conformance fixture, which doesn't exercise any of the real adapter mechanics.

## In scope

### Step 1 — Refactor `internal/builtin/shell/shell.go` against the Go SDK

Today shell embeds its handler directly inside criteria's loader. Refactor to use the SDK pattern:

```go
package shell

import "github.com/brokenbots/criteria-go-adapter-sdk/adapter"

func Serve() error {
    return adapter.Serve(adapter.Config{
        Name:        "shell",
        Version:     "2.0.0",
        Description: "Run shell commands with hardening.",
        SourceURL:   "https://github.com/brokenbots/criteria/internal/builtin/shell",
        ...
        OnExecute: execute,
    })
}
```

But the shell binary is **also** the criteria host binary in v0.3 — same binary, conditionally enters shell mode via a flag. Keep that pattern: `criteria-adapter-shell` (or actually `criteria` invoked with `--builtin-shell` arg) dispatches into the SDK's serve loop.

### Step 2 — Keep `environment.variables` injection

The shell adapter's defining feature is that it takes the `environment.variables` map and injects them as env vars into the child shell process. Per D72 this is the non-secret variables channel. Keep that behavior verbatim — it's separate from `secrets`.

### Step 3 — Apply hardening from the sandbox handler

When the shell adapter is bound to a `sandbox`-type environment, WS10/WS11's sandbox handler already applies isolation. Shell-specific hardening (PATH sanitization, controlled-set warnings for variable names) stays inside the shell adapter.

### Step 4 — Conformance

Pass the WS26 conformance suite against the in-tree shell builtin.

### Step 5 — Tests

Existing `internal/builtin/shell/*_test.go` tests migrate to the v2 SDK API.

## Out of scope

- Extracting to a separate repo — WS42.
- Per-OS sandboxing primitives — WS10/WS11.

## Behavior change

**Yes** — internal: the shell adapter now uses the Go SDK rather than a bespoke gRPC server inside criteria. User-facing behavior is unchanged.

## Tests required

- All existing shell tests pass after migration.
- Conformance suite passes.

## Exit criteria

- `internal/builtin/shell/` consumes the Go SDK and serves protocol v2.
- `make ci` green.

## Files this workstream may modify

- `internal/builtin/shell/` *(refactored)*.
- `internal/cli/` if the builtin-dispatch flag wiring changes.

## Files this workstream may NOT edit

- The Go SDK repo (it's consumed read-only here).
- Other workstream files.
