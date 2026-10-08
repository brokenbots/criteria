package peer

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

// Multi-adapter manifest parsing (KB-213). A peer container hosts ALL
// adapter children of one environment behind one phone-home substrate. The
// adapter set is declared env-first with two mutually-exclusive sources:
//
//   - CRITERIA_REMOTE_ADAPTERS: a comma-separated list of adapter names, optionally
//     "NAME=PATH" to pin a binary path per entry; or
//   - CRITERIA_REMOTE_ADAPTERS_DIR: a directory whose criteria-adapter-* binaries
//     are each hosted as one child.
//
// Per-adapter overrides refine either source:
// CRITERIA_ADAPTER_<NAME>_BINARY/_VERSION/_DIGEST/_MANIFEST, where <NAME> is
// the adapter name uppercased with every non-alphanumeric run replaced by
// underscores. In manifest mode the legacy single-adapter variables are only
// a fallback for the legacy one-child shape and per-adapter digests (not the
// global CRITERIA_REMOTE_DIGEST) pin each child.

// AdapterSpec describes one adapter child the peer hosts.
type AdapterSpec struct {
	Name     string
	Version  string
	Binary   string
	Digest   string
	Manifest string
}

// ScopeTokenSpec is one (scope, adapter) phone-home conn discovered in the
// runner's remote-tokens root. Token file names are "<adapterType>.token".
type ScopeTokenSpec struct {
	// Scope is the scope key: "<scopeName>/<instanceID>", or "/<instanceID>"
	// for the run's root scope (an empty workflow scope label yields a
	// leading slash in the runner's token layout).
	Scope string
	// Adapter is the adapter type the token file is minted for (the
	// "<adapterType>" half of "<adapterType>.token").
	Adapter string
	// Token is the file's token value (whitespace-trimmed).
	Token string
	// TokenPath is the file the token was read from (diagnostics).
	TokenPath string
}

// ParseAdaptersConfig parses the multi-adapter manifest from the environment
// (env-first): the explicit CRITERIA_REMOTE_ADAPTERS list wins; the binary
// directory scan is the fallback; per-adapter overrides refine both. An
// empty result means the legacy single-adapter shape was declared. A
// duplicate adapter name is an error, never a silent co-host.
func ParseAdaptersConfig(getenv func(string) string) ([]AdapterSpec, error) {
	raw := strings.TrimSpace(getenv(EnvAdapters))
	if raw != "" {
		specs, err := ParseAdaptersEnv(raw)
		if err != nil {
			return nil, err
		}
		applyAdapterSpecOverrides(getenv, specs)
		return specs, nil
	}
	if dir := strings.TrimSpace(getenv(EnvAdaptersDir)); dir != "" {
		specs, err := scanAdapterBinaries(dir)
		if err != nil {
			return nil, err
		}
		applyAdapterSpecOverrides(getenv, specs)
		return specs, nil
	}
	return nil, nil
}

// ParseAdaptersEnv parses the CRITERIA_REMOTE_ADAPTERS value: a comma-separated
// list of adapter names, each optionally "NAME=PATH" to pin the binary path.
// Entries are trimmed; empty entries are skipped.
func ParseAdaptersEnv(raw string) ([]AdapterSpec, error) {
	parts := strings.Split(raw, ",")
	specs := make([]AdapterSpec, 0, len(parts))
	seen := make(map[string]bool)
	for _, entry := range parts {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, binary, hasPath := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("%s entry %q has an empty adapter name", EnvAdapters, entry)
		}
		if hasPath {
			binary = strings.TrimSpace(binary)
			if binary == "" {
				return nil, fmt.Errorf("%s entry %q pins an empty binary path", EnvAdapters, entry)
			}
		}
		if seen[name] {
			return nil, fmt.Errorf("%s declares adapter %q more than once", EnvAdapters, name)
		}
		seen[name] = true
		spec := AdapterSpec{Name: name}
		if hasPath {
			spec.Binary = binary
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// scanAdapterBinaries scans dir for conventional adapter binaries and returns
// one spec per binary, name derived from the criteria-adapter- prefix (or the
// file name). Non-directories other than the remote runner itself qualify,
// matching the single-adapter PATH scan shape; the returned list is sorted by
// name for deterministic child boot order.
func scanAdapterBinaries(dir string) ([]AdapterSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s %q: %w", EnvAdaptersDir, dir, err)
	}
	specs := make([]AdapterSpec, 0, len(entries))
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		base := entry.Name()
		if !strings.HasPrefix(base, adapterBinaryPrefix) ||
			strings.Contains(base, "remote-runner") {
			continue
		}
		name := nameFromBinary(base)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		specs = append(specs, AdapterSpec{
			Name:   name,
			Binary: filepath.Join(dir, base),
		})
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, nil
}

// applyAdapterSpecOverrides applies the per-adapter CRITERIA_ADAPTER_<NAME>_*
// override variables to each spec. Overrides only fill or replace fields;
// digest and manifest overrides may also backfill an entry that only
// declared a name.
func applyAdapterSpecOverrides(getenv func(string) string, specs []AdapterSpec) {
	for i := range specs {
		envToken := adapterEnvName(specs[i].Name)
		if envToken == "" {
			continue
		}
		setIfEmpty := func(field, override string) string {
			if field == "" {
				return override
			}
			return field
		}
		prefix := "CRITERIA_ADAPTER_" + envToken + "_"
		specs[i].Binary = setIfEmpty(specs[i].Binary, strings.TrimSpace(getenv(prefix+"BINARY")))
		specs[i].Version = setIfEmpty(specs[i].Version, strings.TrimSpace(getenv(prefix+"VERSION")))
		specs[i].Digest = setIfEmpty(specs[i].Digest, strings.TrimSpace(getenv(prefix+"DIGEST")))
		specs[i].Manifest = setIfEmpty(specs[i].Manifest, strings.TrimSpace(getenv(prefix+"MANIFEST")))
	}
}

// adapterEnvName normalizes an adapter name for a CRITERIA_ADAPTER_<NAME>_*
// override variable: uppercased, every non-alphanumeric character replaced
// with an underscore.
func adapterEnvName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ResolveAdapterSpec resolves one adapter spec with the same precedence as
// the runner (manifest → override binary → conventional install path → PATH
// lookup of criteria-adapter-<name>). The first-unrelated-PATH-binary
// fallback of the single-adapter shape does not apply here: an ambiguous
// multi-child manifest is a configuration error, not a name to guess.
func ResolveAdapterSpec(spec *AdapterSpec) error {
	if err := applySpecManifestDefaults(spec); err != nil {
		return err
	}
	if spec.Binary == "" {
		if err := locateSpecBinary(spec); err != nil {
			return err
		}
	}
	if spec.Name == "" {
		spec.Name = nameFromBinary(spec.Binary)
	}
	if spec.Version == "" {
		spec.Version = DefaultVersion
	}
	if err := resolveSpecDigest(spec); err != nil {
		return err
	}
	return validateSpecBinary(spec)
}

// applySpecManifestDefaults seeds Name/Version/Binary from the adapter
// manifest when the manifest path is set and the fields are unset.
func applySpecManifestDefaults(spec *AdapterSpec) error {
	if spec.Manifest == "" {
		return nil
	}
	m, err := manifest.ParseFile(spec.Manifest)
	if err != nil {
		return fmt.Errorf("read manifest %q: %w", spec.Manifest, err)
	}
	if spec.Name == "" {
		spec.Name = m.Name
	}
	if spec.Version == "" {
		spec.Version = m.Version
	}
	if spec.Binary == "" {
		spec.Binary = defaultBinaryPath(m.Name)
	}
	return nil
}

// locateSpecBinary finds the binary for a name-only spec: PATH lookup of
// criteria-adapter-<name> first, then the conventional install path.
func locateSpecBinary(spec *AdapterSpec) error {
	if spec.Name == "" {
		return nil
	}
	if p, err := exec.LookPath(adapterBinaryPrefix + spec.Name); err == nil {
		spec.Binary = p
	}
	if spec.Binary == "" {
		spec.Binary = defaultBinaryPath(spec.Name)
	}
	if spec.Binary == "" {
		return fmt.Errorf("could not locate an adapter binary for %q; set CRITERIA_ADAPTER_%s_BINARY",
			spec.Name, adapterEnvName(spec.Name))
	}
	return nil
}

// validateSpecBinary resolves bare names against PATH and fails closed when
// the binary is missing: half-booting N-1 children would leave the
// undeclared adapter's sessions waiting on a dead dial instead of a loud
// startup error.
func validateSpecBinary(spec *AdapterSpec) error {
	if !strings.Contains(spec.Binary, string(filepath.Separator)) {
		p, err := exec.LookPath(spec.Binary)
		if err != nil {
			return fmt.Errorf("adapter binary %q not found on PATH: %w", spec.Binary, err)
		}
		spec.Binary = p
	}
	if info, err := os.Stat(spec.Binary); err != nil || info.IsDir() {
		return fmt.Errorf("adapter binary %q for adapter %q is missing or not a regular file", spec.Binary, spec.Name)
	}
	return nil
}

// resolveSpecDigest prefers the digest-addressed binary from the local OCI
// cache when the spec carries a pinned digest. No network pull happens here:
// pulling the artifact is the operator's job (runner parity). When no pinned
// artifact exists locally the resolved binary is kept and the digest is
// still recorded on spawn events for host-side identity.
func resolveSpecDigest(spec *AdapterSpec) error {
	if spec.Digest == "" || spec.Name == "" {
		return nil
	}
	d, err := digest.Parse(spec.Digest)
	if err != nil {
		return fmt.Errorf("invalid digest for adapter %q: %w", spec.Name, err)
	}
	pinned, err := adapterhost.DiscoverBinaryAt(spec.Name, adapterhost.EncodeDigest(d))
	if err != nil {
		var notFound *adapterhost.ErrAdapterNotFound
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("resolve pinned binary for adapter %q: %w", spec.Name, err)
	}
	spec.Binary = pinned
	return nil
}

// ScanRemoteScopes walks dir as the runner's remote-tokens root and returns
// one spec per (scope, adapter) token file, mirroring the runner token
// layout: "<dir>/<scopeName>/<instanceID>/<adapterType>.token" for workflow
// scopes and "<dir>/<instanceID>/<adapterType>.token" for the run's root
// scope (the empty scope label yields a leading slash). The "current"
// record directories are skipped, and instance directories that are not
// UUID-named are ignored exactly like the engine's own token-file scan.
// The result is sorted by (scope, adapter) for deterministic boot order.
func ScanRemoteScopes(dir string) ([]ScopeTokenSpec, error) {
	top, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s %q: %w", EnvRemoteScopesDir, dir, err)
	}
	specs, err := scanRemoteScopesEntries(dir, top)
	if err != nil {
		return nil, err
	}
	sort.Slice(specs, func(i, j int) bool {
		if specs[i].Scope != specs[j].Scope {
			return specs[i].Scope < specs[j].Scope
		}
		return specs[i].Adapter < specs[j].Adapter
	})
	return specs, nil
}

func scanRemoteScopesEntries(dir string, top []os.DirEntry) ([]ScopeTokenSpec, error) {
	specs := make([]ScopeTokenSpec, 0, len(top))
	for _, topEntry := range top {
		if topEntry.Name() == "current" || !topEntry.IsDir() {
			continue
		}
		if err := checkPathLabelScan(topEntry.Name()); err != nil {
			return nil, fmt.Errorf("%s contains unusable entry %q: %w", EnvRemoteScopesDir, topEntry.Name(), err)
		}
		scopePath := filepath.Join(dir, topEntry.Name())
		appendRootScopeTokens(&specs, scopePath, topEntry)
		if err := appendWorkflowScopeTokens(&specs, topEntry, scopePath); err != nil {
			return nil, err
		}
	}
	return specs, nil
}

// appendRootScopeTokens appends the root-scope shape: the first-level
// directory IS the instance UUID and holds the token files directly.
func appendRootScopeTokens(specs *[]ScopeTokenSpec, scopePath string, topEntry os.DirEntry) {
	tokens, err := readScopeTokenDir(scopePath)
	if err != nil {
		return
	}
	if len(tokens) == 0 || uuid.Validate(topEntry.Name()) != nil {
		return
	}
	for _, tok := range tokens {
		*specs = append(*specs, ScopeTokenSpec{
			Scope:     "/" + topEntry.Name(),
			Adapter:   tok.adapter,
			Token:     tok.token,
			TokenPath: tok.path,
		})
	}
}

// appendWorkflowScopeTokens appends the workflow-scope shape: the first-level
// directory is the scope label and UUID subdirectories hold instance tokens.
func appendWorkflowScopeTokens(specs *[]ScopeTokenSpec, topEntry os.DirEntry, scopePath string) error {
	instances, err := os.ReadDir(scopePath)
	if err != nil {
		return fmt.Errorf("read %s %q: %w", EnvRemoteScopesDir, scopePath, err)
	}
	for _, instEntry := range instances {
		if instEntry.Name() == "current" || !instEntry.IsDir() || uuid.Validate(instEntry.Name()) != nil {
			continue
		}
		tokens, err := readScopeTokenDir(filepath.Join(scopePath, instEntry.Name()))
		if err != nil {
			return err
		}
		for _, tok := range tokens {
			*specs = append(*specs, ScopeTokenSpec{
				Scope:     topEntry.Name() + "/" + instEntry.Name(),
				Adapter:   tok.adapter,
				Token:     tok.token,
				TokenPath: tok.path,
			})
		}
	}
	return nil
}

// scopeTokenFile is one <adapterType>.token file found in a token directory
// (internal to the scanner, mirroring <scope-dir>/<uuid> contents).
type scopeTokenFile struct {
	adapter string
	token   string
	path    string
}

// readScopeTokenDir reads every "<adapter>.token" file directly in tokenDir.
func readScopeTokenDir(tokenDir string) ([]scopeTokenFile, error) {
	entries, err := os.ReadDir(tokenDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read token directory %q: %w", tokenDir, err)
	}
	found := make([]scopeTokenFile, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".token") || entry.IsDir() {
			continue
		}
		adapter := strings.TrimSuffix(name, ".token")
		if adapter == "" {
			continue
		}
		if err := checkPathLabelScan(adapter); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(tokenDir, name))
		if err != nil || len(raw) == 0 {
			continue
		}
		found = append(found, scopeTokenFile{
			adapter: adapter,
			token:   strings.TrimSpace(string(raw)),
			path:    filepath.Join(tokenDir, name),
		})
	}
	return found, nil
}

// checkPathLabelScan mirrors the engine's checkPathLabel for scanned
// directory components.
func checkPathLabelScan(label string) error {
	if label == "" || label == "." {
		return nil
	}
	if label == ".." || strings.ContainsAny(label, `/\`) || strings.Contains(label, "..") || strings.ContainsRune(label, 0) {
		return fmt.Errorf("label %q is not usable as a token directory component", label)
	}
	return nil
}
