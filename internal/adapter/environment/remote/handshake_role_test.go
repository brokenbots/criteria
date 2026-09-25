package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

// --- helpers ---

// capturedLogs is a concurrency-safe log buffer. The shim writes slog output
// from its accept goroutines while test goroutines poll the captured text
// (waitForLog, failure reporting), so every read and write must be guarded.
type capturedLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capturedLogs) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturedLogs) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *capturedLogs) contains(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Contains(c.buf.String(), substr)
}

// captureLogs swaps the default slog logger for one writing to a returned
// concurrency-safe buffer, restoring the previous logger on cleanup.
func captureLogs(t *testing.T) *capturedLogs {
	t.Helper()
	var logs capturedLogs
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &logs
}

// waitForLog polls captured log output until it contains the substring or
// the timeout elapses; log lines written from server-side goroutines land
// asynchronously relative to client-observed connection state.
func waitForLog(t *testing.T, logs *capturedLogs, substr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if logs.contains(substr) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// dialRawHandshake opens a conn to the shim, writes the marshaled handshake
// frame, and returns the conn so the test can observe close behavior.
func dialRawHandshake(t *testing.T, addr string, hs *handshakeMessage) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	data, err := json.Marshal(hs)
	if err != nil {
		t.Fatalf("marshal handshake: %v", err)
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		_ = conn.Close()
		t.Fatalf("write handshake: %v", err)
	}
	return conn
}

// padFrame returns the handshake JSON padded with spaces to exactly n bytes
// of content (plus the newline). JSON tolerates surrounding whitespace, so a
// padded frame parses normally when within the cap.
func padFrame(hs *handshakeMessage, n int) []byte {
	data, err := json.Marshal(hs)
	if err != nil {
		panic(err)
	}
	if len(data) > n {
		panic(fmt.Sprintf("frame of %d bytes exceeds pad target %d", len(data), n))
	}
	pad := bytes.Repeat([]byte(" "), n-len(data))
	return append(append([]byte{}, pad...), data...)
}

// assertConnClosed asserts the peer closed the conn within a bounded wait.
// A local read-deadline expiry is NOT accepted as evidence of a close — an
// implementation that emits the rejection but never closes would otherwise
// pass. Only EOF, connection reset, or a closed-network-conn error ends the
// wait; the wait itself must be short so a non-closing implementation fails
// fast and loudly.
func assertConnClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	for {
		_, err := conn.Read(buf)
		if err == nil {
			continue // peer sent data without closing; keep waiting
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("peer did not close the conn within the deadline; a read timeout is not evidence of close")
		}
		// EOF (graceful FIN), ECONNRESET, and net.ErrClosed are the close
		// signals on TCP; any other error is unexpected and must fail loudly.
		if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("unexpected read error while waiting for peer close: %v", err)
		}
		return
	}
}

// recordingPeerAcceptor is a fake PeerAcceptor that records the handed-over
// conn + identity and blocks until released, mirroring the seam contract that
// the acceptor owns the connection.
type recordingPeerAcceptor struct {
	mu      sync.Mutex
	conns   []net.Conn
	idents  []*PeerClientIdentity
	release chan struct{}

	called     chan struct{}
	calledOnce sync.Once
}

func newRecordingPeerAcceptor() *recordingPeerAcceptor {
	return &recordingPeerAcceptor{release: make(chan struct{}), called: make(chan struct{})}
}

func (a *recordingPeerAcceptor) AcceptPeer(ctx context.Context, conn net.Conn, identity *PeerClientIdentity) error {
	a.mu.Lock()
	a.conns = append(a.conns, conn)
	a.idents = append(a.idents, identity)
	a.mu.Unlock()
	a.calledOnce.Do(func() { close(a.called) })
	<-a.release
	_ = conn.Close()
	return nil
}

func (a *recordingPeerAcceptor) waitCalled(timeout time.Duration) bool {
	select {
	case <-a.called:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (a *recordingPeerAcceptor) assertNotCalled(timeout time.Duration) error {
	select {
	case <-a.called:
		return errors.New("peer acceptor was invoked")
	case <-time.After(timeout):
		return nil
	}
}

func (a *recordingPeerAcceptor) lastIdentity() *PeerClientIdentity {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.idents) == 0 {
		return nil
	}
	return a.idents[len(a.idents)-1]
}

func (a *recordingPeerAcceptor) releaseAll() { close(a.release) }

// startTestShim builds and starts a plain-TCP shim with the standard test
// digest verifier.
func startTestShim(t *testing.T, cfg *Config) (shim *Shim, addr string) {
	t.Helper()
	shim, err := NewShim(cfg, &fixedDigestVerifier{allowed: map[string]string{"noop": "sha256:abcd1234"}})
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	if err := shim.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = shim.Stop(context.Background()) })
	return shim, shim.listener.Addr().String()
}

// --- frame cap ---

func TestReadHandshakeFrame_CapBoundaries(t *testing.T) {
	// A frame of exactly cap bytes must be accepted; only longer frames are
	// rejected, so the cap never rejects a compliant frame.
	exact := append(bytes.Repeat([]byte("a"), handshakeFrameCap), '\n')
	header, err := readHandshakeFrame(bufio.NewReader(bytes.NewReader(exact)), handshakeFrameCap)
	if err != nil {
		t.Fatalf("exactly-cap frame rejected: %v", err)
	}
	if len(header) != handshakeFrameCap {
		t.Fatalf("header length = %d, want %d", len(header), handshakeFrameCap)
	}

	// cap+1 bytes without a newline must be rejected with the sentinel.
	over := append(bytes.Repeat([]byte("a"), handshakeFrameCap+1), '\n')
	_, err = readHandshakeFrame(bufio.NewReader(bytes.NewReader(over)), handshakeFrameCap)
	if !errors.Is(err, errHandshakeFrameTooLarge) {
		t.Fatalf("oversize frame err = %v, want errHandshakeFrameTooLarge", err)
	}
}

func TestShim_ReadHandshakeMessage_OversizeFrameRejected(t *testing.T) {
	logs := captureLogs(t)
	verifier := &fixedDigestVerifier{allowed: map[string]string{"noop": "sha256:abcd1234"}}
	shim, err := NewShim(&Config{
		ListenAddress:             "127.0.0.1:0",
		IdentityHandshakeDeadline: 10 * time.Second,
	}, verifier)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// 1 MiB frame: the read must abort at the cap (16385 bytes into the
	// frame), not buffer the whole payload — the write side is expected to
	// fail once the server closes the conn.
	frame := bytes.Repeat([]byte("a"), 1<<20)
	writeErr := make(chan error, 1)
	go func() {
		_, err := client.Write(frame)
		writeErr <- err
	}()

	done := make(chan error, 1)
	go func() {
		_, err := shim.readHandshakeMessage(server)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected oversize frame rejection")
		}
		if !strings.Contains(err.Error(), "identity frame exceeds 16384 bytes") {
			t.Errorf("error %q does not name the frame cap", err.Error())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readHandshakeMessage did not reject oversize frame")
	}

	if !strings.Contains(logs.String(), "identity frame exceeds 16384 bytes") {
		t.Errorf("distinct oversize log line missing, got: %s", logs.String())
	}

	// Closing the conn is part of the rejection: the blocked writer must fail.
	// A close-caused pipe error is required evidence; a timeout here means the
	// conn was not closed (not evidence of anything).
	select {
	case err := <-writeErr:
		if err == nil {
			t.Error("expected client write to fail after server closed conn")
		} else if !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, net.ErrClosed) {
			t.Errorf("client write failed with unexpected error (want closed-pipe/close): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("client write did not observe conn close")
	}
}

func TestShim_Accept_OversizeFrameRejectedAndClosed(t *testing.T) {
	logs := captureLogs(t)
	shim, addr := startTestShim(t, &Config{ListenAddress: "127.0.0.1:0"})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// A valid handshake padded well past the cap must be rejected before any
	// parse, with the rest of the oversized frame never buffered.
	frame := padFrame(&handshakeMessage{
		Name:    "noop",
		Version: "1.0.0",
		Digest:  "sha256:abcd1234",
	}, handshakeFrameCap+1)
	frame = append(frame, bytes.Repeat([]byte("a"), 1<<20)...) // trailing bulk: must not be read
	frame = append(frame, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(frame); err != nil {
		// The server may close mid-write once the cap trips; that is a
		// rejection signal, not a test failure.
		t.Logf("write interrupted by server close (expected shape): %v", err)
	}

	assertConnClosed(t, conn)

	// The rejection is logged on the accept goroutine, which may still be
	// scheduling when the client observes the close; poll for the line.
	if !waitForLog(t, logs, "identity frame exceeds 16384 bytes", 2*time.Second) {
		t.Errorf("distinct oversize log line missing, got: %s", logs.String())
	}

	shim.mu.Lock()
	sessions := len(shim.sessions)
	shim.mu.Unlock()
	if sessions != 0 {
		t.Errorf("expected no session registered after oversize rejection, got %d", sessions)
	}
}

// --- role routing ---

func TestShim_PeerRole_RoutedToPeerAcceptor(t *testing.T) {
	captureLogs(t)
	shim, addr := startTestShim(t, &Config{ListenAddress: "127.0.0.1:0"})

	acceptor := newRecordingPeerAcceptor()
	shim.SetPeerAcceptor(acceptor)

	conn := dialRawHandshake(t, addr, &handshakeMessage{
		Name:    "noop",
		Version: "0.5.7",
		Digest:  "sha256:abcd1234",
		Role:    handshakeRolePeer,
		Peer: &PeerClientIdentity{
			CriteriaVersion: "0.5.7",
			Capabilities:    []string{"adapter.v2.full", "supervision.v1"},
		},
	})
	defer conn.Close()

	if !acceptor.waitCalled(5 * time.Second) {
		t.Fatal("peer dial was not routed to the PeerAcceptor")
	}
	ident := acceptor.lastIdentity()
	if ident == nil {
		t.Fatal("expected parsed peer identity handed to acceptor")
	}
	if ident.CriteriaVersion != "0.5.7" {
		t.Errorf("criteria_version = %q, want 0.5.7", ident.CriteriaVersion)
	}
	if len(ident.Capabilities) != 2 || ident.Capabilities[0] != "adapter.v2.full" || ident.Capabilities[1] != "supervision.v1" {
		t.Errorf("capabilities = %v, want [adapter.v2.full supervision.v1]", ident.Capabilities)
	}

	// The peer path must not take the legacy byte-bridge: the shim itself
	// registers no session (T-06's acceptor owns registration).
	acceptor.releaseAll()
	assertConnClosed(t, conn)

	shim.mu.Lock()
	sessions := len(shim.sessions)
	shim.mu.Unlock()
	if sessions != 0 {
		t.Errorf("expected no legacy session for a peer-role dial, got %d", sessions)
	}

	shortCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := shim.WaitForHandle(shortCtx, "noop", ""); err == nil {
		t.Error("expected WaitForHandle to time out: a peer dial must not register a runner handle")
	}
}

func TestShim_PeerRole_RejectedWithoutAcceptor(t *testing.T) {
	captureLogs(t)
	shim, addr := startTestShim(t, &Config{ListenAddress: "127.0.0.1:0"})

	conn := dialRawHandshake(t, addr, &handshakeMessage{
		Name:    "noop",
		Version: "0.5.7",
		Digest:  "sha256:abcd1234",
		Role:    handshakeRolePeer,
	})
	defer conn.Close()

	assertConnClosed(t, conn)

	shim.mu.Lock()
	sessions := len(shim.sessions)
	shim.mu.Unlock()
	if sessions != 0 {
		t.Errorf("expected no session for rejected peer dial, got %d", sessions)
	}
}

func TestShim_LegacyRole_StillUsesByteBridgeWithAcceptorInstalled(t *testing.T) {
	captureLogs(t)
	shim, addr := startTestShim(t, &Config{ListenAddress: "127.0.0.1:0"})

	// An installed acceptor must not hijack role-absent dials: legacy runner
	// pods keep working through the migration window (regression guard).
	acceptor := newRecordingPeerAcceptor()
	defer acceptor.releaseAll()
	shim.SetPeerAcceptor(acceptor)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		_ = dialFakeAdapter(addr, &handshakeMessage{
			Name:    "noop",
			Version: "1.0.0",
			Digest:  "sha256:abcd1234",
		}, nil)
	}()

	handle, err := shim.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	defer handle.Kill()

	info, err := handle.Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != "noop" {
		t.Errorf("info.Name = %q, want noop", info.Name)
	}

	// The UDS byte-bridge artifact must exist for the legacy path.
	shim.mu.Lock()
	sess, ok := shim.sessions["noop"]
	shim.mu.Unlock()
	if !ok {
		t.Fatal("expected legacy session to be registered")
	}
	if sess.socketPath == "" {
		t.Fatal("expected UDS bridge socket path on the legacy session")
	}
	if _, err := os.Stat(filepath.Dir(sess.socketPath)); err != nil {
		t.Fatalf("UDS bridge socket dir missing: %v", err)
	}

	if err := acceptor.assertNotCalled(300 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestShim_LegacyRunnerHandshakeFixture_Accepted(t *testing.T) {
	logs := captureLogs(t)
	shim, addr := startTestShim(t, &Config{ListenAddress: "127.0.0.1:0"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The exact shape cmd/criteria-adapter-remote-runner sends today: no
	// role, no peer block, plus the sdk_protocol_version field the host does
	// not know — unknown fields must stay ignored.
	go func() {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return
		}
		frame := `{"name":"noop","version":"1.0.0","digest":"sha256:abcd1234","sdk_protocol_version":2}` + "\n"
		if _, err := io.WriteString(conn, frame); err != nil {
			return
		}
		grpcServer := grpc.NewServer()
		v2.RegisterAdapterServiceServer(grpcServer, &fakeAdapterServer{infoName: "noop", infoVersion: "1.0.0"})
		// Serve dispatches the single conn to a handler goroutine and then
		// returns io.EOF (singleConnListener's second Accept); the conn must
		// stay open for the handler, so it is intentionally not closed here.
		_ = grpcServer.Serve(&singleConnListener{conn: conn})
	}()

	handle, err := shim.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Logf("shim logs:\n%s", logs.String())
		t.Fatalf("WaitForHandle: %v", err)
	}
	defer handle.Kill()

	info, err := handle.Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != "noop" || info.Version != "1.0.0" {
		t.Errorf("info = %q/%q, want noop/1.0.0", info.Name, info.Version)
	}
}

// --- handshake JSON surface ---

func TestHandshakeMessage_JSONRoundTrip(t *testing.T) {
	// role + peer parse into the extended struct.
	frame := `{"name":"noop","version":"0.5.7","digest":"sha256:abcd1234","token":"t","scope":"root/scope-1",` +
		`"role":"peer","peer":{"criteria_version":"0.5.7","capabilities":["supervision.v1"]},"unknown_field":42}`
	var hs handshakeMessage
	if err := json.Unmarshal([]byte(frame), &hs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if hs.Role != "peer" {
		t.Errorf("role = %q, want peer", hs.Role)
	}
	if hs.Peer == nil {
		t.Fatal("peer block not parsed")
	}
	if hs.Peer.CriteriaVersion != "0.5.7" {
		t.Errorf("criteria_version = %q, want 0.5.7", hs.Peer.CriteriaVersion)
	}
	if len(hs.Peer.Capabilities) != 1 || hs.Peer.Capabilities[0] != "supervision.v1" {
		t.Errorf("capabilities = %v, want [supervision.v1]", hs.Peer.Capabilities)
	}

	// Absent role/peer and unknown fields: legacy tolerance preserved.
	var legacy handshakeMessage
	if err := json.Unmarshal([]byte(`{"name":"noop","version":"1.0.0","digest":"sha256:x","sdk_protocol_version":2}`), &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if legacy.Role != "" || legacy.Peer != nil {
		t.Errorf("legacy frame picked up role/peer: %+v", legacy)
	}
	if legacy.Name != "noop" || legacy.Digest != "sha256:x" {
		t.Errorf("legacy fields lost: %+v", legacy)
	}

	// omitempty: the legacy wire shape must not grow role/peer keys.
	out, err := json.Marshal(handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"role", "peer"} {
		if strings.Contains(string(out), fmt.Sprintf("%q:", key)) {
			t.Errorf("marshaled frame unexpectedly contains %q key: %s", key, out)
		}
	}

	// role="peer" without a peer block: peer stays nil (tolerated).
	var bare handshakeMessage
	if err := json.Unmarshal([]byte(`{"name":"noop","role":"peer"}`), &bare); err != nil {
		t.Fatalf("unmarshal bare peer frame: %v", err)
	}
	if bare.Role != "peer" || bare.Peer != nil {
		t.Errorf("bare peer frame parsed wrong: %+v", bare)
	}
}

// --- auth rejection matrix (peer-role dials) ---

func TestShim_PeerRole_AuthRejectionMatrix(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(*Config)
		prep func(*Shim)
		hs   func() *handshakeMessage
	}{
		{
			name: "digest mismatch",
			hs: func() *handshakeMessage {
				return &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:bad", Role: handshakeRolePeer}
			},
		},
		{
			name: "bad token",
			cfg:  func(c *Config) { c.AcceptToken = "legacy-token" },
			hs: func() *handshakeMessage {
				return &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234", Token: "wrong-token", Role: handshakeRolePeer}
			},
		},
		{
			name: "unregistered scope",
			cfg:  func(c *Config) { c.AcceptToken = "legacy-token" },
			prep: func(s *Shim) {
				s.SetPerScopeSessions(true)
				s.RegisterScope("root/scope-1", "scope-1-token")
			},
			hs: func() *handshakeMessage {
				return &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234", Scope: "root/other", Token: "scope-1-token", Role: handshakeRolePeer}
			},
		},
		{
			name: "bad token for registered scope",
			cfg:  func(c *Config) { c.AcceptToken = "legacy-token" },
			prep: func(s *Shim) {
				s.SetPerScopeSessions(true)
				s.RegisterScope("root/scope-1", "scope-1-token")
			},
			hs: func() *handshakeMessage {
				return &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234", Scope: "root/scope-1", Token: "wrong-token", Role: handshakeRolePeer}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captureLogs(t)
			cfg := &Config{ListenAddress: "127.0.0.1:0"}
			if tc.cfg != nil {
				tc.cfg(cfg)
			}
			shim, addr := startTestShim(t, cfg)
			if tc.prep != nil {
				tc.prep(shim)
			}

			acceptor := newRecordingPeerAcceptor()
			defer acceptor.releaseAll()
			shim.SetPeerAcceptor(acceptor)

			conn := dialRawHandshake(t, addr, tc.hs())
			defer conn.Close()

			assertConnClosed(t, conn)

			if err := acceptor.assertNotCalled(300 * time.Millisecond); err != nil {
				t.Fatalf("acceptor invoked for rejected dial: %v", err)
			}

			shim.mu.Lock()
			sessions := len(shim.sessions)
			shim.mu.Unlock()
			if sessions != 0 {
				t.Errorf("expected no session after auth rejection, got %d", sessions)
			}
		})
	}
}

func TestShim_PeerRole_AuthenticatedPerScopeDial_ReachesAcceptor(t *testing.T) {
	captureLogs(t)
	shim, addr := startTestShim(t, &Config{ListenAddress: "127.0.0.1:0", AcceptToken: "legacy-token"})

	shim.SetPerScopeSessions(true)
	shim.RegisterScope("root/scope-1", "scope-1-token")

	acceptor := newRecordingPeerAcceptor()
	shim.SetPeerAcceptor(acceptor)

	conn := dialRawHandshake(t, addr, &handshakeMessage{
		Name:    "noop",
		Version: "1.0.0",
		Digest:  "sha256:abcd1234",
		Scope:   "root/scope-1",
		Token:   "scope-1-token",
		Role:    handshakeRolePeer,
		Peer:    &PeerClientIdentity{CriteriaVersion: "0.5.7"},
	})
	defer conn.Close()

	if !acceptor.waitCalled(5 * time.Second) {
		t.Fatal("authenticated per-scope peer dial was not routed to the acceptor")
	}
	acceptor.releaseAll()
	assertConnClosed(t, conn)
}
