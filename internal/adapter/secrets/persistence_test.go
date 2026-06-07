package secrets

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMarshalUnmarshal(t *testing.T) {
	s := BuildSnapshot(map[string]OriginRef{
		"api_key": {Kind: "env", Ref: "API_KEY"},
		"cert":    {Kind: "file", Ref: "/secrets/cert.pem"},
	})

	data, err := MarshalSnapshot(s)
	require.NoError(t, err)

	restored, err := UnmarshalSnapshot(data)
	require.NoError(t, err)
	require.Len(t, restored.Secrets, 2)
	require.Equal(t, OriginRef{Kind: "env", Ref: "API_KEY"}, restored.Secrets["api_key"])
	require.Equal(t, OriginRef{Kind: "file", Ref: "/secrets/cert.pem"}, restored.Secrets["cert"])
}

func TestMarshal_Empty(t *testing.T) {
	s := BuildSnapshot(nil)
	data, err := MarshalSnapshot(s)
	require.NoError(t, err)

	restored, err := UnmarshalSnapshot(data)
	require.NoError(t, err)
	require.NotNil(t, restored.Secrets)
	require.Len(t, restored.Secrets, 0)
}

func TestResolveAndRegister(t *testing.T) {
	t.Setenv("PERSIST_TEST", "val123")

	stack := DefaultStack()

	reg := NewRegistry()
	snap := BuildSnapshot(map[string]OriginRef{
		"k": {Kind: "env", Ref: "PERSIST_TEST"},
	})

	resolved, err := snap.ResolveAndRegister(context.Background(), stack, reg)
	require.NoError(t, err)
	require.Equal(t, "val123", resolved["k"])
	require.Equal(t, "[REDACTED]", reg.Redact("val123"))
}

func TestResolveAndRegister_Missing(t *testing.T) {
	stack := DefaultStack()

	reg := NewRegistry()
	snap := BuildSnapshot(map[string]OriginRef{
		"k": {Kind: "env", Ref: "PERSIST_MISSING"},
	})

	_, err := snap.ResolveAndRegister(context.Background(), stack, reg)
	require.Error(t, err)
	var re *ResolveError
	require.ErrorAs(t, err, &re)
	require.Equal(t, "k", re.Name)
}
