package publish

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/memory"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

// writeFile writes data to a fresh temp file and returns its path.
func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// fetchManifest reads back the assembled image manifest from the memory store.
func fetchManifest(t *testing.T, store *memory.Store, desc ocispec.Descriptor) ocispec.Manifest {
	t.Helper()
	rc, err := store.Fetch(context.Background(), desc)
	if err != nil {
		t.Fatalf("fetch manifest: %v", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	return m
}

func TestBuildManifestInStore_MultiPlatformLayers(t *testing.T) {
	dir := t.TempDir()
	ref, err := oci.Parse("ghcr.io/org/criteria-adapter-copilot:0.5.0")
	if err != nil {
		t.Fatal(err)
	}

	plats := []struct{ os, arch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "amd64"},
		{"darwin", "arm64"},
	}
	var bins []PlatformBinary
	for _, p := range plats {
		// distinct content per platform, same basename
		path := writeFile(t, dir, filepath.Join(p.os, p.arch, "criteria-adapter-copilot"),
			[]byte("fake-"+p.os+"-"+p.arch))
		bins = append(bins, PlatformBinary{OS: p.os, Arch: p.arch, Path: path})
	}

	store := memory.New()
	desc, err := buildManifestInStore(context.Background(), store, ref, bins, []byte("name: copilot\nplatforms:\n  - {os: linux, arch: amd64}\n"))
	if err != nil {
		t.Fatalf("buildManifestInStore: %v", err)
	}

	m := fetchManifest(t, store, desc)

	// config is the empty adapter config object.
	if m.Config.MediaType != mediaTypeAdapterConfig {
		t.Errorf("config mediaType = %q, want %q", m.Config.MediaType, mediaTypeAdapterConfig)
	}

	// Expect 1 adapter.yaml layer + 4 binary layers, with the right titles.
	titles := map[string]string{} // title -> mediaType
	for _, l := range m.Layers {
		titles[l.Annotations[oci.AnnotationTitle]] = l.MediaType
	}
	want := map[string]string{
		"adapter.yaml": ocispec.MediaTypeImageLayer,
		"bin/linux/amd64/criteria-adapter-copilot":  mediaTypeAdapterBinary,
		"bin/linux/arm64/criteria-adapter-copilot":  mediaTypeAdapterBinary,
		"bin/darwin/amd64/criteria-adapter-copilot": mediaTypeAdapterBinary,
		"bin/darwin/arm64/criteria-adapter-copilot": mediaTypeAdapterBinary,
	}
	if len(m.Layers) != len(want) {
		t.Fatalf("got %d layers, want %d: %v", len(m.Layers), len(want), titles)
	}
	for title, mt := range want {
		if got, ok := titles[title]; !ok {
			t.Errorf("missing layer titled %q", title)
		} else if got != mt {
			t.Errorf("layer %q mediaType = %q, want %q", title, got, mt)
		}
	}
}

func TestBuildManifestInStore_RejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	ref, _ := oci.Parse("ghcr.io/org/criteria-adapter-copilot:0.5.0")
	bin := writeFile(t, dir, filepath.Join("linux", "amd64", "criteria-adapter-copilot"), []byte("x"))
	other := writeFile(t, dir, filepath.Join("linux", "arm64", "criteria-adapter-different"), []byte("y"))

	cases := map[string][]PlatformBinary{
		"duplicate platform":  {{OS: "linux", Arch: "amd64", Path: bin}, {OS: "linux", Arch: "amd64", Path: bin}},
		"missing os/arch":     {{OS: "linux", Path: bin}},
		"mismatched basename": {{OS: "linux", Arch: "amd64", Path: bin}, {OS: "linux", Arch: "arm64", Path: other}},
	}
	for name, bins := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := buildManifestInStore(context.Background(), memory.New(), ref, bins, []byte("name: copilot\n")); err == nil {
				t.Fatalf("expected error for %q, got nil", name)
			}
		})
	}
}
