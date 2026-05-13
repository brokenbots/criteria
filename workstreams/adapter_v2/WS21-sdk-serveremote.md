# WS21 — `sdk.serveRemote(...)` across TypeScript / Python / Go SDKs

**Phase:** Adapter v2 · **Track:** Remote · **Owner:** Workstream executor · **Depends on:** [WS20](WS20-remote-environment-and-shim.md), [WS23](WS23-typescript-sdk-v2.md) (skeleton), [WS24](WS24-python-sdk-v2.md) (skeleton), [WS25](WS25-go-sdk-v1.md) (skeleton).

## Context

`README.md` D42. Each SDK adds an entrypoint alongside `serve({...})`:

```ts
serveRemote({
  host: "wss://criteria.example.com:7778",
  mtls: { client_cert, client_key, ca_bundle },
  accept_token: process.env.CRITERIA_REMOTE_TOKEN,
  identity: { name: "claude", version: "1.2.3", digest: "sha256:..." },
  // ...same handler config as serve()
});
```

This dials out to the host shim (WS20), completes the auth + identity handshake, and then serves Info / OpenSession / Execute / etc. over the held HTTP/2 mTLS connection.

## Prerequisites

- WS20 — host shim accepts inbound connections and runs the bridge.
- WS23 / WS24 / WS25 — SDK packages exist (this WS adds one function to each).

## In scope

### Step 1 — Shared design notes

The function signature is consistent across SDKs. The implementation differs by language but the wire interaction is identical:

1. Open mTLS HTTP/2 client to `host`.
2. Send the v2 handshake message (defined by WS02): identity { name, version, digest, accept_token? }.
3. Wait for the host's ack.
4. Switch to gRPC service mode on the same connection. The same `AdapterServiceServer` implementation as `serve(...)` runs.

### Step 2 — TypeScript

In `criteria-typescript-adapter-sdk` (WS23 will land the package; this WS adds the file `src/serveRemote.ts`):

```ts
import { connect } from "@grpc/grpc-js";
import { AdapterServiceService } from "./proto/adapter_grpc_pb";

export async function serveRemote(opts: ServeRemoteOptions): Promise<void> {
  const credentials = grpcChannelCredentialsFromMTLS(opts.mtls);
  const server = new Server();
  server.addService(AdapterServiceService, makeImpl(opts));
  // We act as a gRPC client that opens a stream the host treats as a
  // server connection. Use grpc-js's reverse-connection support OR
  // implement the bridge over a raw HTTP/2 client and shim.
  // ...connect, send handshake, hand off the connection to grpc-js.
}
```

The crux is reusing `@grpc/grpc-js` over a pre-opened HTTP/2 connection. If `@grpc/grpc-js` doesn't expose that hook cleanly, fall back to a custom gRPC framer (this is documented as a risk; v1 ships whichever works).

### Step 3 — Python

In `criteria-python-adapter-sdk` (WS24 will land the package), add `src/criteria_adapter_sdk/serve_remote.py`:

```python
async def serve_remote(opts: ServeRemoteOptions) -> None:
    creds = grpc.ssl_channel_credentials(...)
    # Same pattern: open an mTLS HTTP/2 connection, complete handshake,
    # then attach a grpc.aio.server to that connection. grpc.aio.Server
    # does not expose a "use this socket" API cleanly; a small custom
    # bridge connects the established TLS socket to a Unix socket the
    # gRPC server listens on. ~80 LOC.
```

### Step 4 — Go

In `criteria-go-adapter-sdk` (WS25 will land the package), add `serve_remote.go`:

```go
func ServeRemote(opts ServeRemoteOptions) error {
    conn, err := tls.Dial("tcp", opts.Host, opts.TLSConfig)
    if err != nil { return err }
    if err := sendHandshake(conn, opts.Identity, opts.AcceptToken); err != nil { return err }
    server := grpc.NewServer()
    v2.RegisterAdapterServiceServer(server, makeImpl(opts))
    return server.Serve(&singleConnListener{conn: conn})
}
```

`singleConnListener` is a small `net.Listener` shim that returns the pre-opened TLS connection on its first `Accept()` and EOF afterwards. ~30 LOC.

### Step 5 — Identity handshake

The handshake message (defined by WS02 — add it there if missed) carries `{ name, version, digest, accept_token, sdk_protocol_version }`. The host shim (WS20) reads it before letting gRPC frames flow.

### Step 6 — Tests

- Per-SDK unit tests of the handshake message build/parse.
- Per-SDK integration test (each SDK ships a small Go harness that simulates the host shim) — confirms a phone-home reaches Info() successfully.
- Cross-SDK conformance: the WS26 conformance suite is extended in WS26 to also drive `serveRemote` mode against each SDK.

### Step 7 — Documentation

Each SDK README gains a "Running as a remote adapter" section with:

- An example `serveRemote(...)` invocation.
- A k8s Deployment manifest (under `examples/k8s/`).
- A Dockerfile (under `examples/docker/`).
- A `systemd` unit (under `examples/systemd/`).

These are documentation, not infrastructure (per D44-launch). They live in the SDK starter repos (WS27) and in the SDK source repos themselves.

## Out of scope

- Host shim — WS20.
- Conformance harness extension — WS26.
- Demo runbook — WS22.

## Behavior change

**Yes — new SDK entrypoint.** Existing `serve(...)` flows unchanged.

## Tests required

- Per-SDK unit + integration tests.
- All three SDKs handshake against the shim from WS20 successfully.

## Exit criteria

- A reference example in each SDK starter repo (WS27) running as a remote adapter end-to-end.

## Files this workstream may modify

- `criteria-typescript-adapter-sdk/src/serveRemote.ts` + tests.
- `criteria-python-adapter-sdk/src/criteria_adapter_sdk/serve_remote.py` + tests.
- `criteria-go-adapter-sdk/serve_remote.go` + tests.
- Example deployments in each SDK's `examples/` directory.

## Files this workstream may NOT edit

- WS20 (host shim).
- WS23–WS25 core `serve(...)` code (modify only by adding new files).
- Other workstream files.
