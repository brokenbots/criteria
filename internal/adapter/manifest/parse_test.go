package manifest_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
)

func TestParseFile_RoundTrip(t *testing.T) {
	m, err := manifest.ParseFile("testdata/adapter.yaml")
	require.NoError(t, err)

	assert.Equal(t, 1, m.SchemaVersion)
	assert.Equal(t, "example-adapter", m.Name)
	assert.Equal(t, "1.2.3", m.Version)
	assert.Equal(t, "A canonical example adapter.yaml used for testing and documentation.", m.Description)
	assert.Equal(t, "https://github.com/brokenbots/criteria/tree/main/examples/plugins/greeter", m.SourceURL)
	assert.Equal(t, []string{"parallel_safe", "pause", "resume"}, m.Capabilities)
	assert.Equal(t, []manifest.Platform{
		{OS: "linux", Arch: "amd64"},
		{OS: "linux", Arch: "arm64"},
		{OS: "darwin", Arch: "amd64"},
		{OS: "darwin", Arch: "arm64"},
	}, m.Platforms)
	assert.Equal(t, 2, m.SDKProtocolVersion)

	assert.Len(t, m.ConfigSchema.Fields, 2)
	assert.Equal(t, manifest.SchemaField{
		Type:        "string",
		Required:    true,
		Description: "API key for the upstream service.",
		Sensitive:   true,
	}, m.ConfigSchema.Fields["api_key"])
	assert.Equal(t, manifest.SchemaField{
		Type:        "number",
		Required:    false,
		Description: "Request timeout in seconds.",
		Default:     30,
	}, m.ConfigSchema.Fields["timeout_seconds"])

	assert.Len(t, m.InputSchema.Fields, 2)
	assert.Equal(t, manifest.SchemaField{
		Type:        "string",
		Required:    true,
		Description: "The user prompt to process.",
	}, m.InputSchema.Fields["prompt"])
	assert.Equal(t, manifest.SchemaField{
		Type:        "number",
		Required:    false,
		Description: "Sampling temperature.",
		Default:     0.7,
	}, m.InputSchema.Fields["temperature"])

	assert.Len(t, m.OutputSchema.Fields, 2)
	assert.Equal(t, manifest.SchemaField{
		Type:        "string",
		Required:    true,
		Description: "The generated response text.",
	}, m.OutputSchema.Fields["response"])
	assert.Equal(t, manifest.SchemaField{
		Type:        "number",
		Required:    false,
		Description: "Number of tokens consumed.",
	}, m.OutputSchema.Fields["tokens_used"])

	assert.Len(t, m.Secrets, 2)
	assert.Equal(t, manifest.SecretDecl{
		Name:        "api_key",
		Description: "API key for the upstream service.",
		Required:    true,
	}, m.Secrets[0])
	assert.Equal(t, manifest.SecretDecl{
		Name:        "webhook_token",
		Description: "Optional webhook authentication token.",
		Required:    false,
	}, m.Secrets[1])

	assert.Equal(t, []string{"file.read", "network.outbound"}, m.Permissions)
	assert.Equal(t, []string{"local", "ci"}, m.CompatibleEnvironments)
	assert.Nil(t, m.ContainerImage)
}

func TestParse_Minimal(t *testing.T) {
	data := `schema_version: 1
name: minimal
version: 0.0.1
source_url: https://example.com
platforms:
  - os: linux
    arch: amd64
sdk_protocol_version: 2
`
	m, err := manifest.Parse(strings.NewReader(data))
	require.NoError(t, err)

	assert.Equal(t, 1, m.SchemaVersion)
	assert.Equal(t, "minimal", m.Name)
	assert.Equal(t, "0.0.1", m.Version)
	assert.Equal(t, "https://example.com", m.SourceURL)
	assert.Equal(t, []manifest.Platform{{OS: "linux", Arch: "amd64"}}, m.Platforms)
	assert.Equal(t, 2, m.SDKProtocolVersion)
	assert.Empty(t, m.Description)
	assert.Empty(t, m.Capabilities)
	assert.Empty(t, m.ConfigSchema.Fields)
	assert.Empty(t, m.InputSchema.Fields)
	assert.Empty(t, m.OutputSchema.Fields)
	assert.Empty(t, m.Secrets)
	assert.Empty(t, m.Permissions)
	assert.Empty(t, m.CompatibleEnvironments)
	assert.Nil(t, m.ContainerImage)
}

func TestParse_OmitemptyFieldsAbsent(t *testing.T) {
	data := `schema_version: 1
name: tiny
version: 0.0.1
source_url: https://example.com
platforms:
  - os: linux
    arch: amd64
sdk_protocol_version: 2
`
	m, err := manifest.Parse(strings.NewReader(data))
	require.NoError(t, err)

	assert.Empty(t, m.Description)
	assert.Empty(t, m.Capabilities)
	assert.Empty(t, m.ConfigSchema.Fields)
	assert.Empty(t, m.InputSchema.Fields)
	assert.Empty(t, m.OutputSchema.Fields)
	assert.Empty(t, m.Secrets)
	assert.Empty(t, m.Permissions)
	assert.Empty(t, m.CompatibleEnvironments)
	assert.Nil(t, m.ContainerImage)
}

func TestParseFromFS(t *testing.T) {
	data := `schema_version: 1
name: fs-test
version: 0.1.0
source_url: https://example.com
platforms:
  - os: linux
    arch: amd64
sdk_protocol_version: 2
`
	fsys := fstest.MapFS{
		"adapter.yaml": &fstest.MapFile{Data: []byte(data)},
	}
	m, err := manifest.ParseFromFS(fsys, "adapter.yaml")
	require.NoError(t, err)
	assert.Equal(t, "fs-test", m.Name)
}

func TestParseFromFS_MissingFile(t *testing.T) {
	fsys := fstest.MapFS{}
	_, err := manifest.ParseFromFS(fsys, "adapter.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open")
}
