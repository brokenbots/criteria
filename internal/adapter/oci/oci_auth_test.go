package oci_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

// writeDockerConfig writes a minimal Docker config.json with static base64
// credentials to configDir and returns the path written.
func writeDockerConfig(t *testing.T, configDir, registry, user, pass string) string {
	t.Helper()
	token := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", user, pass)))
	cfg := map[string]any{
		"auths": map[string]any{
			registry: map[string]string{"auth": token},
		},
	}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	path := configDir + "/config.json"
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func TestDefaultAuthProvider_ReadsDockerConfig(t *testing.T) {
	configDir := t.TempDir()
	const (
		registry = "registry.example.com"
		user     = "testuser"
		pass     = "testpassword"
	)
	writeDockerConfig(t, configDir, registry, user, pass)

	// Set DOCKER_CONFIG before calling DefaultAuthProvider so the store picks
	// up our temp config directory.
	t.Setenv("DOCKER_CONFIG", configDir)

	provider := oci.DefaultAuthProvider()

	cred, err := provider.Credential(context.Background(), registry)
	require.NoError(t, err)
	assert.Equal(t, user, cred.Username,
		"DefaultAuthProvider must return username from DOCKER_CONFIG/config.json")
	assert.Equal(t, pass, cred.Password,
		"DefaultAuthProvider must return password from DOCKER_CONFIG/config.json")
}

func TestDefaultAuthProvider_FallsBackToAnonymous(t *testing.T) {
	// Point DOCKER_CONFIG at an empty temp dir (no config.json).
	configDir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", configDir)

	provider := oci.DefaultAuthProvider()

	cred, err := provider.Credential(context.Background(), "unknown.registry.io")
	require.NoError(t, err)
	assert.Empty(t, cred.Username, "no credentials configured: expect anonymous fallback")
	assert.Empty(t, cred.Password, "no credentials configured: expect anonymous fallback")
}

func TestDefaultAuthProvider_NilDockerConfigFallback(t *testing.T) {
	// Point DOCKER_CONFIG at a nonexistent path so NewStoreFromDocker may fail.
	t.Setenv("DOCKER_CONFIG", t.TempDir()+"/does-not-exist")

	// DefaultAuthProvider must not panic and must return a usable (anonymous) provider.
	provider := oci.DefaultAuthProvider()
	require.NotNil(t, provider)

	cred, err := provider.Credential(context.Background(), "some.registry.io")
	require.NoError(t, err)
	assert.Empty(t, cred.Username)
}
