package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"

	"golang.org/x/net/http2"
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
		return &http.Client{Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}}, nil
	case "https":
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
		return &http.Client{Transport: &http2.Transport{TLSClientConfig: cfg}}, nil
	default:
		return nil, fmt.Errorf("unsupported server url scheme %q (want http or https)", u.Scheme)
	}
}
