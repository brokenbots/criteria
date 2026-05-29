package manifest

import "strings"

// OCI annotation keys used for adapter metadata. These use the
// dev.criteria.adapter.* namespace so they survive any future org or
// trademark change (D87).
const (
	AnnotationName         = "dev.criteria.adapter.name"
	AnnotationVersion      = "dev.criteria.adapter.version"
	AnnotationSourceURL    = "dev.criteria.adapter.source_url"
	AnnotationCapabilities = "dev.criteria.adapter.capabilities" // comma-joined
	AnnotationPlatforms    = "dev.criteria.adapter.platforms"    // comma-joined GOOS/GOARCH pairs
	AnnotationProtoVer     = "dev.criteria.adapter.protocol_version"
	AnnotationSchemaVer    = "dev.criteria.adapter.schema_version" // manifest schema_version
	AnnotationSigner       = "dev.criteria.adapter.signer"         // cosign identity (issuer|subject or key fingerprint)
)

// AnnotationMap produces the OCI annotation map for the given manifest.
// The publish action (WS28) embeds this in the image config or manifest
// annotations so consumers can read top-level fields without parsing the
// YAML blob.
func AnnotationMap(m *Manifest) map[string]string {
	a := map[string]string{
		AnnotationName:      m.Name,
		AnnotationVersion:   m.Version,
		AnnotationSourceURL: m.SourceURL,
		AnnotationProtoVer:  fmtInt(m.SDKProtocolVersion),
		AnnotationSchemaVer: fmtInt(m.SchemaVersion),
	}

	if len(m.Capabilities) > 0 {
		a[AnnotationCapabilities] = strings.Join(m.Capabilities, ",")
	}
	if len(m.Platforms) > 0 {
		pairs := make([]string, len(m.Platforms))
		for i, p := range m.Platforms {
			pairs[i] = p.OS + "/" + p.Arch
		}
		a[AnnotationPlatforms] = strings.Join(pairs, ",")
	}

	return a
}

func fmtInt(v int) string {
	if v == 0 {
		return "0"
	}
	// Fast path for small positive ints.
	var buf [20]byte
	i := len(buf) - 1
	for v > 0 {
		buf[i] = byte('0' + v%10)
		v /= 10
		i--
	}
	return string(buf[i+1:])
}
