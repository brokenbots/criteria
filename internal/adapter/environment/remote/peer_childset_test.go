package remote

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

// multiDigests allows several adapter types, mirroring a lockfile that pins
// every adapter of one co-declared environment.
var multiDigests = map[string]string{
	"noop":    "sha256:abcd1234",
	"copilot": "sha256:0000ffff",
}

// --- unit checks ---

func TestCheckPeerChildSet_EmptyOrNil(t *testing.T) {
	if class, err := checkPeerChildSet(nil, "noop", nil); class != rejectNone || err != nil {
		t.Fatalf("nil set = (%v, %v), want no rejection", class, err)
	}
	if class, err := checkPeerChildSet(nil, "noop", []PeerAdapterIdentity{}); class != rejectNone || err != nil {
		t.Fatalf("empty set = (%v, %v), want no rejection", class, err)
	}
}

func TestCheckPeerChildSet_NilVerifierSkipsDigestsButKeepsShape(t *testing.T) {
	// With no verifier installed the digest is not checked (same as the
	// dialing identity); shape checks still run.
	if class, err := checkPeerChildSet(nil, "noop", []PeerAdapterIdentity{
		{Name: "noop"}, {Name: "copilot", Digest: "sha256:anything"},
	}); class != rejectNone || err != nil {
		t.Fatalf("nil-verifier set = (%v, %v), want no rejection", class, err)
	}
	if class, err := checkPeerChildSet(nil, "noop", []PeerAdapterIdentity{{Name: ""}}); class != rejectChildSet || err == nil {
		t.Fatalf("empty-name child = (%v, %v), want rejectChildSet with an error", class, err)
	}
}

func TestCheckPeerChildSet_Rejections(t *testing.T) {
	verifier := &fixedDigestVerifier{allowed: multiDigests}

	cases := []struct {
		name     string
		dialName string
		set      []PeerAdapterIdentity
		wantMsg  string
	}{
		{
			name:     "child digest does not verify",
			dialName: "noop",
			set: []PeerAdapterIdentity{
				{Name: "noop", Digest: "sha256:abcd1234"},
				{Name: "copilot", Digest: "sha256:deadbeef"},
			},
			wantMsg: "peer child \"copilot\" digest verification",
		},
		{
			name:     "missing dialing adapter is self-inconsistent",
			dialName: "noop",
			set:      []PeerAdapterIdentity{{Name: "copilot", Digest: "sha256:0000ffff"}},
			wantMsg:  "does not include it",
		},
		{
			name:     "duplicate child entries",
			dialName: "noop",
			set: []PeerAdapterIdentity{
				{Name: "noop", Digest: "sha256:abcd1234"},
				{Name: "noop", Digest: "sha256:abcd1234"},
			},
			wantMsg: "more than once",
		},
		{
			name:     "nameless child entry",
			dialName: "noop",
			set: []PeerAdapterIdentity{
				{Name: "noop", Digest: "sha256:abcd1234"},
				{},
			},
			wantMsg: "empty name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, err := checkPeerChildSet(verifier, tc.dialName, tc.set)
			if class != rejectChildSet {
				t.Fatalf("class = %v, want rejectChildSet", class)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantMsg)
			}
		})
	}
}

// dialRawHandshakeBytes writes an already-marshaled handshake frame; the
// legacy-shape byte-compat test needs the exact frame bytes.
func dialRawHandshakeBytes(t *testing.T, addr string, frame []byte) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	if _, err := conn.Write(append(frame, '\n')); err != nil {
		_ = conn.Close()
		t.Fatalf("write handshake: %v", err)
	}
	return conn
}

// --- dial acceptance ---

func TestShim_PeerRole_VerifiedChildSetHandedToAcceptor(t *testing.T) {
	captureLogs(t)
	shim, err := NewShim(&Config{
		ListenAddress:             "127.0.0.1:0",
		IdentityHandshakeDeadline: 10 * time.Second,
	}, &fixedDigestVerifier{allowed: multiDigests})
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	if err := shim.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := shim.listener.Addr().String()
	t.Cleanup(func() { _ = shim.Stop(context.Background()) })

	acceptor := newRecordingPeerAcceptor()
	shim.SetPeerAcceptor(acceptor)

	conn := dialRawHandshake(t, addr, &handshakeMessage{
		Name:    "noop",
		Version: "0.5.7",
		Digest:  "sha256:abcd1234",
		Role:    handshakeRolePeer,
		Peer: &PeerClientIdentity{
			Capabilities: []string{"adapter.v2.full"},
		},
		Adapters: []PeerAdapterIdentity{
			{Name: "noop", Version: "0.5.7", Digest: "sha256:abcd1234"},
			{Name: "copilot", Version: "0.5.7", Digest: "sha256:0000ffff"},
		},
	})
	defer conn.Close()

	if !acceptor.waitCalled() {
		t.Fatal("verified child set was not routed to the PeerAcceptor")
	}
	dial := acceptor.lastDial()
	if len(dial.Adapters) != 2 || dial.Adapters[0].Name != "noop" || dial.Adapters[1].Name != "copilot" {
		t.Fatalf("dial.Adapters = %+v, want the two verified children", dial.Adapters)
	}
	acceptor.releaseAll()
}

func TestShim_PeerRole_BadChildDigestRejectsDial(t *testing.T) {
	captureLogs(t)
	shim, addr := startTestShim(t, &Config{ListenAddress: "127.0.0.1:0"})

	acceptor := newRecordingPeerAcceptor()
	shim.SetPeerAcceptor(acceptor)

	conn := dialRawHandshake(t, addr, &handshakeMessage{
		Name:   "noop",
		Digest: "sha256:abcd1234",
		Role:   handshakeRolePeer,
		Adapters: []PeerAdapterIdentity{
			{Name: "noop", Digest: "sha256:abcd1234"},
			{Name: "copilot", Digest: "sha256:replayed"},
		},
	})
	defer conn.Close()

	if err := acceptor.assertNotCalled(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	assertConnClosed(t, conn)
	acceptor.releaseAll()
}

func TestShim_PeerRole_SelfInconsistentChildSetRejectsDial(t *testing.T) {
	captureLogs(t)
	shim, addr := startTestShim(t, &Config{ListenAddress: "127.0.0.1:0"})

	acceptor := newRecordingPeerAcceptor()
	shim.SetPeerAcceptor(acceptor)

	// The frame dials as noop but advertises only copilot: no set the peer
	// claims can back the identity it is dialing under.
	conn := dialRawHandshake(t, addr, &handshakeMessage{
		Name:   "noop",
		Digest: "sha256:abcd1234",
		Role:   handshakeRolePeer,
		Adapters: []PeerAdapterIdentity{
			{Name: "copilot", Digest: "sha256:0000ffff"},
		},
	})
	defer conn.Close()

	if err := acceptor.assertNotCalled(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	assertConnClosed(t, conn)
	acceptor.releaseAll()
}

func TestShim_PeerRole_LegacyFrameWithoutChildSetUnchanged(t *testing.T) {
	captureLogs(t)
	shim, addr := startTestShim(t, &Config{ListenAddress: "127.0.0.1:0"})

	acceptor := newRecordingPeerAcceptor()
	shim.SetPeerAcceptor(acceptor)

	// No `adapters` key: the single-adapter peer shape. It must keep dialing
	// exactly as before, byte-identically.
	data, err := json.Marshal(&handshakeMessage{
		Name:    "noop",
		Version: "0.5.7",
		Digest:  "sha256:abcd1234",
		Role:    handshakeRolePeer,
		Peer:    &PeerClientIdentity{Capabilities: []string{"adapter.v2.full"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "\"adapters\"") {
		t.Fatalf("legacy frame serializes an adapters key: %s", data)
	}
	conn := dialRawHandshakeBytes(t, addr, data)
	defer conn.Close()

	if !acceptor.waitCalled() {
		t.Fatal("legacy peer dial was not routed to the PeerAcceptor")
	}
	if dial := acceptor.lastDial(); dial.Adapters != nil {
		t.Fatalf("dial.Adapters = %+v, want nil for a legacy frame", dial.Adapters)
	}
	acceptor.releaseAll()
}
