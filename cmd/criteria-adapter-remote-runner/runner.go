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
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	hplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
	"github.com/brokenbots/criteria/internal/adapter/manifest"
)

const reconnectDelay = 2 * time.Second

// remoteConfig holds the environment-driven remote connection and identity
// settings.
type remoteConfig struct {
	Host     string
	Token    string
	Digest   string
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

// pluginClient adapts the hashicorp/go-plugin client side for the v2 adapter
// contract. Dispensing the "adapter" plugin yields a v2.AdapterServiceClient.
type pluginClient struct {
	hplugin.NetRPCUnsupportedPlugin
}

func (p *pluginClient) GRPCServer(_ *hplugin.GRPCBroker, _ *grpc.Server) error {
	return errors.New("GRPCServer should not be called on the adapter runner")
}

//nolint:unparam // hashicorp/go-plugin Plugin.GRPCClient interface requires an error return value.
func (p *pluginClient) GRPCClient(_ context.Context, _ *hplugin.GRPCBroker, cc *grpc.ClientConn) (interface{}, error) {
	return v2.NewAdapterServiceClient(cc), nil
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

func startAdapter(binary string) (v2.AdapterServiceClient, func(), error) {
	cmd := exec.Command(binary)
	cmd.Env = adapterEnv()
	client := hplugin.NewClient(&hplugin.ClientConfig{
		HandshakeConfig:  adapterhost.HandshakeConfig,
		Plugins:          map[string]hplugin.Plugin{"adapter": &pluginClient{}},
		Cmd:              cmd,
		AllowedProtocols: []hplugin.Protocol{hplugin.ProtocolGRPC},
		StartTimeout:     30 * time.Second,
		// go-plugin normally re-appends os.Environ() to cmd.Env. We construct
		// the child environment explicitly (filtering the runner's own remote
		// connection variables), so we must prevent that re-addition.
		SkipHostEnv: true,
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

	svc, ok := raw.(v2.AdapterServiceClient)
	if !ok {
		client.Kill()
		return nil, nil, fmt.Errorf("dispense returned unexpected type %T", raw)
	}

	kill := func() {
		client.Kill()
	}
	return svc, kill, nil
}

// proxyService forwards the v2 adapter contract to a local adapter process.
type proxyService struct {
	client v2.AdapterServiceClient
}

func (p *proxyService) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	return p.client.Info(ctx, req)
}

func (p *proxyService) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return p.client.OpenSession(ctx, req)
}

func (p *proxyService) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return p.client.CloseSession(ctx, req)
}

func (p *proxyService) Execute(ctx context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	stream, err := p.client.Execute(ctx, req)
	if err != nil {
		return err
	}
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := sink.Send(ev); err != nil {
			return err
		}
	}
}

func (p *proxyService) Log(ctx context.Context, req *v2.LogRequest, sink adapterhost.LogEventSender) error {
	stream, err := p.client.Log(ctx, req)
	if err != nil {
		return err
	}
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := sink.Send(ev); err != nil {
			return err
		}
	}
}

func (p *proxyService) Permissions(ctx context.Context, requests adapterhost.PermissionsStream) error {
	stream, err := p.client.Permissions(ctx)
	if err != nil {
		return err
	}

	senderCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sendDone := make(chan error, 1)
	go func() { sendDone <- runPermissionSender(senderCtx, stream, requests) }()

	recvErr := recvPermissionDecisions(ctx, stream, requests)
	cancel()
	if senderErr := <-sendDone; recvErr == nil {
		return senderErr
	}
	return recvErr
}

func runPermissionSender(ctx context.Context, stream v2.AdapterService_PermissionsClient, requests adapterhost.PermissionsStream) error {
	for {
		select {
		case <-ctx.Done():
			return stream.CloseSend()
		default:
		}
		req, err := requests.Recv()
		if errors.Is(err, io.EOF) {
			if err := stream.CloseSend(); err != nil && ctx.Err() == nil {
				return err
			}
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // context cancelled; suppressing recv error is intentional
			}
			return err
		}
		if err := stream.Send(req); err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // context cancelled; suppressing send error is intentional
			}
			return err
		}
	}
}

func recvPermissionDecisions(ctx context.Context, stream v2.AdapterService_PermissionsClient, requests adapterhost.PermissionsStream) error {
	for {
		dec, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // context cancelled; suppressing recv error is intentional
			}
			return err
		}
		if err := requests.Send(dec); err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // context cancelled; suppressing send error is intentional
			}
			return err
		}
	}
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

	proxy := &proxyService{client: client}
	opts := &adapterhost.ServeRemoteOptions{
		Host:        cfg.Host,
		TLSConfig:   tlsConf,
		AcceptToken: cfg.Token,
		Identity: adapterhost.RemoteIdentity{
			Name:    cfg.Name,
			Version: cfg.Version,
			Digest:  cfg.Digest,
		},
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- adapterhost.ServeRemote(proxy, opts) }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-serveErr:
		return err
	}
}
