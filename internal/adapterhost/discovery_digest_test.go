package adapterhost

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
)

// TestDiscoverBinaryAtDistinctVersions verifies that two different digests of
// the same adapter type resolve to distinct binaries (the core Q1 guarantee:
// multiple versions of one adapter type can coexist).
func TestDiscoverBinaryAtDistinctVersions(t *testing.T) {
	root := t.TempDir()
	t.Setenv(adaptersEnvVar, root)

	dgA := digest.FromString("version-a")
	dgB := digest.FromString("version-b")
	encA := EncodeDigest(dgA)
	encB := EncodeDigest(dgB)

	writeFakeBinary(t, root, encA, "shell")
	writeFakeBinary(t, root, encB, "shell")

	pathA, err := DiscoverBinaryAt("shell", encA)
	if err != nil {
		t.Fatalf("DiscoverBinaryAt(A): %v", err)
	}
	pathB, err := DiscoverBinaryAt("shell", encB)
	if err != nil {
		t.Fatalf("DiscoverBinaryAt(B): %v", err)
	}
	if pathA == pathB {
		t.Fatalf("expected distinct paths for distinct digests, both = %q", pathA)
	}
	if filepath.Dir(filepath.Dir(pathA)) != root {
		t.Fatalf("path %q not under root %q", pathA, root)
	}
}

func TestDiscoverBinaryAtMissing(t *testing.T) {
	t.Setenv(adaptersEnvVar, t.TempDir())
	if _, err := DiscoverBinaryAt("shell", EncodeDigest(digest.FromString("nope"))); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestEncodeDigestIsFilesystemSafe(t *testing.T) {
	enc := EncodeDigest(digest.FromString("x"))
	if filepath.Base(enc) != enc {
		t.Fatalf("encoded digest %q contains a path separator", enc)
	}
}

func writeFakeBinary(t *testing.T, root, digestEncoded, adapterType string) {
	t.Helper()
	dir := filepath.Join(root, digestEncoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, adapterBinaryPrefix+adapterType)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
