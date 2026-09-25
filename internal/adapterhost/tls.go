package adapterhost

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// LoadClientTLS builds the mTLS client configuration for phone-home
// connections from the CRITERIA_REMOTE_TLS_CERT / CRITERIA_REMOTE_TLS_KEY /
// CRITERIA_REMOTE_CA paths. All three paths are all-or-nothing: when none is
// set the connection is plaintext and nil is returned; when any is set all
// must be present. The returned config is pinned to TLS 1.2+ (the go
// toolchain's floor is 1.2, but it is set explicitly so the policy survives
// future GODEBUG changes). Runners and peers must call this instead of
// building their own tls.Config so the mTLS policy has one implementation.
func LoadClientTLS(certPath, keyPath, caPath string) (*tls.Config, error) {
	if certPath == "" && keyPath == "" && caPath == "" {
		return nil, nil
	}
	if certPath == "" || keyPath == "" || caPath == "" {
		return nil, errors.New("mTLS requires all of CRITERIA_REMOTE_TLS_CERT, CRITERIA_REMOTE_TLS_KEY, and CRITERIA_REMOTE_CA")
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load client key pair: %w", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("parse CA bundle")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
