package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/signing"
)

// genPubKeyPEM returns a fresh Ed25519 public key as PEM and its canonical
// fingerprint (sha256 of PKIX DER), matching signing.NewTrustedKey.
func genPubKeyPEM(t *testing.T) (pemBytes []byte, fingerprint string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return pemBytes, signing.Fingerprint(der)
}

func TestLoadTrustedKeys_GlobalInlineAndWorkflowPath(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	// Global trust config: inline PEM.
	globalPEM, globalFP := genPubKeyPEM(t)
	globalTrust := "trusted_key {\n  key = <<-EOT\n" + string(globalPEM) + "EOT\n}\n"
	if err := os.WriteFile(filepath.Join(stateDir, "trust.hcl"), []byte(globalTrust), 0o600); err != nil {
		t.Fatal(err)
	}

	// Workflow trust config: key by path.
	wfDir := t.TempDir()
	wfPEM, wfFP := genPubKeyPEM(t)
	if err := os.WriteFile(filepath.Join(wfDir, "team.pem"), wfPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	wfTrust := `trusted_key {
  path = "team.pem"
}
`
	if err := os.WriteFile(filepath.Join(wfDir, "trust.hcl"), []byte(wfTrust), 0o600); err != nil {
		t.Fatal(err)
	}

	keys, err := loadTrustedKeys(wfDir, nil)
	if err != nil {
		t.Fatalf("loadTrustedKeys: %v", err)
	}
	got := map[string]bool{}
	for _, k := range keys {
		got[k.Fingerprint] = true
	}
	if !got[globalFP] {
		t.Errorf("global key fingerprint %s not loaded", globalFP)
	}
	if !got[wfFP] {
		t.Errorf("workflow key fingerprint %s not loaded", wfFP)
	}
}

func TestLoadTrustedKeys_MissingFilesAreNoError(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	keys, err := loadTrustedKeys(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("loadTrustedKeys with no configs: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("expected no keys, got %d", len(keys))
	}
}

func TestLoadTrustedKeys_DeduplicatesByFingerprint(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	keyPEM, fp := genPubKeyPEM(t)
	keyPath := filepath.Join(stateDir, "k.pem")
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	// Same key in the global config and as an ad-hoc flag.
	globalTrust := "trusted_key {\n  key = <<-EOT\n" + string(keyPEM) + "EOT\n}\n"
	if err := os.WriteFile(filepath.Join(stateDir, "trust.hcl"), []byte(globalTrust), 0o600); err != nil {
		t.Fatal(err)
	}

	keys, err := loadTrustedKeys("", []string{keyPath})
	if err != nil {
		t.Fatalf("loadTrustedKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 deduped key, got %d", len(keys))
	}
	if keys[0].Fingerprint != fp {
		t.Errorf("fingerprint = %s, want %s", keys[0].Fingerprint, fp)
	}
}

func TestLoadTrustedKeys_InvalidKeyIsError(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTrustedKeys("", []string{bad}); err == nil {
		t.Fatal("expected error for invalid key, got nil")
	}
}
