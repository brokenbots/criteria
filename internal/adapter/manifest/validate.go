package manifest

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"

	"github.com/opencontainers/go-digest"
	"golang.org/x/mod/semver"
)

// AllowUnknownSchemaTypes controls whether unknown SchemaField.Type values
// are accepted as warnings or rejected as errors. This is set by the
// --manifest-allow-unknown-types CLI flag (WS08).
var AllowUnknownSchemaTypes = false

// Well-known schema field types. Unknown types are rejected unless
// AllowUnknownSchemaTypes is true.
var wellKnownTypes = map[string]bool{
	"string":  true,
	"number":  true,
	"boolean": true,
	"object":  true,
	"array":   true,
}

var (
	namePattern         = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	platformOSPattern   = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	platformArchPattern = regexp.MustCompile(`^[a-z0-9_]+$`)
	envPattern          = regexp.MustCompile(`^[a-z][a-z_]*$`)
	schemePattern       = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)
)

// Validate checks the manifest against the spec rules. Each failing rule
// returns an error that names the field and the offending value.
func (m *Manifest) Validate() error {
	if err := validateMeta(m); err != nil {
		return err
	}
	if err := validatePlatforms(m.Platforms); err != nil {
		return err
	}
	if err := validateSchemas(m); err != nil {
		return err
	}
	if err := validateSecrets(m.Secrets); err != nil {
		return err
	}
	if err := validateCompatibleEnvironments(m.CompatibleEnvironments); err != nil {
		return err
	}
	if err := validateContainerImage(m.ContainerImage); err != nil {
		return err
	}
	return nil
}

func validateMeta(m *Manifest) error {
	if m.SchemaVersion < 1 || m.SchemaVersion > ManifestMaxSchemaVersion {
		return fmt.Errorf("manifest: schema_version %d is not supported by this host (max %d)", m.SchemaVersion, ManifestMaxSchemaVersion)
	}
	if !namePattern.MatchString(m.Name) {
		return fmt.Errorf("manifest: name %q does not match ^[a-z][a-z0-9-]*$", m.Name)
	}
	if !semver.IsValid("v" + m.Version) {
		return fmt.Errorf("manifest: version %q is not valid semver", m.Version)
	}
	if err := validateSourceURL(m.SourceURL); err != nil {
		return err
	}
	if m.SDKProtocolVersion < 2 || m.SDKProtocolVersion > ProtocolMaxSDKVersion {
		return fmt.Errorf("manifest: sdk_protocol_version %d is not supported by this host (max %d)", m.SDKProtocolVersion, ProtocolMaxSDKVersion)
	}
	return nil
}

func validateSourceURL(v string) error {
	if v == "" {
		return fmt.Errorf("manifest: source_url is required")
	}
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("manifest: source_url %q is not a valid URL: %w", v, err)
	}
	if !schemePattern.MatchString(u.Scheme) {
		return fmt.Errorf("manifest: source_url %q has unsupported scheme %q", v, u.Scheme)
	}
	return nil
}

func validatePlatforms(ps []Platform) error {
	if len(ps) == 0 {
		return fmt.Errorf("manifest: platforms must be non-empty")
	}
	for i, p := range ps {
		if !platformOSPattern.MatchString(p.OS) {
			return fmt.Errorf("manifest: platforms[%d].os %q does not match ^[a-z][a-z0-9]*$", i, p.OS)
		}
		if !platformArchPattern.MatchString(p.Arch) {
			return fmt.Errorf("manifest: platforms[%d].arch %q does not match ^[a-z0-9_]+$", i, p.Arch)
		}
	}
	return nil
}

func validateSchemas(m *Manifest) error {
	for name, f := range m.ConfigSchema.Fields {
		if err := validateField("config_schema", name, f); err != nil {
			return err
		}
	}
	for name, f := range m.InputSchema.Fields {
		if err := validateField("input_schema", name, f); err != nil {
			return err
		}
	}
	for name, f := range m.OutputSchema.Fields {
		if err := validateField("output_schema", name, f); err != nil {
			return err
		}
	}
	return nil
}

func validateSecrets(ss []SecretDecl) error {
	for i, s := range ss {
		if s.Name == "" {
			return fmt.Errorf("manifest: secrets[%d].name is required", i)
		}
	}
	return nil
}

func validateCompatibleEnvironments(v []string) error {
	envs := normaliseCompatibleEnvironments(v)
	for i, e := range envs {
		if e == "*" {
			continue
		}
		if !envPattern.MatchString(e) {
			return fmt.Errorf("manifest: compatible_environments[%d] %q does not match ^[a-z][a-z_]*$", i, e)
		}
	}
	return nil
}

func validateContainerImage(ci *ContainerImageRef) error {
	if ci != nil && ci.Digest != "" {
		if _, err := digest.Parse(ci.Digest); err != nil {
			return fmt.Errorf("manifest: container_image.digest %q is not a valid OCI digest: %w", ci.Digest, err)
		}
	}
	return nil
}

func validateField(schemaKind, name string, f SchemaField) error {
	if f.Type == "" {
		return fmt.Errorf("manifest: %s.fields[%q].type is required", schemaKind, name)
	}
	if !wellKnownTypes[f.Type] && !AllowUnknownSchemaTypes {
		return fmt.Errorf("manifest: %s.fields[%q].type %q is not a well-known type (use --manifest-allow-unknown-types to permit)", schemaKind, name, f.Type)
	}
	return nil
}

// normaliseCompatibleEnvironments converts an empty slice to the explicit
// wildcard form so callers can treat it as "any".
func normaliseCompatibleEnvironments(v []string) []string {
	if len(v) == 0 {
		return []string{"*"}
	}
	return v
}

// sortedStrings returns a sorted deduplicated copy of s.
func sortedStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
