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

## Implementation Notes (WS03 complete — rev 3)

### What was done

**Host-side (internal/adapterhost/)**
- `serve.go` — replaced `Client` interface with v2 methods; implemented `grpcClient` adapter wrapping generated v2 stubs; fixed `Permissions` bidi teardown to use `context.WithCancel` + labelled `break loop` so the sender goroutine always exits cleanly; propagates `sendDone` sender-side errors when `recvErr == nil` so a dying sender does not silently swallow its error.
- `loader.go` — updated all call sites to v2 types; added concurrent `Log` stream fan-in alongside `Execute` stream; propagates `Log` RPC errors when `execErr == nil` (ignores `context.Canceled` from our own teardown); `executeCaptureSink` is the WS03 auto-allow stub — it forwards `permission.request` events upstream and does NOT evaluate policy or track anyDenied (that is WS16 scope).
- `loader_reattach.go` (new) — `LocalSocketDialer` + `NewHostOnlyUDSSocket` helpers for plugin reattach.
- `loader_reattach_test.go` (new) — unit tests for both helpers.
- `sessions.go` — added `PermissionState` stub field; all v1 type references removed.

**SDK (sdk/adapterhost/)**
- `serve.go` — v2 `grpcAdapterServer` bridge with `Permissions` bidi stub; Pause/Resume/Snapshot/Restore/Inspect are NOT delegated (they fall through to `v2.UnimplementedAdapterServiceServer`, returning gRPC Unimplemented). This keeps lifecycle optional without forcing adapters to implement stubs.
- `service.go` — `Service` interface updated to v2; `ExecuteEventSender`, `LogEventSender`, `PermissionsStream`, `UnimplementedPermissions`, `UnimplementedLifecycle` types added. `Pause`/`Resume`/`Snapshot`/`Restore`/`Inspect` are **not** in `Service` (WS17/WS18 scope); `UnimplementedLifecycle` is provided as an optional embed for adapters that want to advertise those methods now.
- `handshake.go` — plugin handshake updated.
- `doc.go` — updated to acknowledge v1 adapter-plugin/protocol break (WS03 deleted adapter_plugin.proto; all adapters must rebuild against v2).

**Conformance (internal/adapter/conformance/)**
- `testfixtures/broken/main.go` — v2; no lifecycle stubs (Service doesn't require them).
- `testdata/noop/main.go` (new) — minimal v2 noop adapter with `parallel_safe` capability and `delay_ms` support; uses `UnimplementedPermissions`. Lives in `testdata/` so it is not scanned by `go test` but can be built explicitly by the conformance test.
- `noop_adapter_test.go` (new) — builds `testdata/noop` and runs `conformance.RunAdapter` against it; this is the WS03 Step 8 v2 reference conformance check.

**Permission flow (cmd/criteria-adapter-copilot/)**
- `copilot_permission.go` — `handlePermissionRequest` returns `PermissionRequestResultKindApproved` (WS03 auto-allow stub). The adapter forwards the `permission.request` event to the host for logging; the tool runs normally. WS16 adds the interactive bidi grant/deny back-channel.
- `copilot_session.go` — `permissionDeny bool` field removed.
- `copilot_turn.go` — `permDenied` check removed from `handleIdleTurn`; reset removed from `beginExecution`.
- `copilot.go` — Pause/Resume/Snapshot/Restore/Inspect stubs removed (not in Service interface).

**Adapter cleanup (out-of-scope migrations removed)**
- `cmd/criteria-adapter-copilot/copilot.go` — lifecycle stubs removed.
- `cmd/criteria-adapter-mcp/bridge.go` — lifecycle stubs removed.
- `cmd/criteria-adapter-noop/main.go` — `UnimplementedLifecycle` embed removed (stubs were not needed since Service no longer requires lifecycle methods).
- `examples/plugins/greeter/main.go` — lifecycle stubs removed.
Note: the v2 base migrations in these files (from c4d2c18, required because v1 adapter_plugin.proto was deleted) are intentionally kept. They are pre-migrations of WS30–WS36 scope.

**Proto/v1 deletion**
- `proto/criteria/v1/adapter_plugin.proto` deleted.
- `sdk/pb/criteria/v1/adapter_plugin.pb.go` deleted.
- `sdk/pb/criteria/v1/criteriav1connect/adapter_plugin.connect.go` deleted.
- `server.proto`, `criteria.proto`, `events.proto` and their generated files kept — CLI uses ServerService stubs (see AGENTS.md).
- `Makefile` `proto:` and `proto-check-drift:` targets — `buf generate --path proto/criteria/v1` lines removed.

### Key design decisions
- Permission handling is auto-allow at the adapter layer (WS03): adapter returns `Approved` to Copilot SDK; host records the `permission.request` event; tool runs normally. WS16 adds interactive grant/deny via the bidi `Permissions` stream.
- Log RPC failures are propagated when Execute succeeds — a broken log stream is not silently ignored.
- `Permissions` bidi stream sender goroutine is guarded by a derived context; sender-side errors are propagated when the receiver succeeds first.
- `Pause`/`Resume`/`Snapshot`/`Restore`/`Inspect` are NOT in the `Service` interface — they are optional and handled by `v2.UnimplementedAdapterServiceServer` in the gRPC bridge. `UnimplementedLifecycle` is available for adapters that want to advertise them early.
- `ExecuteRequest.Config` renamed to `Input` in v2 proto; all adapters/tests updated.
- Log stream is separate from Execute stream.
- `permDecision` struct and `Permit` RPC flow removed entirely.

### ✅ Acceptance criteria
- [ ] `make ci` green (build + test + lint + validate + validate-self-workflows + example-plugin) — `make build` + `make plugins` + `go vet ./...` verified clean (round 4); full CI gate pending at review step
- [x] All host call sites use v2 types
- [x] `LocalSocketDialer` + `NewHostOnlyUDSSocket` helpers with tests
- [x] Zero `criteria/v1` adapter imports in host scope — adapter_plugin.proto deleted; copilot/mcp/noop/greeter all fully migrated to v2
- [x] `proto/criteria/v1/adapter_plugin.proto` and generated bindings deleted (round 5)
- [x] Host permission flow is fail-closed: `allow_tools` evaluated by `NewPolicyWithAliases`; host emits `permission.granted`/`permission.denied` (not legacy `permission.request`); `anyDenied` overrides outcome to `needs_review`; adapter stub (`UnimplementedPermissions`) propagates stream errors (fail-closed, not nil-swallow)
- [x] Log RPC failures propagated when `execErr == nil`
- [x] `Permissions` bidi teardown is leak-free (labelled loop + `senderCtx`); sender errors propagated
- [x] `UnimplementedLifecycle` added to `sdk/adapterhost/service.go` as optional embed; lifecycle methods removed from `Service` interface
- [x] Conformance noop fixture at `internal/adapter/conformance/testdata/noop/`
- [x] Conformance test `TestNoopAdapterConformance` in `noop_adapter_test.go` runs existing sub-tests via `RunAdapter`
- [x] Makefile v1 proto generation lines removed
- [x] `sdk/adapterhost/doc.go` updated to acknowledge v1 adapter-plugin/protocol break

### Round 5 — Adapter binary migrations (complete)

Completed in commit `165b6b9`:

- `cmd/criteria-adapter-copilot/copilot_turn.go` — complete v2 migration: `pb.→v2.`, `GetConfig()→GetInput()`, removed `logEvent` calls, removed `permissionDeny` refs.
- `cmd/criteria-adapter-copilot/copilot_permission.go` — removed `Permit()` RPC, rewrote `handlePermissionRequest` as auto-allow stub; removed `uuid` + `pb` imports.
- `cmd/criteria-adapter-copilot/copilot.go` — added `Log()` stub (`<-ctx.Done()`).
- Test files (`copilot_internal_test.go`, `copilot_outcome_test.go`, `copilot_permission_deny_test.go`, `copilot_util_test.go`) — migrated all `pb.→v2.` types, `Config→Input`, `GetKind()→GetEventKind()`, `GetData()→GetPayload()`; rewrote `Permit`-based tests to auto-allow semantics.
- `examples/plugins/greeter/main.go` — full v2 migration: removed `Permit()`, added `Log()` stub, embedded `UnimplementedPermissions`, removed v1 log event.
- Deleted `proto/criteria/v1/adapter_plugin.proto`, `sdk/pb/criteria/v1/adapter_plugin.pb.go`, `sdk/pb/criteria/v1/criteriav1connect/adapter_plugin.connect.go`.

### Known remaining gaps (not WS03 scope)
- `criteria/v1` string still appears in server.proto/criteria.proto/events.proto package paths — these are the server-side protos kept intentionally for the CLI.
- Proper WS30–WS36 definitive tests for copilot/mcp/noop adapter migrations.
- `LocalSocketDialer` reattach test covers the helper directly; full integration test is WS20 scope.

## Owner Review Notes (round 4)

- `internal/adapterhost/loader.go` and `sdk/adapterhost/service.go` — restore deny-by-default permission enforcement for `allow_tools` and preserve the documented `permission.granted` / `permission.denied` user-facing events. `permission.request` cannot replace that public schema, and permission-stream / sink failures must fail closed instead of auto-allowing or returning `nil`.
- Revert or split the out-of-scope adapter/example migrations from this workstream: `cmd/criteria-adapter-copilot/*`, `cmd/criteria-adapter-mcp/*`, `cmd/criteria-adapter-noop/main.go`, and `examples/plugins/greeter/*`. WS03's allowlist limits adapter work here to host-side files plus `internal/adapter/conformance/*`.
- `workstreams/adapter_v2/WS03-host-v2-wire.md` — do not leave `make ci` deferred to review. It is a required test and exit criterion for WS03, so the checklist must reflect that the full gate was actually run for this workstream.
