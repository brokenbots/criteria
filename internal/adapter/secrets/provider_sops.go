package secrets

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// SOPSProvider resolves secrets from sops-encrypted files.
// It shells out to the sops CLI.
type SOPSProvider struct{}

func (SOPSProvider) Name() string { return "sops" }

func (SOPSProvider) CanResolve(ref OriginRef) bool {
	return ref.Kind == "sops"
}

// Resolve interprets ref.Ref as "file#key" where file is the encrypted file
// and key is the JSON/YAML key path to extract (e.g. "/run/secrets/app.yaml#api_key").
func (SOPSProvider) Resolve(ctx context.Context, ref OriginRef) (string, error) {
	file, key, ok := strings.Cut(ref.Ref, "#")
	if !ok || key == "" {
		return "", fmt.Errorf("sops provider: ref %q must be file#key", ref.Ref)
	}

	cmd := exec.CommandContext(ctx, "sops", "--decrypt", "--extract", "["+key+"]", file)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("sops provider: sops decrypt failed for %q: %w", ref.Ref, err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}
