package secrets

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVaultProvider_CanResolve(t *testing.T) {
	p := VaultProvider{}
	require.True(t, p.CanResolve(OriginRef{Kind: "vault", Ref: "secret/data/app"}))
	require.False(t, p.CanResolve(OriginRef{Kind: "env", Ref: "FOO"}))
}

func TestVaultProvider_Resolve_MissingAddr(t *testing.T) {
	t.Setenv("VAULT_ADDR", "")
	p := VaultProvider{}
	_, err := p.Resolve(t.Context(), OriginRef{Kind: "vault", Ref: "secret/data/app"})
	require.Error(t, err)
}

func TestVaultProvider_kvPath(t *testing.T) {
	for _, tt := range []struct {
		name  string
		mount string
		path  string
		want  string
	}{
		{"bare", "secret", "myapp", "secret/data/myapp"},
		{"mount_only_prefix", "secret", "secret/myapp", "secret/data/myapp"},
		{"full_kv_v2_path_verbatim", "secret", "secret/data/myapp", "secret/data/myapp"},
		{"nested_path_mid_data_marker", "secret", "app/data/tokens", "secret/data/app/data/tokens"},
		{"mount_name_not_component_matched", "kv-prod", "kv-prod2/myapp", "kv-prod/data/kv-prod2/myapp"},
		{"foreign_mount_stays_confined", "secret", "other/data/x", "secret/data/other/data/x"},
		{"leading_data_marker", "secret", "data/myapp", "secret/data/data/myapp"},
		{"custom_mount_bare", "kv-prod", "myapp", "kv-prod/data/myapp"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, kvPath(tt.mount, tt.path))
		})
	}
}

func TestVaultProvider_mount(t *testing.T) {
	// Unset falls back to the documented default mount.
	require.Equal(t, "secret", VaultProvider{}.mount())
	require.Equal(t, "secret", VaultProvider{Mount: ""}.mount())
	// Custom mounts pass through; stray separators are trimmed so a trailing
	// slash in the configuration cannot double into the KV v2 path.
	require.Equal(t, "kv-prod", VaultProvider{Mount: "kv-prod"}.mount())
	require.Equal(t, "kv-prod", VaultProvider{Mount: "kv-prod/"}.mount())
	// Root-slash-only configs degenerate to the default rather than sending
	// bare "data/..." paths to the CLI.
	require.Equal(t, "secret", VaultProvider{Mount: "/"}.mount())
}
