package adapterhost

import (
"context"
"net"
"os"
"os/exec"
"path/filepath"
"testing"
"time"

hplugin "github.com/hashicorp/go-plugin"

v2 "github.com/brokenbots/criteria/proto/criteria/v2"
"github.com/brokenbots/criteria/internal/testutil"
)

// TestNewHostOnlyUDSSocket verifies that the helper creates a 0700 directory
// and that cleanup removes it.
func TestNewHostOnlyUDSSocket(t *testing.T) {
path, cleanup, err := NewHostOnlyUDSSocket()
if err != nil {
t.Fatalf("NewHostOnlyUDSSocket: %v", err)
}

dir := filepath.Dir(path)
info, err := os.Stat(dir)
if err != nil {
cleanup()
t.Fatalf("stat socket dir: %v", err)
}
if info.Mode()&os.ModePerm != 0o700 {
cleanup()
t.Errorf("socket dir mode=%o want 0700", info.Mode()&os.ModePerm)
}

// Cleanup must remove the directory.
cleanup()
if _, err := os.Stat(dir); !os.IsNotExist(err) {
t.Error("expected socket dir to be removed after cleanup")
}
}

// TestLocalSocketDialer verifies that LocalSocketDialer can reattach to an
// already-running adapter plugin listening on a Unix domain socket and dispatch
// the Info RPC successfully.
func TestLocalSocketDialer(t *testing.T) {
adapterBin := testutil.BuildPermissiveAdapter(t)

// Create a temp directory; go-plugin will create the socket file there
// when PLUGIN_UNIX_SOCKET_DIR is set.
socketDir, err := os.MkdirTemp("", "criteria-reattach-test-*")
if err != nil {
t.Fatal(err)
}
t.Cleanup(func() { _ = os.RemoveAll(socketDir) })

// Start the permissive adapter, instructing go-plugin's server to bind
// to a UDS in socketDir instead of a random TCP port.
cmd := exec.Command(adapterBin)
cmd.Env = append(os.Environ(), "PLUGIN_UNIX_SOCKET_DIR="+socketDir)

firstClient := hplugin.NewClient(&hplugin.ClientConfig{
HandshakeConfig:  HandshakeConfig,
Plugins:          AdapterMap(),
AllowedProtocols: []hplugin.Protocol{hplugin.ProtocolGRPC},
Cmd:              cmd,
Logger:           adapterClientLogger(),
StartTimeout:     30 * time.Second,
})
t.Cleanup(firstClient.Kill)

if _, err := firstClient.Client(); err != nil {
t.Fatalf("start adapter: %v", err)
}

// Retrieve the socket address from the running client.
reattach := firstClient.ReattachConfig()
if reattach == nil {
t.Fatal("no reattach config from first client")
}
unixAddr, ok := reattach.Addr.(*net.UnixAddr)
if !ok {
t.Skipf("adapter bound to %T (not UDS) — set PLUGIN_UNIX_SOCKET_DIR and try again", reattach.Addr)
}

// Reattach using LocalSocketDialer.
ctx := context.Background()
c, client2, err := LocalSocketDialer(ctx, unixAddr.Name)
if err != nil {
t.Fatalf("LocalSocketDialer: %v", err)
}
t.Cleanup(client2.Kill)

resp, err := c.Info(ctx, &v2.InfoRequest{})
if err != nil {
t.Fatalf("Info via reattached client: %v", err)
}
if resp.GetName() != "permissive" {
t.Errorf("adapter name=%q want permissive", resp.GetName())
}
}
