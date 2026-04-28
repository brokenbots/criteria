# Shell Adapter Threat Model

**Scope:** `internal/adapters/shell/shell.go` and associated sandbox helpers.  
**Phase:** Phase 1 — first hardening pass (W05).  
**Deferred to Phase 2:** Platform-specific process isolation (see §Out of scope below).

---

## 1. Trust Boundaries

| Boundary | Trusted | Untrusted |
|---|---|---|
| **Operator** | The person who runs `./bin/criteria apply` on the host machine. Owns the filesystem, the process UID, and the environment of the parent process. | — |
| **Workflow author** | Any party who controls the content of an HCL workflow file and who is **not** simultaneously the operator. In multi-tenant or CI environments this is the common case. | ✓ — treat as untrusted. |
| **Adapter plugin author** | A third party whose plugin binary is installed in `CRITERIA_PLUGINS/` or `~/.criteria/plugins/`. The plugin contract is gRPC over a local transport (the `criteria-adapter-*` binary); anything outside the SDK contract is untrusted. | ✓ — for the shell adapter this is not applicable; the shell adapter is built-in. |
| **Workflow input values** | Values provided by the operator at invocation time via `--var`, ND-JSON event content, or server-mode payloads. Even operator-supplied values should be treated as potentially adversarial after the initial invocation because they flow through external event channels in server mode. | Partially trusted — validate before forwarding to shell. |

**Summary:** only the operator is trusted. Everyone else who can influence the
content of the HCL file or the values flowing into it is untrusted.

---

## 2. Attacker Capabilities

An attacker who controls the HCL workflow file can:

1. **Set arbitrary commands.** The `command` attribute is the shell command string
   passed verbatim to `sh -c` (or `cmd /C` on Windows). An attacker can run any
   command the operator's UID can run.

2. **Control environment variables.** Without sandbox defaults the child process
   inherits the full parent environment. Secrets in the parent's environment
   (tokens, keys, passwords) are accessible to the command.

3. **Set the working directory.** The `working_directory` attribute (Phase 1)
   sets the CWD for the spawned process. Without confinement, paths such as
   `/etc`, `/`, or a relative path with `..` are accepted.

4. **Declare arbitrary PATH entries.** The `command_path` attribute (Phase 1)
   replaces the PATH seen by the child. An attacker could insert a malicious
   `bin/` directory before `/usr/bin` to shadow legitimate binaries.

5. **Control workflow input values.** In server mode, event payloads flow through
   network channels. An attacker who can inject events can influence step inputs.

An attacker does **not**:

- Control the host filesystem beyond what the operator's UID can already touch.
- Gain higher privileges than the operator's UID (assuming no setuid binaries on PATH; see §Defender goals).
- Control network interfaces directly (the shell adapter does not restrict network, but that is a Phase 2 item).

---

## 3. Defender Goals

| Goal | Mechanism (Phase 1) | Status |
|---|---|---|
| **Confidentiality of env secrets** | Environment allowlist — child inherits only `PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`, `LC_*`, `TZ`, `TERM`(tty). All other parent vars are dropped unless explicitly declared in `input.env`. | ✅ Implemented in W05 |
| **PATH integrity** | PATH sanitization — strips empty and non-absolute segments (including `.`) from the inherited PATH; `command_path` replaces PATH entirely when set. Detection of world-writable directories is deferred to Phase 2. | ✅ Implemented in W05 |
| **Working directory confinement** | `working_directory` must resolve under `$HOME` or `CRITERIA_SHELL_ALLOWED_PATHS`; `..` traversal is rejected at runtime. | ✅ Implemented in W05 |
| **Unbounded resource consumption (CPU / wall clock)** | Hard timeout per step (default 5 min; 1s–1h range). On timeout: SIGTERM → 5 s grace → SIGKILL (Unix), Kill (Windows). | ✅ Implemented in W05 |
| **Unbounded resource consumption (output buffer / memory)** | Bounded stdout+stderr capture (default 4 MiB per stream; 1 KiB–64 MiB range). Overflow emits `output_truncated` event; step still succeeds. | ✅ Implemented in W05 |
| **Auditability** | Timeout and truncation events are emitted into the run event stream via `sink.Adapter`. | ✅ Implemented in W05 |
| **Privilege escalation via setuid** | Phase 1 does not prevent execution of setuid binaries that are already on the sanitized PATH. Full mitigation requires syscall filtering (Phase 2). | ⏳ Phase 2 |

---

## 4. Out of Scope (Deferred to Phase 2)

The following capabilities are explicitly NOT delivered by this workstream:

- **Defeating a motivated attacker who is already root.** If the operator runs
  `criteria` as root, the sandbox provides no meaningful isolation.
- **Full filesystem isolation.** chroot, overlayfs, and mount namespaces are
  platform-specific and require deliberate Phase 2 design.
- **Syscall filtering.** seccomp-bpf (Linux), sandbox-exec profiles (macOS),
  and Job Object restrictions (Windows) are deferred. See [ARCH-REVIEW] in
  `workstreams/05-shell-adapter-sandbox.md`.
- **Network egress controls.** The child process inherits the full network
  access of the operator's UID.
- **cgroup-based resource budgeting.** Linux-only; requires cgroup v2 setup.
- **Hardening other adapters.** Copilot and MCP have different threat models and
  are out of scope for this workstream.

---

## 5. Threat → Mitigation Table

| Threat | Attacker capability | Phase 1 mitigation | Phase 2 mitigation |
|---|---|---|---|
| **T1 — secret leakage via env** | Controls HCL env attribute | Allowlist: child inherits only safe vars; additional vars require explicit declaration | — (env allowlist is sufficient) |
| **T2 — PATH hijacking** | Controls `command_path`; may inject `.` or relative segment via env | PATH sanitization strips empty / non-absolute segments (including `.`); `command_path` replaces PATH entirely; `PATH` is reserved in `input.env` | seccomp restricts exec to declared paths; world-writable-dir detection |
| **T3 — arbitrary CWD escape** | Sets `working_directory` to `/etc`, `../../etc`, etc. | Runtime confinement: path must be under `$HOME` or `CRITERIA_SHELL_ALLOWED_PATHS`; `..` traversal rejected | Compile-time HCL diagnostic (adapter compile hook — Phase 2 forward pointer) |
| **T4 — CPU / wall-clock denial** | Provides a `sleep 9999` or equivalent command | Hard timeout (default 5 min); SIGTERM + grace + SIGKILL | cgroup CPU quota (Linux) |
| **T5 — memory / output denial** | Command that emits gigabytes of stdout/stderr | Bounded capture (default 4 MiB/stream); overflow truncated, step continues | cgroup memory limit (Linux) |
| **T6 — privilege escalation via setuid** | Relies on a setuid binary on the sanitized PATH | PATH sanitization (reduces exposure surface); cannot fully prevent without syscall filtering | seccomp-bpf / sandbox-exec |
| **T7 — input injection in server mode** | Injects adversarial values into ND-JSON event payloads | Values flow through `step.Input`; same sandbox controls apply (env, timeout, output limit) | Server-side input validation schema (separate workstream) |

---

## 6. Migration / Opt-Out

The `CRITERIA_SHELL_LEGACY=1` environment variable disables **all** Phase 1
hardening defaults:

- Full environment inheritance is restored.
- PATH is passed through unsanitized.
- Output capture is unbounded.
- Working-directory confinement check is skipped (CWD assignment still applies).
- Timeout behavior uses the step-level `timeout` attribute or caller context;
  no hard 5-minute default is enforced (the attribute's parsed timeout is
  still respected when set in HCL).

`CRITERIA_SHELL_LEGACY=1` is a **time-boxed opt-out**, not a permanent escape
hatch. It will be removed in the `v0.3.0` release cycle (one phase after W05
lands). Operators who rely on it should migrate before the `v0.3.0` release
window, which the team will announce in the CHANGELOG at least one minor version
in advance.

### Migration checklist for existing workflows

1. **Environment variables**: audit which parent env vars your commands depend on.
   Add them explicitly via `input { env = jsonencode({VAR: "$VAR"}) }`.
2. **PATH**: if your command depends on a non-standard tool, either install it in
   a standard location or use `input { command_path = "/usr/local/mytool/bin:/usr/local/bin:/usr/bin" }`.
3. **Working directory**: if `working_directory` is set outside `$HOME`, add the
   path to `CRITERIA_SHELL_ALLOWED_PATHS` in your CI environment.
4. **Timeouts**: if a step legitimately runs longer than 5 minutes, set
   `input { timeout = "30m" }` (maximum: 1h).
5. **Large output**: if a step produces more than 4 MiB of stdout, increase the
   per-stream cap: `input { output_limit_bytes = "16777216" }` (max: 64 MiB).
