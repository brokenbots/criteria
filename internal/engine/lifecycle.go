// Package engine provides lifecycle functions for automatic adapter provisioning and teardown.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// adapterLifecycleRecord holds the metadata needed to emit a controller-visible
// remote-adapter lifecycle event and to clean up the scope on teardown.
type adapterLifecycleRecord struct {
	scopeName       string
	scopeInstanceID string
	scopeKey        string
	adapterName     string
	adapterType     string
	envName         string
	digest          string
	tokenPath       string
	listenAddr      string
	perScope        bool
	runID           string
}

// remoteLifecycleContext carries the lockfile and per-scope provisioning state
// that the engine needs to emit remote adapter lifecycle events. It travels
// in RunState rather than Deps so that Deps stays small for node evaluators.
type remoteLifecycleContext struct {
	lockfile       *lockfile.Lockfile
	scopeLifecycle *scopeLifecycleState
}

// scopeLifecycleState tracks per-scope remote-adapter provisioning metadata.
type scopeLifecycleState struct {
	dataDir string
	runID   string
	mu      sync.Mutex
	records map[string]*adapterLifecycleRecord
}

func newScopeLifecycleState(dataDir string) *scopeLifecycleState {
	return &scopeLifecycleState{
		dataDir: dataDir,
		records: make(map[string]*adapterLifecycleRecord),
	}
}

func (ls *scopeLifecycleState) add(rec *adapterLifecycleRecord) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.records[rec.adapterName] = rec
}

func (ls *scopeLifecycleState) get(adapterName string) *adapterLifecycleRecord {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.records[adapterName]
}

func (ls *scopeLifecycleState) remove(adapterName string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	delete(ls.records, adapterName)
}

func (ls *scopeLifecycleState) setRunID(id string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.runID = id
}

func generateAcceptToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeRotatedToken(dataDir, scopeName, scopeInstanceID, adapterType, token string) (string, error) {
	// Route through the same checked path builder as the readers so the
	// validation rules cannot drift between reads and writes.
	path, err := rotatedTokenPath(dataDir, scopeName, scopeInstanceID, adapterType)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// currentScopeInstanceRecord persists the scope instance an adapter currently
// occupies, keyed by adapter instance within a scope. After a runner-pod
// restart mid-run, the restarted engine reuses the same scope instance ID and
// accept token so surviving adapter pods can re-handshake with the new
// runner's shim instead of being rejected forever (CRI-137). The record is
// cleared whenever the scope is torn down, so deliberate pause/resume flows
// still rotate fresh tokens.
type currentScopeInstanceRecord struct {
	ScopeInstanceID string `json:"scope_instance_id"`
	AdapterType     string `json:"adapter_type"`
}

// checkPathLabel rejects workflow-supplied labels that could escape the token
// directory when embedded in a file path. Labels come from HCL block names,
// which may be arbitrary quoted strings; an empty scopeName is the root scope
// and is fine.
func checkPathLabel(label string) error {
	if label == "" || label == "." {
		return nil
	}
	if label == ".." || strings.ContainsAny(label, `/\`) || strings.Contains(label, "..") || strings.ContainsRune(label, 0) {
		return fmt.Errorf("label %q is not usable as a token directory component", label)
	}
	return nil
}

func remoteTokensDir(dataDir, scopeName string) (string, error) {
	if err := checkPathLabel(scopeName); err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "remote-tokens", scopeName), nil
}

// rotatedTokenPath derives the on-disk path of a scope's accept token file.
// The record file never stores paths; they are always re-derived from
// validated components so a tampered record cannot point reads elsewhere.
func rotatedTokenPath(dataDir, scopeName, scopeInstanceID, adapterType string) (string, error) {
	dir, err := remoteTokensDir(dataDir, scopeName)
	if err != nil {
		return "", fmt.Errorf("scope %q: %w", scopeName, err)
	}
	if err := checkPathLabel(scopeInstanceID); err != nil {
		return "", fmt.Errorf("scope instance %q: %w", scopeInstanceID, err)
	}
	if err := checkPathLabel(adapterType); err != nil {
		return "", fmt.Errorf("adapter type %q: %w", adapterType, err)
	}
	return filepath.Join(dir, scopeInstanceID, adapterType+".token"), nil
}

func currentScopeInstancePath(dataDir, scopeName, adapterInstance string) (string, error) {
	dir, err := remoteTokensDir(dataDir, scopeName)
	if err != nil {
		return "", fmt.Errorf("scope %q: %w", scopeName, err)
	}
	if err := checkPathLabel(adapterInstance); err != nil {
		return "", fmt.Errorf("adapter instance %q: %w", adapterInstance, err)
	}
	return filepath.Join(dir, "current", adapterInstance+".json"), nil
}

// readCurrentScopeInstance loads and validates the persisted record for an
// adapter instance. A missing, corrupt, or structurally invalid record is an
// error; the caller falls back to a fresh rotation.
func readCurrentScopeInstance(dataDir, scopeName, adapterInstance string) (currentScopeInstanceRecord, error) {
	path, err := currentScopeInstancePath(dataDir, scopeName, adapterInstance)
	if err != nil {
		return currentScopeInstanceRecord{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return currentScopeInstanceRecord{}, err
	}
	var rec currentScopeInstanceRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return currentScopeInstanceRecord{}, err
	}
	if _, err := uuid.Parse(rec.ScopeInstanceID); err != nil {
		return currentScopeInstanceRecord{}, fmt.Errorf("scope_instance_id %q is not a UUID: %w", rec.ScopeInstanceID, err)
	}
	return rec, nil
}

// writeCurrentScopeInstance atomically persists the current scope instance for
// an adapter instance so a reader never observes a partial record.
func writeCurrentScopeInstance(dataDir, scopeName, adapterInstance string, rec currentScopeInstanceRecord) error {
	path, err := currentScopeInstancePath(dataDir, scopeName, adapterInstance)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
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

// releaseScopeInstance tombstones a deliberately released scope instance so
// the token-file scan cannot resurrect it on the next scope entry. The live
// record is dropped (rotating a fresh instance on re-entry, as before), the
// rotated token file is kept for post-run forensics, and a claim file under
// current/ records the released instance so it stays excluded from scan reuse
// even after later rotations overwrite the live record.
func releaseScopeInstance(dataDir, scopeName, adapterInstance, scopeInstanceID, adapterType string) {
	if err := writeCurrentScopeInstance(dataDir, scopeName, releasedScopeClaimName(scopeInstanceID), currentScopeInstanceRecord{
		ScopeInstanceID: scopeInstanceID,
		AdapterType:     adapterType,
	}); err != nil {
		slog.Warn("persisting released scope instance tombstone failed; a later token-file scan may reuse the released token",
			"scope", scopeName, "scope_instance", scopeInstanceID, "error", err.Error())
	}
	clearCurrentScopeInstance(dataDir, scopeName, adapterInstance)
}

// releasedScopeClaimName is the record file name under current/ holding a
// released scope instance's tombstone claim.
func releasedScopeClaimName(scopeInstanceID string) string {
	return "released-" + scopeInstanceID
}

// clearCurrentScopeInstance removes the persisted record for an adapter
// instance, forcing the next scope entry to rotate a fresh token.
func clearCurrentScopeInstance(dataDir, scopeName, adapterInstance string) {
	path, err := currentScopeInstancePath(dataDir, scopeName, adapterInstance)
	if err != nil {
		return
	}
	_ = os.Remove(path)
}

func remoteEnvConfig(g *workflow.FSMGraph, ad *workflow.AdapterNode) (*remote.Config, *workflow.EnvironmentNode, bool) {
	envKey := ad.Environment
	if envKey == "" {
		envKey = g.DefaultEnvironment
	}
	env := g.Environments[envKey]
	if env == nil || env.Type != "remote" {
		return nil, nil, false
	}
	cfg, err := remote.ParseConfig(env.RawBody)
	if err != nil {
		return nil, env, false
	}
	return cfg, env, true
}

func lockedDigest(lf *lockfile.Lockfile, adapterType, adapterName string) string {
	if lf == nil {
		return ""
	}
	for i := range lf.Adapters {
		a := &lf.Adapters[i]
		if a.Type == adapterType && a.Name == adapterName {
			return a.ResolvedDigest
		}
	}
	return ""
}

func emitProvisionWanted(deps Deps, lifecycle *remoteLifecycleContext, scopeName, scopeInstanceID, scopeKey, instanceID string, adapter *workflow.AdapterNode, envNode *workflow.EnvironmentNode, tokenPath string) {
	digest := lockedDigest(lifecycle.lockfile, adapter.Type, adapter.Name)
	listenAddr := deps.Sessions.RemoteListenAddr()
	deps.Sink.OnAdapterLifecycleEvent(&AdapterLifecycleEvent{
		RunID:             lifecycle.scopeLifecycle.runID,
		ScopeName:         scopeName,
		ScopeInstanceID:   scopeInstanceID,
		AdapterName:       adapter.Name,
		Digest:            digest,
		ShimListenAddress: listenAddr,
		TokenRef:          tokenPath,
		Status:            "provision_wanted",
	})
	lifecycle.scopeLifecycle.add(&adapterLifecycleRecord{
		scopeName:       scopeName,
		scopeInstanceID: scopeInstanceID,
		scopeKey:        scopeKey,
		adapterName:     instanceID,
		adapterType:     adapter.Type,
		envName:         envNode.Name,
		digest:          digest,
		tokenPath:       tokenPath,
		listenAddr:      listenAddr,
		perScope:        true,
		runID:           lifecycle.scopeLifecycle.runID,
	})
}

// maybeRotateRemoteScope rotates the accept token for a per-scope remote adapter
// and emits a provision-wanted lifecycle event. It returns the shim scope key to
// pass to Verify, or an empty string when per-scope sessions are disabled.
func maybeRotateRemoteScope(deps Deps, lifecycle *remoteLifecycleContext, g *workflow.FSMGraph, adapter *workflow.AdapterNode, instanceID, scopeName string) (string, error) {
	remCfg, envNode, ok := remoteEnvConfig(g, adapter)
	if !ok || !remCfg.PerScopeSessions {
		return "", nil
	}
	if lifecycle == nil || lifecycle.scopeLifecycle == nil || lifecycle.scopeLifecycle.dataDir == "" {
		err := fmt.Errorf("remote environment %q uses per_scope_sessions but no run data directory is configured", envNode.Name)
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
		return "", fmt.Errorf("initialize adapter %q: %w", instanceID, err)
	}
	dataDir := lifecycle.scopeLifecycle.dataDir

	// CRI-137: after a runner-pod restart mid-run, reuse the persisted scope
	// instance so surviving adapter pods can re-handshake with the restarted
	// runner's shim instead of being rejected with a freshly rotated token
	// forever. Any mismatch (missing token file, corrupt record, adapter type
	// change) falls through to a fresh rotation, which self-heals the record.
	scopeKey, reused, err := tryReuseScopeInstance(deps, lifecycle, envNode, adapter, instanceID, scopeName)
	if err != nil {
		return "", err
	}
	if reused {
		return scopeKey, nil
	}

	scopeInstanceID := uuid.NewString()
	scopeKey = scopeName + "/" + scopeInstanceID
	token, err := generateAcceptToken()
	if err != nil {
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
		return "", fmt.Errorf("initialize adapter %q: rotate accept token: %w", instanceID, err)
	}
	tokenPath, err := writeRotatedToken(dataDir, scopeName, scopeInstanceID, adapter.Type, token)
	if err != nil {
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
		return "", fmt.Errorf("initialize adapter %q: write accept token: %w", instanceID, err)
	}
	if err := writeCurrentScopeInstance(dataDir, scopeName, instanceID, currentScopeInstanceRecord{
		ScopeInstanceID: scopeInstanceID,
		AdapterType:     adapter.Type,
	}); err != nil {
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
		return "", fmt.Errorf("initialize adapter %q: persist scope instance: %w", instanceID, err)
	}
	if err := deps.Sessions.RegisterRemoteScope(scopeKey, token); err != nil {
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
		return "", fmt.Errorf("initialize adapter %q: register scope token: %w", instanceID, err)
	}
	emitProvisionWanted(deps, lifecycle, scopeName, scopeInstanceID, scopeKey, instanceID, adapter, envNode, tokenPath)
	return scopeKey, nil
}

// tryReuseScopeInstance reuses the scope instance persisted by a prior runner
// generation when the record is valid and its rotated token file is still
// readable, registering the token with the shim and emitting provision_wanted.
// When the record itself is missing or unusable, it scans the scope's token
// directory for rotated tokens left behind by the previous runner generation
// (the first restart after this code deploys has only the token files — the
// records did not exist yet), registering every surviving token so surviving
// pods remain valid, and reusing the newest unclaimed one. It returns
// reused=false when the caller should fall through to a fresh rotation, and a
// non-nil error only when reuse was possible but registration with the shim
// failed.
func tryReuseScopeInstance(deps Deps, lifecycle *remoteLifecycleContext, envNode *workflow.EnvironmentNode, adapter *workflow.AdapterNode, instanceID, scopeName string) (scopeKey string, reused bool, err error) {
	dataDir := lifecycle.scopeLifecycle.dataDir
	scopeInstanceID, tokenPath, token, reason, ok := reusableScopeToken(dataDir, scopeName, instanceID, adapter.Type)
	if ok {
		scopeKey = scopeName + "/" + scopeInstanceID
		if rerr := deps.Sessions.RegisterRemoteScope(scopeKey, token); rerr != nil {
			deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", rerr.Error())
			return "", false, fmt.Errorf("initialize adapter %q: register scope token: %w", instanceID, rerr)
		}
		emitProvisionWanted(deps, lifecycle, scopeName, scopeInstanceID, scopeKey, instanceID, adapter, envNode, tokenPath)
		return scopeKey, true, nil
	}
	logScopeReuseFallback(scopeName, instanceID, reason)

	// Record-based reuse is impossible: scan for rotated token files the
	// previous runner generation left behind and reuse the newest unclaimed
	// instance instead of rotating a fresh one.
	candidates, scanErr := scanScopeTokenFiles(dataDir, scopeName, adapter.Type)
	if scanErr != nil {
		slog.Warn("remote scope token scan failed; rotating fresh scope instance",
			"scope", scopeName, "adapter_instance", instanceID, "error", scanErr.Error())
		return "", false, nil
	}
	candidates = filterUnclaimedScopeInstances(dataDir, scopeName, candidates)
	if len(candidates) == 0 {
		return "", false, nil
	}
	chosen := candidates[0]
	// Self-heal the record before registering so a persist failure cleanly
	// falls through to a fresh rotation, whose own record write then fails
	// loudly instead of leaving an unregistered token behind.
	if werr := writeCurrentScopeInstance(dataDir, scopeName, instanceID, currentScopeInstanceRecord{
		ScopeInstanceID: chosen.instanceID,
		AdapterType:     adapter.Type,
	}); werr != nil {
		slog.Warn("persisting scanned scope instance failed; rotating fresh scope instance",
			"scope", scopeName, "adapter_instance", instanceID, "error", werr.Error())
		return "", false, nil
	}
	scopeKey = scopeName + "/" + chosen.instanceID
	if rerr := deps.Sessions.RegisterRemoteScope(scopeKey, chosen.token); rerr != nil {
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", rerr.Error())
		return "", false, fmt.Errorf("initialize adapter %q: register scope token: %w", instanceID, rerr)
	}
	// Register every other surviving token too so all of the previous
	// runner's pods remain valid across the restart, not just the one this
	// adapter reuses.
	for _, cand := range candidates[1:] {
		if rerr := deps.Sessions.RegisterRemoteScope(scopeName+"/"+cand.instanceID, cand.token); rerr != nil {
			slog.Warn("registering surviving scope token failed; the matching pod may fail its handshake",
				"scope", scopeName, "scope_instance", cand.instanceID, "error", rerr.Error())
		}
	}
	slog.Info("reusing scope instance recovered from surviving rotated token",
		"scope", scopeName, "adapter_instance", instanceID, "scope_instance", chosen.instanceID)
	emitProvisionWanted(deps, lifecycle, scopeName, chosen.instanceID, scopeKey, instanceID, adapter, envNode, chosen.tokenPath)
	return scopeKey, true, nil
}

// Reuse-fallback reasons surfaced by reusableScopeToken. A missing record is
// the normal first-entry shape; every other reason means a previous runner
// left something behind that could not be reused as-is.
const reuseFallbackNoRecord = "no persisted scope instance record"

func logScopeReuseFallback(scopeName, instanceID, reason string) {
	if reason == reuseFallbackNoRecord {
		slog.Info("no persisted scope instance record; scanning for surviving rotated tokens",
			"scope", scopeName, "adapter_instance", instanceID)
		return
	}
	slog.Warn("persisted scope instance record unusable; scanning for surviving rotated tokens",
		"scope", scopeName, "adapter_instance", instanceID, "reason", reason)
}

// scannedScopeToken is a rotated token file found under a scope directory,
// newest first after sorting.
type scannedScopeToken struct {
	instanceID string
	tokenPath  string
	token      string
	modified   time.Time
}

// scanScopeTokenFiles scans <dataDir>/remote-tokens/<scopeName>/ for rotated
// token files left behind by a prior runner generation. Only directory names
// that parse as UUIDs are considered instance directories ("current" holds
// records, not instances); each must contain a non-empty <adapterType>.token.
func scanScopeTokenFiles(dataDir, scopeName, adapterType string) ([]scannedScopeToken, error) {
	scopeDir, err := remoteTokensDir(dataDir, scopeName)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(scopeDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	found := make([]scannedScopeToken, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "current" {
			continue
		}
		if _, err := uuid.Parse(entry.Name()); err != nil {
			continue
		}
		path, err := rotatedTokenPath(dataDir, scopeName, entry.Name(), adapterType)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		found = append(found, scannedScopeToken{instanceID: entry.Name(), tokenPath: path, token: string(raw), modified: info.ModTime()})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].modified.After(found[j].modified) })
	return found, nil
}

// filterUnclaimedScopeInstances drops scan candidates whose instance is
// already referenced by a readable current record — another adapter's active
// claim or a deliberately released tombstone — so two adapters can never
// share one scope instance and a released token cannot be resurrected.
func filterUnclaimedScopeInstances(dataDir, scopeName string, candidates []scannedScopeToken) []scannedScopeToken {
	scopeDir, err := remoteTokensDir(dataDir, scopeName)
	if err != nil {
		return candidates
	}
	currentDir := filepath.Join(scopeDir, "current")
	entries, err := os.ReadDir(currentDir)
	if err != nil {
		return candidates
	}
	claimed := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		rec, err := readCurrentScopeInstance(dataDir, scopeName, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			continue
		}
		claimed[rec.ScopeInstanceID] = true
	}
	kept := candidates[:0]
	for _, cand := range candidates {
		if !claimed[cand.instanceID] {
			kept = append(kept, cand)
		}
	}
	return kept
}

// reusableScopeToken returns the persisted scope instance ID, token file path
// and token when the persisted record is valid for adapterType and the token
// file is still readable. On failure it returns a reason naming the check
// that failed (missing record, corrupt record, adapter-type mismatch, missing
// token file) so the fallback leaves a diagnostic trail, plus ok=false meaning
// the caller must rotate a fresh scope instance, which also self-heals an
// unusable record.
func reusableScopeToken(dataDir, scopeName, instanceID, adapterType string) (scopeInstanceID, tokenPath, token, reason string, ok bool) {
	rec, recErr := readCurrentScopeInstance(dataDir, scopeName, instanceID)
	if recErr != nil {
		if errors.Is(recErr, os.ErrNotExist) {
			return "", "", "", reuseFallbackNoRecord, false
		}
		return "", "", "", fmt.Sprintf("corrupt or unreadable record: %v", recErr), false
	}
	if rec.AdapterType != adapterType {
		return "", "", "", fmt.Sprintf("adapter type changed from %q to %q", rec.AdapterType, adapterType), false
	}
	path, pathErr := rotatedTokenPath(dataDir, scopeName, rec.ScopeInstanceID, rec.AdapterType)
	if pathErr != nil {
		return "", "", "", fmt.Sprintf("token path is invalid: %v", pathErr), false
	}
	tokenRaw, tokErr := os.ReadFile(path)
	if tokErr != nil {
		return "", "", "", fmt.Sprintf("token file unreadable: %v", tokErr), false
	}
	if len(tokenRaw) == 0 {
		return "", "", "", "token file is empty", false
	}
	return rec.ScopeInstanceID, path, string(tokenRaw), "", true
}

// initScopeAdapters provisions all adapters declared in the given FSMGraph at the start of its execution scope.
// Adapters are provisioned in declaration order (from AdapterOrder).
// If any adapter fails to initialize, all successfully provisioned adapters are torn down in reverse order,
// an event is emitted, and the error is returned.
// Returns the ordered slice of provisioned adapter IDs (for correct LIFO teardown)
// and an error if any adapter failed to initialize.
func initScopeAdapters(ctx context.Context, g *workflow.FSMGraph, deps Deps, vars map[string]cty.Value, workflowDir, scopeName string, secretOrigins map[string]secrets.OriginRef, lifecycle *remoteLifecycleContext) (order []string, err error) {
	if len(g.Adapters) == 0 {
		return nil, nil
	}

	provisioned := make([]string, 0, len(g.Adapters)) // track in order for LIFO rollback

	// Provision adapters in declaration order (from AdapterOrder)
	for _, instanceID := range g.AdapterOrder {
		adapter := g.Adapters[instanceID]

		// Prepare the adapter inputs (secrets, origin refs, working dir, runtime
		// config). A prepare error means the adapter was never opened, so we emit
		// init_failed and return without rolling back already-provisioned peers.
		config, secretMap, originRefs, workingDir, perr := prepareScopeAdapter(ctx, g, instanceID, adapter, vars, workflowDir, deps, scopeName, secretOrigins)
		if perr != nil {
			return nil, perr
		}

		// Reject working_directory values that are structurally invalid before any
		// step runs. A path containing ".." or falling outside the configured
		// allowed roots is an error that a later step cannot fix; only a missing
		// directory is deferred to session binding.
		if fvErr := deps.Sessions.ValidateWorkingDirFaceValue(workingDir); fvErr != nil {
			deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", fvErr.Error())
			return nil, fmt.Errorf("initialize adapter %q: %w", instanceID, fvErr)
		}

		// CRI-115: when the bound remote environment enables per-scope sessions,
		// rotate a fresh accept token for this scope, persist it under the run
		// data directory, register it with the shim, and emit a provision-wanted
		// event *before* blocking on WaitForHandle.
		verifyScope, err := maybeRotateRemoteScope(deps, lifecycle, g, adapter, instanceID, scopeName)
		if err != nil {
			return nil, err
		}

		verifyErr := deps.Sessions.Verify(ctx, instanceID, adapter.Type, adapter.OnCrash, config, secretMap, originRefs, workingDir, scopeName, verifyScope)

		// Silently swallow ErrSessionAlreadyOpen to support subworkflow bodies that
		// re-declare parent adapters for safety through re-declaration. Same-scope
		// duplicate adapters are rejected at compile time by compileAdapters
		// (in workflow/compile_adapters.go:57-61), so already-open here always means
		// a parent-scope adapter being re-opened in a child scope.
		// Only adapters we newly verified are tracked for teardown.
		if verifyErr != nil && !errors.Is(verifyErr, adapterhost.ErrSessionAlreadyOpen) {
			// Rollback: tear down any sessions that were already bound. Verified-only
			// records hold no process, so they need no explicit cleanup.
			for i := len(provisioned) - 1; i >= 0; i-- {
				_ = deps.Sessions.Close(ctx, provisioned[i]) // ignore teardown errors during rollback
			}
			deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", verifyErr.Error())
			return nil, fmt.Errorf("initialize adapter %q: %w", instanceID, verifyErr)
		}
		// Only track adapters that we newly verified (not already-verified ones)
		// This prevents tearing down adapters that belong to a parent scope.
		if verifyErr == nil {
			provisioned = append(provisioned, instanceID)
			deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "verified", "")
		}
	}

	return provisioned, nil
}

// prepareScopeAdapter resolves everything an adapter needs before it is opened:
// secrets, origin refs, the bound environment's working directory, and the
// runtime-evaluated config. On any failure it emits an init_failed lifecycle
// event and returns a wrapped error; the caller must return that error without
// rolling back already-provisioned adapters (the adapter was never opened).
func prepareScopeAdapter(
	ctx context.Context,
	g *workflow.FSMGraph,
	instanceID string,
	adapter *workflow.AdapterNode,
	vars map[string]cty.Value,
	workflowDir string,
	deps Deps,
	scopeName string,
	secretOrigins map[string]secrets.OriginRef,
) (config, secretMap map[string]string, originRefs map[string]secrets.OriginRef, workingDir string, err error) {
	// Resolve adapter secrets (WS13).
	if len(adapter.Secrets) > 0 {
		secretMap, err = resolveAdapterSecrets(ctx, g, adapter, vars, deps.Sessions.RedactionRegistry)
		if err != nil {
			deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
			return nil, nil, nil, "", fmt.Errorf("initialize adapter %q: %w", instanceID, err)
		}
	}

	originRefs, err = buildOriginRefs(adapter, secretOrigins)
	if err != nil {
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
		return nil, nil, nil, "", fmt.Errorf("initialize adapter %q: %w", instanceID, err)
	}

	// Resolve the bound environment's working_directory against the runtime
	// closure now, at adapter init, so the cwd can be dynamic (e.g.
	// var.worktree supplied via --var). The resolved value becomes the
	// adapter process launch cwd.
	workingDir, err = resolveAdapterWorkingDir(g, adapter, vars)
	if err != nil {
		deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", err.Error())
		return nil, nil, nil, "", fmt.Errorf("initialize adapter %q: resolve working_directory: %w", instanceID, err)
	}

	// Re-evaluate adapter config against runtime vars so that var.* references
	// in config blocks resolve to actual runtime values, not compile-time defaults.
	// file() content is served from the graph's compile-time cache so prompt
	// files cannot be altered mid-run.
	config = adapter.Config
	if len(adapter.ConfigExprs) > 0 {
		opts := workflow.DefaultFunctionOptions(workflowDir)
		opts.FileCache = g.FileCache
		runtimeConfig, evalErr := workflow.ResolveInputExprsWithOpts(
			adapter.ConfigExprs, vars, opts,
		)
		if evalErr != nil {
			deps.Sink.OnAdapterLifecycle(scopeName, instanceID, "init_failed", evalErr.Error())
			return nil, nil, nil, "", fmt.Errorf("initialize adapter %q: evaluate config: %w", instanceID, evalErr)
		}
		config = runtimeConfig
	}

	return config, secretMap, originRefs, workingDir, nil
}

// resolveAdapterWorkingDir resolves the working_directory of the environment
// bound to the adapter (its declared environment, or the workflow default)
// against the runtime vars. It returns "" when no environment is bound or the
// environment declares no working_directory.
func resolveAdapterWorkingDir(g *workflow.FSMGraph, ad *workflow.AdapterNode, vars map[string]cty.Value) (string, error) {
	envKey := ad.Environment
	if envKey == "" {
		envKey = g.DefaultEnvironment
	}
	if envKey == "" {
		return "", nil
	}
	return g.Environments[envKey].ResolveWorkingDir(vars)
}

// buildOriginRefs maps an adapter's secret expressions to the declared origins
// of the referenced secret variables or data blocks. This keeps raw secret
// values out of session snapshots while still allowing them to re-resolve on
// resume (WS18, CRI-88).
func buildOriginRefs(adapter *workflow.AdapterNode, secretOrigins map[string]secrets.OriginRef) (map[string]secrets.OriginRef, error) {
	if len(adapter.Secrets) == 0 {
		return nil, nil
	}
	originRefs := make(map[string]secrets.OriginRef, len(adapter.Secrets))
	for k, expr := range adapter.Secrets {
		isVar, key, ok := workflow.SecretBindingRefFromExpr(expr, nil)
		if !ok {
			return nil, fmt.Errorf("adapter %q.%q secrets.%s: cannot build origin ref: expression is not a direct secret reference", adapter.Type, adapter.Name, k)
		}
		var origin secrets.OriginRef
		if isVar {
			origin = secretOrigins[key]
		} else {
			parts := strings.SplitN(key, ".", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("adapter %q.%q secrets.%s: invalid data origin key %q", adapter.Type, adapter.Name, k, key)
			}
			origin = secretOrigins["data."+key]
		}
		if origin.Kind == "" {
			// No recorded origin means the value was a literal default. Persist it
			// as a literal origin so snapshots remain restorable. This path is
			// intended only for narrowly-controlled test fixtures.
			origin = secrets.OriginRef{Kind: "literal", Ref: "(untracked)"}
		}
		originRefs[k] = origin
	}
	return originRefs, nil
}

// tearDownScopeAdapters releases all adapter sessions in the given order in reverse (LIFO).
// The order slice must be the one returned by initScopeAdapters to ensure correct teardown order.
// Errors during teardown are logged via the adapter lifecycle sink but do not change the run's terminal state.
// Always called, even if the run errored or was cancelled.
// Uses context.WithoutCancel to ensure teardown completes even if the run context was canceled.
func tearDownScopeAdapters(ctx context.Context, order []string, deps Deps, lifecycle *remoteLifecycleContext) {
	if len(order) == 0 {
		return
	}

	// Use context.WithoutCancel to detach from parent cancellation,
	// ensuring cleanup runs even if the main run context was cancelled.
	cleanupCtx := context.WithoutCancel(ctx)

	// Teardown in reverse order (LIFO)
	for i := len(order) - 1; i >= 0; i-- {
		adapterID := order[i]

		// CRI-115: emit a release event for per-scope remote adapters before
		// closing the session, then unregister the scope token so a torn-down
		// pod cannot reconnect with the old token.
		if lifecycle != nil && lifecycle.scopeLifecycle != nil {
			if rec := lifecycle.scopeLifecycle.get(adapterID); rec != nil && rec.perScope {
				deps.Sink.OnAdapterLifecycleEvent(&AdapterLifecycleEvent{
					RunID:             rec.runID,
					ScopeName:         rec.scopeName,
					ScopeInstanceID:   rec.scopeInstanceID,
					AdapterName:       rec.adapterName,
					Digest:            rec.digest,
					ShimListenAddress: rec.listenAddr,
					TokenRef:          rec.tokenPath,
					Status:            "released",
				})
				_ = deps.Sessions.UnregisterRemoteScope(rec.scopeKey)
				_ = deps.Sessions.CloseRemoteHandle(cleanupCtx, rec.adapterType, rec.scopeKey)
				// CRI-137: tombstone the persisted scope instance so the
				// next deliberate scope entry rotates a fresh token instead
				// of the token-file scan resurrecting the released one. The
				// rotated token file is kept for post-run forensics.
				releaseScopeInstance(lifecycle.scopeLifecycle.dataDir, rec.scopeName, rec.adapterName, rec.scopeInstanceID, rec.adapterType)
				lifecycle.scopeLifecycle.remove(adapterID)
			}
		}

		err := deps.Sessions.Close(cleanupCtx, adapterID)
		if err != nil {
			// Emit lifecycle event for the failure but don't abort
			deps.Sink.OnAdapterLifecycle("", adapterID, "close_failed", err.Error())
		} else {
			// Emit successful close event
			deps.Sink.OnAdapterLifecycle("", adapterID, "closed", "")
		}
	}
}
