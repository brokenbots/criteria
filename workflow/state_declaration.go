package workflow

import "fmt"

// Well-known mode values for StateDeclaration.Mode (InfoResponse.state.mode,
// CRI-201). Values are free-form strings on the wire; unknown values must
// fail the handshake loudly — never a silent downgrade to none.
const (
	// StateModeNone declares no checkpointable state: a fresh start on every
	// (re)spawn. Identical to an absent declaration.
	StateModeNone = "none"
	// StateModeBlob declares that the engine stores the serialized state
	// object on the adapter's behalf (criteriadb via the engine; storage
	// primitives only).
	StateModeBlob = "blob"
	// StateModeRef declares that state is an opaque token to adapter-owned
	// state (e.g. a harness session id); the engine persists the token,
	// never the payload.
	StateModeRef = "ref"
)

// Well-known granularity values for StateDeclaration.Granularity
// (InfoResponse.state.granularity, CRI-201). Unknown values are ignored for
// forward-compatibility: the engine falls back to its default save policy.
const (
	StateGranularityPerStep  = "per-step"
	StateGranularityPerTurn  = "per-turn"
	StateGranularityOnDemand = "on-demand"
)

// DefaultStateMaxBytes is the engine default cap on a single saved state
// object when the adapter declares none (InfoResponse.state.max_bytes = 0).
// A save exceeding the applicable cap fails loudly and fails the step — it
// is never truncated (a truncated transcript restores as a
// plausible-looking broken session).
const DefaultStateMaxBytes = 400 * 1024

// StateDeclaration is an adapter's declared checkpointable session-state
// surface, translated from InfoResponse.state (CRI-201). It is engine/SDK
// surface only: it carries no checkpoint semantics in any backing store —
// blob payloads flow to the engine's store through the engine, and ref-mode
// adapters point at their own stores. A nil StateDeclaration (the zero case)
// means the adapter declared no state: it starts fresh on every (re)spawn.
type StateDeclaration struct {
	// Mode is where state lives: StateModeNone | StateModeBlob | StateModeRef.
	// An unknown mode fails the handshake loudly — never silently downgraded.
	Mode string
	// Schema is the adapter-defined version tag of the serialized state
	// shape. It travels with every saved state so a restore can reject a
	// mismatched shape instead of misinterpreting the bytes. Required for
	// blob/ref.
	Schema string
	// MaxBytes is the adapter-declared cap on a single saved state object.
	// 0 = engine default (DefaultStateMaxBytes).
	MaxBytes uint32
	// Granularity declares when the adapter expects its state to be saved:
	// StateGranularityPerStep | StateGranularityPerTurn |
	// StateGranularityOnDemand. Unknown values are ignored (engine default
	// save policy).
	Granularity string
}

// Validate checks the declaration against the host-side contract:
//
//   - none (or empty) is always valid and carries no obligations;
//   - blob/ref require a non-empty schema version tag, so a restore can
//     reject a mismatched state shape instead of misinterpreting bytes;
//   - any other mode is rejected — silently treating an unrecognized mode
//     as none would drop checkpointing for a stateful adapter.
func (d *StateDeclaration) Validate() error {
	if d == nil {
		return nil
	}
	switch d.Mode {
	case "", StateModeNone:
		return nil
	case StateModeBlob, StateModeRef:
		if d.Schema == "" {
			return fmt.Errorf("state mode %q requires a non-empty schema version tag", d.Mode)
		}
		return nil
	default:
		return fmt.Errorf("unknown state mode %q (known: %s, %s, %s)", d.Mode, StateModeNone, StateModeBlob, StateModeRef)
	}
}

// EffectiveMaxBytes returns the cap the engine enforces on a single saved
// state object: the adapter-declared MaxBytes when non-zero, otherwise the
// engine default. A save exceeding the cap fails loudly — it is never
// truncated.
func (d *StateDeclaration) EffectiveMaxBytes() uint32 {
	if d == nil || d.MaxBytes == 0 {
		return DefaultStateMaxBytes
	}
	return d.MaxBytes
}
