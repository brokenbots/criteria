package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// --------------------------------------------------------------------------
// dev verb
// --------------------------------------------------------------------------

func TestRunDev_ValidRegistration(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "criteria-adapter-test")
	require.NoError(t, os.WriteFile(bin, []byte("fake"), 0o755))

	var out bytes.Buffer
	require.NoError(t, runDev(bin, "shell.local", &out))
	assert.Contains(t, out.String(), "dev: registered")
	assert.Contains(t, out.String(), "shell.local")
}

func TestRunDev_DirectoryRejection(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := runDev(dir, "shell.local", &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is a directory")
}

func TestRunDev_NonExecutableRejection(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "criteria-adapter-test")
	require.NoError(t, os.WriteFile(bin, []byte("fake"), 0o644)) // not executable

	var out bytes.Buffer
	err := runDev(bin, "shell.local", &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not executable")
}

func TestRunDev_AsFormatValidation(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "criteria-adapter-test")
	require.NoError(t, os.WriteFile(bin, []byte("fake"), 0o755))

	var out bytes.Buffer
	// missing dot
	err := runDev(bin, "shelllocal", &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--as must be")

	// empty type
	err = runDev(bin, ".local", &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--as must be")

	// empty name
	err = runDev(bin, "shell.", &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--as must be")
}

func TestRunDev_DefaultAsFromBasename(t *testing.T) {
	// When --as is omitted, the basename is trimmed of the "criteria-adapter-"
	// prefix. If the result still lacks a dot, the command errors.
	dir := t.TempDir()
	bin := filepath.Join(dir, "criteria-adapter-mycustom")
	require.NoError(t, os.WriteFile(bin, []byte("fake"), 0o755))

	var out bytes.Buffer
	err := runDev(bin, "", &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--as must be")
}

// --------------------------------------------------------------------------
// checkDevAllowed
// --------------------------------------------------------------------------

func TestCheckDevAllowed_NoBinding(t *testing.T) {
	// No dev binding registered for this adapter.
	err := checkDevAllowed("strict", "noop", "default")
	require.NoError(t, err)
}

func TestCheckDevAllowed_StrictMode(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "criteria-adapter-test")
	require.NoError(t, os.WriteFile(bin, []byte("fake"), 0o755))

	var out bytes.Buffer
	require.NoError(t, runDev(bin, "shell.local", &out))

	err := checkDevAllowed("strict", "shell", "local")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not allowed when strict verification is enabled")
}

func TestCheckDevAllowed_NonStrictMode(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "criteria-adapter-test")
	require.NoError(t, os.WriteFile(bin, []byte("fake"), 0o755))

	var out bytes.Buffer
	require.NoError(t, runDev(bin, "shell.local", &out))

	err := checkDevAllowed("warn", "shell", "local")
	require.NoError(t, err)
}

// --------------------------------------------------------------------------
// remove verb
// --------------------------------------------------------------------------

func TestRunRemove_ByName(t *testing.T) {
	root, dg := createTestOCIArtifact(t, "removable-adapter")

	// Use the default cache root override so runRemove finds our layout.
	t.Setenv("CRITERIA_STATE_DIR", root)

	var out bytes.Buffer
	require.NoError(t, runRemove("removable-adapter", false, &out))
	assert.Contains(t, out.String(), "removed")
	assert.Contains(t, out.String(), dg.String())
}

func TestRunRemove_NotFound(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", root)

	var out bytes.Buffer
	err := runRemove("nonexistent-adapter", false, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRunRemove_WithPrune(t *testing.T) {
	root, dg := createTestOCIArtifact(t, "prunable-adapter")
	t.Setenv("CRITERIA_STATE_DIR", root)

	var out bytes.Buffer
	require.NoError(t, runRemove("prunable-adapter", true, &out))
	assert.Contains(t, out.String(), "removed")
	assert.Contains(t, out.String(), dg.String())
	assert.Contains(t, out.String(), "pruned")
}

// --------------------------------------------------------------------------
// prune verb
// --------------------------------------------------------------------------

func TestRunPrune_Basic(t *testing.T) {
	root := t.TempDir()
	// Create an empty but valid OCI layout.
	_, err := oci.Open(root)
	require.NoError(t, err)

	t.Setenv("CRITERIA_STATE_DIR", root)

	var out bytes.Buffer
	require.NoError(t, runPrune("", 0, &out))
	assert.Contains(t, out.String(), "pruned")
}

func TestRunPrune_WithOlderThan(t *testing.T) {
	root := t.TempDir()
	_, err := oci.Open(root)
	require.NoError(t, err)

	t.Setenv("CRITERIA_STATE_DIR", root)

	var out bytes.Buffer
	require.NoError(t, runPrune("30d", 0, &out))
	assert.Contains(t, out.String(), "pruned")
}

func TestRunPrune_InvalidDuration(t *testing.T) {
	root := t.TempDir()
	_, err := oci.Open(root)
	require.NoError(t, err)

	t.Setenv("CRITERIA_STATE_DIR", root)

	var out bytes.Buffer
	err = runPrune("not-a-duration", 0, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse --older-than")
}

// --------------------------------------------------------------------------
// lock verb helpers
// --------------------------------------------------------------------------

func TestPrintLockDiff(t *testing.T) {
	oldLF := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{Type: "noop", Name: "default", Reference: "ghcr.io/a/b:v1", ResolvedDigest: "sha256:aaa", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}
	newLF := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{Type: "noop", Name: "default", Reference: "ghcr.io/a/b:v2", ResolvedDigest: "sha256:bbb", SourceURL: "https://example.com", SDKProtocolVersion: 2},
			{Type: "shell", Name: "local", Reference: "ghcr.io/a/c:v1", ResolvedDigest: "sha256:ccc", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
	}

	var out bytes.Buffer
	printLockDiff(oldLF, newLF, &out)
	diff := out.String()
	assert.Contains(t, diff, "~ noop.default digest sha256:aaa -> sha256:bbb")
	assert.Contains(t, diff, "+ shell.local")
}

func TestMissingRefs(t *testing.T) {
	oldLF := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "noop", Name: "default"},
		},
	}
	adapters := map[string]*workflowAdapter{
		"noop.default": {Type: "noop", Name: "default"},
		"shell.local":  {Type: "shell", Name: "local"},
	}
	got := missingRefs(oldLF, adapters)
	require.Len(t, got, 1)
	assert.Equal(t, "shell.local", got[0])
}

func TestMissingRefs_NoOldLockfile(t *testing.T) {
	adapters := map[string]*workflowAdapter{
		"noop.default": {Type: "noop", Name: "default"},
	}
	got := missingRefs(nil, adapters)
	require.Len(t, got, 1)
	assert.Equal(t, "noop.default", got[0])
}

func TestParseAliasesFromFile(t *testing.T) {
	dir := t.TempDir()
	hcl := `registry "ghcr" {
  source = "ghcr.io/brokenbots"
}

registry "docker" {
  source = "docker.io/library"
}
`
	path := filepath.Join(dir, "aliases.hcl")
	require.NoError(t, os.WriteFile(path, []byte(hcl), 0o644))

	aliases := make(map[string]string)
	require.NoError(t, parseAliasesFromFile(path, aliases))
	assert.Equal(t, "ghcr.io/brokenbots", aliases["ghcr"])
	assert.Equal(t, "docker.io/library", aliases["docker"])
}

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

// createTestOCIArtifact builds a minimal OCI artifact in a temp directory,
// writes it into an OCI layout at <dir>/cache/oci (matching defaultCacheRoot),
// and returns the parent directory (suitable for CRITERIA_STATE_DIR) and the
// manifest digest.
func createTestOCIArtifact(t *testing.T, adapterName string) (string, digest.Digest) {
	t.Helper()
	root := t.TempDir()
	cacheRoot := filepath.Join(root, "cache", "oci")
	layout, err := oci.Open(cacheRoot)
	require.NoError(t, err)

	adapterYAML := []byte(fmt.Sprintf("name: %s\nprotocol: v2\n", adapterName))
	manifestJSON := []byte(fmt.Sprintf(`{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.criteria.adapter.v1+json",
    "digest": "%s",
    "size": 2
  },
  "layers": [
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar",
      "digest": "%s",
      "size": %d,
      "annotations": {
        "org.opencontainers.image.title": "adapter.yaml"
      }
    }
  ]
}`, digest.FromBytes([]byte("{}")), digest.FromBytes(adapterYAML), len(adapterYAML)))

	cfgBlobPath := filepath.Join(cacheRoot, "blobs", "sha256", digest.FromBytes([]byte("{}")).Encoded())
	yamlBlobPath := filepath.Join(cacheRoot, "blobs", "sha256", digest.FromBytes(adapterYAML).Encoded())
	manifestBlobPath := filepath.Join(cacheRoot, "blobs", "sha256", digest.FromBytes(manifestJSON).Encoded())
	require.NoError(t, os.MkdirAll(filepath.Dir(cfgBlobPath), 0o755))
	require.NoError(t, os.WriteFile(cfgBlobPath, []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(yamlBlobPath, adapterYAML, 0o644))
	require.NoError(t, os.WriteFile(manifestBlobPath, manifestJSON, 0o644))

	ix := &ocispec.Index{Manifests: []ocispec.Descriptor{{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    digest.FromBytes(manifestJSON),
		Size:      int64(len(manifestJSON)),
	}}}
	require.NoError(t, layout.WriteIndex(ix))

	return root, digest.FromBytes(manifestJSON)
}
