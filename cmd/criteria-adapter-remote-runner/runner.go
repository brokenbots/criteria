// Package main implements a generic remote adapter runner.
//
// The runner wraps an existing local-mode Criteria adapter binary and exposes
// it to a remote Criteria host via the phone-home model. It starts the adapter
// as a child process, dials the host using environment-driven configuration,
// and proxies the v2 adapter gRPC contract between the host and the local
// adapter.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	hplugin "github.com/hashicorp/go-plugin"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
	"github.com/brokenbots/criteria/internal/adapter/manifest"
	internaladapterhost "github.com/brokenbots/criteria/internal/adapterhost"
)

const reconnectDelay = 2 * time.Second

// remoteConfig holds the environment-driven remote connection and identity
// settings.
type remoteConfig struct {
	Host     string
	Token    string
	Digest   string
	Scope    string
	Name     string
	Version  string
	Binary   string
	Manifest string
	TLSCert  string
	TLSKey   string
	TLSCA    string
	LogLevel string
}

func loadConfig() remoteConfig {
	return remoteConfig{
		Host:     os.Getenv("CRITERIA_REMOTE_HOST"),
		Token:    os.Getenv("CRITERIA_REMOTE_TOKEN"),
		Digest:   os.Getenv("CRITERIA_REMOTE_DIGEST"),
		Scope:    os.Getenv("CRITERIA_REMOTE_SCOPE"),
		Name:     os.Getenv("CRITERIA_ADAPTER_NAME"),
		Version:  os.Getenv("CRITERIA_ADAPTER_VERSION"),
		Binary:   os.Getenv("CRITERIA_ADAPTER_BINARY"),
		Manifest: os.Getenv("CRITERIA_ADAPTER_MANIFEST"),
		TLSCert:  os.Getenv("CRITERIA_REMOTE_TLS_CERT"),
		TLSKey:   os.Getenv("CRITERIA_REMOTE_TLS_KEY"),
		TLSCA:    os.Getenv("CRITERIA_REMOTE_CA"),
		LogLevel: os.Getenv("CRITERIA_LOG_LEVEL"),
	}
}

func (c *remoteConfig) resolve(log *slog.Logger) error {
	if c.Host == "" {
		return errors.New("CRITERIA_REMOTE_HOST is required")
	}

	if err := c.resolveFromManifest(); err != nil {
		return err
	}
	c.resolveDefaults()

	if c.Binary == "" {
		return errors.New("could not locate an adapter binary; set CRITERIA_ADAPTER_BINARY")
	}
	if err := c.resolveBinaryPath(); err != nil {
		return err
	}

	log.Info("remote adapter resolved",
		"name", c.Name,
		"version", c.Version,
		"binary", c.Binary,
		"host", c.Host,
	)
	return nil
}

func (c *remoteConfig) resolveFromManifest() error {
	if c.Manifest == "" {
		return nil
	}
	m, err := manifest.ParseFile(c.Manifest)
	if err != nil {
		return fmt.Errorf("read manifest %q: %w", c.Manifest, err)
	}
	if c.Name == "" {
		c.Name = m.Name
	}
	if c.Version == "" {
		c.Version = m.Version
	}
	if c.Binary == "" {
		c.Binary = defaultBinaryPath(m.Name)
	}
	return nil
}

func (c *remoteConfig) resolveDefaults() {
	if c.Binary == "" {
		c.Binary = defaultBinaryPath(c.Name)
	}
	if c.Binary == "" {
		c.Binary = firstAdapterBinary()
	}
	if c.Name == "" {
		c.Name = nameFromBinary(c.Binary)
	}
	if c.Version == "" {
		c.Version = "0.0.0"
	}
}

func (c *remoteConfig) resolveBinaryPath() error {
	if strings.Contains(c.Binary, string(filepath.Separator)) {
		return nil
	}
	p, err := exec.LookPath(c.Binary)
	if err != nil {
		return fmt.Errorf("adapter binary %q not found on PATH: %w", c.Binary, err)
	}
	c.Binary = p
	return nil
}

func defaultBinaryPath(name string) string {
	if name == "" {
		return ""
	}
	return "/usr/local/bin/criteria-adapter-" + name
}

func nameFromBinary(binary string) string {
	base := filepath.Base(binary)
	const prefix = "criteria-adapter-"
	if strings.HasPrefix(base, prefix) {
		return strings.TrimPrefix(base, prefix)
	}
	return base
}

func firstAdapterBinary() string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "criteria-adapter-") && !strings.Contains(e.Name(), "remote-runner") {
				return filepath.Join(dir, e.Name())
			}
		}
	}
	return ""
}

func buildTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error) {
	if certPath == "" && keyPath == "" && caPath == "" {
		return nil, nil
	}
	if certPath == "" || keyPath == "" || caPath == "" {
		return nil, errors.New("mTLS requires all of CRITERIA_REMOTE_TLS_CERT, CRITERIA_REMOTE_TLS_KEY, and CRITERIA_REMOTE_CA")
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read client cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read client key: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load client key pair: %w", err)
	}

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("parse CA")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
	}, nil
}

// remoteEnvVars lists the runner's own remote-connection settings. They must
// never be inherited by the child adapter, because adapters such as
// criteria-adapter-shell (>= v0.5.3) and criteria-adapter-copilot (>= v0.5.5)
// detect CRITERIA_REMOTE_HOST in their environment and switch into
// ServeRemote/phone-home mode, abandoning the local go-plugin handshake.
var remoteEnvVars = []string{
	"CRITERIA_REMOTE_HOST",
	"CRITERIA_REMOTE_TOKEN",
	"CRITERIA_REMOTE_DIGEST",
	"CRITERIA_REMOTE_TLS_CERT",
	"CRITERIA_REMOTE_TLS_KEY",
	"CRITERIA_REMOTE_CA",
}

// adapterEnv returns the current process environment with the runner's
// remote-connection variables removed, suitable for starting a child adapter.
func adapterEnv() []string {
	env := os.Environ()
	out := env[:0]
	for _, kv := range env {
		name, _, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		if slicesContains(remoteEnvVars, name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func slicesContains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func startAdapter(binary string) (internaladapterhost.Client, func(), error) {
	cmd := exec.Command(binary)
	cmd.Env = adapterEnv()
	client := hplugin.NewClient(&hplugin.ClientConfig{
		HandshakeConfig:  adapterhost.HandshakeConfig,
		Plugins:          internaladapterhost.AdapterMap(),
		Cmd:              cmd,
		AllowedProtocols: []hplugin.Protocol{hplugin.ProtocolGRPC},
		StartTimeout:     30 * time.Second,
		// go-plugin normally re-appends os.Environ() to cmd.Env. We construct
		// the child environment explicitly (filtering the runner's own remote
		// connection variables), so we must prevent that re-addition.
		SkipHostEnv: true,
		// CRI-276: the runner holds this client for the pod's lifetime and
		// may see long idle stretches while the host works other sessions.
		// Disable the 30-minute client idle timeout; the pings stay at or
		// above grpc-go's default server enforcement floor, which the child
		// adapter's go-plugin server uses.
		GRPCDialOptions: internaladapterhost.RemoteKeepaliveDialOptions(),
	})

	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, nil, fmt.Errorf("handshake with adapter: %w", err)
	}

	raw, err := rpcClient.Dispense("adapter")
	if err != nil {
		client.Kill()
		return nil, nil, fmt.Errorf("dispense adapter: %w", err)
	}

	svc, ok := raw.(internaladapterhost.Client)
	if !ok {
		client.Kill()
		return nil, nil, fmt.Errorf("dispense returned unexpected type %T", raw)
	}

	kill := func() {
		client.Kill()
	}
	return svc, kill, nil
}

func runRemote(log *slog.Logger) error {
	cfg := loadConfig()
	if err := cfg.resolve(log); err != nil {
		return err
	}

	tlsConf, err := buildTLSConfig(cfg.TLSCert, cfg.TLSKey, cfg.TLSCA)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	for ctx.Err() == nil {
		if err := serveOnce(ctx, &cfg, tlsConf, log); err != nil {
			log.Error("remote adapter session ended", "error", err)
		}
		if ctx.Err() != nil {
			break
		}
		timer := time.NewTimer(reconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	log.Info("shutting down on signal")
	return nil
}

func serveOnce(ctx context.Context, cfg *remoteConfig, tlsConf *tls.Config, log *slog.Logger) error {
	client, kill, err := startAdapter(cfg.Binary)
	if err != nil {
		return err
	}
	defer kill()

	info, err := client.Info(ctx, &v2.InfoRequest{})
	if err != nil {
		return fmt.Errorf("adapter Info: %w", err)
	}
	log.Info("adapter ready", "adapter_name", info.GetName(), "adapter_version", info.GetVersion())

	serveErr := make(chan error, 1)
	go func() { serveErr <- serveRemoteOnce(ctx, cfg, tlsConf, client, log) }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-serveErr:
		return err
	}
}
