// Package manifest defines the adapter.yaml manifest schema, parser, validator,
// and runtime cross-check used by the Criteria host when loading adapters from
// OCI artifacts or the local filesystem.
package manifest

// ManifestMaxSchemaVersion is the highest manifest schema version this host
// build understands. Forward-compatibility: hosts bump this constant to accept
// newer schemas without breaking older adapters.
const ManifestMaxSchemaVersion = 1

// ProtocolMaxSDKVersion is the highest SDK protocol version this host build
// supports. Currently 2 for the v2 wire contract.
const ProtocolMaxSDKVersion = 2

// Manifest is the static adapter metadata emitted at build time and embedded
// in the OCI artifact as adapter.yaml.
type Manifest struct {
	SchemaVersion          int                `yaml:"schema_version"` // = 1 for v2
	Name                   string             `yaml:"name"`
	Version                string             `yaml:"version"` // semver
	Description            string             `yaml:"description"`
	SourceURL              string             `yaml:"source_url"` // REQUIRED, see D13
	Capabilities           []string           `yaml:"capabilities"`
	Platforms              []Platform         `yaml:"platforms"`            // GOOS/GOARCH list
	SDKProtocolVersion     int                `yaml:"sdk_protocol_version"` // protocol v2 → 2
	ConfigSchema           Schema             `yaml:"config_schema"`
	InputSchema            Schema             `yaml:"input_schema"`
	OutputSchema           Schema             `yaml:"output_schema"`
	Secrets                []SecretDecl       `yaml:"secrets"`
	Permissions            []string           `yaml:"permissions"`
	CompatibleEnvironments []string           `yaml:"compatible_environments"`   // optional; default any (see D36)
	ContainerImage         *ContainerImageRef `yaml:"container_image,omitempty"` // set when WS28 publishes with_image=true
}

// Platform is a GOOS/GOARCH pair the adapter binary supports.
type Platform struct {
	OS   string `yaml:"os"`
	Arch string `yaml:"arch"`
}

// SecretDecl describes a secret the adapter expects at runtime.
type SecretDecl struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

// ContainerImageRef references the container image published alongside the
// adapter binary (set by the WS28 publish action).
type ContainerImageRef struct {
	Ref    string `yaml:"ref"`    // ghcr.io/org/name:v1.2.3-image
	Digest string `yaml:"digest"` // sha256:...
}

// Schema describes a set of typed fields for config, input, or output.
type Schema struct {
	Fields map[string]SchemaField `yaml:"fields"`
}

// SchemaField describes a single field in a schema.
type SchemaField struct {
	Type        string `yaml:"type"` // "string" | "number" | "boolean" | "object" | "array"
	Required    bool   `yaml:"required"`
	Description string `yaml:"description"`
	Default     any    `yaml:"default,omitempty"`
	Sensitive   bool   `yaml:"sensitive,omitempty"` // marks output fields as taint sources (D63)
}
