package peer

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScanRemoteScopes_ReadsDigestSidecar pins the KB-253 digest sidecar:
// the engine stamps the adapter's pinned lockfile digest next to the token
// file ("<key>.sha256"), and the scope-token scan carries it on
// ScopeTokenSpec.Digest. Absent or unreadable sidecars leave Digest empty
// (older engines and digest-less pins stay legal).
func TestScanRemoteScopes_ReadsDigestSidecar(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, "scope", "9e2f8a20-0000-4000-8000-000000000001", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("shell.repo.token", "tok-repo\n")
	write("shell.repo.sha256", "sha256:deadbeef\n")
	write("shell.sh.token", "tok-sh\n")
	// A corrupt sidecar must not break the scan and must leave Digest empty.
	write("shell.sh.sha256", "")

	specs, err := ScanRemoteScopes(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	byAdapter := map[string]ScopeTokenSpec{}
	for _, sc := range specs {
		byAdapter[sc.Adapter] = sc
	}
	repo, ok := byAdapter["shell.repo"]
	if !ok {
		t.Fatalf("shell.repo token missing from scan: %+v", specs)
	}
	if repo.Token != "tok-repo" {
		t.Fatalf("token = %q", repo.Token)
	}
	if repo.Digest != "sha256:deadbeef" {
		t.Fatalf("sidecar digest = %q, want sha256:deadbeef", repo.Digest)
	}
	sh, ok := byAdapter["shell.sh"]
	if !ok {
		t.Fatalf("shell.sh token missing from scan: %+v", specs)
	}
	if sh.Digest != "" {
		t.Fatalf("unreadable sidecar must leave Digest empty, got %q", sh.Digest)
	}
}