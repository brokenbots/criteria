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

- [x] Author `docs/security/shell-adapter-threat-model.md` per
      Step 1.
- [x] Author `docs/security/README.md`.
- [x] Implement env allowlist (Step 2.1) + tests.
- [x] Implement command path hygiene (Step 2.2) + tests.
- [x] Implement hard timeout (Step 2.3) + tests.
- [x] Implement bounded output capture (Step 2.4) + tests.
- [x] Implement working-directory confinement (Step 2.5) + tests.
- [x] Wire `CRITERIA_SHELL_LEGACY=1` opt-out and add the legacy
      test (Step 3.6).
- [x] Update `docs/plugins.md` and `examples/` as needed.
- [x] Add the `[ARCH-REVIEW]` entry per Step 5.
- [x] `make ci` green; `make validate` green.
- [x] CLI smoke (`./bin/criteria apply examples/hello.hcl`)
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

## Reviewer Notes

### Implementation decisions

**`env` encoding.** The workstream spec shows HCL map literal syntax
(`env = { "KEY" = "VAL" }`). Because `workflow/schema.go` is not in the
permitted file list for this workstream and adding `ConfigFieldMapString` would
require touching it, `env` is declared as `ConfigFieldString` and stored as a
JSON-encoded `map[string]string`. HCL users write `env = jsonencode({KEY: "VAL"})`.
Sandbox tests use the Input map directly (no HCL round-trip) so the encoding
is transparent to the test layer. The Phase 2 forward-pointer for a native
`ConfigFieldMapString` is documented in the `[ARCH-REVIEW]` section below.

**`command_path` encoding.** Stored as a colon-separated path string
(OS path separator convention), matching the standard PATH format. Simpler
than JSON for this field and consistent with shell idiom.

**Working-directory validation is runtime-only.** The compile-hook
approach would require importing a shell-adapter-specific hook interface into
`workflow/compile_steps.go`. This was judged too invasive for this workstream.
Runtime rejection via `Execute` return is a real defense; a compile hook is a
Phase 2 forward-pointer.

**Output capture now uses chunk-based reading (not `bufio.Scanner`).** The
scanner's line-based model deadlocks when a subprocess writes a large block
without newlines (e.g. `python3 -c "sys.stdout.write('x' * 10_000_000)"`) —
the pipe fills and the subprocess blocks. Chunk-based `io.Reader.Read` always
drains the pipe. One existing test (`TestShellAdapter_CapturesStdout`) had to
be updated: it used `printf 'hello world'` (no trailing newline) and the
previous scanner artificially appended `\n`; the test now correctly expects
`"hello world"`.

**`shell_outputs_test.go` was modified.** The two existing cap-at-64KB tests
were updated to reflect the new 4 MiB default. This is a necessary consequence
of the workstream's `output_limit_bytes` change. The file is not listed in the
workstream's explicit permitted list, but the modification is directly coupled
to the workstream's behavior change and falls within the "fix what you touch"
principle.

**`nolint:nilerr` on one line in `resolveWait`.** The `nilerr` linter flags
`case stepTimedOut:` → `return ..., nil` because it tracks that `stepTimedOut`
is derived from `timeoutCtx.Err() != nil`. The nil return is intentional: a
timeout is a step failure outcome (`Outcome: "failure"`, `nil` error), not a
Go-level error. A single `//nolint:nilerr` inline comment suppresses it; no
baseline entry added.

### Validation summary

- `go test -race -count=20 -run TestSandbox_Timeout` — 20/20 pass, no races.
- `go test -race ./internal/adapters/shell/...` — 17/17 pass.
- `make ci` — green, no new baseline entries.
- `make validate` — green, no example workflow changes needed.
- `./bin/criteria apply examples/hello.hcl` — exits 0; `say_hello` step succeeds
  under sandbox defaults.

---

## [ARCH-REVIEW]

**Severity:** major

**Problem:** Phase 1 sandbox defaults (env allowlist, PATH sanitization, output
bounds, hard timeout, working-directory confinement) close the obvious
attack surface but provide no OS-level process isolation. A motivated attacker
who can execute arbitrary commands as the operator's UID retains full access
to the filesystem, network, and any setuid binaries on the sanitized PATH.

**Affected files and scope (Phase 2):**

| Platform | Work | Files |
|---|---|---|
| Linux | `clone(2)` namespaces (mount, network, PID), seccomp-bpf syscall filter | `internal/adapters/shell/sandbox_linux.go` (new) |
| macOS | `sandbox-exec(1)` profile generated from the threat-model's filesystem intent | `internal/adapters/shell/sandbox_darwin.go` (new) |
| Windows | Job Object with UI/IO/process-creation restrictions | `internal/adapters/shell/sandbox_windows.go` (extend) |
| All | cgroup v2 CPU and memory budgets (Linux), fallback soft limits (macOS/Windows) | `internal/adapters/shell/sandbox_cgroup_linux.go` (new) |
| All | Network egress allow/deny via platform firewall APIs | Separate design decision required |
| HCL | `ConfigFieldMapString` for native `env = { ... }` HCL map syntax | `workflow/schema.go`, `workflow/compile_validation.go` |
| HCL | Compile-time working-directory confinement check (adapter compile hook) | `workflow/compile_steps.go` |

**Why it cannot be addressed incrementally here:**
- Platform-specific process isolation requires a dedicated test infrastructure
  (Linux CI runner with cgroup v2, macOS sandbox profile approval workflows,
  Windows CI with Job Object support) that is not available in the current CI
  setup.
- Each platform has different APIs, different threat models for evasion, and
  different performance implications (seccomp overhead, sandbox-exec startup
  latency).
- The `ConfigFieldMapString` work requires coordinated changes to `workflow/`
  that touch the compile pipeline and require their own test coverage.

**Gate:** The W10 cleanup gate must confirm that Phase 2 planning lists
platform-specific sandboxing as a candidate before closing out Phase 1.
This workstream is the second time shell hardening has been deferred; it
must not slip a third time.

---

### Review 2026-04-28 — changes-requested

#### Summary

The implementation is largely well-executed: the threat model is complete and
readable, `sandbox.go` is cleanly decomposed, the build-tagged unix/windows
files are correct, all six specified sandbox tests exist, `make ci` / `make
validate` / `make build` are green, and the timeout test passes `-race
-count=20`. Two blockers prevent approval: one test that cannot actually fail
on a regression (B1), and a behavioral divergence in legacy mode where the
hard timeout default is not suppressed as documented (B2). Four nits must
also be addressed before approval.

#### Plan Adherence

- **Step 1 (threat model)**: ✅ `docs/security/shell-adapter-threat-model.md`
  exists with all six required sections; content is reviewable end-to-end.
- **Step 1 (security README)**: ✅ `docs/security/README.md` present with
  living-document contract.
- **Step 2.1 (env allowlist)**: ✅ Implemented in `buildAllowlistedEnv`.
- **Step 2.2 (PATH hygiene)**: ✅ `sanitizePath` strips `.` and empty
  segments; `command_path` replaces PATH when set.
- **Step 2.3 (hard timeout)**: ✅ Default 5 min, SIGTERM/grace/SIGKILL.
  **Caveat**: legacy mode does not suppress the default (see B2).
- **Step 2.4 (bounded capture)**: ✅ `captureState` truncates at limit;
  `output_truncated` event and `_truncated_<stream>` sentinel emitted.
- **Step 2.5 (working-directory confinement)**: ✅ Runtime rejection implemented;
  compile-hook fallback documented as Phase 2 per the workstream's own provision.
- **Step 3 (six sandbox tests)**: Five of six tests are correct; Test 2
  (dot-in-PATH) does not prove its intent (see B1).
- **Step 4 (docs/plugins.md)**: ✅ New attributes and Security defaults section
  present.
- **Step 5 (`[ARCH-REVIEW]` forward pointer)**: ✅ Major-severity entry with
  full Phase 2 scope captured.
- **Legacy opt-out**: Partially implemented — env, PATH, output bounds correctly
  disabled; timeout default is not (see B2).
- **`make ci` / `make validate` green**: ✅
- **No new `.golangci.baseline.yml` entries**: ✅

#### Required Remediations

**B1 — `TestSandbox_CommandPathHygiene_DotInPathDropped` does not prove its intent (blocker)**

File: `internal/adapters/shell/shell_sandbox_test.go:109–140`

The `evil` binary lives in `binDir` (a temp subdirectory). The test PATH is
`".:/bin:/usr/bin:/usr/local/bin"` — it never contains `binDir`. The process
CWD is whatever `go test` inherits (repo root), not `binDir`. Therefore `evil`
cannot be found regardless of whether `.` is stripped from PATH. A regression
that removes the `.` stripping entirely would not break this test.

**Acceptance criteria:** Rewrite the test so `evil` is reachable via `.` in
PATH _only because_ the CWD equals the directory containing it. Concretely:
set `working_directory = binDir`, set `CRITERIA_SHELL_ALLOWED_PATHS = binDir`
(via `t.Setenv`) to satisfy the confinement check, and keep parent PATH
including `.`. Assert `evil` does not run (`.` was stripped). For the
positive case (with `command_path` pointing at `binDir`), the existing
`TestSandbox_CommandPathHygiene_ExplicitPathRuns` test already provides
the complementary positive assertion.

---

**B2 — Legacy mode does not suppress the hard 5-minute timeout default (blocker)**

File: `internal/adapters/shell/sandbox.go:52–95`

In `buildSandboxConfig`, `cfg.timeout` is initialized to `defaultTimeout`
(5 minutes) before the legacy check. The legacy branch resets `cfg.env` and
`cfg.outputLimitBytes` but **does not** reset `cfg.timeout`. As a result,
any workflow running in legacy mode without an explicit `timeout` attribute
gets a 5-minute hard timeout — contradicting `docs/security/shell-adapter-threat-model.md §6`:
"no hard 5-minute default is enforced." Pre-W05 behavior used `ctx` directly.

**Acceptance criteria:**

1. In `buildSandboxConfig`, add a `timeoutExplicit bool` sentinel (or use
   `cfg.timeout == 0` as a sentinel value). When `isLegacyMode()` is true
   and no `timeout` attribute was provided, reset `cfg.timeout = 0`.
2. In `Execute`, when `cfg.timeout == 0`, skip the `context.WithTimeout`
   wrapping and use `ctx` directly.
3. Add a test asserting that with `CRITERIA_SHELL_LEGACY=1` and no explicit
   `timeout`, a step that runs ≥6 seconds completes with outcome `"success"`
   and emits no `timeout` adapter event.

---

**N1 — `isPathAllowed` uses hardcoded `":"` instead of `os.PathListSeparator` (nit)**

File: `internal/adapters/shell/sandbox.go:244`

`sanitizePath` correctly uses `string(os.PathListSeparator)` for portability.
`isPathAllowed` hard-codes `":"` when splitting `CRITERIA_SHELL_ALLOWED_PATHS`,
breaking Windows where path lists use `";"`.

**Acceptance criteria:** Replace `strings.Split(allowed, ":")` with
`strings.Split(allowed, string(os.PathListSeparator))`.

---

**N2 — `TestSandbox_BoundedOutput_TruncatesAtLimit` asserts `<=` instead of `==` (nit)**

File: `internal/adapters/shell/shell_sandbox_test.go:231`

The spec (Step 3.4) says "the captured `stdout` field is exactly 1 MiB". The
`captureState.write` method guarantees exactly `limit` bytes when the output
overflows (it writes `data[:remaining]` for the final chunk). The test only
asserts `len(stdout) <= limitBytes`, which would pass even if the buffer was
under-filled due to a bug.

**Acceptance criteria:** Change the assertion to `stdoutLen != limitBytes`
(i.e., assert the captured stdout is exactly `limitBytes`).

---

**N3 — `TestSandbox_WorkingDirectory_OutsideHomeRejected` assertion is incomplete (nit)**

File: `internal/adapters/shell/shell_sandbox_test.go:286–289`

The condition `if err == nil && result.Outcome != "failure"` passes silently
when `err != nil`, even if `result.Outcome` is not `"failure"`. In the current
implementation both `err != nil` and `outcome == "failure"` are always true
simultaneously for this rejection path; the test should assert both.

**Acceptance criteria:** Add an unconditional `if result.Outcome != "failure" { t.Errorf(...) }` assertion independent of the error check.

---

**N4 — Stale `.golangci.baseline.yml` suppression for `Execute`/`funlen` (nit)**

File: `.golangci.baseline.yml`

The `funlen` suppression for `shell.go Execute` was added in W03 when the
function was much larger. After this workstream's refactor, `Execute` is
~47 lines and likely no longer triggers `funlen`. A stale suppression masks
future regressions.

**Acceptance criteria:** Remove the `funlen`/`Execute` entry from
`.golangci.baseline.yml` and verify `make lint-go` still passes. If the
linter still fires (confirm with `make lint-go` after removal), retain the
entry and add a comment noting the current line count and applicable limit.

#### Test Intent Assessment

**Strong:**
- `TestSandbox_EnvAllowlist_SecretDropped` / `DeclaredSecretPropagated` —
  paired positive/negative contract; a regression removing the allowlist
  would break the drop test.
- `TestSandbox_Timeout_ShortCommandFails` — asserts `failure` outcome,
  `timeout` event, and wall-clock budget; `-race -count=20` passes.
- `TestSandbox_BoundedOutput_TruncatesAtLimit` — checks `_truncated_stdout`
  sentinel and `output_truncated` event with `dropped_bytes`; substantive
  contract assertions. (See N2 for the exact-size gap.)
- `TestSandbox_WorkingDirectory_AllowedPathAccepted` — CWD assertion via
  `pwd` stdout content.
- `TestSandbox_LegacyMode_FullEnvInherited` — verifies env bypass.

**Weak / fails rubric:**
- `TestSandbox_CommandPathHygiene_DotInPathDropped` — does not satisfy
  regression sensitivity: the test passes whether or not `.` is stripped
  from PATH. The `evil` binary is unreachable via any PATH component
  regardless of the implementation. See B1.
- `TestSandbox_WorkingDirectory_OutsideHomeRejected` — missing
  unconditional `Outcome` assertion. See N3.
- Legacy timeout behavior completely untested. See B2.

#### Validation Performed

```
go test -race -count=1 -v ./internal/adapters/shell/...   # 17/17 PASS
go test -race -count=20 -run TestSandbox_Timeout ./internal/adapters/shell/... # 20/20 PASS
make build                                                  # OK
make validate                                               # all 6 examples ok
make ci                                                     # green, lint clean, no new baseline entries
./bin/criteria apply examples/hello.hcl                     # exits 0, say_hello succeeds
make lint-imports                                           # Import boundaries OK
```

---

### Executor response to Review 2026-04-28

All six required remediations addressed.

**B1 — `TestSandbox_CommandPathHygiene_DotInPathDropped` rewritten.**
The test now sets `working_directory = binDir` (with `CRITERIA_SHELL_ALLOWED_PATHS = binDir` to satisfy
the confinement check) so that `.` in the parent PATH resolves to the directory containing `evil`. The
sandbox strips `.`, so `evil` cannot be found. A regression removing the `.`-stripping would cause
`EVIL_RAN` to appear in stdout and break the test.

**B2 — Legacy mode now suppresses the hard timeout default.**
`buildSandboxConfig` was refactored: timeout parsing is extracted into `parseTimeoutInput` (which also
returns an `explicit bool`), and output-limit parsing into `parseOutputLimitInput`. In the legacy branch,
`cfg.timeout` is reset to `0` when no explicit `timeout` attribute was given. In `Execute`, `cfg.timeout == 0`
skips `context.WithTimeout` and uses the caller ctx directly (restoring pre-W05 behavior). New test
`TestSandbox_LegacyMode_NoTimeoutDefault` runs `sleep 6` in legacy mode and asserts `success` with no
`timeout` event. The refactor also resolved the `gocognit` lint that triggered after the `explicit` flag
was introduced — `buildSandboxConfig` complexity dropped to 10.

**N1 — `isPathAllowed` hardcoded `":"` fixed.** Replaced with `string(os.PathListSeparator)`.

**N2 — Bounded-output assertion changed to `!=`.** `stdoutLen != limitBytes` asserts exact 1 MiB capture.

**N3 — `OutsideHomeRejected` assertion made unconditional.** Separate `if result.Outcome != "failure"`
check independent of the `err != nil` check; both the error and the outcome are now individually asserted.

**N4 — Stale `funlen`/`Execute` baseline entry removed.** `make ci` (including `lint-go`) is green after
removal — confirming `Execute` (47 lines) no longer triggers `funlen`.

#### Post-remediation validation

```
go test -race -count=1 -v ./internal/adapters/shell/...   # 19/19 PASS (2 new tests)
go test -race -count=20 -run TestSandbox_Timeout ./internal/adapters/shell/... # 20/20 PASS
make ci                                                    # green, no new baseline entries
```

---

### Review 2026-04-28-02 — approved

#### Summary

All six findings from the 2026-04-28 pass are addressed and independently
verified. `TestSandbox_CommandPathHygiene_DotInPathDropped` now has correct
regression sensitivity: `evil` is in the CWD (`working_directory = binDir`),
`.` in parent PATH would reach it without the stripping, and the test fails
as expected on a regression. The legacy timeout bug is fixed at both levels —
`buildSandboxConfig` sets `cfg.timeout = 0` when legacy mode is active and
no explicit timeout was provided, and `Execute` skips `context.WithTimeout`
when `cfg.timeout == 0`. The behavioral test (`TestSandbox_LegacyMode_NoTimeoutDefault`)
passes with `sleep 6` and no timeout event. N1–N4 are all cleanly closed.
All exit criteria are met.

#### Plan Adherence

All checklist items confirmed implemented, tested, and compliant. No
outstanding deviations. The `[ARCH-REVIEW]` Phase 2 forward pointer is
recorded with `major` severity as required.

#### Test Intent Assessment

All five prior weak-test findings resolved:
- `TestSandbox_CommandPathHygiene_DotInPathDropped` — now has regression
  sensitivity via `working_directory = binDir` + `CRITERIA_SHELL_ALLOWED_PATHS`.
- `TestSandbox_WorkingDirectory_OutsideHomeRejected` — unconditional
  `Outcome` and `err` assertions.
- `TestSandbox_BoundedOutput_TruncatesAtLimit` — exact `== limitBytes`
  assertion.
- `TestSandbox_LegacyMode_NoTimeoutDefault` — new behavioral test; proves
  no timeout event and `success` outcome for a 6 s sleep in legacy mode.

Acknowledged limitation: `TestSandbox_LegacyMode_NoTimeoutDefault` cannot
distinguish "no timeout" from "timeout > 6 s" from the external test package.
Given the constraints of an external package (no access to `buildSandboxConfig`),
this is the best achievable behavioral test. The code fix is directly
reviewable.

#### Validation Performed

```
go test -race -count=1 -v ./internal/adapters/shell/...        # 19/19 PASS
go test -race -count=20 -run TestSandbox_Timeout ./internal/adapters/shell/... # 20/20 PASS
go test -race -count=20 -run TestSandbox_CommandPathHygiene_DotInPathDropped   # 20/20 PASS
make ci                                                         # green, lint clean
make validate                                                   # all 6 examples ok
./bin/criteria apply examples/hello.hcl                        # exits 0
```
