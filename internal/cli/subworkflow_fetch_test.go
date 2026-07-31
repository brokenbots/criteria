package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExtractTarGz_RejectPathTraversal verifies that a tar.gz archive containing
// an entry whose name escapes the destination directory is rejected and does not
// write files outside dst.
func TestExtractTarGz_RejectPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	require.NoError(t, os.MkdirAll(dst, 0o755))

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tr := tar.NewWriter(gz)

	writeTarEntry(t, tr, "workflow.hcl", []byte("workflow {}"))
	writeTarEntry(t, tr, "../escape.txt", []byte("escaped"))

	require.NoError(t, tr.Close())
	require.NoError(t, gz.Close())

	err := extractTarGz(dst, buf.Bytes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes destination directory")

	inside := filepath.Join(dst, "workflow.hcl")
	_, statErr := os.Stat(inside)
	assert.NoError(t, statErr, "valid entry inside dst should be extracted")

	outside := filepath.Join(tmp, "escape.txt")
	_, statErr = os.Stat(outside)
	assert.True(t, os.IsNotExist(statErr), "traversing entry must not write outside dst")
}

// TestExtractTarGz_RejectAbsolutePath verifies that absolute archive entry
// names are rejected.
func TestExtractTarGz_RejectAbsolutePath(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	require.NoError(t, os.MkdirAll(dst, 0o755))

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tr := tar.NewWriter(gz)
	writeTarEntry(t, tr, "/etc/passwd", []byte("root"))
	require.NoError(t, tr.Close())
	require.NoError(t, gz.Close())

	err := extractTarGz(dst, buf.Bytes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute path")
}

// TestExtractZip_RejectPathTraversal verifies that a zip archive containing an
// entry whose name escapes the destination directory is rejected and does not
// write files outside dst.
func TestExtractZip_RejectPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	require.NoError(t, os.MkdirAll(dst, 0o755))

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeZipEntry(t, zw, "workflow.hcl", []byte("workflow {}"))
	writeZipEntry(t, zw, "../escape.txt", []byte("escaped"))
	require.NoError(t, zw.Close())

	err := extractZip(dst, buf.Bytes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes destination directory")

	inside := filepath.Join(dst, "workflow.hcl")
	_, statErr := os.Stat(inside)
	assert.NoError(t, statErr, "valid entry inside dst should be extracted")

	outside := filepath.Join(tmp, "escape.txt")
	_, statErr = os.Stat(outside)
	assert.True(t, os.IsNotExist(statErr), "traversing entry must not write outside dst")
}

// TestExtractZip_RejectAbsolutePath verifies that absolute archive entry names
// are rejected for zip archives.
func TestExtractZip_RejectAbsolutePath(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	require.NoError(t, os.MkdirAll(dst, 0o755))

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeZipEntry(t, zw, "/etc/passwd", []byte("root"))
	require.NoError(t, zw.Close())

	err := extractZip(dst, buf.Bytes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute path")
}

// TestExtractTarGz_DeeplyNestedEntry verifies that a tar.gz archive with a
// deeply nested file extracts correctly and that close-error handling does not
// mask a successful extraction.
func TestExtractTarGz_DeeplyNestedEntry(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	require.NoError(t, os.MkdirAll(dst, 0o755))

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tr := tar.NewWriter(gz)
	writeTarEntry(t, tr, "a/b/c/deep.txt", []byte("deep value"))
	require.NoError(t, tr.Close())
	require.NoError(t, gz.Close())

	require.NoError(t, extractTarGz(dst, buf.Bytes()))

	got, err := os.ReadFile(filepath.Join(dst, "a", "b", "c", "deep.txt"))
	require.NoError(t, err)
	assert.Equal(t, "deep value", string(got))
}

// TestExtractZip_DeeplyNestedEntry verifies that a zip archive with a deeply
// nested file extracts correctly and that close-error handling does not mask a
// successful extraction.
func TestExtractZip_DeeplyNestedEntry(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	require.NoError(t, os.MkdirAll(dst, 0o755))

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeZipEntry(t, zw, "a/b/c/deep.txt", []byte("deep value"))
	require.NoError(t, zw.Close())

	require.NoError(t, extractZip(dst, buf.Bytes()))

	got, err := os.ReadFile(filepath.Join(dst, "a", "b", "c", "deep.txt"))
	require.NoError(t, err)
	assert.Equal(t, "deep value", string(got))
}

func writeTarEntry(t *testing.T, tw *tar.Writer, name string, body []byte) {
	t.Helper()
	h := &tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}
	require.NoError(t, tw.WriteHeader(h))
	_, err := tw.Write(body)
	require.NoError(t, err)
}

func writeZipEntry(t *testing.T, zw *zip.Writer, name string, body []byte) {
	t.Helper()
	w, err := zw.CreateHeader(&zip.FileHeader{
		Name:   name,
		Method: zip.Store,
	})
	require.NoError(t, err)
	_, err = io.Copy(w, bytes.NewReader(body))
	require.NoError(t, err)
}
