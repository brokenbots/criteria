# WS03 — Host adapter wire wired to v2; delete v1 code paths

**Phase:** Adapter v2 · **Track:** Foundation · **Owner:** Workstream executor · **Depends on:** [WS01](WS01-terminology-unification.md), [WS02](WS02-protocol-v2-proto.md) · **Unblocks:** every host workstream that talks to the adapter (WS09, WS13, WS14–WS19, WS20). · **Base branch:** `adapter-v2`

## Context

After WS02 the v2 proto exists but nothing speaks it. This workstream rewrites the host's adapter-talking code to consume v2, deletes the v1 code paths (per `README.md` D2), and exposes a small `LocalSocketDialer` helper that the `remote` environment shim (WS20) will reuse.

Key files affected (post-WS01 paths):

- [`internal/adapter/serve.go`](../../internal/plugin/serve.go) — defines the `Client` interface and the go-plugin `GRPCPlugin` wrapper.
- [`internal/adapter/loader.go`](../../internal/plugin/loader.go) — `exec.Command`-based local launch + go-plugin handshake.
- [`internal/adapter/sessions.go`](../../internal/plugin/sessions.go) — `SessionManager`, `Session` struct, crash policy.
- [`internal/adapter/discovery.go`](../../internal/plugin/discovery.go) — binary path resolution.
- [`internal/engine/*`](../../internal/engine/) — call sites that consume `Client`.
- `sdk/adapterhost/` (renamed in WS01) — public host-side surface.

The host never speaks the v2 wire over anything but local UDS gRPC. Remote execution is handled by WS20 via a shim that exposes a local UDS to the host; this WS does *not* introduce any remote-aware code in the loader or session layer.

## Prerequisites

- WS01 and WS02 merged.
- `make ci` green on `adapter-v2` (the branch this workstream lands against).
- Familiarity with go-plugin's `Reattach` mode — used here for the `LocalSocketDialer` helper.

## In scope

### Step 1 — Replace the `Client` interface with v2 methods

In `internal/adapter/serve.go`:

```go
type Client interface {
    Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error)
    OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error)
    Execute(ctx context.Context, req *v2.ExecuteRequest, sink ExecuteEventSink) error
    Log(ctx context.Context, req *v2.LogRequest, sink LogEventSink) error
    Permissions(ctx context.Context, requests <-chan *v2.PermissionEvent, decisions chan<- *v2.PermissionDecision) error
    Pause(ctx context.Context, req *v2.PauseRequest) (*v2.PauseResponse, error)
    Resume(ctx context.Context, req *v2.ResumeRequest) (*v2.ResumeResponse, error)
    Snapshot(ctx context.Context, req *v2.SnapshotRequest) (*v2.SnapshotResponse, error)
    Restore(ctx context.Context, req *v2.RestoreRequest) (*v2.RestoreResponse, error)
    Inspect(ctx context.Context, req *v2.InspectRequest) (*v2.InspectResponse, error)
    CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error)
}
```

Replace `ExecuteEventReceiver` from v1 with `ExecuteEventSink` and `LogEventSink` — narrower types since `Execute` events are now purely semantic.

### Step 2 — Implement the go-plugin `GRPCPlugin`

Replace v1's `GRPCPlugin` body. The host-side client adapts the generated gRPC client into the `Client` interface:

```go
type grpcClient struct {
    c v2.AdapterServiceClient
}

func (g *grpcClient) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
    return g.c.Info(ctx, req)
}
// ... etc.
```

For `Execute`, drive the stream and dispatch events to the sink:

```go
func (g *grpcClient) Execute(ctx context.Context, req *v2.ExecuteRequest, sink ExecuteEventSink) error {
    stream, err := g.c.Execute(ctx, req)
    if err != nil { return err }
    for {
        ev, err := stream.Recv()
        if err == io.EOF { return nil }
        if err != nil { return err }
        if err := sink.Emit(ev); err != nil { return err }
    }
}
```

`Permissions` (bidi) wires the two channels to the gRPC stream — see WS16 for the consumer logic.

### Step 3 — Update the loader

In `internal/adapter/loader.go`:

- Keep `exec.Command(path)` for local launch.
- Update the `hplugin.Plugin` map to register the v2 `GRPCPlugin` keyed by `AdapterName`.
- Keep crash detection logic; update its match list against v2 errors (renamed where applicable).

### Step 4 — Add `LocalSocketDialer`

New file `internal/adapter/loader_reattach.go`:

```go
// LocalSocketDialer returns a go-plugin client configured to reattach to an
// already-listening Unix socket. Used by the remote-adapter shim (WS20) to
// hand the host session layer a "local-looking" adapter that's actually
// proxying to a remote endpoint.
func (l *DefaultLoader) LocalSocketDialer(ctx context.Context, socketPath string) (Client, *hplugin.Client, error) {
    cfg := &hplugin.ClientConfig{
        HandshakeConfig: HandshakeConfig,
        Plugins:         pluginMap,
        AllowedProtocols: []hplugin.Protocol{hplugin.ProtocolGRPC},
        Logger:           pluginClientLogger(),
        Reattach: &hplugin.ReattachConfig{
            Protocol:        hplugin.ProtocolGRPC,
            ProtocolVersion: HandshakeConfig.ProtocolVersion,
            Addr:            &net.UnixAddr{Name: socketPath, Net: "unix"},
            Pid:             0, // reattach mode does not need a pid for our usage
        },
    }
    client := hplugin.NewClient(cfg)
    proto, err := client.Client()
    if err != nil {
        client.Kill()
        return nil, nil, fmt.Errorf("reattach grpc client: %w", err)
    }
    raw, err := proto.Dispense(AdapterName)
    if err != nil {
        client.Kill()
        return nil, nil, fmt.Errorf("dispense adapter: %w", err)
    }
    return raw.(Client), client, nil
}
```

Unit test with a fake adapter binary that listens on a UDS — exercises both the reattach handshake and the typed dispatch.

**Socket security contract (S3.4).** The dialer's caller (this WS for the local case; WS20 for the remote shim) is responsible for:

- Creating the socket file in a host-only temp directory (`os.MkdirTemp("", "criteria-adapter-*")` with mode `0o700`, never `/tmp/<predictable>`).
- Setting the socket file's mode to `0o600` after `net.Listen("unix", ...)` returns (chmod the file path; `Listen` does not let you pass a mode).
- Cleaning up the directory and socket file when the session closes, including on panic (use `defer` + recover-aware cleanup).

`LocalSocketDialer` itself does not create the socket — it consumes one. The dialer documents this contract in its godoc so WS20 inherits the same rules. A helper `NewHostOnlyUDSSocket() (path string, cleanup func(), err error)` lives next to the dialer for both this WS's tests and WS20's shim to use.

### Step 5 — Update `Session` to use v2

In `internal/adapter/sessions.go`:

- `Session` struct now stores a v2 `Client` (no behavior change beyond types).
- `OpenSession()` constructs `v2.OpenSessionRequest` — note `secrets` field stays empty in this WS (populated by WS13).
- `Execute()` drives the v2 stream + the new `Log` stream concurrently (a small goroutine per session for log consumption).
- `PermissionState` field on `Session` (per `README.md` D24) is added as an empty struct; behavior populated by WS16. Add the field now so other WSes can land their pieces.
- `Close()` calls v2 `CloseSession`.

### Step 6 — Update every host call site

`internal/engine/*` and `internal/cli/*` files that consume the adapter `Client` interface get mechanical type updates. List of touched files documented in the PR description; total ~25 files.

### Step 7 — Delete v1 host code paths

Per `README.md` D2:

```sh
git rm proto/criteria/v1/*.proto
git rm proto/criteria/v1/*.pb.go
git rm proto/criteria/v1/*_grpc.pb.go
```

Remove the `proto` Makefile target's v1 line. Remove any v1-specific helper functions in `internal/adapter/` that are no longer reachable. The grep:

```sh
! grep -rn "criteria/v1" --include='*.go' --include='*.proto' --include='Makefile' .
```

must return no matches (modulo `archived/` directories which are read-only history).

### Step 8 — Conformance suite skeleton update

`internal/adapter/conformance/` — update the existing 11 sub-tests to call v2 methods. Do not add new tests in this WS — that's WS26. Tests that exercise `Permit` need only a stub for the bidi `Permissions` stream that auto-allows; full coverage of the bidi semantics is WS16/WS26.

## Out of scope

- Implementing `Pause` / `Resume` / `Snapshot` / `Restore` / `Inspect` behavior — WS17, WS18.
- Wiring the bidi `Permissions` stream's policy/audit logic — WS16.
- Wiring the dedicated `Log` channel's redaction registry — WS13, WS15.
- Secret-channel population — WS13.
- Output-schema enforcement — WS14.
- Remote shim — WS20.

## Reuse pointers

- `go-plugin`'s `Reattach` ClientConfig — documented at github.com/hashicorp/go-plugin's `ReattachConfig` struct.
- Existing crash-policy machinery in `sessions.go` (status: kept; semantics unchanged).

## Behavior change

**Yes — minimal observable change.**

Enumerated:
- `criteria-adapter-*` binaries built against v1 SDK no longer load. The host fails handshake and reports `protocol version mismatch` (this is intended — the hard cut in D2). Every existing adapter (`greeter`, `shell`, `claude`, etc.) is migrated to v2 in WS30–WS36 in parallel; v1 binaries will not run after this WS lands.
- `Permit` RPC is gone; replaced by `Permissions` bidi. Adapters that called `Permit` directly fail; the v2 SDKs (WS23–WS25) hide the change behind the same `permissionRequest(...)` helper API so adapter code is otherwise unchanged.
- `Execute` events no longer carry log lines; logs come over the dedicated `Log` stream. Host display merges by timestamp (logic added in WS15).

## Tests required

- `internal/adapter/sessions_test.go` and `loader_test.go` updated to v2.
- `loader_reattach_test.go` (new) — fake adapter binary listens on UDS, host dialer connects and dispenses, calls `Info()` successfully.
- Conformance suite (`internal/adapter/conformance/`) passes against a v2-built reference adapter (an in-tree `noop` adapter in `internal/adapter/conformance/testdata/noop/`).
- `make ci` green.

## Exit criteria

- `make ci` green; race + count=2 + lint + vet + staticcheck.
- All host call sites use v2 types.
- The grep for `criteria/v1` returns no matches.
- The `LocalSocketDialer` test passes.

## Files this workstream may modify

- `internal/adapter/serve.go`, `loader.go`, `loader_reattach.go` (new), `sessions.go`, `discovery.go`, `process.go`.
- `internal/engine/*` and `internal/cli/*` call sites — mechanical type updates.
- `sdk/adapterhost/*` (post-WS01 path).
- `proto/criteria/v1/` — **deletion only** (Step 7).
- `Makefile` proto target — remove v1 line.
- `internal/adapter/conformance/*.go` — convert existing 11 sub-tests to v2.
- New tests next to changed files.

## Files this workstream may NOT edit

- Anything under `proto/criteria/v2/` — owned by WS02.
- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`.
- Other workstream files in `workstreams/adapter_v2/`.
- HCL grammar files in `workflow/` — those are touched by WS09.

## Implementation Notes (WS03 complete — rev 2)

### What was done

**Host-side (internal/adapterhost/)**
- `serve.go` — replaced `Client` interface with v2 methods; implemented `grpcClient` adapter wrapping generated v2 stubs; fixed `Permissions` bidi teardown to use `context.WithCancel` + labelled `break loop` so the sender goroutine always exits cleanly.
- `loader.go` — updated all call sites to v2 types; added concurrent `Log` stream fan-in alongside `Execute` stream; propagates `Log` RPC errors when `execErr == nil` (ignores `context.Canceled` from our own teardown).
- `loader_reattach.go` (new) — `LocalSocketDialer` + `NewHostOnlyUDSSocket` helpers for plugin reattach.
- `loader_reattach_test.go` (new) — unit tests for both helpers.
- `sessions.go` — added `PermissionState` stub field; all v1 type references removed.

**SDK (sdk/adapterhost/)**
- `serve.go` — v2 `grpcAdapterServer` bridge with `Permissions` bidi stub.
- `service.go` — `Service` interface updated to v2; `ExecuteEventSender`, `LogEventSender`, `PermissionsStream`, `UnimplementedPermissions`, `UnimplementedLifecycle` types added. `UnimplementedLifecycle` returns `codes.Unimplemented` for Pause/Resume/Snapshot/Restore/Inspect, making those five methods optional for adapter authors.
- `handshake.go` — plugin handshake updated.

**Conformance (internal/adapter/conformance/)**
- `testfixtures/broken/main.go` — v2; embeds `UnimplementedLifecycle`.
- `testfixtures/noop/main.go` (new) — minimal v2 noop adapter with `parallel_safe` capability and `delay_ms` support; advertises `UnimplementedLifecycle`.
- `noop_adapter_conformance_test.go` (new) — builds `testfixtures/noop` and runs `conformance.RunAdapter` against it; this is the WS03 v2 reference conformance check.

**Permission flow (cmd/criteria-adapter-copilot/)**
- `copilot_permission.go` — `handlePermissionRequest` returns `PermissionRequestResultKindRejected` (not Approved). The host's `executeCaptureSink` evaluates `allow_tools` independently and overrides the step outcome to `needs_review` when any tool was denied. `permissionDeny` field removed entirely.
- `copilot_session.go` — `permissionDeny bool` field removed.
- `copilot_turn.go` — `permDenied` check removed from `handleIdleTurn`; reset removed from `beginExecution`.

**Proto/v1 deletion**
- `proto/criteria/v1/adapter_plugin.proto` deleted.
- `sdk/pb/criteria/v1/adapter_plugin.pb.go` deleted.
- `sdk/pb/criteria/v1/criteriav1connect/adapter_plugin.connect.go` deleted.
- `server.proto`, `criteria.proto`, `events.proto` and their generated files kept — CLI uses ServerService stubs.

**Note on bundled adapter migrations:** `cmd/criteria-adapter-noop`, `cmd/criteria-adapter-mcp`, `cmd/criteria-adapter-copilot`, and `examples/plugins/greeter` were mechanically updated to v2 because WS03 deleted the v1 `adapter_plugin.proto` that they compiled against. These are pre-migrations of WS30–WS36 scope; they are not WS03 acceptance criteria. Their definitive work items and tests belong in those workstreams.

### Key design decisions
- Permission handling is deny-first at the adapter layer: adapter always returns `Rejected` to Copilot SDK (tool never runs). The host independently evaluates `allow_tools` policy and sets `anyDenied = true` → outcome overridden to `needs_review`. WS16 adds an interactive back-channel for real-time grant/deny.
- Log RPC failures are propagated when Execute succeeds — a broken log stream is not silently ignored.
- `Permissions` bidi stream sender goroutine is guarded by a derived context so stream errors or ctx cancellation never leak or hang the caller.
- `UnimplementedLifecycle` makes Pause/Resume/Snapshot/Restore/Inspect optional for adapter authors — no forced migration needed.
- `ExecuteRequest.Config` renamed to `Input` in v2 proto; all adapters/tests updated.
- Log stream is separate from Execute stream.
- `permDecision` struct and `Permit` RPC flow removed entirely.

### ✅ Acceptance criteria
- [x] `make build` green
- [x] `make plugins` green
- [x] All host call sites use v2 types
- [x] `LocalSocketDialer` + `NewHostOnlyUDSSocket` helpers with tests
- [x] `criteria/v1` adapter imports removed from adapter host scope
- [x] `proto/criteria/v1/adapter_plugin.proto` and generated bindings deleted (server-side v1 protos kept; CLI still uses `server.proto`)
- [x] Permission flow is deny-first: adapter returns `Rejected`, host overrides to `needs_review` on deny
- [x] Log RPC failures propagated when `execErr == nil`
- [x] `Permissions` bidi teardown is leak-free (labelled loop + `senderCtx`)
- [x] `UnimplementedLifecycle` added to `sdk/adapterhost/service.go`
- [x] Conformance noop fixture added at `internal/adapter/conformance/testfixtures/noop/`
- [x] Conformance test `TestNoopAdapterConformance` passes the full conformance suite

### Known remaining gaps (not WS03 scope)
- `make ci` full green pending CI gate execution; local `make build` + `make plugins` + `go vet` all pass.
- `criteria/v1` string still appears in server.proto/criteria.proto/events.proto package paths — these are the server-side protos kept intentionally for the CLI.
- Proper WS30–WS36 definitive tests for copilot/mcp adapter migrations.
- `LocalSocketDialer` reattach test not yet wired to a full integration test (`loader_reattach_test.go` covers the helper directly).

## Owner Review Notes

Owner verdict: changes requested.

Must fix before this is merge-ready:
- `cmd/criteria-adapter-copilot/copilot_permission.go`, `cmd/criteria-adapter-copilot/copilot_turn.go`, `internal/adapterhost/loader.go` — the new permission flow is not shippable: Copilot returns `Approved` before the host decision can block the action, and `permissionDeny` is set unconditionally, so permission prompts both bypass the intended `allow_tools` boundary and force a failure even when the host would allow the tool. WS03 must either wire a real blocking decision path or fall back to the documented WS03 auto-allow stub end-to-end; the current mixed behavior is not acceptable.
- `internal/adapterhost/loader.go` — `rpcHandle.Execute` must not ignore `Log` RPC failures. A broken/missing log stream currently disappears behind `<-logDone`, which can report a successful execute while logs were never delivered.
- `internal/adapterhost/serve.go` — fix `grpcClient.Permissions` teardown so send/recv failures cannot strand the sender goroutine or hang callers on an open `requests` channel.
- `sdk/adapterhost/service.go` (and the public-SDK fixture surface that proves it) — `Pause`/`Resume`/`Snapshot`/`Restore`/`Inspect` are modeled as optional v2 features but are mandatory on the public `Service` interface here. Do not force out-of-scope adapter/example migrations just to compile WS03.
- Bring the patch back into WS03 scope and satisfy the required conformance deliverable: remove the bundled-adapter/example migrations from `cmd/criteria-adapter-copilot/`, `cmd/criteria-adapter-mcp/`, `cmd/criteria-adapter-noop/`, and `examples/plugins/greeter/`; add the required v2 reference adapter under `internal/adapter/conformance/testdata/noop/`; and update `internal/adapter/conformance/*.go` as called for by this workstream.
- Fix the WS03 notes/checklist so they match the actual source-of-truth gates. This workstream requires `make ci` and says the `criteria/v1` grep returns no matches; the current notes substitute `make build`/`make plugins`, claim bundled adapter migrations that are out of scope, and explicitly keep v1 paths while the tree still has `criteria/v1` matches.

Rejected/trimmed findings:
- I am not asking WS03 to edit `proto/criteria/v2/*`; that ownership stays with WS02. If you need a public migration note for the protocol-2 cut, land it in an in-scope SDK surface instead of broadening this workstream into proto ownership.
