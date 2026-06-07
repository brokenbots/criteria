package secrets

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// SOPSProvider resolves secrets from sops-encrypted files.
// It intentionally shells out to the sops CLI to avoid adding the heavy
// getsops/sops Go SDK dependency.
//
// Future work (tracked): migrate to the sops Go library for richer
// decryption options and error handling. The current shell-out uses
// exec.CommandContext so cancellation and timeouts are respected.
type SOPSProvider struct{}

func (SOPSProvider) Name() string { return "sops" }

func (SOPSProvider) CanResolve(ref OriginRef) bool {
	return ref.Kind == "sops"
}

// Resolve interprets ref.Ref as "file#key" where file is the encrypted file
// and key is the JSON/YAML key path to extract (e.g. "/run/secrets/app.yaml#api_key").
func (SOPSProvider) Resolve(ctx context.Context, ref OriginRef) (string, error) {
	slog.Debug("sops provider resolving secret", "ref", ref.Ref)

	file, key, ok := strings.Cut(ref.Ref, "#")
	if !ok || key == "" {
		return "", fmt.Errorf("sops provider: ref %q must be file#key", ref.Ref)
	}

	cmd := exec.CommandContext(ctx, "sops", "--decrypt", "--extract", "["+key+"]", file)
	out, err := cmd.Output()
	if err != nil {
		slog.Debug("sops provider resolution failed", "ref", ref.Ref, "error", err)
		return "", fmt.Errorf("sops provider: sops decrypt failed for %q: %w", ref.Ref, err)
	}
	val := strings.TrimRight(string(out), "\r\n")
	slog.Debug("sops provider resolved secret", "ref", ref.Ref, "file", file, "key", key)
	return val, nil
}
