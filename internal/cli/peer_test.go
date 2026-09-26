package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/peer"
)

// readIdentityFrame reads one newline-terminated identity frame with a
// bounded deadline, clearing the deadline before returning so the held
// connection keeps serving the peer's gRPC traffic.
func readIdentityFrame(t *testing.T, conn net.Conn) ([]byte, error) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return nil, err
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return line[:len(line)-1], nil
}

// TestPeerRunPresentsIdentityFrameToHost pins the production phone-home
// wiring: `criteria peer` must actually dial the host and present the
// role="peer" identity frame after boot (ADR-0007 D4). Regression guard for
// a wiring bug where runPeer booted the child and then blocked forever
// without ever starting the phone-home loop.
func TestPeerRunPresentsIdentityFrameToHost(t *testing.T) {
	if testing.Short() {
		t.Skip("phone-home contract test boots a real adapter subprocess")
	}
	bin := buildNoopAdapterBinary(t)
	binData, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read noop adapter binary: %v", err)
	}
	sum := sha256.Sum256(binData)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// Fake host: accept the phone-home dial, read the identity frame, and
	// hold the connection open so the peer keeps serving on it.
	frameCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		frame, err := readIdentityFrame(t, conn)
		if err != nil {
			errCh <- err
			return
		}
		frameCh <- frame
		// Hold the conn until the test ends: the peer serves gRPC on it, and
		// an early close would just log a reconnect.
		<-t.Context().Done()
	}()

	t.Setenv(peer.EnvRemoteHost, ln.Addr().String())
	t.Setenv(peer.EnvRemoteToken, "contract-token")
	t.Setenv(peer.EnvRemoteDigest, digest)
	t.Setenv(peer.EnvAdapterName, "noop")
	t.Setenv(peer.EnvAdapterBinary, bin)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- runPeer(ctx) }()

	var frame []byte
	select {
	case frame = <-frameCh:
	case err := <-errCh:
		t.Fatalf("fake host: %v", err)
	case err := <-runErr:
		t.Fatalf("runPeer returned before the identity frame: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("peer did not present an identity frame within 30s")
	}

	var hs struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
		Token  string `json:"token"`
		Role   string `json:"role"`
		// SDKProtocolVersion is a number on the wire (2 = v2 adapter proto).
		SDKProtocolVersion int `json:"sdk_protocol_version"`
	}
	if err := json.Unmarshal(frame, &hs); err != nil {
		t.Fatalf("identity frame is not JSON: %v\nframe: %s", err, frame)
	}
	if hs.Role != "peer" {
		t.Errorf("identity role = %q, want peer", hs.Role)
	}
	if hs.Name != "noop" {
		t.Errorf("identity name = %q, want noop", hs.Name)
	}
	if hs.Digest != digest {
		t.Errorf("identity digest = %q, want %q", hs.Digest, digest)
	}
	if hs.Token != "contract-token" {
		t.Errorf("identity token = %q, want the configured accept token", hs.Token)
	}
	if hs.SDKProtocolVersion != 2 {
		t.Errorf("sdk_protocol_version = %d, want 2", hs.SDKProtocolVersion)
	}

	// Cancelling the run context must end the phone-home loop cleanly (the
	// CLI maps a signal-driven end to a nil error).
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("runPeer after cancel = %v, want nil (signal-driven end exits 0)", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("runPeer did not return after cancel")
	}
}

// TestPeerHelpDocumentsEveryEnvVar is the acceptance gate: `criteria peer
// --help` must document every env var the peer consumes.
func TestPeerHelpDocumentsEveryEnvVar(t *testing.T) {
	cmd := NewPeerCmd()
	var help bytes.Buffer
	cmd.SetOut(&help)
	cmd.SetErr(&help)
	if err := cmd.Help(); err != nil {
		t.Fatalf("help: %v", err)
	}
	out := help.String()
	for _, env := range []string{
		peer.EnvRemoteHost,
		peer.EnvRemoteToken,
		peer.EnvRemoteScope,
		peer.EnvRemoteDigest,
		peer.EnvRemoteTLSCert,
		peer.EnvRemoteTLSKey,
		peer.EnvRemoteCA,
		peer.EnvAdapterName,
		peer.EnvAdapterVersion,
		peer.EnvAdapterBinary,
		peer.EnvAdapterManifest,
		peer.EnvLogLevel,
		peer.EnvChildKeepAlive,
		peer.EnvJournalLimit,
		peer.EnvBackoffMin,
		peer.EnvBackoffMax,
	} {
		if !strings.Contains(out, env) {
			t.Errorf("peer --help missing %s:\n%s", env, out)
		}
	}
	if !strings.Contains(out, "all-or-nothing") {
		t.Errorf("peer --help should note TLS all-or-nothing semantics:\n%s", out)
	}
}

func TestPeerLogLevelMapping(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	cases := map[string]int{
		"debug":   -4, // slog.LevelDebug
		"info":    0,
		"":        0,
		"warn":    4,
		"warning": 4,
		"error":   8,
	}
	for raw, want := range cases {
		if got := int(peerLogLevel(logger, raw)); got != want {
			t.Errorf("peerLogLevel(%q) = %d, want %d", raw, got, want)
		}
	}
	if got := int(peerLogLevel(logger, "banana")); got != 0 {
		t.Errorf("unknown level fallback = %d, want info (0)", got)
	}
}
