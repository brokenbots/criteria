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

	"github.com/brokenbots/criteria/internal/testutil"
	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
)

// TestNoopAttachedRunnerWaitBlocksUntilKill verifies that Wait does not return
// immediately (the former bug) and only unblocks after Kill is called.
func TestNoopAttachedRunnerWaitBlocksUntilKill(t *testing.T) {
	r := newNoopAttachedRunner()

	done := make(chan error, 1)
	go func() {
		done <- r.Wait(context.Background())
	}()

	// Wait must still be blocking after a short interval.
	select {
	case err := <-done:
		t.Fatalf("Wait returned early (before Kill): %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Kill must unblock Wait.
	if err := r.Kill(context.Background()); err != nil {
		t.Fatalf("Kill returned error: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait returned non-nil error after Kill: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not unblock within 2 s after Kill")
	}
}

// TestNoopAttachedRunnerWaitContextCancel verifies that Wait unblocks when the
// context is cancelled even without calling Kill.
func TestNoopAttachedRunnerWaitContextCancel(t *testing.T) {
	r := newNoopAttachedRunner()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- r.Wait(ctx)
	}()

	// Wait must still be blocking before cancel.
	select {
	case err := <-done:
		t.Fatalf("Wait returned early (before cancel): %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Wait returned nil; expected context.Canceled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not unblock within 2 s after context cancel")
	}
}

// TestNoopAttachedRunnerKillIdempotent verifies that calling Kill multiple
// times does not panic (close of closed channel guard).
func TestNoopAttachedRunnerKillIdempotent(t *testing.T) {
	r := newNoopAttachedRunner()
	if err := r.Kill(context.Background()); err != nil {
		t.Fatalf("first Kill: %v", err)
	}
	if err := r.Kill(context.Background()); err != nil {
		t.Fatalf("second Kill: %v", err)
	}
}

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
