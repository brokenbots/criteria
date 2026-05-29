package lockfile

// LockedAdapter records the resolved metadata for a single adapter referenced
// by a workflow.
type LockedAdapter struct {
	Type                           string                `hcl:"type,label"`
	Name                           string                `hcl:"name,label"`
	Reference                      string                `hcl:"reference"`
	ResolvedDigest                 string                `hcl:"resolved_digest"`
	SourceURL                      string                `hcl:"source_url"`
	SDKProtocolVersion             int                   `hcl:"sdk_protocol_version"`
	Platforms                      []string              `hcl:"platforms,optional"`
	Signature                      *LockedSignature      `hcl:"signature,block"`
	ContainerImage                 *LockedContainerImage `hcl:"container_image,block"`
	Remote                         *LockedRemote         `hcl:"remote,block"`
	CompatibleEnvironmentsOverride []string              `hcl:"compatible_environments_override,optional"`
	OverriddenBy                   string                `hcl:"overridden_by,optional"`
}

// LockedSignature records the verified signer identity for an adapter artifact.
type LockedSignature struct {
	Keyless *LockedKeyless `hcl:"keyless,block"`
	Key     *LockedKey     `hcl:"key,block"`
}

// LockedKeyless describes a keyless (Sigstore OIDC) signer identity.
type LockedKeyless struct {
	Issuer  string `hcl:"issuer"`
	Subject string `hcl:"subject"`
}

// LockedKey describes an explicit-key signer identity.
type LockedKey struct {
	Algorithm   string `hcl:"algorithm"`
	Fingerprint string `hcl:"fingerprint"`
}

// LockedContainerImage records the container image published alongside an
// adapter binary (present only when the manifest declares one).
type LockedContainerImage struct {
	Ref    string `hcl:"ref"`
	Digest string `hcl:"digest"`
}

// LockedRemote records the remote-endpoint pin for an adapter bound to a
// remote environment (populated by WS20).
type LockedRemote struct {
	ListenAddress         string `hcl:"listen_address"`
	ServerCertFingerprint string `hcl:"server_cert_fingerprint"`
}

// RemoteFields carries the data needed to populate LockedRemote. It is
// provided by the caller (WS20) when an adapter is bound to a remote
// environment.
type RemoteFields struct {
	ListenAddress         string
	ServerCertFingerprint string
}

// ChangeKind categorises the kinds of differences between two lockfiles.
type ChangeKind int

const (
	Added ChangeKind = iota
	Removed
	DigestChanged
	SignerChanged
	PlatformsChanged
	ContainerImageChanged
	RemoteChanged
	OverrideChanged
)

// Change records a single difference for a specific adapter.
type Change struct {
	Adapter string // "<type>.<name>"
	Kind    ChangeKind
	Before  any // previous value where applicable
	After   any
}
