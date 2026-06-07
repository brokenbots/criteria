package secrets

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileProvider(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	_ = os.MkdirAll(root, 0o755)

	secretPath := filepath.Join(root, "secret.txt")
	require.NoError(t, os.WriteFile(secretPath, []byte("secret-value\n"), 0o600))

	p := &FileProvider{Root: root}
	require.Equal(t, "file", p.Name())
	require.True(t, p.CanResolve(OriginRef{Kind: "file", Ref: secretPath}))
	require.False(t, p.CanResolve(OriginRef{Kind: "env", Ref: "FOO"}))

	val, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: secretPath})
	require.NoError(t, err)
	require.Equal(t, "secret-value", val)
}

func TestFileProvider_StripsNewline(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	_ = os.MkdirAll(root, 0o755)

	path := filepath.Join(root, "secret.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello\n\n"), 0o600))

	p := &FileProvider{Root: root}
	val, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: path})
	require.NoError(t, err)
	require.Equal(t, "hello", val)
}

func TestFileProvider_DefaultRoot(t *testing.T) {
	p := &FileProvider{}
	require.Equal(t, "", p.Root) // Root is empty but resolved to home at runtime
}

func TestFileProvider_PathConfinement(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	_ = os.MkdirAll(root, 0o755)

	outside := filepath.Join(tmp, "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("bad"), 0o600))

	p := &FileProvider{Root: root}

	_, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: outside})
	require.Error(t, err)
	require.Contains(t, err.Error(), "escapes root")
}

func TestFileProvider_SymlinkEscapesRoot(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	_ = os.MkdirAll(root, 0o755)

	outside := filepath.Join(tmp, "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("bad"), 0o600))

	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(outside, link))

	p := &FileProvider{Root: root}

	_, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: link})
	require.Error(t, err)
	require.Contains(t, err.Error(), "escapes root")
}
