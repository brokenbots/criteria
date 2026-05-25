package oci

import (
	"fmt"
	"strings"

	"github.com/opencontainers/go-digest"
)

// Reference is a parsed OCI reference. Supported forms:
//
//	registry/org/name:tag       – tagged reference
//	registry/org/name@sha256:… – digest-pinned reference
//	registry/org/name           – registry+repo only (no tag/digest)
//
// Short alias forms (e.g. "claude:1.2.3") are intentionally not resolved
// here; alias resolution is the responsibility of a higher layer (WS08).
type Reference struct {
	// Registry is the hostname[:port] of the registry, e.g. "ghcr.io".
	Registry string
	// Repo is the repository path within the registry, e.g. "org/name".
	Repo string
	// Tag is the mutable tag, e.g. "latest" or "1.2.3". Empty when Digest is set.
	Tag string
	// Digest is the immutable content address. If present, Tag is ignored on
	// resolution.
	Digest digest.Digest
}

// Parse parses the OCI reference string s.
// Recognised forms (examples):
//
//	ghcr.io/brokenbots/claude:1.2.3
//	ghcr.io/brokenbots/claude@sha256:abc123…
//	ghcr.io/brokenbots/claude
func Parse(s string) (Reference, error) {
	if s == "" {
		return Reference{}, fmt.Errorf("oci: empty reference")
	}

	var ref Reference

	// Split off digest (@sha256:…) first because it takes priority.
	if idx := strings.Index(s, "@"); idx != -1 {
		ref.Digest = digest.Digest(s[idx+1:])
		if err := ref.Digest.Validate(); err != nil {
			return Reference{}, fmt.Errorf("oci: invalid digest in %q: %w", s, err)
		}
		s = s[:idx]
	}

	// Split off tag (:tag) — but only if no digest was found, because the
	// digest separator also contains a colon.
	if ref.Digest == "" {
		if idx := strings.LastIndex(s, ":"); idx != -1 {
			// Distinguish "host:port/path" from "path:tag" by checking
			// whether the colon precedes the first slash.
			firstSlash := strings.Index(s, "/")
			if firstSlash == -1 || idx > firstSlash {
				ref.Tag = s[idx+1:]
				if ref.Tag == "" {
					return Reference{}, fmt.Errorf("oci: empty tag in %q", s)
				}
				s = s[:idx]
			}
		}
	}

	// The remaining s is "registry/repo…" or just "registry".
	slashIdx := strings.Index(s, "/")
	if slashIdx == -1 {
		// No slash: treat as registry only (unusual but valid for tests).
		ref.Registry = s
	} else {
		ref.Registry = s[:slashIdx]
		ref.Repo = s[slashIdx+1:]
	}

	if ref.Registry == "" {
		return Reference{}, fmt.Errorf("oci: missing registry in %q", s)
	}
	if ref.Repo == "" && ref.Tag == "" && ref.Digest == "" {
		return Reference{}, fmt.Errorf("oci: reference %q has only a registry component", s)
	}

	return ref, nil
}

// String reassembles the canonical string form of the reference.
func (r Reference) String() string {
	var b strings.Builder
	b.WriteString(r.Registry)
	if r.Repo != "" {
		b.WriteByte('/')
		b.WriteString(r.Repo)
	}
	if r.Digest != "" {
		b.WriteByte('@')
		b.WriteString(r.Digest.String())
	} else if r.Tag != "" {
		b.WriteByte(':')
		b.WriteString(r.Tag)
	}
	return b.String()
}

// FullyQualified reports whether the reference contains a registry and at
// least one of a tag or digest. References without a tag or digest cannot
// be pulled unambiguously.
func (r Reference) FullyQualified() bool {
	return r.Registry != "" && (r.Tag != "" || r.Digest != "")
}
