package adapterhost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"

	"github.com/brokenbots/criteria/internal/dirs"
)

const (
	adapterBinaryPrefix = "criteria-adapter-"
	adaptersEnvVar      = "CRITERIA_ADAPTERS"
)

var ErrInvalidAdapterName = errors.New("invalid adapter name")

// ErrAdapterNotFound reports that adapter discovery failed after checking all
// configured adapter directories.
type ErrAdapterNotFound struct {
	Name     string
	Searched []string
}

func (e *ErrAdapterNotFound) Error() string {
	if e == nil {
		return "adapter not found"
	}
	if len(e.Searched) == 0 {
		return fmt.Sprintf("adapter %q not found", e.Name)
	}
	return fmt.Sprintf("adapter %q not found (searched: %s)", e.Name, strings.Join(e.Searched, ", "))
}

// adaptersRoots returns the directories that hold installed adapter binaries,
// in search order: $CRITERIA_ADAPTERS (if set) then $CRITERIA_HOME/adapters.
func adaptersRoots() []string {
	return AdaptersRoots()
}

// AdaptersRoots returns the directories that hold installed adapter binaries,
// in search order: $CRITERIA_ADAPTERS (if set) then $CRITERIA_HOME/adapters.
// It is exported so diagnostics can report the paths that were consulted.
func AdaptersRoots() []string {
	roots := make([]string, 0, 2)
	if envDir := strings.TrimSpace(os.Getenv(adaptersEnvVar)); envDir != "" {
		roots = append(roots, envDir)
	}
	defaultDir, err := dirs.DefaultAdaptersDir()
	if err == nil && defaultDir != "" {
		// Avoid listing the same directory twice if CRITERIA_ADAPTERS happens
		// to match the default.
		if len(roots) == 0 || roots[0] != defaultDir {
			roots = append(roots, defaultDir)
		}
	}
	return roots
}

// InstallRoot returns the primary directory adapter binaries are written to:
// $CRITERIA_ADAPTERS if set, otherwise $CRITERIA_HOME/adapters.
func InstallRoot() (string, error) {
	return dirs.AdaptersDir()
}

// EncodeDigest renders a digest as a filesystem-safe directory segment, e.g.
// "sha256:abc…" -> "sha256-abc…".
func EncodeDigest(d digest.Digest) string {
	return strings.ReplaceAll(d.String(), ":", "-")
}

// AdapterBinaryName renders the conventional binary basename for an adapter,
// e.g. "claude-agent" -> "criteria-adapter-claude-agent".
func AdapterBinaryName(name string) string {
	return adapterBinaryPrefix + name
}

// AdapterInstallPath returns the on-disk path where the binary for adapterType
// pinned to digestEncoded is installed under the primary install root.
func AdapterInstallPath(adapterType, digestEncoded string) (string, error) {
	root, err := InstallRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, digestEncoded, AdapterBinaryName(adapterType)), nil
}

// DiscoverBinary resolves an adapter binary by type from the flat install root
// (used for dev/test bindings that are not digest-pinned). For OCI adapters
// pinned in the lockfile, prefer DiscoverBinaryAt.
//
// name may be a bare adapter type ("noop") or an adapter DEFINITION identity
// ("noop.demo", KB-242); binaries on the flat install root are keyed by type,
// so the definition half is stripped for lookup (KB-246, mirroring the #527
// lockfileDigestVerifier split) while the definition identity stays the
// child key.
//
// Discovery intentionally does not consult PATH to avoid unintentionally
// executing similarly named binaries from user/system toolchains.
func DiscoverBinary(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("adapter name is required")
	}
	if !IsValidAdapterName(name) {
		return "", fmt.Errorf("%w %q", ErrInvalidAdapterName, name)
	}
	binary := AdapterBinaryName(definitionType(name))
	roots := adaptersRoots()
	searched := make([]string, 0, len(roots))
	for _, root := range roots {
		candidate := filepath.Join(root, binary)
		searched = append(searched, candidate)
		if IsRunnableFile(candidate) {
			return candidate, nil
		}
	}
	return "", &ErrAdapterNotFound{Name: name, Searched: searched}
}

// DiscoverBinaryAt resolves the installed binary for an adapter pinned to a
// specific resolved digest (digestEncoded as produced by EncodeDigest). This is
// the digest-addressed path that lets multiple versions of the same adapter
// type coexist.
//
// name may be a bare adapter type ("noop") or an adapter DEFINITION identity
// ("noop.demo", KB-242): digest-pinned children are keyed by definition, but
// binaries under a digest directory are installed by type, so lookup strips
// the definition half (KB-246, mirroring the #527 lockfileDigestVerifier
// split) while the definition identity stays the child key.
func DiscoverBinaryAt(name, digestEncoded string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("adapter type is required")
	}
	if !IsValidAdapterName(name) {
		return "", fmt.Errorf("%w %q", ErrInvalidAdapterName, name)
	}
	binary := AdapterBinaryName(definitionType(name))
	roots := adaptersRoots()
	searched := make([]string, 0, len(roots))
	for _, root := range roots {
		candidate := filepath.Join(root, digestEncoded, binary)
		searched = append(searched, candidate)
		if IsRunnableFile(candidate) {
			return candidate, nil
		}
	}
	return "", &ErrAdapterNotFound{Name: name, Searched: searched}
}

// IsRunnableFile reports whether path is an executable regular file.
func IsRunnableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	if info.Mode()&0o111 == 0 {
		return false
	}
	return true
}

// IsValidAdapterName reports whether name is a valid adapter identity: either
// a legacy flat (bare type) name ("noop") or an adapter DEFINITION identity
// ("<type>.<name>" — exactly one dot separator, both halves non-empty and
// valid, KB-242's per-definition child keying). Names are used directly as
// path segments, so anything that is not a segment character is rejected.
func IsValidAdapterName(name string) bool {
	if name == "" {
		return false
	}
	// strings.Cut keeps any further dots in the definition half and the
	// segment check below rejects them, so multi-dot names stay invalid.
	typ, def, hasDef := strings.Cut(name, ".")
	if !hasDef {
		return isValidAdapterNameSegment(name)
	}
	return isValidAdapterNameSegment(typ) && isValidAdapterNameSegment(def)
}

// definitionType returns the adapter type half of an adapter identity: the
// bare name passes through; a definition identity "<type>.<name>" strips the
// definition half so binaries (installed and listed by type) are discoverable
// under the definition-keyed child grammar (KB-246). Callers must only pass
// names already validated with IsValidAdapterName.
func definitionType(name string) string {
	typ, _, hasDef := strings.Cut(name, ".")
	if !hasDef {
		return name
	}
	return typ
}

// isValidAdapterNameSegment reports whether s is one valid identity segment:
// lowercase alphanumerics, dashes, and underscores; non-empty.
func isValidAdapterNameSegment(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}
