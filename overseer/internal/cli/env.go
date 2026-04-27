package cli

import (
	"os"
	"strings"
)

// envOrDefault resolves configuration with precedence env -> default.
// Cobra flags can then override this default value.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseVarOverrides converts a slice of "key=value" strings (from --var flags)
// into a map. Entries without "=" are silently ignored.
func parseVarOverrides(raw []string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for _, kv := range raw {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		out[k] = v
	}
	return out
}
