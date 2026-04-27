package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	pluginBinaryPrefix = "overseer-adapter-"
	pluginsEnvVar      = "OVERSEER_PLUGINS"
)

var ErrInvalidAdapterName = errors.New("invalid adapter name")

// ErrPluginNotFound reports that adapter discovery failed after checking all
// configured plugin directories.
type ErrPluginNotFound struct {
	Name     string
	Searched []string
}

func (e *ErrPluginNotFound) Error() string {
	if e == nil {
		return "plugin not found"
	}
	if len(e.Searched) == 0 {
		return fmt.Sprintf("plugin %q not found", e.Name)
	}
	return fmt.Sprintf("plugin %q not found (searched: %s)", e.Name, strings.Join(e.Searched, ", "))
}

// DiscoverBinary resolves an adapter plugin binary path.
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
	binary := pluginBinaryPrefix + name
	searched := make([]string, 0, 2)

	if envDir := strings.TrimSpace(os.Getenv(pluginsEnvVar)); envDir != "" {
		candidate := filepath.Join(envDir, binary)
		searched = append(searched, candidate)
		if isRunnableFile(candidate) {
			return candidate, nil
		}
	}

	home, err := os.UserHomeDir()
	if err == nil && strings.TrimSpace(home) != "" {
		candidate := filepath.Join(home, ".overseer", "plugins", binary)
		searched = append(searched, candidate)
		if isRunnableFile(candidate) {
			return candidate, nil
		}
	}

	return "", &ErrPluginNotFound{Name: name, Searched: searched}
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
