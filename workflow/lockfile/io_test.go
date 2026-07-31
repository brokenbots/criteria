package lockfile_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

func TestRead_FullFixture(t *testing.T) {
	lf, err := lockfile.Read("testdata/full.lock.hcl")
	require.NoError(t, err)
	require.NotNil(t, lf)

	assert.Equal(t, 1, lf.SchemaVersion)
	require.Len(t, lf.Adapters, 2)

	// First adapter: claude.default (sorted alphabetically before copilot.default)
	a := lf.Adapters[0]
	assert.Equal(t, "claude", a.Type)
	assert.Equal(t, "default", a.Name)
	assert.Equal(t, "ghcr.io/criteria-adapters/claude:1.2.3", a.Reference)
	assert.Equal(t, "sha256:abc123def456", a.ResolvedDigest)
	assert.Equal(t, "https://github.com/criteria-adapters/claude", a.SourceURL)
	assert.Equal(t, 2, a.SDKProtocolVersion)
	assert.Equal(t, []string{"linux/amd64", "linux/arm64", "darwin/arm64"}, a.Platforms)

	require.NotNil(t, a.Signature)
	require.NotNil(t, a.Signature.Keyless)
	assert.Equal(t, "https://token.actions.githubusercontent.com", a.Signature.Keyless.Issuer)
	assert.Equal(t, "https://github.com/criteria-adapters/claude/.github/workflows/publish.yml@refs/tags/v1.2.3", a.Signature.Keyless.Subject)
	assert.Nil(t, a.Signature.Key)

	require.NotNil(t, a.ContainerImage)
	assert.Equal(t, "ghcr.io/criteria-adapters/claude:1.2.3-image", a.ContainerImage.Ref)
	assert.Equal(t, "sha256:def456abc789", a.ContainerImage.Digest)

	assert.Nil(t, a.Remote)
	assert.Empty(t, a.CompatibleEnvironmentsOverride)
	assert.Empty(t, a.OverriddenBy)

	// Second adapter: copilot.default
	a = lf.Adapters[1]
	assert.Equal(t, "copilot", a.Type)
	assert.Equal(t, "default", a.Name)
	assert.Equal(t, "ghcr.io/criteria-adapters/copilot:0.5.0", a.Reference)
	assert.Equal(t, "sha256:789012", a.ResolvedDigest)
	assert.Equal(t, 2, a.SDKProtocolVersion)
	assert.Equal(t, []string{"linux/amd64"}, a.Platforms)

	require.NotNil(t, a.Signature)
	assert.Nil(t, a.Signature.Keyless)
	require.NotNil(t, a.Signature.Key)
	assert.Equal(t, "ed25519", a.Signature.Key.Algorithm)
	assert.Equal(t, "sha256:pubkeyfp", a.Signature.Key.Fingerprint)

	require.NotNil(t, a.Remote)
	assert.Equal(t, "0.0.0.0:7778", a.Remote.ListenAddress)
	assert.Equal(t, "sha256:certfp", a.Remote.ServerCertFingerprint)

	assert.Equal(t, []string{"shell"}, a.CompatibleEnvironmentsOverride)
	assert.Equal(t, "workflow.hcl:42", a.OverriddenBy)
}

func TestRead_MinimalFixture(t *testing.T) {
	lf, err := lockfile.Read("testdata/minimal.lock.hcl")
	require.NoError(t, err)
	require.Len(t, lf.Adapters, 1)

	a := lf.Adapters[0]
	assert.Equal(t, "noop", a.Type)
	assert.Equal(t, "default", a.Name)
	assert.Equal(t, "sha256:000000", a.ResolvedDigest)
	assert.Nil(t, a.Signature)
	assert.Nil(t, a.ContainerImage)
	assert.Nil(t, a.Remote)
	assert.Empty(t, a.Platforms)
	assert.Empty(t, a.CompatibleEnvironmentsOverride)
}

func TestRead_RemoteFixture(t *testing.T) {
	lf, err := lockfile.Read("testdata/remote.lock.hcl")
	require.NoError(t, err)
	require.Len(t, lf.Adapters, 1)

	a := lf.Adapters[0]
	assert.Equal(t, "copilot", a.Type)
	require.NotNil(t, a.Remote)
	assert.Equal(t, "0.0.0.0:7778", a.Remote.ListenAddress)
	assert.Equal(t, "sha256:certfp", a.Remote.ServerCertFingerprint)
	assert.Nil(t, a.Signature)
	assert.Nil(t, a.ContainerImage)
}

func TestRead_ContainerImageFixture(t *testing.T) {
	lf, err := lockfile.Read("testdata/container_image.lock.hcl")
	require.NoError(t, err)
	require.Len(t, lf.Adapters, 1)

	a := lf.Adapters[0]
	require.NotNil(t, a.ContainerImage)
	assert.Equal(t, "ghcr.io/criteria-adapters/claude:1.2.3-image", a.ContainerImage.Ref)
	assert.Equal(t, "sha256:def456abc789", a.ContainerImage.Digest)
	assert.Nil(t, a.Signature)
	assert.Nil(t, a.Remote)
}

func TestWrite_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".criteria.lock.hcl")

	original := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{
				Type:               "claude",
				Name:               "default",
				Reference:          "ghcr.io/criteria-adapters/claude:1.2.3",
				Version:            "1.2.3",
				ResolvedDigest:     "sha256:abc123",
				SourceURL:          "https://github.com/criteria-adapters/claude",
				SDKProtocolVersion: 2,
				Platforms:          []string{"linux/amd64", "linux/arm64"},
				Signature: &lockfile.LockedSignature{
					Keyless: &lockfile.LockedKeyless{
						Issuer:  "https://token.actions.githubusercontent.com",
						Subject: "subj",
					},
				},
				ContainerImage: &lockfile.LockedContainerImage{
					Ref:    "ghcr.io/criteria-adapters/claude:1.2.3-image",
					Digest: "sha256:img123",
				},
				Remote: &lockfile.LockedRemote{
					ListenAddress:         "0.0.0.0:7778",
					ServerCertFingerprint: "sha256:cert123",
				},
				CompatibleEnvironmentsOverride: []string{"shell"},
				OverriddenBy:                   "workflow.hcl:7",
			},
			{
				Type:               "noop",
				Name:               "default",
				Reference:          "ghcr.io/criteria-adapters/noop:latest",
				ResolvedDigest:     "sha256:000000",
				SourceURL:          "https://github.com/criteria-adapters/noop",
				SDKProtocolVersion: 2,
			},
		},
	}

	require.NoError(t, lockfile.Write(path, original))

	reloaded, err := lockfile.Read(path)
	require.NoError(t, err)

	assert.Equal(t, original.SchemaVersion, reloaded.SchemaVersion)
	require.Len(t, reloaded.Adapters, 2)

	// Verify sorting: noop.default should come before claude.default alphabetically.
	assert.Equal(t, "claude", reloaded.Adapters[0].Type)
	assert.Equal(t, "noop", reloaded.Adapters[1].Type)

	// Deep-equal the first adapter (claude).
	a := reloaded.Adapters[0]
	assert.Equal(t, "claude", a.Type)
	assert.Equal(t, "default", a.Name)
	assert.Equal(t, "1.2.3", a.Version)
	assert.Equal(t, "sha256:abc123", a.ResolvedDigest)
	assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, a.Platforms)
	require.NotNil(t, a.Signature)
	require.NotNil(t, a.Signature.Keyless)
	assert.Equal(t, "subj", a.Signature.Keyless.Subject)
	require.NotNil(t, a.ContainerImage)
	assert.Equal(t, "sha256:img123", a.ContainerImage.Digest)
	require.NotNil(t, a.Remote)
	assert.Equal(t, "sha256:cert123", a.Remote.ServerCertFingerprint)
	assert.Equal(t, []string{"shell"}, a.CompatibleEnvironmentsOverride)
	assert.Equal(t, "workflow.hcl:7", a.OverriddenBy)

	// Deep-equal the second adapter (noop).
	a = reloaded.Adapters[1]
	assert.Equal(t, "noop", a.Type)
	assert.Nil(t, a.Signature)
	assert.Nil(t, a.ContainerImage)
	assert.Nil(t, a.Remote)
}

func TestWrite_ByteStability(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock.hcl")

	lf := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{
				Type:               "z",
				Name:               "last",
				Reference:          "r",
				ResolvedDigest:     "sha256:d1",
				SourceURL:          "https://example.com",
				SDKProtocolVersion: 2,
				Platforms:          []string{"linux/amd64"},
			},
			{
				Type:               "a",
				Name:               "first",
				Reference:          "r",
				ResolvedDigest:     "sha256:d2",
				SourceURL:          "https://example.com",
				SDKProtocolVersion: 2,
			},
		},
	}

	require.NoError(t, lockfile.Write(path, lf))
	first, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, lockfile.Write(path, lf))
	second, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.True(t, bytes.Equal(first, second), "two Write calls with identical inputs must produce identical bytes")
}

func TestReadFromDir_Found(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".criteria.lock.hcl")
	require.NoError(t, os.WriteFile(path, []byte(`schema_version = 1

adapter "x" "y" {
  reference            = "r"
  resolved_digest      = "sha256:d"
  source_url           = "https://example.com"
  sdk_protocol_version = 2
}
`), 0o644))

	lf, err := lockfile.ReadFromDir(dir)
	require.NoError(t, err)
	require.NotNil(t, lf)
	assert.Equal(t, 1, lf.SchemaVersion)
}

func TestReadFromDir_NotFound(t *testing.T) {
	dir := t.TempDir()
	lf, err := lockfile.ReadFromDir(dir)
	require.NoError(t, err)
	assert.Nil(t, lf)
}

func TestRead_MissingFile(t *testing.T) {
	_, err := lockfile.Read("testdata/does_not_exist.lock.hcl")
	require.Error(t, err)
}

func TestRead_InvalidHCL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.lock.hcl")
	require.NoError(t, os.WriteFile(path, []byte(`not valid hcl {`), 0o644))

	_, err := lockfile.Read(path)
	require.Error(t, err)
}

func TestRead_InvalidSchema(t *testing.T) {
	// Syntactically valid HCL that fails semantic decode into Lockfile.
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-schema.lock.hcl")
	require.NoError(t, os.WriteFile(path, []byte(`schema_version = "not-a-number"
`), 0o644))

	_, err := lockfile.Read(path)
	require.Error(t, err)
}

func TestRead_FullWorkflowRefFixture(t *testing.T) {
	lf, err := lockfile.Read("testdata/complete.lock.hcl")
	require.NoError(t, err)
	require.NotNil(t, lf)
	require.Len(t, lf.Adapters, 1)
	require.Len(t, lf.WorkflowRefs, 1)
	assert.Equal(t, "loop", lf.WorkflowRefs[0].Name)
	assert.Equal(t, "abc", lf.WorkflowRefs[0].ResolvedRef)
}

func TestWrite_WorkflowRefBlock_TrailingCompatibility(t *testing.T) {
	// Locks regressed when adapters were sorted after workflow refs; this asserts
	// that two workflow_ref blocks round-trip cleanly and the resulting HCL file
	// retains the canonical shape (source, resolved_ref, kind attributes).
	dir := t.TempDir()
	path := filepath.Join(dir, ".criteria.lock.hcl")

	original := &lockfile.Lockfile{
		SchemaVersion: 1,
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "pair_programming_loop", Source: "./loop", ResolvedRef: "abc123", Kind: "git"},
			{Name: "release", Source: "https://github.com/example/release", ResolvedRef: "v1.2.3", Kind: "git"},
		},
	}
	require.NoError(t, lockfile.Write(path, original))

	reloaded, err := lockfile.Read(path)
	require.NoError(t, err)
	require.Len(t, reloaded.WorkflowRefs, 2)

	// Sorted alphabetically by Name.
	assert.Equal(t, "pair_programming_loop", reloaded.WorkflowRefs[0].Name)
	assert.Equal(t, "abc123", reloaded.WorkflowRefs[0].ResolvedRef)
	assert.Equal(t, "git", reloaded.WorkflowRefs[0].Kind)
	assert.Equal(t, "release", reloaded.WorkflowRefs[1].Name)
	assert.Equal(t, "./loop", reloaded.WorkflowRefs[0].Source)
}

func TestWrite_WorkflowRefBlock_EmitsBlockShape(t *testing.T) {
	// Verifies that workflow_ref blocks are emitted with the canonical HCL block
	// shape (lane header followed by the three attributes), as the read side
	// depends on it. hclwrite pads attribute names so we only assert on the
	// values, not the exact spacing.
	dir := t.TempDir()
	path := filepath.Join(dir, ".criteria.lock.hcl")

	lf := &lockfile.Lockfile{
		SchemaVersion: 1,
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop", ResolvedRef: "abc", Kind: "git"},
		},
	}
	require.NoError(t, lockfile.Write(path, lf))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(raw)
	assert.Contains(t, body, `workflow_ref "loop"`)
	assert.Contains(t, body, `"./loop"`)
	assert.Contains(t, body, `"abc"`)
	assert.Contains(t, body, `"git"`)

	// Each attribute should appear in the body on its own line. Use a more
	// lenient match to allow hclwrite to vary spacing.
	assert.Regexp(t, `(?m)^\s*source\s*=\s*"\./loop"\s*$`, body)
	assert.Regexp(t, `(?m)^\s*resolved_ref\s*=\s*"abc"\s*$`, body)
	assert.Regexp(t, `(?m)^\s*kind\s*=\s*"git"\s*$`, body)
}

func TestWrite_MixedAdaptersAndWorkflowRefs_SortsBoth(t *testing.T) {
	// Locks must remain sorted across both kinds: adapters by "<type>.<name>"
	// and workflow_refs by Name. Both are emitted in a single file.
	dir := t.TempDir()
	path := filepath.Join(dir, ".criteria.lock.hcl")

	lf := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{Type: "noop", Name: "default", Reference: "r", ResolvedDigest: "sha256:d", SourceURL: "https://example.com", SDKProtocolVersion: 2},
		},
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "zeta", Source: "./z", ResolvedRef: "z1", Kind: "git"},
			{Name: "alpha", Source: "./a", ResolvedRef: "a1", Kind: "git"},
		},
	}
	require.NoError(t, lockfile.Write(path, lf))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(raw)

	// Adapter block must come before workflow_ref blocks.
	adapterAt := strings.Index(body, "adapter")
	refAt := strings.Index(body, "workflow_ref")
	require.GreaterOrEqual(t, refAt, 0)
	require.GreaterOrEqual(t, adapterAt, 0)
	assert.Less(t, adapterAt, refAt, "adapter blocks must be emitted before workflow_ref blocks")

	// workflow_ref blocks must be sorted alphabetically by Name.
	alphaAt := strings.Index(body, `workflow_ref "alpha"`)
	zetaAt := strings.Index(body, `workflow_ref "zeta"`)
	require.GreaterOrEqual(t, alphaAt, 0)
	require.GreaterOrEqual(t, zetaAt, 0)
	assert.Less(t, alphaAt, zetaAt)
}

func TestWrite_AdapterWithKeySignature(t *testing.T) {
	// Exercises writeAdapterBlock's signature.key branch, which is distinct
	// from the keyless path already covered by TestWrite_RoundTrip.
	dir := t.TempDir()
	path := filepath.Join(dir, ".criteria.lock.hcl")

	lf := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{
				Type:               "copilot",
				Name:               "default",
				Reference:          "ghcr.io/criteria-adapters/copilot:0.5.0",
				ResolvedDigest:     "sha256:789012",
				SourceURL:          "https://github.com/criteria-adapters/copilot",
				SDKProtocolVersion: 2,
				Signature: &lockfile.LockedSignature{
					Key: &lockfile.LockedKey{Algorithm: "ed25519", Fingerprint: "sha256:pubkeyfp"},
				},
			},
		},
	}
	require.NoError(t, lockfile.Write(path, lf))

	reloaded, err := lockfile.Read(path)
	require.NoError(t, err)
	require.Len(t, reloaded.Adapters, 1)
	require.NotNil(t, reloaded.Adapters[0].Signature)
	require.NotNil(t, reloaded.Adapters[0].Signature.Key)
	assert.Equal(t, "ed25519", reloaded.Adapters[0].Signature.Key.Algorithm)
	assert.Equal(t, "sha256:pubkeyfp", reloaded.Adapters[0].Signature.Key.Fingerprint)
}

func TestWrite_EmptyAdaptersWithWorkflowRefs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".criteria.lock.hcl")

	lf := &lockfile.Lockfile{
		SchemaVersion: 1,
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "loop", Source: "./loop", ResolvedRef: "abc", Kind: "git"},
		},
	}
	require.NoError(t, lockfile.Write(path, lf))

	reloaded, err := lockfile.Read(path)
	require.NoError(t, err)
	assert.Empty(t, reloaded.Adapters)
	require.Len(t, reloaded.WorkflowRefs, 1)
	assert.Equal(t, "loop", reloaded.WorkflowRefs[0].Name)
}

func TestWrite_ReadOnlyDirectory(t *testing.T) {
	// os.WriteFile error path: write into a directory that lacks write permission.
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o555))
	defer os.Chmod(dir, 0o755) // ensure cleanup can remove the directory

	path := filepath.Join(dir, ".criteria.lock.hcl")
	err := lockfile.Write(path, &lockfile.Lockfile{SchemaVersion: 1})
	require.Error(t, err)
}

func TestReadFromDir_MalformedLockfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".criteria.lock.hcl")
	require.NoError(t, os.WriteFile(path, []byte(`schema_version = "not-a-number"`), 0o644))

	_, err := lockfile.ReadFromDir(dir)
	require.Error(t, err)
}

func TestReadFromDir_StatError(t *testing.T) {
	// Passing a regular file as the workflow directory makes os.Stat fail with
	// ENOTDIR, which is not os.IsNotExist and must be returned as an error.
	dir := t.TempDir()
	filePath := filepath.Join(dir, "a-file")
	require.NoError(t, os.WriteFile(filePath, []byte("not a dir"), 0o644))

	_, err := lockfile.ReadFromDir(filePath)
	require.Error(t, err)
}

func TestReadFromDir_ReadsWorkflowRefs(t *testing.T) {
	// ReadFromDir must surface WorkflowRefs alongside Adapters so the recursive
	// lock/run paths can see them without re-parsing.
	dir := t.TempDir()
	path := filepath.Join(dir, ".criteria.lock.hcl")
	require.NoError(t, os.WriteFile(path, []byte(`schema_version = 1

workflow_ref "loop" {
  source        = "./loop"
  resolved_ref  = "abc123"
  kind          = "git"
}
`), 0o644))

	lf, err := lockfile.ReadFromDir(dir)
	require.NoError(t, err)
	require.NotNil(t, lf)
	require.Len(t, lf.WorkflowRefs, 1)
	assert.Equal(t, "loop", lf.WorkflowRefs[0].Name)
	assert.Equal(t, "abc123", lf.WorkflowRefs[0].ResolvedRef)
}
