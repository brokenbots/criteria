package adapterhost

// KB-224 (moved from Linear CRI-292; mirrored in
// brokenbots/criteria-adapter-copilot#31): a locally launched agent adapter
// (copilot) shares the criteria process's ambient home directory, so every
// criteria invocation on a workstation reads the same ambient session state
// (e.g. ~/.copilot/session-store.db). A Copilot CLI session that once resumed
// from that shared store can enter a state where builtin tool executions deny
// before the host permission bridge is consulted — the observed signature is a
// develop turn whose only outcome is submit_outcome(need_help) with "bash
// returned permission denied" and zero permission.* adapter events. In k8s
// pods, where ~/.copilot is fresh per pod, the failure has never been
// observed: fresh ambient state per invocation is the confirmed-healthy
// pattern.
//
// This file implements that pattern for local runs: every locally launched
// adapter instance whose adapter type is in ambientHomeIsolatedAdapterTypes
// gets its own scratch home directory for the lifetime of the criteria
// invocation. Within one invocation the same adapter instance keeps the same
// scratch home (adapter session continuity across steps is preserved, exactly
// as within one pod); the next criteria invocation allocates a new scratch
// root, so ambient state never crosses invocations. The scratch root is
// removed when the SessionManager shuts down.
//
// Because HOME is rewritten before the adapter process starts, everything the
// adapter runs — including the tools the copilot agent itself executes
// (git, gh, credential helpers) — also runs with the scratch home. The CLI's
// own authentication does not depend on it: the spawn flags the adapter hands
// the CLI pass the auth token via the environment
// (--auth-token-env COPILOT_SDK_AUTH_TOKEN) with --no-auto-login, so no
// credential is read from the store, and GH_TOKEN/GITHUB_TOKEN/WORKFLOW_GITHUB_TOKEN
// (used for git/gh pushes) pass through via the copied host environment. The
// identity- and helper-bearing operator files that are resolved relative to
// $HOME are preserved explicitly instead of copied (see
// populateCredentialEntries and buildAmbientHomeCustomizer): a scratch-home
// symlink keeps $HOME-relative paths resolvable, GIT_CONFIG_GLOBAL keeps the
// global git identity, and GH_CONFIG_DIR keeps gh auth. Ambient session state
// itself (notably ~/.copilot) is deliberately never linked, which is what
// makes the per-invocation isolation effective.
//
// Scope guard rails, mirroring the confirmed evidence:
//   - shell-type adapters are unaffected (their steps rely on operator
//     configuration living in the real home, and the ticket records them as
//     working throughout the incident);
//   - sandbox-, container-, and remote-bound adapters are unaffected (a
//     sandbox owns the adapter environment entirely, and container/remote
//     launches already get a fresh home per pod/instance);
//   - anything outside the isolated adapter types keeps today's behavior.

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/brokenbots/criteria/workflow"
)

// ambientHomeIsolatedAdapterTypes lists adapter types whose locally launched
// processes must not see the criteria process's ambient home directory. The
// set is deliberately small: only agent adapters whose out-of-process CLI
// keeps mutable session state under $HOME are affected by KB-224.
var ambientHomeIsolatedAdapterTypes = map[string]struct{}{
	"copilot": {},
}

// ambientHomeInstanceIDEnv is the environment variable under which a scratch
// home records which adapter instance it serves; useful for diagnostics when
// inspecting a scratch directory after a run.
const ambientHomeInstanceIDEnv = "CRITERIA_AMBIENT_HOME_INSTANCE"

// sanitizeAmbientHomeInstanceID maps an adapter instance ID to a single safe
// path element. Instance IDs look like "copilot.main"; anything outside
// [A-Za-z0-9._-] is collapsed to '_'. Collisions are harmless: two instances
// sharing one directory still differ from the host home, and the exposure
// only widens from per-instance to per-invocation.
func sanitizeAmbientHomeInstanceID(instanceID string) string {
	var b strings.Builder
	for _, r := range instanceID {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "adapter"
	}
	return b.String()
}

// ambientHomeAllocator hands out one scratch home directory per locally
// launched agent-adapter instance for the lifetime of one criteria
// invocation. The scratch root is created lazily on the first isolated
// launch and removed by cleanup at SessionManager shutdown.
type ambientHomeAllocator struct {
	mu    sync.Mutex
	root  string
	homes map[string]string
	// sourceHome overrides the operator home the credential/config entries
	// are read from; empty uses the criteria process's own $HOME. Injectable
	// in tests so the preservation logic never depends on the host's real
	// home layout.
	sourceHome string
	// rootDir overrides the parent directory of the scratch root; empty uses
	// the OS temp dir. Injectable in tests to exercise the fail-open path
	// when the scratch location is unwritable.
	rootDir string
}

// resolveSourceHomeDir returns the operator home the scratch home must
// preserve entries from. Must be called with the allocator lock held.
func (a *ambientHomeAllocator) resolveSourceHomeDir() string {
	if a.sourceHome != "" {
		return a.sourceHome
	}
	return os.Getenv("HOME")
}

// sourceHomeDir reports the operator home the allocator reads preserved
// entries from.
func (a *ambientHomeAllocator) sourceHomeDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.resolveSourceHomeDir()
}

// homeFor returns the scratch home directory for the given adapter instance,
// creating the scratch root and the per-instance directory on first use. The
// enabled result reports whether an isolated home applies at all; adapter
// types outside ambientHomeIsolatedAdapterTypes always disable it so the
// eligibility check lives next to the seam that consults it. adapterType is
// the graph-verified adapter type; the leading segment of the instance ID is
// only a fallback for callers without a graph.
func (a *ambientHomeAllocator) homeFor(instanceID, adapterType string) (home string, enabled bool, err error) {
	if _, ok := ambientHomeIsolatedAdapterTypes[adapterType]; !ok {
		if _, ok := ambientHomeIsolatedAdapterTypes[instanceIDType(instanceID)]; !ok {
			return "", false, nil
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.homes == nil {
		a.homes = map[string]string{}
	}
	if home, ok := a.homes[instanceID]; ok {
		return home, true, nil
	}
	if err := a.ensureRoot(); err != nil {
		return "", false, err
	}
	home = filepath.Join(a.root, sanitizeAmbientHomeInstanceID(instanceID))
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", false, fmt.Errorf("create ambient home for adapter %q: %w", instanceID, err)
	}
	a.populateCredentialEntries(home)
	a.homes[instanceID] = home
	return home, true, nil
}

// ensureRoot creates the invocation-wide scratch root. Idempotent; must be
// called with the allocator lock held.
func (a *ambientHomeAllocator) ensureRoot() error {
	if a.root != "" {
		if _, err := os.Stat(a.root); err == nil {
			return nil
		}
	}
	root, err := os.MkdirTemp(a.rootDir, "criteria-ambient-home-")
	if err != nil {
		return fmt.Errorf("create ambient home scratch root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return fmt.Errorf("restrict ambient home scratch root: %w", err)
	}
	a.root = root
	return nil
}

// cleanup removes the invocation's scratch root and everything under it. It
// is safe to call more than once and safe to call when no isolated adapter
// ever launched.
func (a *ambientHomeAllocator) cleanup() {
	a.mu.Lock()
	root := a.root
	a.root = ""
	a.homes = nil
	a.mu.Unlock()
	if root != "" {
		_ = os.RemoveAll(root)
	}
}

// ambientCredentialEntries lists operator-home paths that the agent's builtin
// tools resolve relative to $HOME and that are therefore linked into every
// scratch home when they exist in the operator home. Files are linked as
// files, directories as directories; a leading ".config/" segment is created
// (mode 0700) in the scratch home so the nested entry can be linked.
var ambientCredentialEntries = []string{
	".git-credentials",
	".netrc",
	".ssh",
	".config/git",
}

// populateCredentialEntries links the preserved credential/config entries
// from the operator home into the freshly created scratch home. Absent
// entries are skipped silently (a workstation without them is normal);
// entries that cannot be linked degrade the launch to "fresh home without
// that entry" with a warning rather than failing the allocation, because the
// isolation property itself (fresh ambient session state) must never depend
// on credential preservation succeeding. Ambient session state (notably
// ~/.copilot) has no entry here by design. Must be called with the allocator
// lock held.
func (a *ambientHomeAllocator) populateCredentialEntries(home string) {
	source := a.resolveSourceHomeDir()
	if source == "" {
		return
	}
	for _, rel := range ambientCredentialEntries {
		linkAmbientHomeEntry(home, source, rel)
	}
}

// linkAmbientHomeEntry creates a symlink at <home>/<rel> pointing at
// <source>/<rel> when the source exists. The symlink never copies content:
// the credentials stay owned by, readable under the mode bits of, and
// writable only by the same operator account the adapter already runs as.
func linkAmbientHomeEntry(home, source, rel string) {
	src := filepath.Join(source, rel)
	if _, err := os.Stat(src); err != nil {
		return
	}
	dst := filepath.Join(home, rel)
	if parent := filepath.Dir(dst); parent != home {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			slog.Warn("ambient home could not be prepared (continuing without preserving an operator entry)",
				"entry", rel, "error", err)
			return
		}
	}
	if err := os.Symlink(src, dst); err != nil {
		slog.Warn("ambient home could not preserve an operator entry (continuing without it)",
			"entry", rel, "error", err)
	}
}

// ambientGitGlobalConfigPath returns the path for GIT_CONFIG_GLOBAL when the
// operator home carries a global git config, or "" when it does not.
func ambientGitGlobalConfigPath(source string) string {
	path := filepath.Join(source, ".gitconfig")
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		return ""
	}
	return path
}

// ambientGhConfigDir returns the path for GH_CONFIG_DIR when the operator
// home carries a gh config directory (XDG_CONFIG_HOME-aware), or "" when it
// does not.
func ambientGhConfigDir(source string) string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(source, ".config")
	}
	dir := filepath.Join(base, "gh")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ""
	}
	return dir
}

// appendEnvIfAbsent appends key=value to env only when key is not already
// present — unlike replaceEnvValue it never overrides an operator-provided
// value.
func appendEnvIfAbsent(env []string, key, value string) []string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return env
		}
	}
	return append(env, prefix+value)
}

// buildAmbientHomeCustomizer returns a command customizer that launches the
// given locally resolved adapter instance with an isolated scratch home, or
// nil when the instance is not eligible. Eligibility is resolved against the
// graph: the adapter type must be in ambientHomeIsolatedAdapterTypes and the
// bound environment (if any) must be shell-type or absent — sandbox
// environments own the adapter environment entirely, and container/remote
// launches do not run on this host.
//
// The customizer preserves the host environment and rewrites only HOME plus
// the explicit credential/config pointers:
//   - HOME moves to the scratch home; ~/.copilot is absent there, which is
//     what makes the per-invocation isolation effective (the CLI's auth is
//     environment-based regardless, see the file header).
//   - GIT_CONFIG_GLOBAL points at the operator's global git config when one
//     exists, so the agent's git commits keep the operator's identity; an
//     operator-provided GIT_CONFIG_GLOBAL is never overridden.
//   - GH_CONFIG_DIR points at the operator's gh config directory (XDG-aware)
//     when one exists, so gh keeps working without a token environment
//     variable; an operator-provided GH_CONFIG_DIR is never overridden.
//   - HOME-relative tool state that cannot be redirected by an environment
//     variable (.git-credentials, .netrc, .ssh, .config/git) is linked from
//     the operator home into the scratch home at allocation time.
//
// It sets the environment itself because providing any customizer to the
// loader makes go-plugin skip its host-env re-addition (SkipHostEnv; see
// loader.go). On an allocation failure the customizer is not installed and
// the launch degrades to the un-isolated host environment with a warning: a
// read-only scratch location must degrade to today's behavior rather than
// wedge the run. The returned cleanup is a no-op by contract: the
// per-instance scratch home must outlive any single launch because it is
// reused across steps within the invocation; root removal is owned by
// ambientHomeAllocator.cleanup at SessionManager shutdown.
func (m *SessionManager) buildAmbientHomeCustomizer(instanceID string) (customizer func(name string, cmd *exec.Cmd), cleanup func()) {
	if m.ambientHomes == nil {
		return nil, nil
	}
	adapter, envNode := m.adapterAndEnvironment(instanceID)
	adapterType := ""
	if adapter != nil {
		adapterType = adapter.Type
	}
	if envNode != nil && envNode.Type != "" && envNode.Type != "shell" {
		return nil, nil
	}
	home, enabled, err := m.ambientHomes.homeFor(instanceID, adapterType)
	if err != nil {
		slog.Warn("ambient home isolation unavailable, launching with host environment",
			"instance", instanceID, "error", err)
		return nil, nil
	}
	if !enabled {
		return nil, nil
	}
	source := m.ambientHomes.sourceHomeDir()
	gitGlobal := ambientGitGlobalConfigPath(source)
	ghConfigDir := ambientGhConfigDir(source)
	customizer = func(name string, cmd *exec.Cmd) {
		cmd.Env = replaceEnvValue(os.Environ(), "HOME", home)
		if gitGlobal != "" {
			cmd.Env = appendEnvIfAbsent(cmd.Env, "GIT_CONFIG_GLOBAL", gitGlobal)
		}
		if ghConfigDir != "" {
			cmd.Env = appendEnvIfAbsent(cmd.Env, "GH_CONFIG_DIR", ghConfigDir)
		}
		cmd.Env = append(cmd.Env, ambientHomeInstanceIDEnv+"="+instanceID)
	}
	return customizer, func() {}
}

// adapterAndEnvironment resolves the adapter node and its bound environment
// node, tolerating a nil graph or unknown references. The DefaultEnvironment
// fallback mirrors how sandboxEnvAndPolicy binds an adapter without an
// explicit environment reference.
func (m *SessionManager) adapterAndEnvironment(instanceID string) (*workflow.AdapterNode, *workflow.EnvironmentNode) {
	if m.graph == nil || m.graph.Adapters == nil {
		return nil, nil
	}
	adapter, ok := m.graph.Adapters[instanceID]
	if !ok {
		return nil, nil
	}
	envKey := adapter.Environment
	if envKey == "" {
		envKey = m.graph.DefaultEnvironment
	}
	var envNode *workflow.EnvironmentNode
	if envKey != "" && m.graph.Environments != nil {
		envNode = m.graph.Environments[envKey]
	}
	return adapter, envNode
}

// instanceIDType returns the adapter-type segment of an instance ID
// ("type.name"), or the whole ID when there is no dot.
func instanceIDType(instanceID string) string {
	if idx := strings.IndexByte(instanceID, '.'); idx >= 0 {
		return instanceID[:idx]
	}
	return instanceID
}

// replaceEnvValue returns a copy of env with key set to value, appending when
// the key is absent. Existing entries for key are dropped so the value cannot
// be shadowed by duplicates.
func replaceEnvValue(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, prefix+value)
}
