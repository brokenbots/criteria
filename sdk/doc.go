// Package overseer is the overseer→orchestrator wire contract SDK.
//
// # Overview
//
// This package is the single boundary surface between an overseer execution
// engine and any orchestrator that stores and distributes run events. It
// re-exports — without wrapping or transforming — the generated Connect/gRPC
// service stubs, envelope and payload types, and a small set of helper
// functions that every implementation needs.
//
// Pre-split module path:  github.com/brokenbots/overlord/shared/sdk/overseer
// Post-split module path: github.com/brokenbots/overseer/sdk
// (The import path changes in W08/W09; call sites in the orchestrator
// migrate then. Until that point, this in-monorepo path is canonical.)
//
// # What this package exports
//
//   - [ServiceClient] / [ServiceHandler] — Connect interface aliases for the
//     generated OverseerService stubs. Use [NewServiceClient] to construct a
//     client; implement [ServiceHandler] to expose the service.
//     [OverseerServiceClient] and [OverseerServiceHandler] are migration-
//     compatibility aliases for the same types; prefer the Service* forms for
//     new code.
//   - Envelope and payload type aliases for every event shape defined in
//     proto/overlord/v1/events.proto.
//   - [NewEnvelope], [TypeString], [IsTerminal] — thin wrappers over the
//     shared/events helpers. After W08 the implementations move into this
//     package; call sites do not change.
//   - [SchemaVersion] — the current event protocol version constant.
//
// # What this package does NOT export
//
// The SDK is deliberately minimal. None of the following cross the boundary:
//
//   - Storage interfaces (sql.DB, store.Store, or any orchestrator-specific
//     persistence layer).
//   - In-memory fan-out / control-message routing components.
//   - Run-state machine helpers (the orchestrator decides what "paused" means
//     from the semantic events it receives; the execution engine does not
//     prescribe a state model).
//   - Any type or symbol defined under castle/internal/, overseer/internal/,
//     workflow/, or parapet/.
//
// # Type alias vs wrapper function rule
//
// Types are re-exported as Go type aliases (type Foo = pb.Foo). Aliases
// preserve assignability: code that already holds a *pb.RunStarted can pass
// it as an *overseer.RunStarted without conversion. This is important during
// the migration period (W07) before all consumers have moved to the SDK path.
//
// Functions are re-exported as wrapper functions (func F(...) { return impl.F(...) }).
// Wrappers allow the implementation to move from shared/events into this
// package (W08) without requiring call sites to change. A wrapper adds one
// stack frame; this is acceptable at the SDK boundary.
//
// # Auth contract
//
// All RPCs except [ServiceHandler.Register] require a bearer token in one of:
//   - HTTP Authorization header (Bearer scheme)
//   - X-Overseer-Token header
//   - overseer-token Connect metadata key
//
// Tokens are SHA-256 compared against the orchestrator's stored token hash.
// Implementations MUST enforce caller-ownership on every mutating RPC: the
// authenticated caller's overseer ID must own the overseer or run being
// mutated. Register is bootstrap-only; implementations MUST gate it behind a
// deployment-defined bootstrap credential (e.g. a pre-shared secret in a
// deployment-defined header) or return Unimplemented when no bootstrap
// mechanism is configured.
//
// # Schema version semantics
//
// [SchemaVersion] = 1 is the v0.1 SDK. The value matches the proto package
// major version (overlord.v1). A bump to SchemaVersion 2 introduces a new
// proto package (overlord.v2) and a new SDK minor release; both sides must
// coordinate. Until a real versioning need arises, schema negotiation helpers
// are out of scope.
package overseer
