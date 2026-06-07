package secrets

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKeychainProvider_CanResolve(t *testing.T) {
	p := &KeychainProvider{}
	require.True(t, p.CanResolve(OriginRef{Kind: "keychain", Ref: "my-account"}))
	require.False(t, p.CanResolve(OriginRef{Kind: "env", Ref: "FOO"}))
}

func TestKeychainProvider_Resolve_NotFound(t *testing.T) {
	p := &KeychainProvider{}
	_, err := p.Resolve(t.Context(), OriginRef{Kind: "keychain", Ref: "nonexistent"})
	require.Error(t, err)
}
