package adapterhost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
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
// in search order: $CRITERIA_ADAPTERS (if set) then ~/.criteria/adapters.
func adaptersRoots() []string {
	roots := make([]string, 0, 2)
	if envDir := strings.TrimSpace(os.Getenv(adaptersEnvVar)); envDir != "" {
		roots = append(roots, envDir)
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		roots = append(roots, filepath.Join(home, ".criteria", "adapters"))
	}
	return roots
}

// InstallRoot returns the primary directory adapter binaries are written to:
// $CRITERIA_ADAPTERS if set, otherwise ~/.criteria/adapters.
func InstallRoot() (string, error) {
	roots := adaptersRoots()
	if len(roots) == 0 {
		return "", errors.New("no adapter install root: HOME unset and CRITERIA_ADAPTERS unset")
	}
	return roots[0], nil
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
// Discovery intentionally does not consult PATH to avoid unintentionally
// executing similarly named binaries from user/system toolchains.
func DiscoverBinary(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("adapter name is required")
	}
	if !isValidAdapterName(name) {
		return "", fmt.Errorf("%w %q", ErrInvalidAdapterName, name)
	}
	binary := adapterBinaryPrefix + name
	roots := adaptersRoots()
	searched := make([]string, 0, len(roots))
	for _, root := range roots {
		candidate := filepath.Join(root, binary)
		searched = append(searched, candidate)
		if isRunnableFile(candidate) {
			return candidate, nil
		}
	}
	return "", &ErrAdapterNotFound{Name: name, Searched: searched}
}

// DiscoverBinaryAt resolves the installed binary for adapterType pinned to a
// specific resolved digest (digestEncoded as produced by EncodeDigest). This is
// the digest-addressed path that lets multiple versions of the same adapter
// type coexist.
func DiscoverBinaryAt(adapterType, digestEncoded string) (string, error) {
	adapterType = strings.TrimSpace(adapterType)
	if adapterType == "" {
		return "", errors.New("adapter type is required")
	}
	if !isValidAdapterName(adapterType) {
		return "", fmt.Errorf("%w %q", ErrInvalidAdapterName, adapterType)
	}
	binary := adapterBinaryPrefix + adapterType
	roots := adaptersRoots()
	searched := make([]string, 0, len(roots))
	for _, root := range roots {
		candidate := filepath.Join(root, digestEncoded, binary)
		searched = append(searched, candidate)
		if isRunnableFile(candidate) {
			return candidate, nil
		}
	}
	return "", &ErrAdapterNotFound{Name: adapterType, Searched: searched}
}

func isRunnableFile(path string) bool {
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

func isValidAdapterName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}
