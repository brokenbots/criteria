package signing

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"fmt"

	sigcrypto "github.com/sigstore/sigstore/pkg/cryptoutils"
)

// Fingerprint returns the canonical fingerprint (hex SHA-256) of a public-key
// encoding. Callers pass PKIX DER bytes so the fingerprint is independent of PEM
// formatting; this is the value recorded in the lockfile and compared during
// key-based verification (see verifyKeyBased).
func Fingerprint(der []byte) string { return fingerprintBytes(der) }

// NewTrustedKey parses a PEM-encoded public key (Ed25519, ECDSA, or RSA) into a
// KeyIdentity whose RawKey is normalized to PKIX DER and whose Fingerprint is
// the SHA-256 of that DER. It is the single constructor for entries that
// populate Policy.TrustedKeys, keeping fingerprinting consistent between trust
// configuration, lock-time pinning, and verify-time matching.
func NewTrustedKey(pemData []byte) (KeyIdentity, error) {
	pub, err := sigcrypto.UnmarshalPEMToPublicKey(pemData)
	if err != nil {
		return KeyIdentity{}, fmt.Errorf("parse public key: %w", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return KeyIdentity{}, fmt.Errorf("marshal public key: %w", err)
	}
	return KeyIdentity{
		Algorithm:   publicKeyAlgorithm(pub),
		Fingerprint: fingerprintBytes(der),
		RawKey:      der,
	}, nil
}

func publicKeyAlgorithm(pub any) string {
	switch k := pub.(type) {
	case ed25519.PublicKey:
		return "ed25519"
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256():
			return "ecdsa-p256"
		case elliptic.P384():
			return "ecdsa-p384"
		case elliptic.P521():
			return "ecdsa-p521"
		default:
			return "ecdsa"
		}
	case *rsa.PublicKey:
		return "rsa"
	default:
		return "unknown"
	}
}
