package secrets

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// EnvProvider resolves secrets from environment variables.
type EnvProvider struct{}

func (EnvProvider) Name() string { return "env" }

func (EnvProvider) CanResolve(ref OriginRef) bool {
	return ref.Kind == "env" && ref.Ref != ""
}

func (EnvProvider) Resolve(_ context.Context, ref OriginRef) (string, error) {
	val := os.Getenv(ref.Ref)
	if val == "" {
		return "", fmt.Errorf("env provider: %q is not set", ref.Ref)
	}
	return strings.TrimRight(val, "\r\n"), nil
}
