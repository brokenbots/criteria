# WS10 — Linux sandbox: in-process namespaces + landlock + seccomp (pure Go, no cgo)

**Phase:** Adapter v2 · **Track:** Security · **Owner:** Workstream executor · **Depends on:** [WS09](WS09-environment-block-and-secret-taint.md). · **Unblocks:** [WS40](WS40-v2-release-gate.md) verification gate 1.

## Context

`README.md` D28–D31. The criteria host applies isolation in-process before exec'ing the adapter binary. The constraint: **no cgo anywhere in the criteria core binary** (D28), and **single static binary** (D29). Approach:

- Namespaces via `syscall.SysProcAttr.Cloneflags`: `CLONE_NEWUSER | CLONE_NEWNS | CLONE_NEWPID | CLONE_NEWNET | CLONE_NEWIPC | CLONE_NEWUTS`.
- Landlock via `github.com/landlock-lsm/go-landlock` (syscall-based, no cgo).
- Seccomp via `github.com/elastic/go-seccomp-bpf` (pure Go BPF compiler — no cgo, no libseccomp).
- Bubblewrap (`bwrap`) as a soft optional dependency (D30): used when present and opted-in via the environment block.

Capability degradation: missing primitives are logged; strict mode fails closed (D31).

## Prerequisites

WS09 merged — the `sandbox` environment type handler skeleton exists and parses fields.

## In scope

### Step 1 — Linux sandbox handler

`internal/adapter/environment/sandbox/linux.go` (build tag `//go:build linux`):

```go
type LinuxPrepared struct {
    SysProcAttr *syscall.SysProcAttr
    Landlock    *landlock.Config
    SeccompBPF  *seccomp.Filter
    PostSpawn   func(pid int) error  // optional: attach cgroup limits, etc.
}

func (h *Handler) prepareLinux(ctx PrepareContext) (LinuxPrepared, error)
```

The function consumes the `ResolvedPolicy` from WS09 (filesystem reach, network allow list, resource limits, policy_mode) and produces:

- A `SysProcAttr` with the appropriate `Cloneflags` and UID/GID mappings for user-namespace mode.
- A landlock config rooted at `filesystem.read` / `filesystem.write` paths.
- A seccomp filter using a default-deny allow-list approach; the base allowlist covers what go-plugin'd adapters need (file ops on permitted paths, network ops on permitted endpoints, basic IPC syscalls).

### Step 2 — Resource limits

Apply `setrlimit` for CPU/memory/timeout via `syscall.Setrlimit` from a post-fork hook (the child process inherits limits). Cgroups v2 support (preferred where available) via writing to `/sys/fs/cgroup/...` — leave that as an optional path enabled when the user explicitly requests cgroup limits (a `resources.cgroup = true` flag on the environment block).

### Step 3 — Bubblewrap soft alternative

`internal/adapter/environment/sandbox/bubblewrap.go`:

```go
// MaybeUseBubblewrap inspects the environment and host. If
// bwrap is on PATH and the environment opts in
// (environment.sandbox = "bwrap"), this returns a command wrapper
// that exec's `bwrap` with the appropriate args, replacing the in-process
// namespace setup. Returns nil if not applicable.
func MaybeUseBubblewrap(prep LinuxPrepared, env *workflow.EnvironmentNode) *exec.Cmd
```

Translation of policy fields to `bwrap` flags is captured in a small table — documented in `docs/adapters.md` (WS39). Bubblewrap path never required; absence is fine.

### Step 4 — Capability detection

`internal/adapter/environment/sandbox/probe.go`:

```go
// Probe checks the host kernel for sandbox primitive support. Cached
// per process. Results affect what's logged at session open in
// permissive mode and what's accepted in strict mode.
func Probe() Capabilities

type Capabilities struct {
    UserNamespaces  bool
    Landlock        bool
    Seccomp         bool
    Cgroupv2        bool
    Bubblewrap      bool   // bwrap on PATH
}
```

### Step 5 — Loader integration

In `internal/adapter/loader.go`, when launching an adapter bound to a `sandbox`-type environment:

1. Call `sandbox.Handler.Prepare(...)` → `LinuxPrepared`.
2. Configure the `exec.Cmd` with `SysProcAttr`, env-var scrub, `Cwd`.
3. Fork+exec.
4. In a `PostSpawn` step (parent side, after fork), apply landlock + seccomp via the new process's `pidfd` mechanism *or* (simpler) the child sets them up itself in a pre-exec hook (we ship a tiny shim invoked before the real adapter is exec'd — but per D29 we want pure in-process; settle on the parent-side `pidfd` ptrace approach).

Note: applying seccomp from outside the target process requires either a pre-exec hook in the child (cleanest, but means we run a tiny Go shim before the real binary) OR using `pidfd_send_signal` patterns from very recent kernels. **Chosen approach**: a `prctl(PR_SET_NO_NEW_PRIVS)` + landlock/seccomp setup in `os.StartProcess`'s pre-exec callback path (set via the experimental `syscall.SysProcAttr.AmbientCaps`-adjacent mechanism in newer Go versions, or via a `runtime.LockOSThread()` + manual `clone3()` syscall pattern). Document the chosen path in a leading comment.

### Step 6 — Tests (Linux-only)

- `linux_test.go` (build tag `//go:build linux`):
  - Unit-test the field-to-`SysProcAttr` conversion table.
  - Integration test: launch a tiny test binary that attempts to open `/etc/passwd`, connect to `8.8.8.8:53`, and `setuid(0)` — assert each fails when the corresponding policy is set.
  - Probe tests on a docker container with various capabilities masked.
- macOS (build-tag-excluded) sees only the skeleton from WS09.

## Out of scope

- macOS sandbox-exec — WS11.
- Container runtime — WS12.
- Windows — out of project scope (D3).

## Reuse pointers

- `github.com/landlock-lsm/go-landlock` (pure Go).
- `github.com/elastic/go-seccomp-bpf` (pure Go BPF).
- `internal/adapter/loader.go` (host loader from WS03).
- WS09's `ResolvedPolicy` and `Handler` interface.

## Behavior change

**Yes** — adapters bound to a `sandbox`-type environment on Linux now run inside namespaces with landlock + seccomp. Failure to apply any primitive in `policy_mode = "strict"` aborts the session with a clear error; in `permissive` mode, a degradation log is emitted and the session continues.

## Tests required

- All Linux tests pass on CI runners with kernel >= 5.13 (landlock) and unprivileged user-namespace support.
- A degradation test simulates missing landlock; permissive mode logs + continues; strict mode aborts.

## Exit criteria

- `internal/adapter/environment/sandbox/linux.go` complete; tests green.
- Integration test on a docker host: prohibited operations fail with expected errors.

## Files this workstream may modify

- `internal/adapter/environment/sandbox/linux.go`, `bubblewrap.go`, `probe.go` *(new)*.
- `internal/adapter/loader.go` — wire the prepare/spawn hooks.
- Test fixtures.

## Files this workstream may NOT edit

- `internal/adapter/environment/sandbox/darwin.go` — WS11.
- WS09 territory (schema, taint).
- Other workstream files.
