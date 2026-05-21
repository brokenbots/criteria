package adapterhost

import (
"context"
"fmt"
"net"
"os"

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
func LocalSocketDialer(ctx context.Context, socketPath string) (Client, *hplugin.Client, error) {
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
return &noopAttachedRunner{}, nil
}
}

// noopAttachedRunner is an AttachedRunner whose lifecycle is managed entirely
// by the caller. Kill and Wait are no-ops; the external process is not touched.
type noopAttachedRunner struct{}

func (*noopAttachedRunner) Wait(_ context.Context) error                             { return nil }
func (*noopAttachedRunner) Kill(_ context.Context) error                             { return nil }
func (*noopAttachedRunner) ID() string                                               { return "external" }
func (*noopAttachedRunner) PluginToHost(n, a string) (string, string, error)        { return n, a, nil }
func (*noopAttachedRunner) HostToPlugin(n, a string) (string, string, error)        { return n, a, nil }

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
