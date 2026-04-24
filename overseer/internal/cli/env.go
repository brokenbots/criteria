package cli

import "os"

// envOrDefault resolves configuration with precedence env -> default.
// Cobra flags can then override this default value.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
