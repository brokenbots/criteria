package oci

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// AnnotationTitle is the OCI annotation key that specifies the file name/path
// for a layer blob within the adapter artifact.
const AnnotationTitle = "org.opencontainers.image.title"

// Open returns a read-only fs.FS synthesised from the adapter artifact whose
// manifest is identified by d. The returned FS exposes:
//
//	adapter.yaml           – the manifest blob (identified by media type or title annotation)
//	bin/<platform>         – per-platform binary blobs
//	signatures/cosign.sig  – cosign signature blob, if present
//
// Callers use this to:
//   - read adapter.yaml without parsing OCI layers directly, and
//   - obtain the binary path for execve in the loader (WS08).
func (l *Layout) Open(d digest.Digest) (fs.FS, error) {
	if !l.HasBlob(d) {
		return nil, fmt.Errorf("oci: manifest blob %s not found in layout", d)
	}

	data, err := os.ReadFile(l.BlobPath(d))
	if err != nil {
		return nil, fmt.Errorf("oci: read manifest %s: %w", d, err)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("oci: parse manifest %s: %w", d, err)
	}

	vfs := &artifactFS{layout: l, files: make(map[string]digest.Digest)}

	for _, layer := range manifest.Layers {
		title, ok := layer.Annotations[AnnotationTitle]
		if !ok || title == "" {
			// Skip layers without a title annotation — they are not addressable
			// by the adapter FS interface.
			continue
		}
		// Normalise the path: strip leading slashes to keep it relative.
		title = strings.TrimPrefix(title, "/")
		vfs.files[title] = layer.Digest
	}

	return vfs, nil
}

// artifactFS implements fs.FS over a set of OCI blobs keyed by their
// title annotations.
type artifactFS struct {
	layout *Layout
	// files maps the title-annotated path within the artifact to its
	// content-addressed blob digest.
	files map[string]digest.Digest
}

// Open implements fs.FS.
func (a *artifactFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return a.openDir(".")
	}

	d, ok := a.files[name]
	if !ok {
		// Check if it is a directory prefix.
		for key := range a.files {
			if strings.HasPrefix(key, name+"/") {
				return a.openDir(name)
			}
		}
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	blobPath := a.layout.BlobPath(d)
	f, err := os.Open(blobPath)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return f, nil
}

// openDir returns a synthetic directory entry for the given prefix.
func (a *artifactFS) openDir(prefix string) (fs.File, error) {
	var entries []fs.DirEntry
	seen := make(map[string]bool)
	for key := range a.files {
		if prefix == "." {
			// Top-level: get first path component.
			parts := strings.SplitN(key, "/", 2)
			name := parts[0]
			if seen[name] {
				continue
			}
			seen[name] = true
			if len(parts) == 1 {
				entries = append(entries, &blobDirEntry{name: name, isDir: false, layout: a.layout, digest: a.files[key]})
			} else {
				entries = append(entries, &blobDirEntry{name: name, isDir: true})
			}
			continue
		}
		if strings.HasPrefix(key, prefix+"/") {
			rest := key[len(prefix)+1:]
			parts := strings.SplitN(rest, "/", 2)
			name := parts[0]
			if seen[name] {
				continue
			}
			seen[name] = true
			if len(parts) == 1 {
				entries = append(entries, &blobDirEntry{name: name, isDir: false, layout: a.layout, digest: a.files[key]})
			} else {
				entries = append(entries, &blobDirEntry{name: name, isDir: true})
			}
		}
	}
	return &syntheticDir{name: path.Base(prefix), entries: entries}, nil
}

// syntheticDir is an fs.File representing a virtual directory.
type syntheticDir struct {
	name    string
	entries []fs.DirEntry
	offset  int
}

func (d *syntheticDir) Stat() (fs.FileInfo, error) { return d, nil }
func (d *syntheticDir) Read(_ []byte) (int, error) { return 0, fmt.Errorf("is a directory") }
func (d *syntheticDir) Close() error               { return nil }

func (d *syntheticDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.offset >= len(d.entries) {
		if n <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}
	if n <= 0 {
		result := d.entries[d.offset:]
		d.offset = len(d.entries)
		return result, nil
	}
	end := d.offset + n
	if end > len(d.entries) {
		end = len(d.entries)
	}
	result := d.entries[d.offset:end]
	d.offset = end
	return result, nil
}

// fs.FileInfo implementation for syntheticDir.
func (d *syntheticDir) Name() string       { return d.name }
func (d *syntheticDir) Size() int64        { return 0 }
func (d *syntheticDir) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (d *syntheticDir) ModTime() time.Time { return time.Time{} }
func (d *syntheticDir) IsDir() bool        { return true }
func (d *syntheticDir) Sys() any           { return nil }

// blobDirEntry implements fs.DirEntry for a blob or virtual sub-directory.
type blobDirEntry struct {
	name   string
	isDir  bool
	layout *Layout
	digest digest.Digest
}

func (e *blobDirEntry) Name() string      { return e.name }
func (e *blobDirEntry) IsDir() bool       { return e.isDir }
func (e *blobDirEntry) Type() fs.FileMode { return e.fileMode().Type() }

func (e *blobDirEntry) Info() (fs.FileInfo, error) {
	if e.isDir {
		return &syntheticDir{name: e.name}, nil
	}
	if e.layout != nil && e.digest != "" {
		fi, err := os.Stat(e.layout.BlobPath(e.digest))
		if err == nil {
			return fi, nil
		}
	}
	return nil, fmt.Errorf("oci: blob %s not found", e.digest)
}

func (e *blobDirEntry) fileMode() fs.FileMode {
	if e.isDir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
