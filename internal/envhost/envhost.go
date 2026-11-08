// Package envhost is the environment host-of-record seam (ADR-0014 D1).
//
// Every environment declaration materializes one Host object; the session
// layer dispatches against the seam instead of reaching into per-environment
// plumbing. The contract is narrow and non-adapter-flavored: a scope
// engages definitions through [Host.Ensure], ends its engagement through a
// declared [FatePolicy] via [Host.Release], reads supervision truth through
// [Host.Journal], and drives per-(definition, scope) verbs through
// [Host.Control].
//
// Stage 1 (ADR-0014 M2 carve) binds ONLY the remote environment and is
// behavior-preserving: the bound host wraps the existing peer session
// provider plus the remote server, and the consuming session surfaces are
// the same verbs the pre-carve shim knobs performed. Nothing observable
// may change at this stage.
//
// Import boundary (enforced from M4 onward): the host seam sits BELOW the
// adapter session layer and ABOVE the environment bindings —
// internal/adapterhost may depend on this package, but the seam must never
// import adapter session-layer types. Card KB-377's rule, to enforce
// verbatim at M4: "adapterhost must not import remote shim types" — i.e.
// internal/adapterhost (the consuming session layer) must not import the
// remote environment package's session/shim plumbing
// (internal/adapter/environment/remote) once the remote environment owns
// this seam end to end. The carve records the rule here; a later milestone
// wires it into make lint-imports.
package envhost

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ScopeRef identifies the scope instance a host engages adapters for. The
// pair reproduces today's phone-home scope key "<scopeName>/<scopeInstanceID>"
// byte-for-byte: an empty Scope with a non-empty Instance is the root scope
// shape ("/<instanceID>"), and an empty Instance is today's legacy no-scope
// shape (the empty key).
type ScopeRef struct {
	// Scope is the scope name half; "" names the root scope (workflow-run
	// handler and adapter-scoped runs without an instance-scoped name).
	Scope string
	// Instance is the scope instance UUID half; "" is the legacy no-scope
	// shape carried by non-per-scope provisioning.
	Instance string
}

// Key renders the phone-home scope key the bound environment's plumbing
// consumes: "<scope>/<instance>", "/<instance>" for the root scope, and the
// empty key for the legacy no-scope shape. Round-trips [ParseScopeRef].
func (r ScopeRef) Key() string {
	switch {
	case r.Instance == "":
		return r.Scope
	case r.Scope == "":
		return "/" + r.Instance
	default:
		return r.Scope + "/" + r.Instance
	}
}

// String mirrors Key so scope refs log and error-render in the exact shape
// the phone-home plumbing (and therefore pre-existing logs) carries them.
func (r ScopeRef) String() string {
	return r.Key()
}

// ParseScopeRef splits a phone-home scope key into its scope halves using
// the same first-slash rule the engine's scope split uses. It round-trips
// [ScopeRef.Key] for every key shape in production: the legacy empty key,
// the root "/<instanceID>", and "<scopeName>/<instanceID>". A key without a
// slash parses as a scope-only ref only when empty; any other slashless key
// is malformed and reported as not-ok.
func ParseScopeRef(key string) (ScopeRef, bool) {
	if key == "" {
		return ScopeRef{}, true
	}
	scope, inst, found := strings.Cut(key, "/")
	if !found {
		// The legacy shape is the only slashless key callers carry.
		return ScopeRef{}, false
	}
	return ScopeRef{Scope: scope, Instance: inst}, true
}

// Definition is one adapter definition a scope's workflow engages, keyed by
// its instance identity. Name is the "<type>.<name>" adapter-instance key
// every per-scope wait/kill verb names; Type is the bare adapter type.
type Definition struct {
	// Name is the adapter instance identity ("<type>.<name>").
	Name string
	// Type is the adapter's type (the process subject the definition runs).
	Type string
}

// Fate is the declared fate of a scope's engagement when its workflow body
// ends. Values are stable identifiers, safe for journaling.
type Fate string

const (
	// FateRelease ends the engagement for good: close the scope's live
	// sessions, unregister its accept token, and tombstone the instance so
	// the next run provisions fresh.
	FateRelease Fate = "release"
	// FateDetach keeps the engagement alive beyond the workflow body so a
	// sibling or re-entering scope can adopt it; a ledger entry replaces the
	// teardown.
	FateDetach Fate = "detach"
)

// FatePolicy carries a scope's DECLARED fate into Release. The engine
// derives it from the workflow's scope lifecycle declarations; enforcement
// is the host's job.
type FatePolicy struct {
	Fate Fate
}

// Handle is the live adapter handle a host settles for a scope's engaged
// definition. Stage 1 keeps existing handle identities verbatim: a binding
// returns the same handle object its dispatch lane produced before the seam
// existed, and the consuming session layer recovers its own typed handle
// from it. A dedicated seam-owned handle vocabulary arrives with the
// host-owned session model; until then this carrier intentionally exposes
// no methods.
type Handle any

// Handles is the per-definition handle table Ensure settles. Keys are
// [Definition.Name] values.
type Handles map[string]Handle

// Get returns the handle settled for a definition name.
func (h Handles) Get(name string) (Handle, bool) {
	v, ok := h[name]
	return v, ok
}

// ErrJournalUnavailable reports that a binding carries no supervision
// journal yet. Journal consumers must treat it as "ledger lives elsewhere",
// not as a transport failure.
var ErrJournalUnavailable = errors.New("envhost: supervision journal unavailable for this binding")

// JournalKind classifies a journal entry. Values mirror today's
// provisioning event vocabulary so entries read like the events they
// shadow; the vocabulary is the host's contract going forward.
type JournalKind string

const (
	// JournalProvisionWanted: a scope asked for a definition and none was
	// settled yet.
	JournalProvisionWanted JournalKind = "provision_wanted"
	// JournalDetached: an engagement detached at body exit instead of
	// releasing.
	JournalDetached JournalKind = "detached"
	// JournalReleased: an engagement released with tombstone semantics.
	JournalReleased JournalKind = "released"
	// JournalSpawned: a child process was spawned for the definition
	// (process supervision truth; the in-process container host's spawn
	// journal).
	JournalSpawned JournalKind = "spawned"
	// JournalExited: a child process exited with its wait status.
	JournalExited JournalKind = "exited"
	// JournalCrashClassified: an unexpected exit was classified into a
	// stable crash reason.
	JournalCrashClassified JournalKind = "crash_classified"
)

// JournalEntry is one supervision-truth record for a scope's engagement of
// a definition.
type JournalEntry struct {
	Scope   ScopeRef
	Adapter string // definition identity ("<type>.<name>")
	Kind    JournalKind
	At      time.Time
	Detail  string
}

// JournalReader streams a scope's engagement ledger for one definition.
// Implementations must return entries in causal order.
type JournalReader interface {
	// Entries returns the journal entries recorded so far. A binding with
	// no journal returns [ErrJournalUnavailable].
	Entries(ctx context.Context) ([]JournalEntry, error)
}

// ControlKind enumerates the per-(definition, scope) verb arms. Arms are
// append-only across milestones; never repurpose a value. The numbering
// is internal only — the wire-level control arms live in
// proto/criteria/v1 (peer.proto ControlRequest) and map onto these as the
// host-owned control plane lands.
type ControlKind int

const (
	// ControlRegisterScope accepts (or rotates) a scope's phone-home
	// accept token so reconnects under the fresh token are admitted.
	ControlRegisterScope ControlKind = iota + 1
	// ControlUnregisterScope withdraws a scope's accept token so the old
	// token is rejected.
	ControlUnregisterScope
	// ControlCloseHandle closes the definition's live session for the
	// scope (per-definition child kill with per-scope peers).
	ControlCloseHandle
	// ControlListenAddress asks for the host's bound listen address.
	ControlListenAddress
	// ControlEnsureFreshHandle waits for a live handle for the definition
	// that is not the stale one named in the request (respawn =
	// reconnect).
	ControlEnsureFreshHandle
)

// ControlRequest names one verb for (Scope, Adapter). Adapter is unused by
// scope-level arms.
type ControlRequest struct {
	Scope   ScopeRef
	Adapter string // definition identity for per-definition arms
	Kind    ControlKind

	// Token carries the rotated accept token for ControlRegisterScope.
	Token string
	// Stale excludes an already-dead handle for ControlEnsureFreshHandle.
	// It must carry the binding's own handle identity; a nil value waits
	// for any live handle.
	Stale Handle
}

// ControlResponse carries the arm's result. Exactly one field is
// meaningful per kind; a failing arm reports through the error return.
type ControlResponse struct {
	// Handle is the settled live handle for ControlEnsureFreshHandle.
	Handle Handle
	// Address is the bound listen address for ControlListenAddress.
	Address string
}

// Host is the environment host-of-record seam. One host exists per
// environment declaration and owns the environment's scope engagements.
type Host interface {
	// Ensure registers the scope's adapter definitions and returns live
	// handles. Idempotent per (scope, definition): re-entry re-adopts, it
	// never re-provisions. Definitions are engaged in order; the first
	// failure stops the engagement and is returned, with the handles
	// settled so far still reported.
	Ensure(ctx context.Context, scope ScopeRef, defs []Definition) (Handles, error)

	// Release ends a scope's engagement per the scope's DECLARED fate
	// policy: release closes the scope's live sessions and withdraws its
	// accept token; detach keeps the engagement alive (a ledger entry
	// replaces teardown).
	Release(ctx context.Context, scope ScopeRef, policy FatePolicy) error

	// Journal exposes supervision truth (spawn/exit/crash-classified) for
	// every child the scope engaged with the definition — one diagnostic
	// surface across environments.
	Journal(scope ScopeRef, adapter string) JournalReader

	// Control drives kill/respawn/open/close verbs per (definition,
	// scope). It replaces the scattered per-environment knobs.
	Control(ctx context.Context, req *ControlRequest) (ControlResponse, error)
}

// VerifyDeferrer is the optional capability of a bound host to defer the
// blocking adapter-info verify handshake to scope entry. Remote bindings
// with per-scope sessions defer: the per-scope token does not exist before
// scope entry, so an eager verify would block before the adapter can
// phone home. Bindings without the capability verify every adapter
// eagerly. Consumers check the capability structurally — they must not
// import a specific binding package to ask.
type VerifyDeferrer interface {
	DefersVerify() bool
}
