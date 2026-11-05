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
type VaultProvider struct {
	// Mount is the KV v2 mount every resolved ref is confined to.
	// Empty selects DefaultVaultMount ("secret"). Custom KV mounts are
	// configured from a secrets policy (workflow SecretsPolicy.Mount, set
	// via `secrets { mount = ... }`) or, in its absence, the EnvVaultMount
	// environment variable — refs are never sent to a different mount than
	// the configured one.
	Mount string
}

// DefaultVaultMount is the KV v2 mount VaultProvider resolves refs against
// when Mount is empty.
const DefaultVaultMount = "secret"

// EnvVaultMount is the operator-facing environment knob that selects the KV
// v2 mount for vault providers built by DefaultStack and
// StackFromEnvironment (e.g. CRITERIA_VAULT_MOUNT=kv-prod). It applies only
// when the workflow's secrets policy does not configure a Mount itself;
// policy-level configuration wins.
const EnvVaultMount = "CRITERIA_VAULT_MOUNT"

// vaultMountFromEnv reads the EnvVaultMount operator knob. Empty means "no
// operator override", which VaultProvider resolves to DefaultVaultMount.
func vaultMountFromEnv() string {
	return strings.TrimSpace(os.Getenv(EnvVaultMount))
}

func (vp VaultProvider) Name() string { return "vault" }

func (vp VaultProvider) CanResolve(ref OriginRef) bool {
	return ref.Kind == "vault"
}

// Resolve interprets ref.Ref as a Vault KV v2 path in the configured mount.
// With M = the mount (Mount, default "secret"), supported forms:
//   - "D#field"          → secret data path D within M (resolved as M/data/D)
//   - "M/D#field"        → same, mount given without the KV v2 data marker
//   - "M/data/D#field"   → used as-is (full KV v2 path)
//
// Any other shape — including paths containing "/data/" mid-string — is a
// data path within M, so "app/data/tokens#f" resolves to M/data/app/data/
// tokens instead of being misrouted to a foreign mount. A ref naming a
// different mount (e.g. "other/data/x") stays inside M for the same reason;
// to use a non-default KV mount, configure Mount.
func (vp VaultProvider) Resolve(ctx context.Context, ref OriginRef) (string, error) {
	slog.Debug("vault provider resolving secret", "ref", ref.Ref)

	path, field, ok := strings.Cut(ref.Ref, "#")
	if !ok || field == "" {
		return "", fmt.Errorf("vault provider: ref %q must be path#field", ref.Ref)
	}

	// Ensure VAULT_ADDR is set.
	if os.Getenv("VAULT_ADDR") == "" {
		return "", fmt.Errorf("vault provider: VAULT_ADDR is not set")
	}

	mount := vp.mount()
	path = kvPath(mount, path)
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

// mount returns the effective KV v2 mount: the configured Mount, or
// DefaultVaultMount when unset. Stray separators are trimmed so a trailing
// slash in the configuration cannot double into the KV v2 path.
func (vp VaultProvider) mount() string {
	mount := strings.Trim(vp.Mount, "/")
	if mount == "" {
		return DefaultVaultMount
	}
	return mount
}

// kvPath normalizes a mount-relative secret path to the full KV v2 path the
// vault CLI expects ("<mount>/data/<secret path>"). A leading configured-
// mount prefix is recognized only at a path-component boundary.
func kvPath(mount, path string) string {
	switch {
	case strings.HasPrefix(path, mount+"/data/"):
		return path
	case strings.HasPrefix(path, mount+"/"):
		return mount + "/data/" + strings.TrimPrefix(path, mount+"/")
	default:
		return mount + "/data/" + path
	}
}
