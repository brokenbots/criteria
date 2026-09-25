package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/peer"
)

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
