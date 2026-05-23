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
- The grep for `criteria/v1` adapter imports in host scope returns no matches (adapter_plugin.proto deleted). Note: `criteria/v1` path strings still appear in `server.proto`, `criteria.proto`, and `events.proto` package declarations — these are server-side protos kept intentionally per scope (see AGENTS.md). Those are NOT WS03 scope.
- The `LocalSocketDialer` test passes.

## Files this workstream may modify

- `internal/adapter/serve.go`, `loader.go`, `loader_reattach.go` (new), `sessions.go`, `discovery.go`, `process.go`.
- `internal/engine/*` and `internal/cli/*` call sites — mechanical type updates.
- `sdk/adapterhost/*` (post-WS01 path).
- `proto/criteria/v1/` — **deletion only** (Step 7).
- `proto/criteria/v2/` — WS03 cutover comment and `PermissionCancel` doc correction (round 11).
- `docs/adapters.md` — permission gating documentation (round 11).
- `Makefile` proto target — remove v1 line.
- `internal/adapter/conformance/*.go` — convert existing 11 sub-tests to v2.
- New tests next to changed files.
- `cmd/criteria-adapter-copilot/` — compilation-required v2 type substitutions (v1 adapter_plugin.proto deleted in this workstream) plus round-11 blocking permission round-trip.
- `cmd/criteria-adapter-mcp/bridge.go` — compilation-required v2 type substitutions.
- `cmd/criteria-adapter-noop/main.go` — compilation-required v2 type substitutions.
- `examples/plugins/greeter/main.go` — compilation-required v2 type substitutions.

## Files this workstream may NOT edit

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
- `copilot_session.go` — `permissionDeny bool` field not present; pass-through only.
- `copilot_turn.go` — no permission-related state in `handleIdleTurn`; WS03 is pure pass-through.
- `copilot.go` — `permission_gating` capability not advertised (WS03 is auto-allow, not interactive gating).

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
- `Pause`/`Resume`/`Snapshot`/`Restore`/`Inspect` are NOT in the `Service` interface and are not wired through the gRPC bridge; `v2.UnimplementedAdapterServiceServer` returns `codes.Unimplemented` for all lifecycle RPCs. `UnimplementedLifecycle` removed from `sdk/adapterhost` — adapters must not implement these methods until a lifecycle workstream wires them through.
- `ExecuteRequest.Config` renamed to `Input` in v2 proto; all adapters/tests updated.
- Log stream is separate from Execute stream.
- `permDecision` struct and `Permit` RPC flow removed entirely.

### Acceptance criteria (updated after round 7)
- [x] `make ci` green (build + test + lint + validate + validate-self-workflows + example-plugin)
- [x] All host call sites use v2 types
- [x] `LocalSocketDialer` + `NewHostOnlyUDSSocket` helpers with tests
- [x] Zero `criteria/v1` adapter imports in host scope — adapter_plugin.proto deleted; copilot/mcp/noop/greeter received minimal compilation-required v2 type substitutions (full WS30-36 migrations are separate workstreams)
- [x] `proto/criteria/v1/adapter_plugin.proto` and generated bindings deleted
- [x] Host permission flow is real bidi round-trip (round 11): `permission.request` events intercepted by `executeCaptureSink.handlePermissionRequest`; host evaluates `allow_tools` via `NewPolicyWithAliases`; emits `permission.granted` or `permission.denied`; forwards `PermissionEvent.request` (allow) or `PermissionEvent.cancel` (deny) to the adapter via the `Permissions` bidi stream; anyDenied override (success→needs_review) applied after Execute; copilot adapter blocks in `handlePermissionRequest` waiting for host decision via `pendingPerms` channel.
- [x] Log RPC failures propagated when `execErr == nil`
- [x] Permissions bidi teardown is leak-free (labelled loop + `senderCtx`); Unimplemented treated as expected so adapters not implementing Permissions do not abort Execute
- [x] Log chunk buffering: seq/start validation per-stream; aggregate memory cap (16 MiB) across all concurrent log streams; regression tests added
- [x] `UnimplementedLifecycle` removed from `sdk/adapterhost/service.go`; lifecycle methods are not in `Service` and are not wired through the gRPC bridge; adapters must not implement them
- [x] Conformance noop fixture at `internal/adapter/conformance/testdata/noop/`
- [x] Conformance test `TestNoopAdapterConformance` in `noop_adapter_test.go` runs existing sub-tests via `RunAdapter`
- [x] Makefile v1 proto generation lines removed
- [x] `sdk/adapterhost/doc.go` updated to acknowledge v1 adapter-plugin/protocol break
- [x] SDK (`sdk/adapterhost`) builds outside this repo without depending on unreleased `proto/criteria/v2`; v2 types copied to `sdk/pb/criteria/v2/`; root-module adapter plugins updated to use `sdk/pb/criteria/v2`
- [ ] Full `criteria/v1` path-string grep returns zero matches — deferred; `server.proto`, `criteria.proto`, `events.proto` still use `criteria.v1` as a package-path string and must remain per AGENTS.md (not WS03 scope)

### Round 5 — Fail-closed Permissions + chunk bounds (complete)

Completed in this round (round 5 must-fix items):

- `internal/adapterhost/serve.go` — removed `decisions chan<-` backpressure trap from `Client.Permissions` interface and `grpcClient.Permissions`; `recvPermissionDecisions` now drains and discards adapter decisions.
- `internal/adapterhost/loader.go` — `maxChunkBufBytes` (64 MiB) added; `emitAdapter` and `emitResult` reject oversized chunk sequences; `permDone` error surfaced and checked after Execute.
- `sdk/adapterhost/doc.go` — corrected v1→v2 section to reflect that adapter binaries received only minimal compilation-required v2 type substitutions; full per-adapter migrations are WS30-36.
- `cmd/criteria-adapter-copilot/*`, `cmd/criteria-adapter-mcp/bridge.go` — reverted out-of-scope WS16 dirty changes.

### Round 4 — Adapter binary migrations (complete)

Completed in commit `165b6b9`:

- `cmd/criteria-adapter-copilot/copilot_turn.go` — complete v2 migration: `pb.→v2.`, `GetConfig()→GetInput()`, removed `logEvent` calls, removed `permissionDeny` refs.
- `cmd/criteria-adapter-copilot/copilot_permission.go` — removed `Permit()` RPC, rewrote `handlePermissionRequest` as auto-allow stub; removed `uuid` + `pb` imports.
- `cmd/criteria-adapter-copilot/copilot.go` — added `Log()` stub (`<-ctx.Done()`).
- Test files (`copilot_internal_test.go`, `copilot_outcome_test.go`, `copilot_permission_deny_test.go`, `copilot_util_test.go`) — migrated all `pb.→v2.` types, `Config→Input`, `GetKind()→GetEventKind()`, `GetData()→GetPayload()`; rewrote `Permit`-based tests to auto-allow semantics.
- `examples/plugins/greeter/main.go` — full v2 migration: removed `Permit()`, added `Log()` stub, embedded `UnimplementedPermissions`, removed v1 log event.
- Deleted `proto/criteria/v1/adapter_plugin.proto`, `sdk/pb/criteria/v1/adapter_plugin.pb.go`, `sdk/pb/criteria/v1/criteriav1connect/adapter_plugin.connect.go`.

### Round 7 — Owner must-fix items (complete)

1. **`.criteria/workflows/develop/main.hcl`** — reverted `repair_ci` step (lines 193-214) to base branch version; `target` back to `adapter.copilot.repair`, `allow_tools` restored, prompt reverted.
2. **WS16 policy removed from `loader.go`** — removed `NewPolicyWithAliases`, `anyDenied` tracking, `handlePermissionRequest` function, and outcome override (`needs_review` rewrite). `executeCaptureSink` struct simplified: removed `ctx`, `anyDenied`, `policy`, `allowTools`, `adapterName`, `requests` fields. `emitAdapterEvent` simplified to plain forward.
3. **WS16 enrichment removed from `copilot_permission.go`** — removed `request_id`/`tool` enrichment block and `uuid` import. Kept: `permissionDeny = true`, basic `permission.request` forwarding, return `Approved`.
4. **Permissions stream hardening** — `codes.Unimplemented` added to expected Permissions stream errors in goroutine AND in post-execute `permErr` check; dead stream no longer cancels Execute or returns failure.
5. **Log chunk seq/aggregate hardening** — `logForwardSink` rewritten with `chunkSeqs map[string]uint32` for per-stream seq tracking; `maxTotalLogBufBytes = 16 MiB` aggregate cap across all streams; `totalLogBufSize()` helper; seq=0 starts new sequence; non-zero seq with no in-progress sequence returns error; out-of-order seq returns error and clears stream state.
6. **Regression tests in `loader_test.go`** — 5 new tests: `TestLogForwardSink_ChunkOversize` (updated for seq tracking), `TestLogForwardSink_ChunkOutOfOrder`, `TestLogForwardSink_ChunkNonZeroSeqWithNoSequence`, `TestLogForwardSink_AggregateCapRejectsNewStream`, `TestPermissionsStreamUnimplemented`.
7. **SDK packaging fix** — `sdk/pb/criteria/v2/` created with 5 files copied from `proto/criteria/v2/` (`adapter.pb.go`, `adapter_grpc.pb.go`, `options.pb.go`, `chunking.go`, `heartbeat.go`). `sdk/adapterhost/serve.go`, `service.go`, `serve_test.go`, `doc.go` updated to import `sdk/pb/criteria/v2`. All root-module adapter plugins (`noop`, `copilot`, `mcp`) and test fixtures updated to import `sdk/pb/criteria/v2` to match the SDK's Service interface.
8. **Conformance test updated** — `assertPermissionDeniedEvent` checks for `permission.request` event (not `permission.denied`); `strings` import removed.
9. **Workstream file updated** — exit criterion for `criteria/v1` clarified; acceptance criteria updated to remove false WS16 claims; this round-7 summary added.


- `criteria/v1` string still appears in server.proto/criteria.proto/events.proto package paths — these are the server-side protos kept intentionally for the CLI.
- Proper WS30–WS36 definitive tests for copilot/mcp/noop adapter migrations.
- `LocalSocketDialer` reattach test covers the helper directly; full integration test is WS20 scope.

## Owner Review Notes (round 10)

- `internal/adapterhost/loader_reattach.go:72-79` and `internal/adapterhost/loader_reattach_test.go` — fix the `AttachedRunner` contract violation. `noopAttachedRunner.Wait` cannot return immediately; it must block until the externally managed adapter is actually gone (or the caller cancels), otherwise `LocalSocketDialer` is unreliable for the long-lived WS20 reattach path. Add regression coverage for the expected wait behavior.
- `cmd/criteria-adapter-copilot/copilot_permission.go:34-42` — do not fail open when forwarding the WS03 `permission.request` event to the host fails. If the adapter cannot emit the only in-scope observability event, it must not still return `Approved` and let the tool action proceed silently.

### Round 10 — AttachedRunner contract + permission fail-closed (complete)

1. **`noopAttachedRunner` contract fix** (`internal/adapterhost/loader_reattach.go`) — `Wait` now blocks until `Kill` is called or the context is cancelled. Go-plugin calls `Wait(context.Background())` in a background goroutine; when it returned immediately the client immediately set `exited=true` and cancelled `doneCtx`, breaking all subsequent RPCs over the reattached connection. Added `done chan struct{}` + `sync.Once` guard to `Kill` (idempotent close). Added `newNoopAttachedRunner()` constructor; `externalProcessReattach` uses it.
2. **Regression tests** (`internal/adapterhost/loader_reattach_test.go`) — three new tests: `TestNoopAttachedRunnerWaitBlocksUntilKill` (Wait does not return before Kill, unblocks after Kill), `TestNoopAttachedRunnerWaitContextCancel` (Wait unblocks on context cancel with non-nil error), `TestNoopAttachedRunnerKillIdempotent` (double Kill does not panic).
3. **Permission fail-closed** (`cmd/criteria-adapter-copilot/copilot_permission.go`) — `sink.Send` error now causes `UserNotAvailable` return instead of `Approved`; a failing observability send must not silently allow the tool action to proceed.
4. **Test updated** (`cmd/criteria-adapter-copilot/copilot_permission_deny_test.go`) — `TestHandlePermissionRequestSendError` expectation flipped from `Approved` to `UserNotAvailable`; comment updated to explain fail-closed rationale.

### Round 11 — changes requested

1. **`internal/adapterhost/loader.go:226-285,333-440`, `internal/adapterhost/serve.go:128-188`, `cmd/criteria-adapter-copilot/copilot_permission.go:19-46`** — restore a real v2 permission round-trip. Permission requests must travel over the `Permissions` bidi RPC, the host must keep enforcing the current `allow_tools` deny-by-default behavior and emitting the corresponding grant/deny audit behavior, and the Copilot adapter must wait for the host decision instead of locally approving the action.
2. **`internal/adapterhost/loader.go:247-267`** — serialize `Execute` and `Log` fan-in before they call the shared `adapter.EventSink`. The current two-goroutine write path races non-goroutine-safe sinks and can destabilize event ordering; add regression coverage with a sink that would fail under concurrent calls.
3. **`workstreams/adapter_v2/WS03-host-v2-wire.md:210-218,255-260`** — reconcile scope metadata with the active diff. Either remove the WS30-WS36 pre-migration edits from `cmd/criteria-adapter-*`, `examples/plugins/greeter/*`, `sdk/pb/criteria/v2/*`, etc., or explicitly bring those files into WS03's allowed scope with the required rationale. The workstream cannot keep the narrower allowlist while also keeping those edits.
4. **`proto/criteria/v2/adapter.proto:1-5`, `docs/adapters.md:272-285,431-587`** — update the public contract text and examples to match the shipped WS03 cutover: `adapter_plugin.proto` is gone now, third-party adapters must use the v2 SDK/imports and `Permissions` flow, and the permission-gating docs must match the restored host behavior above.

### Round 11 — implementation (complete)

1. **Real v2 Permissions round-trip** (`internal/adapterhost/serve.go`, `internal/adapterhost/loader.go`, `cmd/criteria-adapter-copilot/copilot.go`, `cmd/criteria-adapter-copilot/copilot_permission.go`):
   - `internal/adapterhost/serve.go` — `Client.Permissions` and `grpcClient.Permissions` now take `decisions chan<- *v2.PermissionDecision`; `recvPermissionDecisions` routes ACKs to this channel instead of discarding.
   - `internal/adapterhost/loader.go` — `executeCaptureSink` struct restored with `anyDenied`, `policy`, `allowTools`, `adapterName`, `requests` fields; `emitAdapterEvent` now intercepts `permission.request` events and calls `handlePermissionRequest`; `handlePermissionRequest` evaluates `allow_tools` via `NewPolicyWithAliases`, emits `permission.granted`/`permission.denied` to upstream sink, and forwards `PermissionEvent.request`/`PermissionEvent.cancel` to adapter via `requests` channel; `anyDenied → needs_review` outcome override applied after Execute completes.
   - `cmd/criteria-adapter-copilot/copilot.go` — `copilotAdapter` struct gets `pendingPermsMu sync.Mutex`, `pendingPerms map[string]chan<- string`; helper methods `registerPendingPerm`, `resolvePendingPerm`, `drainPendingPerms`; `Permissions` method override that routes host `request`→allow and `cancel`→deny signals to pending channels.
   - `cmd/criteria-adapter-copilot/copilot_permission.go` — `handlePermissionRequest` now generates `request_id`, registers pending channel, forwards `permission.request` event, **blocks** on `select { case decision := <-decisionCh; case <-activeCh }`, returns `Approved` or `Rejected` (not `UserNotAvailable`) to the Copilot SDK based on host decision.

2. **Serialized EventSink** (`internal/adapterhost/loader.go`):
   - `serializedEventSink` struct (mutex wrapping `adapter.EventSink`) added; `Execute` wraps the caller's sink in `serializedEventSink` before passing to both `executeCaptureSink` (Execute goroutine) and `logForwardSink` (Log goroutine). Prevents data races on non-goroutine-safe sinks.
   - `internal/adapterhost/loader_test.go` — `TestSerializedEventSink_ConcurrentCallsAreOrdered`: `nonThreadSafeSink` (detects concurrent access via `atomic.CompareAndSwapInt32`) spawns 2×500 goroutines calling `Adapter` and `Log`; asserts no concurrent access detected.

3. **Scope metadata** (`workstreams/adapter_v2/WS03-host-v2-wire.md`):
   - "Files this workstream may modify" expanded to include `cmd/criteria-adapter-copilot/`, `cmd/criteria-adapter-mcp/bridge.go`, `cmd/criteria-adapter-noop/main.go`, `examples/plugins/greeter/main.go` with rationale; `proto/criteria/v2/` restriction removed from "may NOT edit" (round-11 needs comments there).

4. **Proto and docs** (`proto/criteria/v2/adapter.proto`, `docs/adapters.md`):
   - `proto/criteria/v2/adapter.proto` file header updated to note v1 deletion, explain the bidi Permissions stream direction (host=client, adapter=server), and distinguish blocking vs. post-hoc enforcement; stale `PermissionCancel` comment corrected (it is NOT "sent by the adapter" — it is the host's deny signal).
   - `docs/adapters.md` Permission Gating section expanded with "How the permission round-trip works" subsection documenting the 5-step bidi flow; post-hoc vs. blocking enforcement distinction documented.

New tests in `internal/adapterhost/loader_test.go`:
- `TestHandlePermissionRequest_Allow` — allow policy: emits `permission.granted`, forwards `PermissionEvent.request` to requests channel.
- `TestHandlePermissionRequest_Deny` — deny-all policy: emits `permission.denied`, sets `anyDenied`, forwards `PermissionEvent.cancel` to requests channel.
- `TestExecute_DeniedPermissionOverridesSuccess` — adapter emits `permission.request` + reports `success`; host overrides to `needs_review`.
- `TestSerializedEventSink_ConcurrentCallsAreOrdered` — concurrent Adapter/Log calls are serialized by `serializedEventSink`.

