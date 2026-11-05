package secrets

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/brokenbots/criteria/internal/dirs"
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

// TestFileProvider_DefaultRoot tests the documented default: when Root is
// empty it resolves to the criteria state root (dirs.Home()), not the raw
// user home. The resolution is asserted behaviorally by confining within and
// rejecting outside that root.
func TestFileProvider_DefaultRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")

	root, err := dirs.Home()
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret.txt"), []byte("state-root-secret\n"), 0o600))

	// A file under $HOME but outside the criteria root must be refused.
	require.NoError(t, os.WriteFile(filepath.Join(home, "home-secret.txt"), []byte("home secret"), 0o600))

	p := &FileProvider{}
	val, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: "secret.txt"})
	require.NoError(t, err)
	require.Equal(t, "state-root-secret", val)

	_, err = p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: filepath.Join(home, "home-secret.txt")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "escapes root")
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

func TestFileProvider_PathBoundary(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	outsideDir := filepath.Join(tmp, "root-secret")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.MkdirAll(outsideDir, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "value"), []byte("inside\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(outsideDir, "value"), []byte("escaped\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "outside.txt"), []byte("outside\n"), 0o600))

	// Symlink inside root that points to a sibling-prefix file outside root.
	require.NoError(t, os.Symlink(filepath.Join(outsideDir, "value"), filepath.Join(root, "sibling-link")))

	// Symlink inside root that points to a file inside root (should remain allowed).
	require.NoError(t, os.Symlink(filepath.Join(root, "value"), filepath.Join(root, "inside-link")))

	tests := []struct {
		name        string
		root        string
		ref         string
		want        string
		wantErr     bool
		wantErrLike string
	}{
		{
			name:        "sibling_prefix_absolute",
			root:        root,
			ref:         filepath.Join(outsideDir, "value"),
			wantErr:     true,
			wantErrLike: "escapes root",
		},
		{
			name:        "sibling_prefix_relative_traversal",
			root:        root,
			ref:         "../root-secret/value",
			wantErr:     true,
			wantErrLike: "escapes root",
		},
		{
			name:        "symlink_inside_root_to_sibling_prefix",
			root:        root,
			ref:         filepath.Join(root, "sibling-link"),
			wantErr:     true,
			wantErrLike: "escapes root",
		},
		{
			name:        "root_with_trailing_separator",
			root:        root + string(filepath.Separator),
			ref:         filepath.Join(outsideDir, "value"),
			wantErr:     true,
			wantErrLike: "escapes root",
		},
		{
			name: "normal_descendant",
			root: root,
			ref:  filepath.Join(root, "value"),
			want: "inside",
		},
		{
			name: "relative_reference_inside_root",
			root: root,
			ref:  "value",
			want: "inside",
		},
		{
			name: "symlink_inside_root_to_inside_file",
			root: root,
			ref:  filepath.Join(root, "inside-link"),
			want: "inside",
		},
		{
			name:        "obvious_escape_no_shared_prefix",
			root:        root,
			ref:         filepath.Join(tmp, "outside.txt"),
			wantErr:     true,
			wantErrLike: "escapes root",
		},
		{
			name:        "path_equals_root_is_directory_read_error",
			root:        root,
			ref:         root,
			wantErr:     true,
			wantErrLike: "read",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &FileProvider{Root: tt.root}
			got, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: tt.ref})
			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErrLike)
				require.NotContains(t, got, "escaped")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
