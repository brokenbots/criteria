# peer-remote: peer-mode remote adapters with per-scope sessions

This example is the end-to-end demonstration of **peer-mode remote adapters**
(ADR-0007): two adapter peers dial into a criteria host over the phone-home
shim, and the workflow runs under **per-scope sessions** — every adapter
instance gets its own rotated accept token and scope key, so no peer can
present another scope's credentials.

It is also the acceptance proof for the crash-facts initiative: killing the
peer's adapter child with `kill -9` makes the host log the **precise**
`ProcessExited{signal:9}` fact plus the peer's `CrashClassified` diagnosis —
instead of guessing from a closed transport.

```
                ┌──────────────────────────────┐
                │  criteria-host               │
                │  criteria apply workflow.hcl │
                │  remote shim  0.0.0.0:7778   │
                └──────────▲─────────▲───────────┘
                           │ phone-home (gRPC)
              ┌────────────┴───┐   ┌───┴────────────┐
              │ shell-peer     │   │ noop-peer      │
              │ criteria peer  │   │ criteria peer  │
              │  └─ shell      │   │  └─ noop       │
              │     adapter    │   │    adapter     │
              │     (child)    │   │    (child)     │
              └────────────────┘   └────────────────┘
```

## Topology

- **criteria-host** runs `criteria apply` on `workflow.hcl`. The workflow
  declares a remote environment with `per_scope_sessions = true`; the host
  rotates one accept token per adapter instance per scope and serves the
  phone-home shim on `0.0.0.0:7778`.
- **shell-peer** and **noop-peer** are built from
  [images/remote-adapters/Dockerfile.peer](../../images/remote-adapters/Dockerfile.peer).
  Each packages the criteria engine itself with the entrypoint `criteria
  peer`: the peer supervisor launches the adapter as a local go-plugin child,
  serves the full v2 AdapterService contract plus PeerService supervision
  (Supervise / Control) on the phone-home connection, and keeps the child
  alive across host disconnects with a bounded journal that replays crash
  evidence after reconnects.

Each peer's entrypoint acts as the **provisioning operator**: it waits for
the host to rotate a per-scope accept token (written under
`$CRITERIA_HOME/runs/<run_id>/remote-tokens/…` and announced via the
`provision_wanted` lifecycle event), then exports
`CRITERIA_REMOTE_HOST/TOKEN/SCOPE/DIGEST` and execs `criteria peer`. The peer
retries its connection with full-jitter backoff, so no health gate is needed
for startup ordering.

## Bring-up

```sh
cd examples/peer-remote
docker compose build
docker compose up
```

The workflow then runs: `greet` → `deliberate_failure` → `recover` →
`crash_demo` → `isolation_gate` → `done`. See
[workflow.hcl](./workflow.hcl) for the step-by-step narrative; the failure
path is **routed**, not a crash — the `deliberate_failure` step makes the
shell adapter exit 3 and the workflow routes that outcome into `recover`,
which completes successfully.

## What to observe in the host's structured logs

`docker compose logs criteria-host` (JSON lines, slog):

- `provision_wanted` lifecycle events, one per adapter instance, each
  carrying the scope name, scope instance id, rotated token, and shim
  address — the operator contract documented in
  [docs/adapter-remote-deployment.md](../../docs/adapter-remote-deployment.md).
- Step execution flowing through the peers: step outcomes
  (`failure` routed from `deliberate_failure`), and the run completing in
  `done`.
- `peer adapter process exited` warnings when a peer's adapter child dies —
  see the kill -9 demo below for the exact lines.

## What to observe in the peers' structured logs

`docker compose logs shell-peer noop-peer` (JSON lines, slog):

- `peer child spawned` — the peer supervisor launched the adapter child
  (adapter type, binary path, digest, pid, scope).
- `peer ready` — the supervisor is up.
- `peer phone-home connected` — the dial to the host's shim succeeded
  (host, scope, digest).
- On a crash (see the demo below): `ERROR adapter child exited unexpectedly`
  with the supervision journal's classification.

## The demo: kill -9 the peer's adapter child (the acceptance proof)

While the `crash_demo` step is sleeping (`sleep 300`), SIGKILL the shell
peer's **adapter child** — the go-plugin child process, not the `criteria
peer` supervisor itself:

```sh
# find the adapter child inside the shell-peer container
docker compose exec shell-peer sh -c \
  'ps -o pid,args | grep criteria-adapter-shell'
docker compose exec shell-peer kill -9 <child pid>
```

What the two sides log — this is the acceptance proof for the whole
crash-facts initiative:

**Peer** (`docker compose logs shell-peer`), from the child-side supervision
journal:

```
{"level":"ERROR","msg":"adapter child exited unexpectedly","reason":"adapter process terminated","exit_code":-1,"signal":9,...}
```

**Host** (`docker compose logs criteria-host`), from the peer's replayed
supervision journal:

```
{"level":"WARN","msg":"peer adapter process exited","adapter":"shell","scope":"<scope-key>","reason":"process_exited","detail":"exit_code=-1 signal=9 idle_ms=<n>"}
{"level":"WARN","msg":"peer adapter process exited","adapter":"shell","scope":"<scope-key>","reason":"adapter process terminated","detail":"adapter child exited while supervised (exit code -1, signal 9)"}
```

The first line is the peer's plain `Exited` journal record arriving first
(exit code, signal, idle time — the precise `ProcessExited{signal:9}` fact).
The second line is the `CrashClassified` record overwriting the placeholder:
the peer watched the child directly, so its classification
(`adapter process terminated`) outranks every host-side heuristic. Before
this initiative the host could only guess `"transport closed"` from the
dropped connection; now the diagnosis on the host is the peer's verbatim wire
fact.

The workflow's `on_crash = "respawn"` policy then re-opens the shell
session: the host rotates a **new** scope instance and emits a fresh
`provision_wanted`. Restart the shell peer so its entrypoint dials with the
newly rotated token (the entrypoint picks the newest token file, skipping
the released prior scope):

```sh
docker compose restart shell-peer
```

The retry of `crash_demo` runs against the respawned session and the
workflow completes into `done`. The deliberate-failure and recovery steps
already succeeded before the crash, so the run output reports
`peer-remote-demo complete`.

## Per-scope isolation

With `per_scope_sessions = true`, each adapter instance (shell.main, noop.gate)
runs in its own scope: its own scope instance id, its own rotated token, its
own `/<scope-instance-id>` (or `<scope-name>/<scope-instance-id>`) scope key.
Observe in the logs:

- two distinct `provision_wanted` events (one per adapter type), each with a
  different scope instance id and token;
- each peer dialing with its own scope key — a peer presenting another
  scope's key or token is rejected by the shim.

## Notes and constraints

- **wait/approval nodes are not exercised here.** `wait { signal = ... }` and
  `approval { ... }` nodes require a server-compatible orchestrator
  (`criteria apply --server ...`); a local-only run rejects those node kinds
  with a clear error. This example demonstrates remote *adapters*, not
  server-mode run control.
- **Digest pinning.** The committed `.criteria.lock.hcl` pins the real OCI
  digests (and keyless signatures) for the two adapters, as produced by
  `criteria adapter lock`. The compose peer entrypoints read their pinned
  digest from the mounted lockfile and present it on dial; the host's digest
  gate is fail-closed and rejects any dial whose presented digest does not
  match the pin. Set `PEER_DIGEST` to override (e.g. when the operator
  injects the digest from the `provision_wanted` lifecycle event); see
  [docs/adapter-remote-deployment.md](../../docs/adapter-remote-deployment.md).
- **No Docker? Run the same building blocks as tests.** The smoke tests in
  [internal/ci/smoke/peer_example_test.go](../../internal/ci/smoke/peer_example_test.go)
  drive real `criteria peer` subprocesses against a real shim without
  Docker, including the kill -9 crash-fidelity assertion. Run them with:

  ```sh
  CRITERIA_PEER_E2E=1 go test ./internal/ci/smoke/ -run TestPeerSmoke -v
  ```