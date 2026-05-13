# WS11 — macOS sandbox: auto-generated `sandbox-exec` profile

**Phase:** Adapter v2 · **Track:** Security · **Owner:** Workstream executor · **Depends on:** [WS09](WS09-environment-block-and-secret-taint.md). · **Unblocks:** [WS40](WS40-v2-release-gate.md) verification gate 1 on darwin.

## Context

`README.md` D32–D34. macOS host-native sandbox primary is `/usr/bin/sandbox-exec` with an SBPL profile auto-generated per session from the merged adapter manifest hints + environment policy. The profile is written to `$TMPDIR/criteria-sb-<session>.sb`, applied via `sandbox-exec -f <profile> <adapter-binary>`, and deleted on exit.

Apple has deprecated `sandbox-exec` but it remains the only host-native option without third-party tooling. No macOS soft alternative (D30 / D33). Cross-platform escape hatch is container mode (D12c, WS12).

## Prerequisites

WS09 merged.

## In scope

### Step 1 — Profile renderer

`internal/adapter/environment/sandbox/darwin.go` (build tag `//go:build darwin`):

```go
type Profile struct {
    AllowFileReads    []string
    AllowFileWrites   []string
    AllowNetworkHosts []string  // hostname:port; resolved to IPs for the rule
    AllowExec         []string  // explicit allowlist; empty = deny all exec
    BlockSysctl       bool
    BlockMachLookup   bool
    DefaultDeny       bool
}

// Render produces an SBPL-formatted profile string.
func (p *Profile) Render() string
```

The SBPL grammar is documented at: <https://github.com/apple-opensource/Security/blob/master/sandbox/man/sandbox.7.in> (and similar Apple archives). Base profile:

```scheme
(version 1)
(deny default)
(allow process-fork)
(allow process-exec
  (literal "/path/to/adapter-binary"))
(allow file-read*
  (literal "/path/that/adapter/needs"))
(allow network-outbound
  (remote ip "1.2.3.4:443"))
...
```

### Step 2 — Policy → profile translation

`internal/adapter/environment/sandbox/darwin_translate.go`:

```go
// FromPolicy translates a ResolvedPolicy from WS09 into a Profile.
func FromPolicy(p workflow.ResolvedPolicy, adapterBinary string) Profile
```

Hostname-to-IP resolution for network rules happens at translation time and is cached for the session. DNS lookups for allowed hosts happen before exec; if a hostname fails to resolve, error in strict mode and skip-with-warning in permissive.

### Step 3 — Loader integration

In `internal/adapter/loader.go`, when launching an adapter bound to a `sandbox`-type environment on darwin:

```go
func launchSandboxedDarwin(cmd *exec.Cmd, profile sandbox.Profile) error {
    tmpPath, err := writeProfile(profile)  // $TMPDIR/criteria-sb-<random>.sb
    if err != nil { return err }
    defer os.Remove(tmpPath)
    wrapped := &exec.Cmd{
        Path: "/usr/bin/sandbox-exec",
        Args: []string{"sandbox-exec", "-f", tmpPath, cmd.Path, ...cmd.Args[1:]},
        Env:  cmd.Env, ...
    }
    return wrapped.Run()
}
```

### Step 4 — Fallback when sandbox-exec is missing or fails

Per D34: if `sandbox-exec` is unavailable (a future macOS removing it, or a corporate device with execution policy blocking it), fall back to process-hardening primitives (env scrub, working-dir confinement, PATH sanitization, secret redaction, rlimits). In `policy_mode = "strict"` mode, fail closed; in permissive, log the degradation.

### Step 5 — Tests (darwin-only)

- Integration test that runs a tiny test binary under a generated profile and asserts:
  - File read outside allowlist fails with EPERM.
  - Network connect outside allowlist fails.
  - Allowed paths succeed.
- Translation test: table-driven over `ResolvedPolicy` shapes → expected SBPL snippets.

### Step 6 — Profile template versioning

A `profile_version = 1` literal embedded in each rendered profile (as a comment). When we later need to evolve the template, we bump the version and the renderer emits an annotation that the host can read back when debugging.

## Out of scope

- Linux sandbox — WS10.
- Container runtime — WS12.
- Future-macOS path when sandbox-exec is gone — left as a TODO with the documented fallback for now.

## Reuse pointers

- `internal/adapter/environment/sandbox/probe.go` (WS10's probe; expose macOS-side checks).
- WS09's `ResolvedPolicy`.

## Behavior change

**Yes** — on darwin, adapters bound to a `sandbox`-type environment run inside `sandbox-exec` with a rendered SBPL profile. Existing macOS users who relied on no sandboxing (the v0.3 default) see no change because the new behavior only activates for `sandbox`-type environments; `shell` (the legacy default) is unchanged.

## Tests required

- `darwin_test.go` (build tag) covers translation + an integration test on macOS CI runners.
- Probe-failure simulation: rename `/usr/bin/sandbox-exec` in CI sandbox to test the missing-binary path.

## Exit criteria

- darwin tests green on macOS CI runner.
- Profile template version recorded in render output.

## Files this workstream may modify

- `internal/adapter/environment/sandbox/darwin.go`, `darwin_translate.go` *(new)*.
- `internal/adapter/loader.go` — wire the macOS launch path.
- Test fixtures.

## Files this workstream may NOT edit

- `internal/adapter/environment/sandbox/linux.go` — WS10.
- `internal/adapter/environment/container/` — WS12.
- Other workstream files.
