package signing

// LockfileFields returns the signer-identity fields to record in a lockfile
// entry. Used by WS07's lockfile writer.
func LockfileFields(id *SignerIdentity) map[string]any {
	if id == nil {
		return nil
	}

	m := make(map[string]any)
	if id.Keyless != nil {
		m["keyless"] = map[string]any{
			"issuer":  id.Keyless.Issuer,
			"subject": id.Keyless.Subject,
		}
	}
	if id.Key != nil {
		m["key"] = map[string]any{
			"algorithm":   id.Key.Algorithm,
			"fingerprint": id.Key.Fingerprint,
		}
	}
	return m
}
