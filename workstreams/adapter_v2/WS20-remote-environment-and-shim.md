# WS20 — `remote` environment type + host phone-home shim

**Phase:** Adapter v2 · **Track:** Remote · **Owner:** Workstream executor · **Depends on:** [WS03](WS03-host-v2-wire.md), [WS07](WS07-lockfile.md), [WS09](WS09-environment-block-and-secret-taint.md), [WS19](WS19-remote-framing-heartbeats.md). · **Unblocks:** [WS21](WS21-sdk-serveremote.md), [WS22](WS22-remote-demo-runbook.md).

## Context

`README.md` D40–D44 + D44-launch + D44-reachability + D44-isolation. **Reverse phone-home model**: the adapter dials into the host. criteria contains no ECS / k8s / SSH client code. The host has a tiny shim that listens for inbound adapter connections, terminates mTLS, verifies identity against the lockfile, and presents the connection to the session layer as if it were local (via go-plugin `Reattach`).

## Prerequisites

WS03 (`LocalSocketDialer` exists), WS07 (lockfile knows endpoint pins), WS09 (`remote` environment type skeleton registered), WS19 (chunking + heartbeats).

## In scope

### Step 1 — `remote` environment block fields

In `internal/adapter/environment/remote/handler.go`, fully implement the `remote` type that WS09 stubbed. Schema:

```hcl
environment "remote" "<name>" {
  listen_address    = "0.0.0.0:7778"   # or "unix:/run/criteria-remote.sock"
  accept_token      = env("CRITERIA_REMOTE_TOKEN")    # optional bearer
  policy_mode       = "permissive" | "strict"        # advisory for remote

  mtls {
    server_cert  = "/etc/criteria/certs/server.pem"
    server_key   = "/etc/criteria/certs/server-key.pem"
    client_ca    = "/etc/criteria/certs/adapter-ca.pem"
    client_identity_pattern = "CN=criteria-adapter-.*"  # regex on cert subject
  }

  accept_digest_from = "lockfile"  # default; matches the lockfile entry for the adapter ref

  # Standard policy fields are advisory for remote (D44-isolation):
  network    { allow = [...] }       # advisory; host cannot enforce
  filesystem { read = [...]; write = [...] }   # advisory
  resources  { timeout = "10m" }     # enforced as session timeout
}
```

### Step 2 — Shim listener

`internal/adapter/environment/remote/shim.go`:

```go
type Shim struct {
    listenAddr string
    tlsConfig  *tls.Config
    acceptToken string
    digestVerifier DigestVerifier  // checks reported identity against lockfile
    sessions   map[string]*Session  // adapter ref → session (one active per ref)
}

// Start binds the listener. Called at workflow startup if any remote env
// is referenced; skipped if no remote env is referenced (compile-time fold).
func (s *Shim) Start(ctx context.Context) error

// Accept handles inbound mTLS connections, validates identity + lockfile
// digest, creates a local UDS, spawns the bridge goroutine, and produces
// a Reattach-mode Client for the session layer.
func (s *Shim) Accept(conn net.Conn) (adapter.Client, error)
```

The Accept flow:

1. Complete mTLS handshake; extract cert subject; match against `client_identity_pattern`.
2. Read the handshake message from the adapter (defined in v2 proto by WS02; carries identity: name, version, digest).
3. Verify the digest matches the lockfile entry for the adapter being requested.
4. If `accept_token` is configured, verify it.
5. Create a tmp Unix socket; spawn a bidirectional bridge goroutine: bytes from the local UDS flow to the HTTP/2 connection; bytes from HTTP/2 flow to the UDS.
6. Use `loader.LocalSocketDialer(ctx, socketPath)` (from WS03) to produce a Client.
7. Return the Client to the session layer.

### Step 3 — Disconnect & crash handling

If the inbound connection drops, the bridge goroutine closes the local UDS. The host's existing crash-policy machinery (`fail` / `respawn` / `abort_run`) handles it. **No new "remote crash" concept** — D44-rotation.

`respawn` for a remote adapter means waiting for the adapter to dial back in (with the configured timeout); the shim continues listening. If the adapter is configured to exit on disconnect (D44-rotation), it won't reconnect and respawn fails after timeout.

### Step 4 — Compile-time folding

If a workflow doesn't reference any `remote` environment, the listener isn't started. The compile pass that decides whether `Shim.Start` is invoked lives in `internal/engine/run_setup.go` (or equivalent).

### Step 5 — Tests

- Unit: shim accepts a known-good identity, rejects unknown digests, rejects bad mTLS.
- Integration: a fake "remote adapter" goroutine in the test process dials the shim; assert the session layer can call Info/Execute through the bridge transparently.
- Reconnect: kill the bridge connection mid-stream; assert crash policy kicks in; reconnect succeeds when respawn is configured.

## Out of scope

- The SDK `serveRemote` adapter-side API — WS21.
- Demo runbook + CI smoke test — WS22.
- Any ECS / k8s / SSH client code in criteria. **None.**

## Behavior change

**Yes** — a new environment type is accepted in HCL; workflows referencing it bring up a shim listener.

## Tests required

- Unit + integration tests as above.

## Exit criteria

- Workflow with a `remote` environment compiles, starts the listener, accepts a phone-home from a fake adapter, runs a step.

## Files this workstream may modify

- `internal/adapter/environment/remote/*.go` (filling in the WS09 skeleton).
- `internal/engine/run_setup.go` (or equivalent) for listener startup.
- Test fixtures under `internal/adapter/environment/remote/testdata/`.

## Files this workstream may NOT edit

- WS09 territory (taint compiler, type registry).
- The SDKs — WS21.
- Other workstream files.
