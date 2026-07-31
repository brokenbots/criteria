package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"testing/fstest"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// TestAssertLockfileCoversAdapters exercises the lockfile-coverage check: an
// OCI-referenced adapter with no lockfile entry errors, non-OCI adapters (no
// source) are ignored, and a fully-covered set succeeds.
func TestAssertLockfileCoversAdapters(t *testing.T) {
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "foo", Name: "inst", Reference: "reg/foo", ResolvedDigest: "sha256:" + "ab", SourceURL: "https://example.com/foo"},
		},
	}

	t.Run("missing_entry_errors", func(t *testing.T) {
		ociAdapters := map[string]*workflowAdapter{
			"foo.inst":  {Type: "foo", Name: "inst", Source: "reg/foo"},
			"bar.other": {Type: "bar", Name: "other", Source: "reg/bar"},
		}
		workflowDir := t.TempDir()
		err := assertLockfileCoversAdapters(lf, workflowDir, ociAdapters)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "lockfile missing entries")
		assert.Contains(t, err.Error(), "bar.other")
	})

	t.Run("non_oci_skipped", func(t *testing.T) {
		// A non-OCI adapter (empty Source) must not be required in the lockfile.
		workflowDir := t.TempDir()
		ociAdapters := map[string]*workflowAdapter{
			"foo.inst":  {Type: "foo", Name: "inst", Source: "reg/foo"},
			"local.dev": {Type: "local", Name: "dev", Source: ""},
		}
		err := assertLockfileCoversAdapters(lf, workflowDir, ociAdapters)
		require.NoError(t, err)
	})

	t.Run("covered_ok", func(t *testing.T) {
		workflowDir := t.TempDir()
		ociAdapters := map[string]*workflowAdapter{
			"foo.inst": {Type: "foo", Name: "inst", Source: "reg/foo"},
		}
		err := assertLockfileCoversAdapters(lf, workflowDir, ociAdapters)
		require.NoError(t, err)
	})
}

// TestAutoPullPolicy exercises the policy resolver end-to-end. The global
// trust-config path is redirected at an empty temp dir via CRITERIA_STATE_DIR
// so the test does not depend on the developer's ~/.criteria/trust.hcl.
func TestAutoPullPolicy(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	workflowDir := t.TempDir()

	t.Run("allow_unsigned_off_mode", func(t *testing.T) {
		spec := &workflow.Spec{Header: &workflow.WorkflowHeaderSpec{Verification: "off"}}
		policy, err := autoPullPolicy(workflowDir, spec, true)
		require.NoError(t, err)
		// allowUnsigned forces ModeOff regardless of the workflow attribute.
		assert.Equal(t, signing.ModeOff, policy.Mode)
	})

	t.Run("strict_no_keys", func(t *testing.T) {
		spec := &workflow.Spec{Header: &workflow.WorkflowHeaderSpec{Verification: "strict"}}
		policy, err := autoPullPolicy(workflowDir, spec, false)
		require.NoError(t, err)
		assert.Equal(t, signing.ModeStrict, policy.Mode)
	})

	t.Run("invalid_mode_errors", func(t *testing.T) {
		spec := &workflow.Spec{Header: &workflow.WorkflowHeaderSpec{Verification: "bogus"}}
		_, err := autoPullPolicy(workflowDir, spec, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "signing policy")
	})

	t.Run("nil_header_defaults", func(t *testing.T) {
		// A nil header exercises the empty-verification default path.
		policy, err := autoPullPolicy(workflowDir, &workflow.Spec{}, true)
		require.NoError(t, err)
		assert.Equal(t, signing.ModeOff, policy.Mode)
	})
}

// TestHasOCIReferences covers all three branches: no adapters, adapters
// without a source, and adapters with a source.
func TestHasOCIReferences(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		assert.False(t, hasOCIReferences(&workflow.Spec{}))
	})
	t.Run("no_source", func(t *testing.T) {
		spec := &workflow.Spec{Adapters: []workflow.AdapterDeclSpec{
			{Type: "local", Name: "dev"},
		}}
		assert.False(t, hasOCIReferences(spec))
	})
	t.Run("with_source", func(t *testing.T) {
		spec := &workflow.Spec{Adapters: []workflow.AdapterDeclSpec{
			{Type: "local", Name: "dev"},
			{Type: "foo", Name: "inst", Source: "reg/foo"},
		}}
		assert.True(t, hasOCIReferences(spec))
	})
}

// TestListHCLFiles confirms that only .hcl workflow sources are returned, that
// the lockfile (criteria.lock.hcl) is excluded, and that subdirectories and
// non-HCL files are skipped.
func TestListHCLFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.hcl"), []byte(""), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.hcl"), []byte(""), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.md"), []byte(""), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, workflow.LockfileName), []byte(""), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "c.hcl"), []byte(""), 0o644))

	got, err := listHCLFiles(dir)
	require.NoError(t, err)
	sort.Strings(got)
	want := []string{filepath.Join(dir, "a.hcl"), filepath.Join(dir, "b.hcl")}
	assert.Equal(t, want, got)
}

// TestArtifactBinaryPath_Ambiguous covers the multi-file branch: when the
// artifact publishes more than one binary for the host platform and no valid
// manifest name selects one, the path is ambiguous and must error.
func TestArtifactBinaryPath_Ambiguous(t *testing.T) {
	dir := path.Join("bin", runtime.GOOS, runtime.GOARCH)
	mapFS := fstest.MapFS{
		path.Join(dir, "adapter-a"): {Data: []byte("a")},
		path.Join(dir, "adapter-b"): {Data: []byte("b")},
	}
	_, err := artifactBinaryPath(mapFS)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must select one")
}

// TestArtifactBinaryPath_NoneFound covers the empty-platform branch: a platform
// directory with no plain files errors with the published-platforms summary.
func TestArtifactBinaryPath_NoneFound(t *testing.T) {
	dir := path.Join("bin", runtime.GOOS, runtime.GOARCH)
	// Only a subdirectory, no plain binary file.
	mapFS := fstest.MapFS{
		path.Join(dir, "nested") + "/": {Mode: os.ModeDir},
	}
	_, err := artifactBinaryPath(mapFS)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no binary")
}

// TestArtifactPlatforms covers the "no bin/ at all" branch (returns "none")
// and the populated case.
func TestArtifactPlatforms(t *testing.T) {
	t.Run("no_bin", func(t *testing.T) {
		mapFS := fstest.MapFS{}
		assert.Equal(t, []string{"none"}, artifactPlatforms(mapFS))
	})
	t.Run("populated", func(t *testing.T) {
		mapFS := fstest.MapFS{
			"bin/linux/":              {Mode: os.ModeDir},
			"bin/linux/amd64/":        {Mode: os.ModeDir},
			"bin/linux/amd64/adapter": {Data: []byte("x")},
			"bin/darwin/":             {Mode: os.ModeDir},
			"bin/darwin/arm64/":       {Mode: os.ModeDir},
		}
		got := artifactPlatforms(mapFS)
		// Order is filesystem-read dependent; just assert both platforms show.
		assert.Contains(t, got, "linux/amd64")
		assert.Contains(t, got, "darwin/arm64")
	})
}

// TestEnsureAdapterCachedPullsMissingBlobByDigest verifies that the run-time
// auto-pull path pulls a missing adapter by its pinned digest, never by tag.
// A fake puller copies the artifact into the cache on pull and records the
// reference it was asked to resolve, so the test fails if the tag is left set.
func TestEnsureAdapterCachedPullsMissingBlobByDigest(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_ADAPTERS", t.TempDir())

	// Source layout holds the artifact; target layout is empty.
	sourceRoot := filepath.Join(t.TempDir(), "source")
	targetRoot := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.MkdirAll(sourceRoot, 0o755))
	require.NoError(t, os.MkdirAll(targetRoot, 0o755))

	sourceLayout, err := oci.Open(sourceRoot)
	require.NoError(t, err)
	targetLayout, err := oci.Open(targetRoot)
	require.NoError(t, err)

	binContent := []byte("#!/bin/sh\necho pulled\n")
	manifestDigest, _ := fakeArtifactFixture(t, sourceLayout, "noop", binContent)

	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{{
			Type:               "noop",
			Name:               "default",
			Reference:          "ghcr.io/brokenbots/criteria-adapter-noop:0.5.4",
			Version:            "0.5.4",
			ResolvedDigest:     manifestDigest.String(),
			SourceURL:          "https://github.com/brokenbots/criteria-adapter-noop",
			SDKProtocolVersion: 2,
		}},
	}

	wa := &workflowAdapter{Type: "noop", Name: "default", Source: "ghcr.io/brokenbots/criteria-adapter-noop", Version: "0.5.4"}
	policy := signing.Policy{Mode: signing.ModeOff}
	puller := &copyingFakePuller{
		layout: targetLayout,
		source: sourceLayout,
		manifests: map[string]digest.Digest{
			"ghcr.io/brokenbots/criteria-adapter-noop@" + manifestDigest.String(): manifestDigest,
		},
	}

	require.NoError(t, ensureAdapterCached(ctx, "noop.default", wa, lf, targetLayout, puller, &policy))

	require.Len(t, puller.calls, 1, "puller should have been called for the missing blob")
	called := puller.calls[0]
	assert.Empty(t, called.Tag, "pull must not use a mutable tag")
	assert.Equal(t, manifestDigest, called.Digest, "pull must use the pinned digest")
	assert.True(t, targetLayout.HasBlob(manifestDigest), "pulled manifest blob must now be present in target layout")
}

// TestEnsureAdapterCachedRejectsDigestMismatch verifies that if the registry
// returns a different digest than the one pinned in the lockfile, the pull is
// rejected rather than silently installing the wrong artifact.
func TestEnsureAdapterCachedRejectsDigestMismatch(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_ADAPTERS", t.TempDir())

	// The source layout only exists to provide a known manifest digest. The
	// target layout is empty so ensureAdapterCached takes the pull path.
	sourceRoot := filepath.Join(t.TempDir(), "source")
	targetRoot := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.MkdirAll(sourceRoot, 0o755))
	require.NoError(t, os.MkdirAll(targetRoot, 0o755))

	sourceLayout, err := oci.Open(sourceRoot)
	require.NoError(t, err)
	targetLayout, err := oci.Open(targetRoot)
	require.NoError(t, err)

	binContent := []byte("#!/bin/sh\necho locked\n")
	manifestDigest, _ := fakeArtifactFixture(t, sourceLayout, "noop", binContent)

	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{{
			Type:               "noop",
			Name:               "default",
			Reference:          "ghcr.io/brokenbots/criteria-adapter-noop:0.5.4",
			Version:            "0.5.4",
			ResolvedDigest:     manifestDigest.String(),
			SourceURL:          "https://github.com/brokenbots/criteria-adapter-noop",
			SDKProtocolVersion: 2,
		}},
	}

	wa := &workflowAdapter{Type: "noop", Name: "default", Source: "ghcr.io/brokenbots/criteria-adapter-noop", Version: "0.5.4"}
	policy := signing.Policy{Mode: signing.ModeOff}
	wrongDg := digest.FromString("wrong").String()
	puller := &badFakePuller{returned: digest.Digest(wrongDg)}

	err = ensureAdapterCached(ctx, "noop.default", wa, lf, targetLayout, puller, &policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "digest mismatch")

	require.Len(t, puller.calls, 1)
	called := puller.calls[0]
	assert.Empty(t, called.Tag, "pull must not use a mutable tag")
	assert.Equal(t, manifestDigest, called.Digest, "pull must use the pinned digest")
}

// copyingFakePuller is a test double that copies a pre-staged artifact from a
// source layout into a target layout when Pull is invoked. It records every
// reference it is asked to pull so tests can assert the digest/tag handling.
type copyingFakePuller struct {
	layout    *oci.Layout
	source    *oci.Layout
	manifests map[string]digest.Digest // ref.String() -> manifest digest
	calls     []oci.Reference
}

func (f *copyingFakePuller) Pull(ctx context.Context, ref oci.Reference) (digest.Digest, error) {
	f.calls = append(f.calls, ref)
	dg, ok := f.manifests[ref.String()]
	if !ok {
		return "", fmt.Errorf("copyingFakePuller: reference %s not staged", ref)
	}
	if err := copyArtifactBlobs(f.source, f.layout, dg); err != nil {
		return "", err
	}
	return dg, nil
}

// copyArtifactBlobs copies a manifest and all of its config/layer blobs from src
// to dst, and registers the manifest in dst's index.json. This is enough for the
// post-pull verification and extraction steps to succeed in tests.
func copyArtifactBlobs(src, dst *oci.Layout, dg digest.Digest) error {
	data, err := os.ReadFile(src.BlobPath(dg))
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", dg, err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse manifest %s: %w", dg, err)
	}

	for _, d := range append([]ocispec.Descriptor{manifest.Config}, manifest.Layers...) {
		if err := copyBlob(src, dst, d.Digest); err != nil {
			return err
		}
	}
	if err := copyBlob(src, dst, dg); err != nil {
		return err
	}

	ix, err := dst.Index()
	if err != nil {
		return err
	}
	ix.Manifests = append(ix.Manifests, ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    dg,
		Size:      int64(len(data)),
	})
	return dst.WriteIndex(ix)
}

func copyBlob(src, dst *oci.Layout, dg digest.Digest) error {
	r, err := os.Open(src.BlobPath(dg))
	if err != nil {
		return fmt.Errorf("open blob %s: %w", dg, err)
	}
	defer r.Close()
	return dst.WriteBlob(r, dg)
}

// badFakePuller returns a fixed digest regardless of the reference. It records
// the reference so tests can verify the digest/tag handling.
type badFakePuller struct {
	returned digest.Digest
	calls    []oci.Reference
}

func (f *badFakePuller) Pull(_ context.Context, ref oci.Reference) (digest.Digest, error) {
	f.calls = append(f.calls, ref)
	return f.returned, nil
}
