package cli

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// TestApplyCastleAddrValidation pins the KB-328 local-mode-only rule and the
// eager URL validation (apply.go): --castle-addr is refused in server mode,
// malformed console targets are refused before any run starts, and a valid
// http(s) URL passes validation (the run then fails on the missing workflow
// file instead — nothing castle-shaped and no network is dialed).
func TestApplyCastleAddrValidation(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.hcl")
	fixture := func(castleAddr, serverURL string) applyOptions {
		return applyOptions{
			workflowPath: missing,
			castleAddr:   castleAddr,
			serverURL:    serverURL,
			log:          slog.New(slog.DiscardHandler),
		}
	}

	opts := fixture("http://127.0.0.1:8080", "http://127.0.0.1:9000")
	if err := runApply(context.Background(), opts); err == nil ||
		!strings.Contains(err.Error(), "local-mode flag") {
		t.Errorf("castle+server = %v, want the local-mode refusal", err)
	}

	for _, bad := range []string{
		"castle.internal",          // no scheme (constructor sees empty scheme/host)
		"ftp://castle.internal",    // unsupported scheme
		"http://castle/internal/",  // path prefix
		"http://castle.internal/x", // path prefix
	} {
		opts := fixture(bad, "")
		if err := runApply(context.Background(), opts); err == nil ||
			!strings.Contains(err.Error(), "invalid castle address") {
			t.Errorf("castle %q = %v, want an invalid-castle-address refusal", bad, err)
		}
	}

	opts = fixture("http://127.0.0.1:8080", "")
	err := runApply(context.Background(), opts)
	if err == nil {
		t.Fatal("a valid --castle-addr must still fail on the missing workflow file")
	}
	// The failure must come from the workflow load, not from castle
	// validation (whose wording is fixed) — and the temp-dir path embeds
	// this test's name, so match the constructor's wording, not "castle".
	if strings.Contains(err.Error(), "invalid castle address") ||
		strings.Contains(err.Error(), "local-mode flag") {
		t.Errorf("valid --castle-addr refused: %v", err)
	}
}
