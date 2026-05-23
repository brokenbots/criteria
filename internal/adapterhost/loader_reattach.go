package adapterhost

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	hplugin "github.com/hashicorp/go-plugin"
	"github.com/hashicorp/go-plugin/runner"
)

// LocalSocketDialer returns a Client that is reattached to an already-listening
// Unix domain socket. Used by the remote-adapter shim (WS20) to hand the host
// session layer a "local-looking" adapter that is actually proxying to a remote
// endpoint.
//
// Socket security contract (S3.4): the caller is responsible for creating the
// socket in a host-only directory (mode 0o700), setting the socket file's mode
// to 0o600 after Listen, and cleaning up the directory on session close.
// LocalSocketDialer does not create or manage the socket file.
func LocalSocketDialer(ctx context.Context, socketPath string) (adapterClient Client, pluginClient *hplugin.Client, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	cfg := &hplugin.ClientConfig{
		HandshakeConfig:  HandshakeConfig,
		Plugins:          AdapterMap(),
		AllowedProtocols: []hplugin.Protocol{hplugin.ProtocolGRPC},
		Logger:           adapterClientLogger(),
		Reattach: &hplugin.ReattachConfig{
			Protocol:        hplugin.ProtocolGRPC,
			ProtocolVersion: int(HandshakeConfig.ProtocolVersion),
			Addr:            &net.UnixAddr{Name: socketPath, Net: "unix"},
			ReattachFunc:    externalProcessReattach(socketPath),
		},
	}
	client := hplugin.NewClient(cfg)
	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, nil, fmt.Errorf("reattach grpc client at %q: %w", socketPath, err)
	}
	raw, err := rpcClient.Dispense(AdapterName)
	if err != nil {
		client.Kill()
		return nil, nil, fmt.Errorf("dispense adapter at %q: %w", socketPath, err)
	}
	c, ok := raw.(Client)
	if !ok {
		client.Kill()
		return nil, nil, fmt.Errorf("unexpected adapter client type %T at %q", raw, socketPath)
	}
	return c, client, nil
}

// externalProcessReattach returns a ReattachFunc that connects to the given
// Unix socket without assuming ownership of any local process. This is used by
// LocalSocketDialer where the adapter process is managed externally (e.g., a
// remote shim in WS20 or a test helper that started the server separately).
func externalProcessReattach(socketPath string) runner.ReattachFunc {
	return func() (runner.AttachedRunner, error) {
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			return nil, fmt.Errorf("verify socket %q: %w", socketPath, err)
		}
		_ = conn.Close()
		return newNoopAttachedRunner(), nil
	}
}

// noopAttachedRunner is an AttachedRunner whose lifecycle is managed entirely
// by the caller. The external process is never touched; Kill signals Wait to
// return so that go-plugin's internal client goroutine unblocks cleanly.
type noopAttachedRunner struct {
	once sync.Once
	done chan struct{}
}

func newNoopAttachedRunner() *noopAttachedRunner {
	return &noopAttachedRunner{done: make(chan struct{})}
}

// Wait blocks until Kill is called or the context is cancelled.  This is
// required so that go-plugin's client goroutine does not mark the plugin as
// exited (and cancel all in-flight RPCs) the instant the reattach handshake
// completes.
func (r *noopAttachedRunner) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return nil
	}
}

// Kill unblocks Wait, signalling that the externally managed adapter has been
// released by the host side. It does not send any signal to the external process.
func (r *noopAttachedRunner) Kill(_ context.Context) error {
	r.once.Do(func() { close(r.done) })
	return nil
}

func (r *noopAttachedRunner) ID() string                                              { return "external" }
func (r *noopAttachedRunner) PluginToHost(n, a string) (host, addr string, err error) { return n, a, nil }
func (r *noopAttachedRunner) HostToPlugin(n, a string) (plugin, addr string, err error) {
	return n, a, nil
}

// NewHostOnlyUDSSocket creates a host-private temporary directory (mode 0o700)
// and returns a socket path within it. The caller must pass the directory
// (filepath.Dir of the returned path) to the adapter binary via
// PLUGIN_UNIX_SOCKET_DIR so that go-plugin creates the socket there.
//
// The returned cleanup function removes the directory and all its contents.
// Call it from a defer immediately after a successful return.
func NewHostOnlyUDSSocket() (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "criteria-adapter-*")
	if err != nil {
		return "", nil, fmt.Errorf("create adapter socket dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("chmod adapter socket dir: %w", err)
	}
	socketPath := dir + "/adapter.sock"
	cleanup = func() { _ = os.RemoveAll(dir) }
	return socketPath, cleanup, nil
}
