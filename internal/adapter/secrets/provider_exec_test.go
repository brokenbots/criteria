package secrets

// provider_exec_test.go — exercises the shell-out providers (sops, vault,
// keychain) end to end against fake backend binaries on PATH. Each provider
// intentionally shells out to its CLI; faking the binary keeps these tests
// hermetic while still covering the real exec paths (argument construction,
// output trimming, failure wrapping) that unit tests of helper functions
// cannot reach.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeBinDir builds a directory containing the named fake executables (POSIX
// shell snippets) and prepends it to PATH for the duration of the test.
func fakeBinDir(t *testing.T, scripts map[string]string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake PATH binaries require a POSIX shell")
	}

	dir := t.TempDir()
	for name, body := range scripts {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// fakeBinArgsFile returns the path where fake binaries record their argv,
// seeded clean for each test.
func fakeBinArgsFile(t *testing.T) string {
	t.Helper()
	argsPath := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("CRITERIA_FAKE_BIN_ARGS", argsPath)
	return argsPath
}

func TestSOPSProvider_Resolve(t *testing.T) {
	argsFile := fakeBinArgsFile(t)
	fakeBinDir(t, map[string]string{
		"sops": `printf '%s\n' "$*" > "$CRITERIA_FAKE_BIN_ARGS"; echo decrypted-value`,
	})

	p := SOPSProvider{}
	val, err := p.Resolve(context.Background(), OriginRef{Kind: "sops", Ref: "/run/secrets/app.yaml#api_key"})
	require.NoError(t, err)
	require.Equal(t, "decrypted-value", val)

	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	require.Contains(t, string(args), "--extract [api_key]")
	require.Contains(t, string(args), "/run/secrets/app.yaml")
}

func TestSOPSProvider_Resolve_TrailingNewlineTrimmed(t *testing.T) {
	fakeBinDir(t, map[string]string{
		"sops": `printf 'value\r\n'`,
	})
	p := SOPSProvider{}
	val, err := p.Resolve(context.Background(), OriginRef{Kind: "sops", Ref: "secrets.yaml#key"})
	require.NoError(t, err)
	require.Equal(t, "value", val)
}

func TestSOPSProvider_Resolve_BadRef(t *testing.T) {
	p := SOPSProvider{}
	for _, ref := range []string{"no-key", "file#", ""} {
		_, err := p.Resolve(context.Background(), OriginRef{Kind: "sops", Ref: ref})
		require.Error(t, err, "ref %q must be rejected", ref)
		require.Contains(t, err.Error(), "must be file#key")
	}
}

func TestSOPSProvider_Resolve_DecryptFailure(t *testing.T) {
	fakeBinDir(t, map[string]string{
		"sops": `echo "cipher failure" >&2; exit 7`,
	})
	p := SOPSProvider{}
	_, err := p.Resolve(context.Background(), OriginRef{Kind: "sops", Ref: "/run/secrets/app.yaml#api_key"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "sops decrypt failed")
}

func TestSOPSProvider_Name(t *testing.T) {
	require.Equal(t, "sops", SOPSProvider{}.Name())
}

func TestVaultProvider_Resolve(t *testing.T) {
	argsFile := fakeBinArgsFile(t)
	fakeBinDir(t, map[string]string{
		"vault": `printf '%s\n' "$*" > "$CRITERIA_FAKE_BIN_ARGS"; echo vaulted-value`,
	})
	t.Setenv("VAULT_ADDR", "https://vault.example.invalid")

	p := VaultProvider{}

	// Bare path is normalized to the KV v2 data path.
	val, err := p.Resolve(context.Background(), OriginRef{Kind: "vault", Ref: "myapp#api_key"})
	require.NoError(t, err)
	require.Equal(t, "vaulted-value", val)

	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	require.Contains(t, string(args), "kv get")
	require.Contains(t, string(args), "-field=api_key")
	require.Contains(t, string(args), "secret/data/myapp")
}

func TestVaultProvider_Resolve_PathWithDataURL(t *testing.T) {
	argsFile := fakeBinArgsFile(t)
	fakeBinDir(t, map[string]string{
		"vault": `printf '%s\n' "$*" > "$CRITERIA_FAKE_BIN_ARGS"; echo vaulted-value`,
	})
	t.Setenv("VAULT_ADDR", "https://vault.example.invalid")

	p := VaultProvider{}
	_, err := p.Resolve(context.Background(), OriginRef{Kind: "vault", Ref: "secret/data/myapp#api_key"})
	require.NoError(t, err)

	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	// An explicit data URL is used as-is; no second prefix is applied.
	require.Contains(t, string(args), " secret/data/myapp")
	require.NotContains(t, string(args), "secret/data/secret/data")
}

func TestVaultProvider_Resolve_BadRef(t *testing.T) {
	t.Setenv("VAULT_ADDR", "https://vault.example.invalid")
	p := VaultProvider{}
	_, err := p.Resolve(context.Background(), OriginRef{Kind: "vault", Ref: "myapp"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be path#field")
}

func TestVaultProvider_Resolve_CommandFailure(t *testing.T) {
	fakeBinDir(t, map[string]string{
		"vault": `echo Permission denied >&2; exit 1`,
	})
	t.Setenv("VAULT_ADDR", "https://vault.example.invalid")

	p := VaultProvider{}
	_, err := p.Resolve(context.Background(), OriginRef{Kind: "vault", Ref: "myapp#api_key"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "vault kv get failed")
}

func TestVaultProvider_Name(t *testing.T) {
	require.Equal(t, "vault", VaultProvider{}.Name())
}

func TestKeychainProvider_Resolve_Linux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("resolveLinux is only reachable on linux, not %s", runtime.GOOS)
	}

	argsFile := fakeBinArgsFile(t)
	fakeBinDir(t, map[string]string{
		"secret-tool": `printf '%s\n' "$*" > "$CRITERIA_FAKE_BIN_ARGS"; printf 'keychain-value\n'`,
	})

	p := &KeychainProvider{}
	val, err := p.Resolve(context.Background(), OriginRef{Kind: "keychain", Ref: "criteria:api_key"})
	require.NoError(t, err)
	require.Equal(t, "keychain-value", val)

	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	require.Equal(t, "lookup criteria api_key", strings.TrimSpace(string(args)))
}

func TestKeychainProvider_Resolve_Linux_BadRef(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("resolveLinux is only reachable on linux, not %s", runtime.GOOS)
	}

	p := &KeychainProvider{}
	_, err := p.Resolve(context.Background(), OriginRef{Kind: "keychain", Ref: "incomplete"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "attribute:value")
}

func TestKeychainProvider_Resolve_EmptyRef(t *testing.T) {
	p := &KeychainProvider{}
	_, err := p.Resolve(context.Background(), OriginRef{Kind: "keychain"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty reference")
}

func TestKeychainProvider_Resolve_NativeMissing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("native resolution path is OS-specific; this case exercises the linux branch")
	}

	// No secret-tool on PATH: native resolution must fail and the fallback
	// provider must win.
	p := &KeychainProvider{Fallbacks: []Provider{
		&staticProvider{name: "fb", kind: "keychain", val: "from-fallback"},
	}}
	val, err := p.Resolve(context.Background(), OriginRef{Kind: "keychain", Ref: "criteria:api_key"})
	require.NoError(t, err)
	require.Equal(t, "from-fallback", val)
}

func TestKeychainProvider_Resolve_NativeError_FallbackCannotHandle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("native resolution path is OS-specific; this case exercises the linux branch")
	}

	// secret-tool exists but fails; the configured fallback cannot handle the
	// ref, so the native error must be propagated.
	fakeBinDir(t, map[string]string{
		"secret-tool": `echo "The name was not found" >&2; exit 1`,
	})
	p := &KeychainProvider{Fallbacks: []Provider{EnvProvider{}}}
	_, err := p.Resolve(context.Background(), OriginRef{Kind: "keychain", Ref: "criteria:api_key"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "keychain provider: ")
}

func TestKeychainProvider_Name(t *testing.T) {
	require.Equal(t, "keychain", (&KeychainProvider{}).Name())
}

func TestOriginRef_StringAndParse(t *testing.T) {
	require.Equal(t, "env:FOO", OriginRef{Kind: "env", Ref: "FOO"}.String())

	require.Equal(t, OriginRef{Kind: "env", Ref: "FOO"}, ParseOriginRef("env:FOO"))
	require.Equal(t, OriginRef{Kind: "file", Ref: "/run/secrets/k"}, ParseOriginRef("file:/run/secrets/k"))
	require.Equal(t, OriginRef{Kind: "vault", Ref: "secret/data/app#key"}, ParseOriginRef("vault:secret/data/app#key"))
	// A ref may itself contain colons; only the first separates the kind.
	require.Equal(t, OriginRef{Kind: "keychain", Ref: "service:account"}, ParseOriginRef("keychain:service:account"))
	// No colon, or a leading colon, means a literal value.
	require.Equal(t, OriginRef{Kind: "literal", Ref: "raw-value"}, ParseOriginRef("raw-value"))
	require.Equal(t, OriginRef{Kind: "literal", Ref: ":weird"}, ParseOriginRef(":weird"))
}

func TestOriginRef_JSONRoundTrip(t *testing.T) {
	// Compact string form.
	var fromString OriginRef
	require.NoError(t, json.Unmarshal([]byte(`"env:FOO"`), &fromString))
	require.Equal(t, OriginRef{Kind: "env", Ref: "FOO"}, fromString)

	// Structured object form.
	var fromObject OriginRef
	require.NoError(t, json.Unmarshal([]byte(`{"kind":"file","ref":"/run/secrets/k"}`), &fromObject))
	require.Equal(t, OriginRef{Kind: "file", Ref: "/run/secrets/k"}, fromObject)

	// Invalid JSON surfaces an error.
	var broken OriginRef
	require.Error(t, json.Unmarshal([]byte(`[1,2,3`), &broken))

	// Marshalling always emits the compact string form.
	b, err := json.Marshal(OriginRef{Kind: "vault", Ref: "secret/data/app#key"})
	require.NoError(t, err)
	require.Equal(t, `"vault:secret/data/app#key"`, string(b))
}

func TestOriginRef_TextRoundTrip(t *testing.T) {
	var ref OriginRef
	require.NoError(t, ref.UnmarshalText([]byte("file:/run/secrets/k")))
	require.Equal(t, OriginRef{Kind: "file", Ref: "/run/secrets/k"}, ref)

	text, err := ref.MarshalText()
	require.NoError(t, err)
	require.Equal(t, "file:/run/secrets/k", string(text))
}

func TestResolveString(t *testing.T) {
	t.Setenv("RESOLVE_TEST_SECRET", "shhh")
	stack := DefaultStack()

	// Literals pass through untouched.
	val, err := ResolveString(context.Background(), "plain-value", stack)
	require.NoError(t, err)
	require.Equal(t, "plain-value", val)

	// Provider refs resolve through the stack.
	val, err = ResolveString(context.Background(), "env:RESOLVE_TEST_SECRET", stack)
	require.NoError(t, err)
	require.Equal(t, "shhh", val)
}

func TestResolveString_NoStack(t *testing.T) {
	_, err := ResolveString(context.Background(), "env:RESOLVE_TEST_SECRET", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no provider stack")
}

func TestResolveError_Formatting(t *testing.T) {
	cause := errors.New("env provider: \"X\" is not set")
	err := &ResolveError{Name: "k", Origin: OriginRef{Kind: "env", Ref: "X"}, Cause: cause}
	require.Equal(t, `secret "k" (origin env:X): env provider: "X" is not set`, err.Error())
	require.Equal(t, cause, err.Unwrap())
	require.True(t, errors.Is(err, cause))
}