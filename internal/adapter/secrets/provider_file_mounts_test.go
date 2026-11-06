package secrets

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The declared-mount shapes the k8s runner uses (live: KB-369): CSI/SPC
// mounts are absolute paths OUTSIDE dirs.SecretsDir(); the operator stamps
// CRITERIA_SECRET_FILE_ROOTS from the same plan.secrets entries it already
// derives CRITERIA_SECRET_VARS from.

func TestFileProvider_DeclaredMountInsideTrustedRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRITERIA_HOME", filepath.Join(t.TempDir(), "app")) // config root set
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("CRITERIA_SECRET_FILE_ROOTS", root)
	// NOTE: $CRITERIA_HOME/<config>/secrets is NOT created — the default root
	// stays absent; resolution must still succeed through the declared root.

	mount := filepath.Join(root, "kanboard-secrets") // deployment layout: mountPath UNDER the declared root
	require.NoError(t, os.MkdirAll(filepath.Join(root, "kanboard-secrets"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(mount, "kanboard_app_token"), []byte("kb-token-value\n"), 0o600))

	p := &FileProvider{}
	val, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: filepath.Join(mount, "kanboard_app_token")})
	require.NoError(t, err)
	require.Equal(t, "kb-token-value", val)
}

func TestFileProvider_TrustedRootIsItselfAMountPath(t *testing.T) {
	// The mountPath itself (not a parent) is declared: mount=/tmp/x/kanboard-secrets,
	// origin /tmp/x/kanboard-secrets/kanboard_app_token.
	tmp := t.TempDir()
	mount := filepath.Join(tmp, "kanboard-secrets")
	require.NoError(t, os.MkdirAll(mount, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(mount, "kanboard_url"), []byte("http://127.0.0.1\n"), 0o600))

	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("HOME", t.TempDir()) // XDG default roots land under an empty temp $HOME (absent dirs)
	t.Setenv("CRITERIA_SECRET_FILE_ROOTS", mount)

	p := &FileProvider{}
	val, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: filepath.Join(mount, "kanboard_url")})
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1", val)
}

func TestFileProvider_UndeclaredMountStillFailsClosed(t *testing.T) {
	tmp := t.TempDir()
	mount := filepath.Join(tmp, "evil-secrets") // NOT declared
	require.NoError(t, os.MkdirAll(mount, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(mount, "key"), []byte("stolen"), 0o600))

	declared := filepath.Join(tmp, "kanboard-secrets")
	require.NoError(t, os.MkdirAll(declared, 0o700))

	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRITERIA_SECRET_FILE_ROOTS", declared)

	p := &FileProvider{}
	_, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: filepath.Join(mount, "key")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "escapes root")
	// The declared root exists and is evaluable; the escape error must name
	// THE DECLARED root, and the refused path must be the undeclared one.
	require.Contains(t, err.Error(), "2 declared secret roots")
	require.Contains(t, err.Error(), filepath.Join(tmp, "evil-secrets"))
}

func TestFileProvider_SymlinkEscapeViaTrustedRootFailsClosed(t *testing.T) {
	tmp := t.TempDir()
	mount := filepath.Join(tmp, "kanboard-secrets")
	require.NoError(t, os.MkdirAll(mount, 0o700))

	outside := filepath.Join(tmp, "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("leak"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(mount, "leak")))

	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRITERIA_SECRET_FILE_ROOTS", mount)

	p := &FileProvider{}
	_, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: filepath.Join(mount, "leak")})
	require.Error(t, err)
}

func TestFileProvider_MissingDefaultRootWithNoDeclaredRootsFailsClosed(t *testing.T) {
	// Back-compat of the 0.5.57 hardening: nothing declared, nothing provisioned — refuses.
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRITERIA_SECRET_FILE_ROOTS", "")

	p := &FileProvider{}
	_, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: "secret.txt"})
	require.Error(t, err)
}

func TestFileProvider_ExplicitSingleRootWins(t *testing.T) {
	root := filepath.Join(t.TempDir(), "single")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "s"), []byte("v"), 0o600))
	t.Setenv("CRITERIA_SECRET_FILE_ROOTS", "/tmp/other-must-be-ignored")

	p := &FileProvider{Root: root}
	val, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: "s"})
	require.NoError(t, err)
	require.Equal(t, "v", val)
}

func TestFileProvider_RelativeRefUnderDefaultRootStillWorks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("CRITERIA_SECRET_FILE_ROOTS", "")

	// XDG default config root: $HOME/.config/criteria/secrets
	root := filepath.Join(home, ".config", "criteria", "secrets")
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret.txt"), []byte("secrets-dir-secret\n"), 0o600))

	p := &FileProvider{}
	val, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: "secret.txt"})
	require.NoError(t, err)
	require.Equal(t, "secrets-dir-secret", val)
}

func TestFileProvider_TrustedRootEntriesDedupedAndWhitespaceIgnored(t *testing.T) {
	tmp := t.TempDir()
	mount := filepath.Join(tmp, "mount")
	require.NoError(t, os.MkdirAll(mount, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(mount, "k"), []byte("v"), 0o600))

	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRITERIA_SECRET_FILE_ROOTS", "\n  \n"+mount+"\n"+mount+"\n")

	roots, err := secretFileRoots("")
	require.NoError(t, err)
	count := 0
	for _, r := range roots {
		if r == mount {
			count++
		}
	}
	require.Equal(t, 1, count, "declared root must appear exactly once")

	p := &FileProvider{}
	val, err := p.Resolve(t.Context(), OriginRef{Kind: "file", Ref: filepath.Join(mount, "k")})
	require.NoError(t, err)
	require.Equal(t, "v", val)
}
