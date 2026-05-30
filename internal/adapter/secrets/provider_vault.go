package secrets

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// VaultProvider resolves secrets from HashiCorp Vault KV v2.
// It intentionally shells out to the vault CLI so that the caller's
// VAULT_ADDR, VAULT_TOKEN, VAULT_ROLE_ID, etc. are respected without
// duplicating Vault's authentication logic in Go.
//
// Future work (tracked): migrate to github.com/hashicorp/vault/api for
// richer error handling and connection pooling. The current shell-out
// uses exec.CommandContext so cancellation and timeouts are respected.
type VaultProvider struct{}

func (VaultProvider) Name() string { return "vault" }

func (VaultProvider) CanResolve(ref OriginRef) bool {
	return ref.Kind == "vault"
}

// Resolve interprets ref.Ref as a Vault KV v2 path.
// Supported forms:
//   - "secret/data/myapp#api_key"  → read secret/myapp, field api_key
//   - "secret/myapp#api_key"     → same, data/ prefix is optional
//   - "myapp#api_key"            → path myapp, field api_key
func (VaultProvider) Resolve(ctx context.Context, ref OriginRef) (string, error) {
	slog.Debug("vault provider resolving secret", "ref", ref.Ref)

	path, field, ok := strings.Cut(ref.Ref, "#")
	if !ok || field == "" {
		return "", fmt.Errorf("vault provider: ref %q must be path#field", ref.Ref)
	}

	// Ensure VAULT_ADDR is set.
	if os.Getenv("VAULT_ADDR") == "" {
		return "", fmt.Errorf("vault provider: VAULT_ADDR is not set")
	}

	// Normalize path for KV v2.
	if !strings.Contains(path, "/data/") {
		path = "secret/data/" + path
	}

	cmd := exec.CommandContext(ctx, "vault", "kv", "get", "-format=json", "-field="+field, path)
	out, err := cmd.Output()
	if err != nil {
		slog.Debug("vault provider resolution failed", "ref", ref.Ref, "error", err)
		return "", fmt.Errorf("vault provider: vault kv get failed for %q: %w", ref.Ref, err)
	}
	val := strings.TrimRight(string(out), "\r\n")
	slog.Debug("vault provider resolved secret", "ref", ref.Ref, "path", path, "field", field)
	return val, nil
}
