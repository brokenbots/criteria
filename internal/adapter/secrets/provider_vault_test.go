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
