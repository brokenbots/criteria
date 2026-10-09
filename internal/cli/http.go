package cli

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
)

// serverHTTPClient builds the HTTP/2 client used by `criteria` CLI commands
// that talk to the server. It mirrors the runtime agent transport: cleartext
// h2c for plain `http://` URLs, standard TLS for `https://`, and mTLS when a
// client cert+key are provided.
//
// serverURL selects the scheme. When caFile is non-empty it is loaded as the
// root bundle for server verification; certFile/keyFile enable mTLS.
func serverHTTPClient(serverURL, caFile, certFile, keyFile string) (*http.Client, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("parse server url: %w", err)
	}
	switch u.Scheme {
	case "http":
		if certFile != "" || keyFile != "" || caFile != "" {
			return nil, errors.New("TLS flags require an https:// server url")
		}
		// h2c with prior knowledge and no protocol fallback — parity with the
		// deprecated http2.Transport{AllowHTTP} this client was built on.
		p := &http.Protocols{}
		p.SetUnencryptedHTTP2(true)
		return &http.Client{Transport: &http.Transport{Protocols: p}}, nil
	case "https":
		return buildHTTPSClient(caFile, certFile, keyFile)
	default:
		return nil, fmt.Errorf("unsupported server url scheme %q (want http or https)", u.Scheme)
	}
}

// buildHTTPSClient constructs an HTTP/2 client with TLS configured for https://
// server URLs. When caFile is set, it overrides the system root pool. When both
// certFile and keyFile are set, mTLS is enabled.
func buildHTTPSClient(caFile, certFile, keyFile string) (*http.Client, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, errors.New("invalid ca bundle")
		}
		cfg.RootCAs = pool
	}
	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, errors.New("mtls requires both --tls-cert and --tls-key")
		}
		crt, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load client cert: %w", err)
		}
		cfg.Certificates = []tls.Certificate{crt}
	}
	// h2 over TLS via ALPN only, no protocol fallback — parity with the
	// deprecated http2.Transport{TLSClientConfig} this client was built on.
	p := &http.Protocols{}
	p.SetHTTP2(true)
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, Protocols: p}}, nil
}
