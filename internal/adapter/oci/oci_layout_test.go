package oci_test

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

func TestOpen_CreatesLayout(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)
	require.NotNil(t, l)

	// oci-layout marker must exist.
	data, err := os.ReadFile(filepath.Join(root, "oci-layout"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "1.0.0")

	// index.json must exist.
	_, err = os.Stat(filepath.Join(root, "index.json"))
	require.NoError(t, err)

	// blobs/sha256 must exist.
	_, err = os.Stat(filepath.Join(root, "blobs", "sha256"))
	require.NoError(t, err)
}

func TestOpen_IdempotentOnExistingLayout(t *testing.T) {
	root := t.TempDir()
	_, err := oci.Open(root)
	require.NoError(t, err)
	// Open again — must not error.
	l2, err := oci.Open(root)
	require.NoError(t, err)
	assert.NotNil(t, l2)
}

func TestOpen_RejectsIncompatibleLayoutVersion(t *testing.T) {
	root := t.TempDir()
	// Write a bad oci-layout marker.
	err := os.WriteFile(filepath.Join(root, "oci-layout"), []byte(`{"imageLayoutVersion":"9.9.9"}`), 0o640)
	require.NoError(t, err)
	_, err = oci.Open(root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "9.9.9")
}

func TestWriteBlob_RoundTrip(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	content := []byte("hello OCI world")
	d := digest.FromBytes(content)

	require.NoError(t, l.WriteBlob(bytes.NewReader(content), d))
	assert.True(t, l.HasBlob(d))

	got, err := os.ReadFile(l.BlobPath(d))
	require.NoError(t, err)
	assert.Equal(t, content, got)
}

func TestWriteBlob_DigestMismatch(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	content := []byte("hello OCI world")
	wrong := digest.FromBytes([]byte("different"))

	err = l.WriteBlob(bytes.NewReader(content), wrong)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "digest mismatch")
	// Bad blob must NOT be persisted.
	assert.False(t, l.HasBlob(wrong))
}

func TestWriteBlob_Idempotent(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	content := []byte("same content")
	d := digest.FromBytes(content)

	require.NoError(t, l.WriteBlob(bytes.NewReader(content), d))
	// Second write must not error.
	require.NoError(t, l.WriteBlob(bytes.NewReader(content), d))
}

func TestWriteIndex_RoundTrip(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	// Write a blob so we can reference it.
	content := []byte("manifest payload")
	d := digest.FromBytes(content)
	require.NoError(t, l.WriteBlob(bytes.NewReader(content), d))

	ix := &ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    d,
				Size:      int64(len(content)),
				Annotations: map[string]string{
					oci.AnnotationProtocolVersion: "2",
					oci.AnnotationSchemaVersion:   "1",
				},
			},
		},
	}

	require.NoError(t, l.WriteIndex(ix))

	got, err := l.Index()
	require.NoError(t, err)
	require.Len(t, got.Manifests, 1)
	assert.Equal(t, d, got.Manifests[0].Digest)
	assert.Equal(t, "2", got.Manifests[0].Annotations[oci.AnnotationProtocolVersion])
}

func TestArtifactProtocolVersion(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	content := []byte("some manifest")
	d := digest.FromBytes(content)
	require.NoError(t, l.WriteBlob(bytes.NewReader(content), d))

	ix := &ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{
				Digest: d,
				Size:   int64(len(content)),
				Annotations: map[string]string{
					oci.AnnotationProtocolVersion: "2",
				},
			},
		},
	}
	require.NoError(t, l.WriteIndex(ix))

	assert.Equal(t, uint32(2), l.ArtifactProtocolVersion(d))
}

func TestArtifactProtocolVersion_Missing(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	unknown := digest.FromBytes([]byte("not in index"))
	assert.Equal(t, uint32(0), l.ArtifactProtocolVersion(unknown))
}

func TestAnnotate_MergesAndPersists(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	manifestDigest := buildFixtureArtifact(t, l)

	// buildFixtureArtifact wrote the manifest blob but did not register it
	// in index.json. Register it now so Annotate can find it.
	ix := &ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{MediaType: ocispec.MediaTypeImageManifest, Digest: manifestDigest, Size: 100},
		},
	}
	require.NoError(t, l.WriteIndex(ix))

	// Annotate merges the extra map into the descriptor's Annotations.
	require.NoError(t, l.Annotate(manifestDigest, map[string]string{
		oci.AnnotationReference: "ghcr.io/brokenbots/ccriteria-adapter-test:1.2.3",
		oci.AnnotationSourceURL: "https://example.com/test",
	}))

	ix, err = l.Index()
	require.NoError(t, err)
	var found bool
	for _, m := range ix.Manifests {
		if m.Digest == manifestDigest {
			found = true
			assert.Equal(t, "ghcr.io/brokenbots/ccriteria-adapter-test:1.2.3", m.Annotations[oci.AnnotationReference])
			assert.Equal(t, "https://example.com/test", m.Annotations[oci.AnnotationSourceURL])
		}
	}
	require.True(t, found, "manifest must be in index after annotate")

	// A second Annotate call must merge, not overwrite unrelated keys.
	require.NoError(t, l.Annotate(manifestDigest, map[string]string{
		"extra.key": "extra-value",
	}))
	ix, err = l.Index()
	require.NoError(t, err)
	for _, m := range ix.Manifests {
		if m.Digest == manifestDigest {
			assert.Equal(t, "ghcr.io/brokenbots/ccriteria-adapter-test:1.2.3", m.Annotations[oci.AnnotationReference])
			assert.Equal(t, "extra-value", m.Annotations["extra.key"])
		}
	}
}

func TestAnnotate_UnknownDigestErrors(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	// Write a manifest blob so the layout has at least one index entry to
	// have something to be not-found against.
	manifestDigest := writeManifestWithLayer(t, l, []byte("payload"))
	ix := &ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{MediaType: ocispec.MediaTypeImageManifest, Digest: manifestDigest, Size: 100},
		},
	}
	require.NoError(t, l.WriteIndex(ix))

	unknown := digest.FromBytes([]byte("not in index"))
	err = l.Annotate(unknown, map[string]string{"k": "v"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestAnnotate_NilExtraIsNoOp(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	manifestDigest := writeManifestWithLayer(t, l, []byte("payload"))
	ix := &ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{MediaType: ocispec.MediaTypeImageManifest, Digest: manifestDigest, Size: 100},
		},
	}
	require.NoError(t, l.WriteIndex(ix))

	// Nil/empty maps must not write any annotations or persist a new index.
	beforeBytes, err := os.ReadFile(filepath.Join(root, "index.json"))
	require.NoError(t, err)
	require.NoError(t, l.Annotate(manifestDigest, nil))
	require.NoError(t, l.Annotate(manifestDigest, map[string]string{}))
	afterBytes, err := os.ReadFile(filepath.Join(root, "index.json"))
	require.NoError(t, err)
	assert.Equal(t, string(beforeBytes), string(afterBytes))
}

func TestLock_PreventsDataRace(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	const goroutines = 5
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.Lock()
			if err != nil {
				errCh <- err
				return
			}
			// Do a tiny amount of work inside the lock.
			time.Sleep(2 * time.Millisecond)
			release()
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("goroutine lock error: %v", err)
	}
}

func TestDefaultCacheRoot_HonoursEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", dir)

	root, err := oci.DefaultCacheRoot()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "cache", "oci"), root)
}

func TestDefaultCacheRoot_FallsBackToHome(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", "")

	root, err := oci.DefaultCacheRoot()
	require.NoError(t, err)
	// Should end with /.criteria/cache/oci
	assert.True(t, strings.HasSuffix(root, filepath.Join(".criteria", "cache", "oci")),
		"expected root ending in .criteria/cache/oci, got %q", root)
}

// fuzzBlob is a helper that writes n random bytes and returns digest.
func writeFuzzBlob(t *testing.T, l *oci.Layout, n int) ([]byte, digest.Digest) {
	t.Helper()
	buf := make([]byte, n)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	d := digest.FromBytes(buf)
	require.NoError(t, l.WriteBlob(bytes.NewReader(buf), d))
	return buf, d
}

func TestBlobPath(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	content := []byte("test blob")
	d := digest.FromBytes(content)
	expected := filepath.Join(root, "blobs", "sha256", d.Encoded())
	assert.Equal(t, expected, l.BlobPath(d))
}

func TestWriteBlob_LargePayload(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	_, d := writeFuzzBlob(t, l, 1<<16) // 64 KiB
	assert.True(t, l.HasBlob(d))
	fi, err := os.Stat(l.BlobPath(d))
	require.NoError(t, err)
	assert.Equal(t, int64(1<<16), fi.Size())
}

func TestHasBlob_Missing(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	missing := digest.FromBytes([]byte("does not exist"))
	assert.False(t, l.HasBlob(missing))
}

func TestWriteIndex_AtomicWrite(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	// Read the index.json path — it must be a regular file.
	fi, err := os.Stat(filepath.Join(root, "index.json"))
	require.NoError(t, err)
	assert.False(t, fi.IsDir())

	// Write and verify the file is still readable.
	require.NoError(t, l.WriteIndex(&ocispec.Index{MediaType: ocispec.MediaTypeImageIndex}))
	_, err = l.Index()
	require.NoError(t, err)
}

func TestWriteBlob_WritesIntoCorrectAlgorithmDir(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	b := []byte("algo test")
	d := digest.FromBytes(b)

	require.NoError(t, l.WriteBlob(bytes.NewReader(b), d))

	// Must be under blobs/sha256/
	_, err = os.Stat(filepath.Join(root, "blobs", "sha256", d.Encoded()))
	require.NoError(t, err)
}

func TestWriteBlob_ReaderError(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	d := digest.FromBytes([]byte("something"))
	err = l.WriteBlob(io.LimitReader(bytes.NewReader([]byte{}), 0), d)
	require.Error(t, err)
}
