# Workstream 5 — Shell adapter sandbox: design + first hardening

**Owner:** Workstream executor (security-focused) · **Depends on:** [W01](01-flaky-test-fix.md), [W02](02-golangci-lint-adoption.md) · **Unblocks:** future Phase 2 platform-specific sandboxing.

## Context

The shell adapter ([internal/adapters/shell/shell.go](../internal/adapters/shell/shell.go))
runs commands declared in HCL workflows directly via `os/exec`. There
is no isolation: a workflow author with write access to an HCL file
gets full process-level execution as the user running `criteria`.

This was acceptable while the only consumer was the (now-renamed)
internal team. It is the **single largest pre-deployment security
risk** flagged by the Phase 0 tech evaluation, and it was deferred
once already from Phase 0 (the original W04 shell-adapter-sandbox
shipped only the threat-model placeholder; the tech eval marks it as
"Critical / Pre-v1.0").

This workstream is **plan-and-first-pass**, exactly as the original
Phase 0 W04 was scoped. It produces:

1. A revised, complete threat model.
2. A first hardening pass implementing the cheap, high-value
   defaults that close the obvious holes without OS-specific work.
3. An explicit `[ARCH-REVIEW]` follow-up entry capturing the
   platform-specific sandboxing (sandbox-exec / seccomp / Job
   Objects) that Phase 2 will own.

Full filesystem isolation, syscall filtering, network egress
controls, and cgroup-based resource budgeting remain out of scope.
Those require platform-specific code, separate test infrastructure,
and a deliberate Phase 2 design decision.

## Prerequisites

- [W01](01-flaky-test-fix.md) merged (deterministic CI; the new
  hardening tests must not become the next flake source).
- [W02](02-golangci-lint-adoption.md) merged (new shell adapter
  files land linted).
- `make ci` green on `main`.

## In scope

### Step 1 — Author the threat model

Write **`docs/security/shell-adapter-threat-model.md`** with these
sections in order:

1. **Trust boundaries.**
   - Trusted: the operator who runs `./bin/criteria apply`; the
     filesystem they own.
   - Untrusted: HCL file authors who are not also the operator;
     adapter plugin authors operating outside the SDK contract;
     network-borne content embedded in workflow inputs.
2. **Attacker capabilities.**
   - Controls HCL file content (commands, env, working directory
     hints, allow-tools list).
   - May control workflow input values (CLI `--var`, ND-JSON
     event content, server-mode payloads).
   - Does **not** control the host filesystem outside what the
     operator's UID can already touch.
3. **Defender goals.**
   - Preserve confidentiality of files outside the workflow's
     declared working directory.
   - Prevent unintended privilege escalation (sudo prompts, setuid
     binaries on PATH, etc.).
   - Prevent unbounded resource consumption (CPU / memory /
     output buffer / wall clock).
   - Make every shell invocation auditable in the event stream.
4. **Out of scope (deferred to Phase 2).**
   - Defeating a motivated attacker who is already root.
   - Full filesystem isolation (chroot / overlayfs / mount
     namespaces).
   - Syscall filtering (seccomp-bpf, sandbox-exec profiles, Job
     Object restrictions).
   - Network egress controls.
   - cgroup-based resource budgeting.
   - Hardening any other adapter (Copilot, MCP). Different threat
     models, different work.
5. **Threat → mitigation table** that maps each in-scope attacker
   capability to a Step 2 hardening item, with a column for
   "deferred to Phase 2" entries.
6. **Migration / opt-out.** The `CRITERIA_SHELL_LEGACY=1`
   environment variable disables every Step 2 default for users
   whose workflows depend on the un-hardened path. Removed in
   `v0.3.0` (one phase after this lands). The doc names a date
   range, not a specific date — operators set the exact removal
   date in the changelog when v0.3.0 ships.

The document is a real review artifact; it must be readable
end-to-end by someone who has not seen the code. Reviewer rejects
"placeholder" content.

### Step 2 — First-pass hardening (implement)

Implement the following defaults in
[internal/adapters/shell/shell.go](../internal/adapters/shell/shell.go).
Each default has a corresponding test in Step 3.

#### 2.1 Environment allowlist

Default behavior: the spawned shell process inherits **only**:

- `PATH` (sanitized — see 2.2)
- `HOME`
- `USER` / `LOGNAME`
- `LANG` / `LC_*`
- `TZ`
- `TERM` (only when stdin is a TTY)

All other parent-process env vars are dropped. The HCL `step`
block gains an optional `env` attribute (`map(string)`) that
declares additional vars to inherit verbatim from the parent or
to set explicitly:

```hcl
step "build" {
  adapter = "shell"
  input {
    command = "make build"
    env = {
      "GOFLAGS" = "$GOFLAGS"   // inherit from parent
      "DEBUG"   = "1"          // set explicitly
    }
  }
}
```

The `$NAME` syntax is the only inheritance escape; everything
else is a literal value. This keeps the inheritance contract
auditable (the HCL declares every parent var that crosses the
boundary).

`CRITERIA_SHELL_LEGACY=1` restores full env inheritance.

#### 2.2 Command path hygiene

- The `command` attribute is parsed with the existing
  `defaultShell()` invocation (`sh -c <command>` or equivalent on
  Windows). That parsing is preserved.
- A new `command_path` attribute (optional, list of strings)
  declares the PATH the shell sees. When set, this **replaces** the
  inherited PATH. When absent, PATH is inherited but stripped of
  any `.` or empty-segment entries (which silently expand to CWD
  and are a privilege-escalation vector).

`CRITERIA_SHELL_LEGACY=1` restores the unsanitized PATH.

#### 2.3 Hard timeout

Every shell step gets a hard timeout. Default: 5 minutes.
HCL-overridable via a new `timeout` attribute on the step input
(string, parsed by `time.ParseDuration`). Bounds:

- Minimum: `1s` (sub-second timeouts are unreliable across OSes).
- Maximum: `1h`. Workflows that genuinely need longer must split
  into multiple steps, or set `CRITERIA_SHELL_LEGACY=1`.

On timeout, the adapter sends `SIGTERM`, waits 5 seconds, then
`SIGKILL` (Unix). On Windows, `Process.Kill()` directly. The
adapter emits an `adapter` event with `event_type = "timeout"`
and the configured limit, then returns `Outcome: "failure"`.

#### 2.4 Bounded output capture

Stdout and stderr are captured into bounded buffers. Default
limit per stream: 4 MiB. HCL-overridable via `output_limit_bytes`
on the step input. Bounds: 1 KiB to 64 MiB.

Behavior on overflow:

- The buffer truncates at the limit.
- An `adapter` event with `event_type = "output_truncated"` and
  `stream`, `dropped_bytes`, `limit_bytes` is emitted.
- The step still completes (truncation does not by itself cause
  failure); the `outputs` map carries the truncated content with
  a `_truncated_<stream>: "true"` sentinel key.

This replaces the current unbounded `bytes.Buffer` capture in
`captureOutputs` ([shell.go:103](../internal/adapters/shell/shell.go)).

`CRITERIA_SHELL_LEGACY=1` restores unbounded capture.

#### 2.5 Working-directory confinement

A new `working_directory` attribute on the step input declares the
CWD for the spawned process. When absent, the process inherits
the operator's CWD (current behavior).

When set, the value must resolve under the operator's home or a
path explicitly listed in `CRITERIA_SHELL_ALLOWED_PATHS` (a
colon-separated env var). Values containing `..` after path
cleaning are rejected at compile time.

Reject at compile time, not runtime: surface the diagnostic via
HCL diagnostics so `criteria validate` catches it. The check
plugs into [workflow/compile_steps.go](../workflow/compile_steps.go)
(post-W04 location) via an adapter-specific compile hook.

If introducing an adapter-specific compile hook is too invasive
for this workstream, fall back to runtime rejection with a
clear error and document the hook as a Phase 2 follow-up — the
runtime check is still a real defense.

`CRITERIA_SHELL_LEGACY=1` disables the path-confinement check
(but keeps the CWD assignment).

### Step 3 — Tests

One focused test per default. All run under `make test`; no
network, no external binaries beyond what's already on a
standard Linux CI runner. macOS-only behavior (e.g. signal
mapping) gets a `runtime.GOOS` guard.

Tests live in `internal/adapters/shell/shell_sandbox_test.go`
(new):

1. **Env allowlist.** Set `SECRET=value` in the test process via
   `t.Setenv`; run a shell step that prints `$SECRET`. Assert the
   stdout is empty. Then set `env = { "SECRET" = "$SECRET" }` in
   HCL; assert stdout is `value`.
2. **Command path hygiene.** Construct a temp dir with a `bin/`
   containing a script `evil` that the test would not want run.
   Set parent PATH to include `.`. Assert that running
   `command = "evil"` (relative) does not find the temp script,
   producing `command not found`. Then with explicit
   `command_path = ["<tempdir>/bin"]`, assert the script runs.
3. **Timeout.** A workflow with `command = "sleep 10"` and
   `timeout = "1s"`. Assert the step returns `failure`, completes
   within 7s wall-clock (1s timeout + 5s grace + buffer), and
   emits an `adapter` event with `event_type = "timeout"`.
4. **Output bounds.** A workflow that emits 10 MiB of stdout
   with `output_limit_bytes = 1048576` (1 MiB). Assert the
   process returns success, the captured `stdout` field is
   exactly 1 MiB, an `adapter` event with
   `event_type = "output_truncated"` is emitted with
   `dropped_bytes ≈ 9 MiB`, and the host RSS does not exceed a
   sanity threshold (proves no unbounded buffer).
5. **Working-directory confinement.** A workflow with
   `working_directory = "/etc"` (or another path outside HOME)
   fails `criteria validate` with a clear diagnostic naming the
   attribute and the offending path. With
   `CRITERIA_SHELL_ALLOWED_PATHS=/etc`, validation passes.
6. **Legacy opt-out.** With `CRITERIA_SHELL_LEGACY=1`, the test
   from (1) shows full env inheritance (asserts `$SECRET = value`
   without HCL declaration). One legacy-opt-out test is
   sufficient — it proves the env var actually disables the
   defaults.

Tests must be deterministic and `-race`-clean (the timeout test
is the most likely flake source; use a generous wall-clock
budget and assert relative ordering, not exact timings).

### Step 4 — Documentation updates

Update **`docs/plugins.md`** with the new HCL attributes and a
short "Security defaults" section pointing at the threat model.

Update **`examples/`** if any existing example workflow violates
the new defaults — the `make validate` target gates this. Prefer
fixing the example over loosening the default; if a legitimate
example needs broader access (unlikely), document it inline with
a comment naming the security tradeoff.

Add **`docs/security/README.md`** as the index for the
`docs/security/` directory (currently empty per the original W04
deferral). One-line entry per doc.

### Step 5 — Forward pointer for Phase 2

Append an `[ARCH-REVIEW]` entry to this workstream's reviewer
notes capturing the platform-specific sandboxing work that Phase
1 explicitly defers:

- macOS: `sandbox-exec` profile generated from the threat-model's
  filesystem confinement intent.
- Linux: namespaces (mount, network, PID) and seccomp-bpf
  filter for the shell process tree.
- Windows: Job Objects with UI, IO, and process-creation
  restrictions.
- cgroup-based resource budgeting (Linux only initially).
- Network egress allow/deny.

Severity: `major`. The `[ARCH-REVIEW]` entry feeds Phase 2
planning; this workstream does not implement any of it.

## Out of scope

- Platform-specific sandboxing (sandbox-exec, seccomp, Job Objects).
  Documented in the threat model; deferred to Phase 2.
- Filesystem isolation (chroot / overlayfs / mount namespaces).
- Network egress controls.
- cgroup-based resource budgeting.
- Hardening any other adapter (Copilot, MCP).
- Replacing `os/exec` with a different process-spawning library.
- Adding new permission-prompt UI.

## Files this workstream may modify

**Created:**

- `docs/security/shell-adapter-threat-model.md`
- `docs/security/README.md`
- `internal/adapters/shell/shell_sandbox_test.go`
- `internal/adapters/shell/sandbox.go` (extracted helpers; keeps
  `shell.go` readable)
- `internal/adapters/shell/sandbox_unix.go` (build-tagged
  `//go:build unix`)
- `internal/adapters/shell/sandbox_windows.go` (build-tagged
  `//go:build windows`)

**Modified:**

- `internal/adapters/shell/shell.go`
- `workflow/compile_steps.go` (post-W04 location; adapter compile
  hook for `working_directory` validation, only if the hook
  approach is adopted)
- `docs/plugins.md`
- `examples/*.hcl` (only if existing examples break under the
  new defaults)
- `.golangci.baseline.yml` (delete entries pointed at this
  workstream, if any)

This workstream may **not** edit `README.md`, `PLAN.md`,
`AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any
other workstream file. CHANGELOG entries are deferred to
[W10](10-phase1-cleanup-gate.md).

## Tasks

- [ ] Author `docs/security/shell-adapter-threat-model.md` per
      Step 1.
- [ ] Author `docs/security/README.md`.
- [ ] Implement env allowlist (Step 2.1) + tests.
- [ ] Implement command path hygiene (Step 2.2) + tests.
- [ ] Implement hard timeout (Step 2.3) + tests.
- [ ] Implement bounded output capture (Step 2.4) + tests.
- [ ] Implement working-directory confinement (Step 2.5) + tests.
- [ ] Wire `CRITERIA_SHELL_LEGACY=1` opt-out and add the legacy
      test (Step 3.6).
- [ ] Update `docs/plugins.md` and `examples/` as needed.
- [ ] Add the `[ARCH-REVIEW]` entry per Step 5.
- [ ] `make ci` green; `make validate` green.
- [ ] CLI smoke (`./bin/criteria apply examples/hello.hcl`)
      exits 0 under the new defaults.

## Exit criteria

- `docs/security/shell-adapter-threat-model.md` exists and is
  reviewed end-to-end by a human (the workstream reviewer is
  acceptable for this first iteration).
- All five Step 2 hardening defaults are implemented with the
  matching Step 3 tests.
- The `CRITERIA_SHELL_LEGACY=1` opt-out is wired and tested.
- `make ci`, `make test`, `make validate`, and the CLI smoke
  exit 0 against the new defaults.
- No new entries in `.golangci.baseline.yml`.
- `[ARCH-REVIEW]` follow-up captured in reviewer notes with
  severity `major`.
- The hardening tests pass under `go test -race -count=20` (the
  timeout test is the most likely flake source; this is the
  gate).

## Tests

Six tests, listed verbatim in Step 3. All must run in `make test`
and gate CI. No new package; tests live in
`internal/adapters/shell/shell_sandbox_test.go`.

## Risks

| Risk | Mitigation |
|---|---|
| Hardening breaks an example workflow that authors rely on | The legacy opt-out preserves the old path; the threat model documents the migration. `make validate` catches breakage at PR time. Fix the example first if it violates a security default; only set `CRITERIA_SHELL_LEGACY=1` for a tracked, time-boxed exception. |
| Hard timeout flakes on slow CI runners | The timeout test asserts relative ordering (`failure` outcome + `timeout` event), not exact wall-clock. The grace period is 5s; CI runners that can't honor 1s+5s are too slow for this codebase regardless. |
| Bounded output capture truncates a legitimate large-output workflow | `output_limit_bytes` is HCL-overridable up to 64 MiB; `CRITERIA_SHELL_LEGACY=1` restores unbounded. Truncation is non-fatal and clearly signaled in the event stream. |
| Working-directory confinement check rejects valid CI paths (e.g. `/runner/_work`) | `CRITERIA_SHELL_ALLOWED_PATHS` opt-in covers this. CI documentation updates follow if/when CI workflows hit it; the env var is the blast valve. |
| The `[ARCH-REVIEW]` for Phase 2 sandboxing turns into a forever-deferred note | This workstream is the **second** time shell hardening has been scoped; the original Phase 0 W04 deferred most of it. The `[ARCH-REVIEW]` note is graded `major` and the W10 cleanup gate explicitly checks that Phase 2 planning lists platform-specific sandboxing as a candidate. |
| The threat-model doc rots once written | Treat it as living. The exit criterion is "reviewed end-to-end by a human"; future workstreams that touch the shell adapter must update the threat model in the same PR. Document this contract in `docs/security/README.md`. |
| Adapter-specific compile hook for `working_directory` validation is too invasive | Step 2.5 lists runtime rejection as the documented fallback. Take the fallback if the compile hook would balloon the diff; record the choice in reviewer notes and add the compile hook as a Phase 2 forward-pointer. |
| Build-tag fragmentation (`sandbox_unix.go`, `sandbox_windows.go`) leads to OS-specific behavior drift | All OS-conditional code stays inside the two build-tagged files behind a single helper interface (`platformSandbox`); the Step 3 tests run on the CI Linux runner and provide signal for the unix path. macOS-specific paths get `runtime.GOOS == "darwin"` skips with a follow-up note. |
