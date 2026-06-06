package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// pushBlob stores raw bytes and returns their descriptor.
func pushBlob(t *testing.T, ctx context.Context, s *orasoci.Store, mt string, data []byte) ocispec.Descriptor {
	t.Helper()
	d := ocispec.Descriptor{MediaType: mt, Digest: digest.FromBytes(data), Size: int64(len(data))}
	if err := s.Push(ctx, d, bytes.NewReader(data)); err != nil {
		t.Fatalf("push %s: %v", mt, err)
	}
	return d
}

func pushManifest(t *testing.T, ctx context.Context, s *orasoci.Store, m *ocispec.Manifest) ocispec.Descriptor {
	t.Helper()
	m.MediaType = ocispec.MediaTypeImageManifest
	m.SchemaVersion = 2
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	d := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	if err := s.Push(ctx, d, bytes.NewReader(raw)); err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	return d
}

// TestCopyReferrers copies a signature-style referrer (a manifest whose Subject
// is the artifact) from a source store into a destination store, mirroring what
// the puller does so signature verification can find it locally.
func TestCopyReferrers(t *testing.T) {
	ctx := context.Background()
	src, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Artifact: config + one layer + manifest.
	cfg := pushBlob(t, ctx, src, "application/vnd.criteria.adapter.v1+json", []byte(`{"adapter":"test"}`))
	lay := pushBlob(t, ctx, src, "application/vnd.criteria.adapter.binary.v1", []byte("binary"))
	artifact := pushManifest(t, ctx, src, &ocispec.Manifest{Config: cfg, Layers: []ocispec.Descriptor{lay}})

	// Referrer (signature-like): empty config + payload layer + Subject -> artifact.
	rcfg := pushBlob(t, ctx, src, ocispec.MediaTypeEmptyJSON, []byte("{}"))
	pay := pushBlob(t, ctx, src, "application/vnd.dev.cosign.simplesigning.v1+json", []byte("payload"))
	referrer := pushManifest(t, ctx, src, &ocispec.Manifest{
		Config:  rcfg,
		Layers:  []ocispec.Descriptor{pay},
		Subject: &artifact,
	})

	dst, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := copyReferrers(ctx, src, dst, &artifact); err != nil {
		t.Fatalf("copyReferrers: %v", err)
	}

	// The referrer manifest (and its blobs) must now be present in dst.
	for _, d := range []ocispec.Descriptor{referrer, rcfg, pay} {
		ok, err := dst.Exists(ctx, d)
		if err != nil {
			t.Fatalf("exists %s: %v", d.Digest, err)
		}
		if !ok {
			t.Errorf("expected %s (%s) copied into dst", d.MediaType, d.Digest)
		}
	}
}
