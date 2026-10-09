package engine

// Registry epoch marker for per-scope adapter token registries.
//
// A run data directory accumulates rotated per-scope accept tokens
// (<dataDir>/remote-tokens/...). A crashed runner-pod re-entry legitimately
// re-registers those tokens with the restarted engine's shim (CRI-137), and a
// replay after a consumed checkpoint legitimately adopts them from prior
// invocation directories (CRI-304) — but once an invocation reaches a
// terminal run-record status, the operator tears the prior pod fleet down and
// the fresh shim registry serving a successor invocation has no rows for
// those scopes. Adopting a completed invocation's tokens therefore dials
// adapter pods with stale claims that the fresh registry rejects forever
// (CRI-137 rejection storm; KB-227).
//
// To make the crashed-vs-completed distinction explicit, every invocation
// that initializes scope sessions first bumps this marker with state "open"
// (the registry epoch this shim registry generation serves); completing the
// run — at any named terminal state or through a run failure — seals the
// marker. Adoption refuses directories whose marker is sealed and rotates a
// fresh scope instance instead, no matter how fresh the rotated tokens look.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// registryEpochMarkerName is the marker file under the run data
	// directory's remote-tokens root.
	registryEpochMarkerName = "registry-epoch.json"

	// RegistryStateOpen marks an invocation whose scope-token registry is (or
	// was without completing) still serving; adoption from such a directory
	// is the CRI-137/CRI-304 recovery path.
	RegistryStateOpen = "open"
	// RegistryStateSealed marks an invocation that reached a terminal run
	// status; its registry closed with the pod fleet it served.
	RegistryStateSealed = "sealed"
)

// registryEpochRecord is the on-disk marker describing the run data
// directory's scope-token registry lifecycle.
type registryEpochRecord struct {
	// RegistryEpoch counts the registry generations this data directory has
	// served: each invocation that initializes scope sessions bumps it, so an
	// adoption log can name the mismatch between the prior invocation's epoch
	// and the adopting shim registry's.
	RegistryEpoch uint64 `json:"registry_epoch"`
	State         string `json:"state"`
	// FinalState is the named terminal state a sealed registry completed at
	// (empty when sealed by a run failure, which has no named terminal).
	FinalState string `json:"final_state,omitempty"`
	// Success mirrors the terminal completion's success bit; nil while open.
	Success *bool `json:"success,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// registryEpochPath derives the marker path; scopeless because the marker
// describes the whole data directory's registry.
func registryEpochPath(dataDir string) string {
	return filepath.Join(dataDir, "remote-tokens", registryEpochMarkerName)
}

// readScopeRegistryEpoch returns the data directory's registry marker. ok is
// false when no marker exists (legacy directory, or an invocation that never
// initialized scope sessions) — such directories stay adoptable so the
// CRI-137/CRI-304 recovery paths keep working.
func readScopeRegistryEpoch(dataDir string) (registryEpochRecord, bool) {
	raw, err := os.ReadFile(registryEpochPath(dataDir))
	if err != nil {
		return registryEpochRecord{}, false
	}
	var rec registryEpochRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return registryEpochRecord{}, false
	}
	return rec, true
}

// writeScopeRegistryEpoch atomically persists the marker so a reader never
// observes a partial record.
func writeScopeRegistryEpoch(dataDir string, rec registryEpochRecord) error {
	path := registryEpochPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	rec.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// bumpScopeRegistryEpoch opens a new registry generation for the data
// directory: the epoch is incremented (or seeded) and the state flips to
// open. Called once per invocation that initializes scope sessions, before
// any rotation, so an on-disk open marker always names a registry whose
// tokens a crashed re-entry can re-register.
func bumpScopeRegistryEpoch(dataDir string) (uint64, error) {
	prev, _ := readScopeRegistryEpoch(dataDir)
	rec := registryEpochRecord{
		// A directory without (or with a corrupt) marker starts a fresh epoch
		// count; the epoch is purely diagnostic for adoption logs.
		RegistryEpoch: prev.RegistryEpoch + 1,
		State:         RegistryStateOpen,
	}
	if err := writeScopeRegistryEpoch(dataDir, rec); err != nil {
		return 0, fmt.Errorf("bump scope registry epoch: %w", err)
	}
	return rec.RegistryEpoch, nil
}

// sealScopeRegistryEpoch seals the registry: the invocation completed at a
// terminal run-record status, so the pod fleet the registry served is being
// torn down and every rotated token with it. Later adoption must refuse the
// directory and rotate fresh (KB-227). A directory with no marker is left
// alone: without scope sessions there is nothing to seal or adopt.
func sealScopeRegistryEpoch(dataDir, finalState string, success bool) error {
	prev, ok := readScopeRegistryEpoch(dataDir)
	if !ok {
		return nil
	}
	if prev.State == RegistryStateSealed {
		return nil
	}
	rec := registryEpochRecord{
		RegistryEpoch: prev.RegistryEpoch,
		State:         RegistryStateSealed,
		FinalState:    finalState,
		Success:       &success,
	}
	if err := writeScopeRegistryEpoch(dataDir, rec); err != nil {
		return fmt.Errorf("seal scope registry epoch: %w", err)
	}
	return nil
}

// PriorRunRegistrySealed reports whether a prior run data directory's scope
// registry is sealed (the invocation reached a terminal run status). The CLI
// adoptable-run-dirs computation mirrors the engine's adoption gate with this
// predicate so completed invocations are excluded from the candidate set
// before any token material is read; the engine-side gate remains the source
// of truth. Directories without a marker are never sealed.
func PriorRunRegistrySealed(dataDir string) bool {
	rec, ok := readScopeRegistryEpoch(dataDir)
	return ok && rec.State == RegistryStateSealed
}