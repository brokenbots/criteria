package publish

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gopkg.in/yaml.v3"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
)

// mockRegistry serves just enough of the OCI distribution API for
// remote.Repository.Resolve: a /v2/ ping and a manifest HEAD/GET that returns a
// content digest. It models the registry where the adapter's CI already pushed
// the runnable image.
func newMockRegistry(t *testing.T) (host string, imageDigest digest.Digest) {
	t.Helper()
	body := []byte(`{"schemaVersion":2,"mediaType":"` + ocispec.MediaTypeImageManifest + `"}`)
	imageDigest = digest.FromBytes(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			w.Header().Set("Docker-Content-Digest", imageDigest.String())
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			if r.Method == http.MethodGet {
				_, _ = w.Write(body)
			}
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), imageDigest
}

func writeMinimalManifest(t *testing.T) string {
	t.Helper()
	m := manifest.Manifest{
		SchemaVersion:      1,
		Name:               "test",
		Version:            "0.5.0",
		SourceURL:          "https://github.com/brokenbots/test",
		Platforms:          []manifest.Platform{{OS: "linux", Arch: "amd64"}},
		SDKProtocolVersion: 2,
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("seed manifest invalid: %v", err)
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "adapter.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRecordContainerImage_ResolvesAndEmbeds proves --image records the
// already-pushed image's resolved digest into the manifest's container_image
// block (D12b), failing closed when the image is absent.
func TestRecordContainerImage_ResolvesAndEmbeds(t *testing.T) {
	host, wantDigest := newMockRegistry(t)
	imageRef := host + "/org/name:0.5.0-image"
	mfPath := writeMinimalManifest(t)

	err := RecordContainerImage(context.Background(), mfPath, imageRef, Options{PlainHTTP: true})
	if err != nil {
		t.Fatalf("RecordContainerImage: %v", err)
	}

	m, err := manifest.ParseFile(mfPath)
	if err != nil {
		t.Fatal(err)
	}
	if m.ContainerImage == nil {
		t.Fatal("container_image not recorded")
	}
	if m.ContainerImage.Ref != imageRef {
		t.Errorf("ref = %q, want %q", m.ContainerImage.Ref, imageRef)
	}
	if m.ContainerImage.Digest != wantDigest.String() {
		t.Errorf("digest = %q, want %q", m.ContainerImage.Digest, wantDigest.String())
	}
	// The recorded manifest must remain valid (it is embedded + verified later).
	if err := m.Validate(); err != nil {
		t.Fatalf("recorded manifest invalid: %v", err)
	}
}

func TestRecordContainerImage_RejectsBareReference(t *testing.T) {
	mfPath := writeMinimalManifest(t)
	err := RecordContainerImage(context.Background(), mfPath, "name:0.5.0-image", Options{})
	if err == nil {
		t.Fatal("expected error for non-fully-qualified image reference")
	}
}
