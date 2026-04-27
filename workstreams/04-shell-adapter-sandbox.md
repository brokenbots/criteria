# Workstream 4 — Shell adapter sandbox plan

**Owner:** Security agent · **Depends on:** none · **Unblocks:** [W08](08-phase0-cleanup-gate.md).

## Context

The shell adapter ([internal/adapters/shell/](../internal/adapters/shell/))
runs commands declared in HCL workflows directly via `os/exec` against
the user's shell. There is no isolation — a workflow author with
write access to an HCL file gets full execution as the user running
`overseer`. This was acceptable for an internal tool used by people
who trust each other; it is not acceptable as a default for a public
release.

The split-era reviewer notes flagged shell adapter sandboxing as
deferred work (W08 reviewer, "sandbox planning / hardening for the
shell adapter"). Phase 0 is the explicit catch-up.

This workstream is **plan-and-first-pass**. It produces a written
threat model and a hardening pass that closes the most obvious
defaults; it does not need to deliver a perfect sandbox in one go.

## Prerequisites

- `make build`, `make test` green on `main`.
- Existing shell adapter tests pass and exercise the failure modes
  enough that a hardening change has signal.

## In scope

### Step 1 — Threat model

Author **`docs/security/shell-adapter-threat-model.md`**:

- Who is trusted (HCL author, plugin author, CLI runner, network).
- What an attacker controls (the HCL file content; potentially env;
  potentially CWD).
- Goals (preserve confidentiality of files outside the workflow;
  avoid privilege escalation; prevent network egress unless
  explicitly granted; bound resource usage).
- Threats explicitly out of scope (full VM-level isolation; running
  untrusted compiled binaries as if from the network; defeating a
  motivated attacker with root).

The model lives in `docs/security/`; this is the first file there.

### Step 2 — First-pass hardening

Implement the **defaults that are cheap and high-value**:

- Run with a clean / allow-listed environment (drop secrets-bearing
  vars unless the HCL declares them).
- Ban relative `command` paths unless explicitly allowed; require
  absolute paths or a documented PATH allowlist.
- Hard timeout on every shell step (default 5 minutes; HCL-overridable
  with bounds).
- Capture stdout/stderr to bounded buffers (no unbounded memory).
- A clear error when shell adapter is invoked from an HCL file that
  doesn't declare `shell` in some allow-list mechanism (deferred
  hard-stop opt-in if needed; at minimum a warning today).

Anything platform-specific (`sandbox-exec` on macOS, seccomp /
namespaces on Linux, Job Objects on Windows) is **out of this
workstream's scope**. Document it in the threat-model file as the
next logical step; do not implement.

### Step 3 — Tests

Each hardening default gets a focused test:

- Env-allow-list test: a workflow that expects `$SECRET` set in the
  parent process does not see it unless the HCL declared it.
- Path test: a relative `command = "rm"` fails with a clear error.
- Timeout test: a workflow with a `sleep 10` and a 1s timeout
  terminates and returns a clear failure event.
- Output bounds test: a workflow that emits 100MB of stdout fails
  cleanly without OOM-ing the host.

### Step 4 — Migration / opt-out

Document an `OVERSEER_SHELL_LEGACY=1` env var that restores the old
behavior for any internal user who depends on the un-hardened path,
with a clear deprecation timeline (e.g., "removed in v0.2.0").
Coordinate with the overlord team — paste the env-var name into the
overlord-side runbook.

## Out of scope

- Platform-specific sandboxes (macOS `sandbox-exec`, Linux
  namespaces/seccomp, Windows Job Objects). Plan in the
  threat-model doc; implement in a later phase.
- Filesystem isolation (chroot / overlayfs). Same.
- Network egress controls. Same.
- A cgroup-based resource budget. Same.
- Hardening any other adapter (Copilot, MCP). Different threat
  models, different work.

## Files this workstream may modify

- `internal/adapters/shell/*.go`
- `internal/adapters/shell/*_test.go`
- `docs/security/shell-adapter-threat-model.md` (new)
- `docs/security/README.md` (new — short index)

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
or other workstream files. If the security work needs CHANGELOG
entries or release-note coordination, defer to [W08](08-phase0-cleanup-gate.md).

## Tasks

- [ ] Author the threat-model doc.
- [ ] Implement the cheap defaults from Step 2.
- [ ] Add the four tests from Step 3.
- [ ] Document the legacy opt-out env var.
- [ ] Reviewer notes capture which defaults were applied vs deferred.

## Exit criteria

- `docs/security/shell-adapter-threat-model.md` exists and is
  reviewed by a human.
- Every default from Step 2 is implemented with a corresponding
  test from Step 3.
- `make test` and `make validate` green.
- The legacy opt-out is documented in the threat model and (if
  needed) `docs/plugins.md` or the new threat-model doc itself.
- The CLI smoke (`./bin/overseer apply examples/hello.hcl`) still
  exits 0 — `examples/hello.hcl` should run fine under the new
  defaults; if it doesn't, fix the example or the default before
  declaring exit.

## Tests

Listed in Step 3. All four must run in `make test` and gate CI.

## Risks

| Risk | Mitigation |
|---|---|
| Hardening breaks an existing internal user's workflow | The legacy opt-out env var preserves the old path; document it loudly in the threat-model doc and notify the overlord team in the PR description. |
| Threat model is too narrow and a real attacker class is missed | Accept; the threat model is an iterative document. Phase 0 ships v1 of it; later phases revise. |
| Cheap defaults leak into platform-specific code paths that aren't tested on all OSes | Keep all OS-conditional code in a single helper; test what's in the helper, even if some paths are no-op on a given OS. |
| Bounded output buffer truncates a legitimate large-output workflow | Make the bound configurable from HCL with a sensible upper limit; document in `docs/workflow.md`. |
