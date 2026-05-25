// Package oci implements an OCI Image Layout-compliant local cache for
// adapter artifacts. The cache lives at ~/.criteria/cache/oci/ (or
// $CRITERIA_STATE_DIR/cache/oci/) and can be inspected by any OCI-aware
// tool such as oras.
package oci

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ociLayoutVersion is the value written into the oci-layout marker file.
const ociLayoutVersion = "1.0.0"

// Annotation keys written by the puller and consumed by the loader.
const (
	AnnotationProtocolVersion = "dev.criteria.adapter.protocol_version"
	AnnotationSchemaVersion   = "dev.criteria.adapter.schema_version"
)

// Layout is a handle to an OCI Image Layout on disk.
type Layout struct {
	// Root is the absolute path to the layout directory.
	Root string

	// mu guards in-process concurrent writes; on-disk races are handled by Lock.
	mu sync.Mutex
}

// DefaultCacheRoot returns the default OCI cache root, honouring
// CRITERIA_STATE_DIR (falls back to ~/.criteria/cache/oci).
func DefaultCacheRoot() (string, error) {
	base := os.Getenv("CRITERIA_STATE_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("oci: resolve home dir: %w", err)
		}
		base = filepath.Join(home, ".criteria")
	}
	return filepath.Join(base, "cache", "oci"), nil
}

// Open opens the OCI Image Layout at root, creating it if it does not exist.
// Returns an error if an existing directory contains an incompatible
// oci-layout marker.
func Open(root string) (*Layout, error) {
	if err := os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o750); err != nil {
		return nil, fmt.Errorf("oci: create layout dirs: %w", err)
	}

	markerPath := filepath.Join(root, "oci-layout")
	if _, err := os.Stat(markerPath); errors.Is(err, os.ErrNotExist) {
		if err := writeOCILayoutMarker(markerPath); err != nil {
			return nil, err
		}
	} else if err == nil {
		if err := validateOCILayoutMarker(markerPath); err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("oci: stat oci-layout: %w", err)
	}

	indexPath := filepath.Join(root, "index.json")
	if _, err := os.Stat(indexPath); errors.Is(err, os.ErrNotExist) {
		empty := ocispec.Index{
			MediaType: ocispec.MediaTypeImageIndex,
		}
		if err := writeJSON(indexPath, empty); err != nil {
			return nil, fmt.Errorf("oci: create index.json: %w", err)
		}
	}

	return &Layout{Root: root}, nil
}

// Index reads and returns the current OCI index from index.json.
func (l *Layout) Index() (*ocispec.Index, error) {
	data, err := os.ReadFile(filepath.Join(l.Root, "index.json"))
	if err != nil {
		return nil, fmt.Errorf("oci: read index.json: %w", err)
	}
	var ix ocispec.Index
	if err := json.Unmarshal(data, &ix); err != nil {
		return nil, fmt.Errorf("oci: parse index.json: %w", err)
	}
	return &ix, nil
}

// WriteIndex atomically replaces index.json with ix.
func (l *Layout) WriteIndex(ix *ocispec.Index) error {
	return writeJSONAtomic(filepath.Join(l.Root, "index.json"), ix)
}

// BlobPath returns the on-disk path of the blob identified by d.
func (l *Layout) BlobPath(d digest.Digest) string {
	return filepath.Join(l.Root, "blobs", d.Algorithm().String(), d.Encoded())
}

// HasBlob reports whether the blob identified by d is present on disk.
func (l *Layout) HasBlob(d digest.Digest) bool {
	_, err := os.Stat(l.BlobPath(d))
	return err == nil
}

// WriteBlob streams the contents of reader into the layout, verifying that the
// resulting digest matches expect. The write is atomic: a temporary file in the
// same directory is used and then renamed into place. If a blob with the same
// digest already exists the write is skipped.
func (l *Layout) WriteBlob(reader io.Reader, expect digest.Digest) error {
	if l.HasBlob(expect) {
		return nil
	}

	dir := filepath.Join(l.Root, "blobs", expect.Algorithm().String())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("oci: mkdir blobs/%s: %w", expect.Algorithm(), err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return fmt.Errorf("oci: create temp blob: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	verifier := expect.Verifier()
	w := io.MultiWriter(tmp, verifier)
	if _, err := io.Copy(w, reader); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("oci: write blob data: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("oci: close temp blob: %w", err)
	}
	if !verifier.Verified() {
		return fmt.Errorf("oci: digest mismatch for %s", expect)
	}

	dest := l.BlobPath(expect)
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("oci: rename blob into place: %w", err)
	}
	return nil
}

// Lock acquires an exclusive in-process lock on the layout.  The caller must
// call release when the critical section is complete.
//
// For cross-process locking on Linux/macOS this uses flock(2); see
// lockFile() in layout_lock.go for the platform implementation.
// On Windows the implementation falls back to an in-process mutex (see
// layout_lock_windows.go); cross-process safety via LockFileEx is a known
// gap on that platform.
func (l *Layout) Lock() (release func(), err error) {
	l.mu.Lock()

	lockPath := filepath.Join(l.Root, ".lock")
	rel, err := lockFile(lockPath)
	if err != nil {
		l.mu.Unlock()
		return nil, fmt.Errorf("oci: acquire flock: %w", err)
	}

	return func() {
		rel()
		l.mu.Unlock()
	}, nil
}

// ArtifactProtocolVersion returns the protocol version annotation stored on
// the index.json descriptor whose manifest digest is d, or 0 if absent.
// A return value of 0 means "unknown — re-read adapter.yaml".
func (l *Layout) ArtifactProtocolVersion(d digest.Digest) uint32 {
	ix, err := l.Index()
	if err != nil {
		return 0
	}
	for _, desc := range ix.Manifests {
		if digest.Digest(desc.Digest.String()) == d {
			if v, ok := desc.Annotations[AnnotationProtocolVersion]; ok {
				var n uint32
				if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
					return n
				}
			}
			return 0
		}
	}
	return 0
}

// — helpers —

type ociLayoutMarker struct {
	ImageLayoutVersion string `json:"imageLayoutVersion"`
}

func writeOCILayoutMarker(path string) error {
	return writeJSON(path, ociLayoutMarker{ImageLayoutVersion: ociLayoutVersion})
}

func validateOCILayoutMarker(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("oci: read oci-layout: %w", err)
	}
	var m ociLayoutMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("oci: parse oci-layout: %w", err)
	}
	if m.ImageLayoutVersion != ociLayoutVersion {
		return fmt.Errorf("oci: unsupported imageLayoutVersion %q (want %q)", m.ImageLayoutVersion, ociLayoutVersion)
	}
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("oci: marshal json: %w", err)
	}
	return os.WriteFile(path, data, 0o640)
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("oci: marshal json: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-index-")
	if err != nil {
		return fmt.Errorf("oci: create temp index: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("oci: write temp index: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("oci: close temp index: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("oci: rename index into place: %w", err)
	}
	return nil
}
