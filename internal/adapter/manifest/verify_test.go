package manifest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

func staticManifest() *manifest.Manifest {
	return &manifest.Manifest{
		SchemaVersion:      1,
		Name:               "claude",
		Version:            "1.2.3",
		SourceURL:          "https://github.com/brokenbots/criteria",
		Capabilities:       []string{"parallel_safe"},
		Platforms:          []manifest.Platform{{OS: "linux", Arch: "amd64"}},
		SDKProtocolVersion: 2,
		ConfigSchema: manifest.Schema{Fields: map[string]manifest.SchemaField{
			"model": {Type: "string", Required: true, Sensitive: false},
		}},
		InputSchema: manifest.Schema{Fields: map[string]manifest.SchemaField{
			"prompt": {Type: "string", Required: true},
		}},
		OutputSchema: manifest.Schema{Fields: map[string]manifest.SchemaField{
			"response": {Type: "string", Required: true},
		}},
		Secrets:                []manifest.SecretDecl{{Name: "api_key"}},
		CompatibleEnvironments: []string{"local"},
	}
}

func runtimeResponse() *v2.InfoResponse {
	return &v2.InfoResponse{
		Name:                   "claude",
		Version:                "1.2.3",
		Capabilities:           []string{"parallel_safe"},
		Platforms:              []string{"linux/amd64"},
		SdkProtocolVersion:     "2",
		SourceUrl:              "https://github.com/brokenbots/criteria",
		ConfigSchema:           &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{"model": {Type: "string", Required: true, Sensitive: false}}},
		InputSchema:            &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{"prompt": {Type: "string", Required: true}}},
		OutputSchema:           &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{"response": {Type: "string", Required: true}}},
		Secrets:                map[string]string{"api_key": "API key"},
		CompatibleEnvironments: []string{"local"},
	}
}

func TestVerify_Identical(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	assert.NoError(t, manifest.Verify(static, runtime))
}

func TestVerify_NameMismatch(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.Name = "different"
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name: manifest=\"claude\" runtime=\"different\"")
}

func TestVerify_VersionMismatch(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.Version = "1.2.2"
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version: manifest=\"1.2.3\" runtime=\"1.2.2\"")
}

func TestVerify_SDKProtocolVersionMismatch(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.SdkProtocolVersion = "3"
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sdk_protocol_version")
}

func TestVerify_CapabilitiesMismatch(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.Capabilities = []string{"pause"}
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "capabilities")
}

func TestVerify_CapabilitiesOrderInsensitive(t *testing.T) {
	static := staticManifest()
	static.Capabilities = []string{"b", "a"}
	runtime := runtimeResponse()
	runtime.Capabilities = []string{"a", "b"}
	assert.NoError(t, manifest.Verify(static, runtime))
}

func TestVerify_CapabilitiesDuplicateInsensitive(t *testing.T) {
	static := staticManifest()
	static.Capabilities = []string{"a", "a"}
	runtime := runtimeResponse()
	runtime.Capabilities = []string{"a"}
	assert.NoError(t, manifest.Verify(static, runtime))
}

func TestVerify_PlatformsMismatch(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.Platforms = []string{"darwin/arm64"}
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "platforms")
}

func TestVerify_PlatformsOrderInsensitive(t *testing.T) {
	static := staticManifest()
	static.Platforms = []manifest.Platform{
		{OS: "darwin", Arch: "arm64"},
		{OS: "linux", Arch: "amd64"},
	}
	runtime := runtimeResponse()
	runtime.Platforms = []string{"linux/amd64", "darwin/arm64"}
	assert.NoError(t, manifest.Verify(static, runtime))
}

func TestVerify_ConfigSchemaMismatch(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.ConfigSchema = &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{
		"model": {Type: "number", Required: true},
	}}
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config_schema.fields[\"model\"].type")
}

func TestVerify_ConfigSchemaExtraField(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.ConfigSchema.Fields["extra"] = &v2.ConfigFieldProto{Type: "string"}
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config_schema: manifest has 1 fields but runtime has 2")
}

func TestVerify_ConfigSchemaMissingField(t *testing.T) {
	static := staticManifest()
	static.ConfigSchema.Fields["extra"] = manifest.SchemaField{Type: "string"}
	runtime := runtimeResponse()
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config_schema: manifest has 2 fields but runtime has 1")
}

func TestVerify_ConfigSchemaRequiredMismatch(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.ConfigSchema.Fields["model"].Required = false
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config_schema.fields[\"model\"].required")
}

func TestVerify_ConfigSchemaSensitiveMismatch(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.ConfigSchema.Fields["model"].Sensitive = true
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config_schema.fields[\"model\"].sensitive")
}

func TestVerify_ConfigSchemaDescriptionIgnored(t *testing.T) {
	static := staticManifest()
	static.ConfigSchema.Fields["model"] = manifest.SchemaField{Type: "string", Required: true, Description: "manifest desc"}
	runtime := runtimeResponse()
	runtime.ConfigSchema.Fields["model"].Description = "runtime desc"
	assert.NoError(t, manifest.Verify(static, runtime))
}

func TestVerify_ConfigSchemaDefaultIgnored(t *testing.T) {
	static := staticManifest()
	static.ConfigSchema.Fields["model"] = manifest.SchemaField{Type: "string", Required: true, Default: "foo"}
	runtime := runtimeResponse()
	runtime.ConfigSchema.Fields["model"].DefaultStr = "bar"
	assert.NoError(t, manifest.Verify(static, runtime))
}

func TestVerify_InputSchemaMismatch(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.InputSchema = nil
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "input_schema")
}

func TestVerify_OutputSchemaMismatch(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.OutputSchema = nil
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "output_schema")
}

func TestVerify_SecretsMismatch(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.Secrets = map[string]string{"other": "desc"}
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secrets")
}

func TestVerify_CompatibleEnvironmentsMismatch(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.CompatibleEnvironments = []string{"ci"}
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compatible_environments")
}

func TestVerify_CompatibleEnvironmentsNormalised(t *testing.T) {
	static := staticManifest()
	static.CompatibleEnvironments = nil
	runtime := runtimeResponse()
	runtime.CompatibleEnvironments = []string{"*"}
	assert.NoError(t, manifest.Verify(static, runtime))
}

func TestVerify_AdvisoryFieldsAllowedToDiffer(t *testing.T) {
	static := staticManifest()
	static.Description = "manifest description"
	static.Permissions = []string{"file.read"}
	runtime := runtimeResponse()
	runtime.Description = "runtime description"
	runtime.Permissions = []string{"network.outbound"}
	assert.NoError(t, manifest.Verify(static, runtime))
}

func TestVerify_MultipleDivergences(t *testing.T) {
	static := staticManifest()
	runtime := runtimeResponse()
	runtime.Name = "different"
	runtime.Version = "9.9.9"
	err := manifest.Verify(static, runtime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name:")
	assert.Contains(t, err.Error(), "version:")
}

func TestVerify_EmptySchemasMatchNilProto(t *testing.T) {
	static := staticManifest()
	static.ConfigSchema = manifest.Schema{}
	static.InputSchema = manifest.Schema{}
	static.OutputSchema = manifest.Schema{}
	runtime := runtimeResponse()
	runtime.ConfigSchema = nil
	runtime.InputSchema = nil
	runtime.OutputSchema = nil
	assert.NoError(t, manifest.Verify(static, runtime))
}
