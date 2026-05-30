package secrets

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSOPSProvider_CanResolve(t *testing.T) {
	p := SOPSProvider{}
	require.True(t, p.CanResolve(OriginRef{Kind: "sops", Ref: "secrets.yaml"}))
	require.False(t, p.CanResolve(OriginRef{Kind: "env", Ref: "FOO"}))
}
