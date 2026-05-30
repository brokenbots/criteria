package secrets

import "context"

// Provider resolves secret values from a specific backend.
type Provider interface {
	// Name returns the provider identifier (e.g. "env", "file", "keychain", "vault", "sops").
	Name() string

	// Resolve returns the secret value for the given origin reference.
	// If the reference is not valid for this provider, Resolve returns an error.
	Resolve(ctx context.Context, ref OriginRef) (string, error)

	// CanResolve returns true if this provider can handle the given reference.
	CanResolve(ref OriginRef) bool
}
