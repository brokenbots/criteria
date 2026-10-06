package remote

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// stubPluginClient records Kill calls so tests can observe whether the shim
// released an abandoned reattach plugin client. Without the abandon drain in
// bridgeAndDial a canceled bridge leaves the dialer's eventual successful
// result stranded: the client is never killed and its goroutines leak (the
// internal/engine goleak failure).
type stubPluginClient struct{ kills atomic.Int32 }

func (p *stubPluginClient) Kill() { p.kills.Add(1) }
func (p *stubPluginClient) Exited() bool { return p.kills.Load() > 0 }

// stubDialer stands in for the reattach dialer: it blocks until release is
// closed and then reports a successful dial carrying the plugin client, so the
// abandon window in bridgeAndDial stays open until the test chooses to close
// it. It deliberately ignores ctx: the real LocalSocketDialer also completes
// independently of cancellation once past its entry check.
type stubDialer struct {
	release chan struct{}
	plugin  adapterhost.PluginLifecycle
}

func (d *stubDialer) dial(_ context.Context, _ string) (adapterhost.Client, adapterhost.PluginLifecycle, error) {
	<-d.release
	return nil, d.plugin, nil
}

// stubAddr satisfies net.Addr for the test listener.
type stubAddr struct{}

func (stubAddr) Network() string { return "stub" }
func (stubAddr) String() string  { return "stub" }

// controllableListener hands out connections pushed to it and only errors on
// close, so tests decide exactly when Accept completes.
type controllableListener struct {
	deliver   chan net.Conn
	stop      chan struct{}
	closeOnce sync.Once
}

func newControllableListener() *controllableListener {
	return &controllableListener{deliver: make(chan net.Conn, 1), stop: make(chan struct{})}
}

func (l *controllableListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.deliver:
		return c, nil
	case <-l.stop:
		return nil, io.EOF
	}
}

func (l *controllableListener) Close() error {
	l.closeOnce.Do(func() { close(l.stop) })
	return nil
}

func (l *controllableListener) Addr() net.Addr { return stubAddr{} }

func waitPluginKilled(t *testing.T, plugin *stubPluginClient, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := plugin.kills.Load(); got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("abandoned plugin client Kill count = %d, want %d", plugin.kills.Load(), want)
}

// pumpByte pushes one byte across an established bridge (writeEnd is the
// adapter-facing remote end, readEnd the host-facing remote end) so the test
// synchronizes on the bridge copiers actually running without sleeps. It
// blocks until the byte has traversed the full bridge or within elapses.
func pumpByte(writeEnd, readEnd net.Conn, within time.Duration) error {
	go func() {
		_, _ = writeEnd.Write([]byte("x"))
	}()
	_ = readEnd.SetReadDeadline(time.Now().Add(within))
	buf := make([]byte, 1)
	if _, err := readEnd.Read(buf); err != nil {
		return errors.New("bridge copier did not relay the byte in time")
	}
	return nil
}

// TestShim_BridgeAndDial_CancelBeforeUdsAccept pins the abandon drain in the
// ctx.Done arm of the first select: cancellation wins before the UDS accept
// completes and the still-in-flight dial later succeeds, so its plugin client
// must be killed instead of leaked.
func TestShim_BridgeAndDial_CancelBeforeUdsAccept(t *testing.T) {
	plugin := &stubPluginClient{}
	dialer := &stubDialer{release: make(chan struct{}), plugin: plugin}

	hostConn, remoteEnd := net.Pipe()
	t.Cleanup(func() { _ = hostConn.Close(); _ = remoteEnd.Close() })
	lis := newControllableListener()
	t.Cleanup(func() { _ = lis.Close() })

	shim := &Shim{dialLocal: dialer.dial}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // abandon before any select arm can succeed

	errCh := make(chan error, 1)
	go func() {
		_, err := shim.bridgeAndDial(ctx, hostConn, lis, "unused.sock")
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("bridgeAndDial err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridgeAndDial did not return after cancellation")
	}

	close(dialer.release) // the dial now "completes" successfully
	waitPluginKilled(t, plugin, 1)
}

// TestShim_BridgeAndDial_CancelAfterUdsAccept pins the abandon drain in the
// ctx.Done arm of the second select — the arm the internal/engine goleak
// failure hits: the UDS accept and the bridge copiers are already running,
// then cancellation beats the still-in-flight dial, whose eventual successful
// result must be drained and killed.
func TestShim_BridgeAndDial_CancelAfterUdsAccept(t *testing.T) {
	plugin := &stubPluginClient{}
	dialer := &stubDialer{release: make(chan struct{}), plugin: plugin}

	hostConn, remoteEnd := net.Pipe()
	t.Cleanup(func() { _ = hostConn.Close(); _ = remoteEnd.Close() })
	udsConn, udsRemote := net.Pipe()
	t.Cleanup(func() { _ = udsConn.Close(); _ = udsRemote.Close() })
	lis := newControllableListener()
	t.Cleanup(func() { _ = lis.Close() })

	shim := &Shim{dialLocal: dialer.dial}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := shim.bridgeAndDial(ctx, hostConn, lis, "unused.sock")
		errCh <- err
	}()

	lis.deliver <- udsConn // the UDS accept now wins the first select
	if err := pumpByte(udsRemote, remoteEnd, 2*time.Second); err != nil {
		t.Fatalf("%v", err)
	}

	cancel() // abandon the bridge while the dial is still blocked
	close(dialer.release)

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("bridgeAndDial err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridgeAndDial did not return after cancellation")
	}
	waitPluginKilled(t, plugin, 1)
}

// TestShim_BridgeAndDial_UDSAcceptError pins the abandon drain in the
// udsErrCh arm: a failed accept abandons the bridge while the dial is still
// in flight, so its eventual successful plugin client must be killed.
func TestShim_BridgeAndDial_UDSAcceptError(t *testing.T) {
	plugin := &stubPluginClient{}
	dialer := &stubDialer{release: make(chan struct{}), plugin: plugin}

	hostConn, remoteEnd := net.Pipe()
	t.Cleanup(func() { _ = hostConn.Close(); _ = remoteEnd.Close() })
	lis := newControllableListener()
	_ = lis.Close() // fail the accept immediately

	shim := &Shim{dialLocal: dialer.dial}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := shim.bridgeAndDial(ctx, hostConn, lis, "unused.sock")
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if !strings.Contains(err.Error(), "uds accept") {
			t.Fatalf("bridgeAndDial err = %v, want a wrapped uds accept error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridgeAndDial did not return after the accept failed")
	}

	close(dialer.release)
	waitPluginKilled(t, plugin, 1)
}