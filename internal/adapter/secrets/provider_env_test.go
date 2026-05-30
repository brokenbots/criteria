package secrets

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvProvider(t *testing.T) {
	// Set a known env var for the test.
	t.Setenv("CRITERIA_TEST_SECRET", "shh")

	p := EnvProvider{}
	require.Equal(t, "env", p.Name())
	require.True(t, p.CanResolve(OriginRef{Kind: "env", Ref: "CRITERIA_TEST_SECRET"}))
	require.False(t, p.CanResolve(OriginRef{Kind: "file", Ref: "/tmp/foo"}))

	val, err := p.Resolve(t.Context(), OriginRef{Kind: "env", Ref: "CRITERIA_TEST_SECRET"})
	require.NoError(t, err)
	require.Equal(t, "shh", val)

	_, err = p.Resolve(t.Context(), OriginRef{Kind: "env", Ref: "CRITERIA_TEST_MISSING"})
	require.Error(t, err)
}

func TestEnvProvider_StripsNewline(t *testing.T) {
	t.Setenv("CRITERIA_TEST_NL", "hello\n")

	p := EnvProvider{}
	val, err := p.Resolve(t.Context(), OriginRef{Kind: "env", Ref: "CRITERIA_TEST_NL"})
	require.NoError(t, err)
	require.Equal(t, "hello", val)
}

func TestEnvProvider_EmptyRef(t *testing.T) {
	p := EnvProvider{}
	require.False(t, p.CanResolve(OriginRef{Kind: "env", Ref: ""}))
}
