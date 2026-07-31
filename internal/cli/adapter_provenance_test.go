package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// TestListInstalledShowsProvenance verifies that `criteria adapter list` prints
// the reference and source_url for cached adapters, and labels unattributed
// entries when annotations are absent.
func TestListInstalledShowsProvenance(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	cacheRoot, err := defaultCacheRoot()
	require.NoError(t, err)
	layout, err := oci.Open(cacheRoot)
	require.NoError(t, err)

	binContent := []byte("#!/bin/sh\necho test\n")
	manifestDigest, _ := fakeArtifactFixture(t, layout, "noop", binContent)

	require.NoError(t, layout.Annotate(manifestDigest, map[string]string{
		oci.AnnotationReference: "ghcr.io/brokenbots/criteria-adapter-noop:0.5.4",
		oci.AnnotationSourceURL: "https://github.com/brokenbots/criteria-adapter-noop",
	}))

	var out bytes.Buffer
	require.NoError(t, runList(&out, true, false))
	line := out.String()
	assert.Contains(t, line, manifestDigest.String())
	assert.Contains(t, line, "ghcr.io/brokenbots/criteria-adapter-noop:0.5.4")
	assert.Contains(t, line, "https://github.com/brokenbots/criteria-adapter-noop")
	assert.NotContains(t, line, "(unknown)")
}

// TestListInstalledUnattributedLabel verifies that cached entries without
// provenance annotations are explicitly labelled (unattributed) rather than blank.
func TestListInstalledUnattributedLabel(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	cacheRoot, err := defaultCacheRoot()
	require.NoError(t, err)
	layout, err := oci.Open(cacheRoot)
	require.NoError(t, err)

	binContent := []byte("#!/bin/sh\necho test\n")
	manifestDigest, _ := fakeArtifactFixture(t, layout, "unattributed", binContent)
	_ = manifestDigest

	var out bytes.Buffer
	require.NoError(t, runList(&out, true, false))
	line := out.String()
	assert.Contains(t, line, "(unattributed)")
}

// TestEnsureAdapterCachedPullsByDigest verifies that the run-time resolution
// path pulls the artifact by its pinned digest and annotates provenance. The
// artifact is already present in the layout, so no network call is needed.
func TestEnsureAdapterCachedPullsByDigest(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	cacheRoot, err := defaultCacheRoot()
	require.NoError(t, err)
	layout, err := oci.Open(cacheRoot)
	require.NoError(t, err)

	binContent := []byte("#!/bin/sh\necho locked\n")
	manifestDigest, _ := fakeArtifactFixture(t, layout, "noop", binContent)

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
	puller := &oci.Puller{Layout: layout}

	require.NoError(t, ensureAdapterCached(context.Background(), "noop.default", wa, lf, layout, puller, &policy))

	// The cached entry should now carry provenance annotations.
	ix, err := layout.Index()
	require.NoError(t, err)
	var found bool
	for _, m := range ix.Manifests {
		if m.Digest == manifestDigest {
			found = true
			assert.Equal(t, "ghcr.io/brokenbots/criteria-adapter-noop:0.5.4", m.Annotations[oci.AnnotationReference])
			assert.Equal(t, "https://github.com/brokenbots/criteria-adapter-noop", m.Annotations[oci.AnnotationSourceURL])
		}
	}
	require.True(t, found, "manifest not found in index")
}

// TestIncompleteLockfileEntryRejected verifies that lockfile entries missing a
// required provenance field are rejected before any pull or run.
func TestIncompleteLockfileEntryRejected(t *testing.T) {
	cases := []struct {
		name   string
		entry  lockfile.LockedAdapter
		wantIn string
	}{
		{
			name:   "missing reference",
			entry:  lockfile.LockedAdapter{Type: "noop", Name: "default", ResolvedDigest: "sha256:aa", SourceURL: "https://example.com"},
			wantIn: "incomplete",
		},
		{
			name:   "missing digest",
			entry:  lockfile.LockedAdapter{Type: "noop", Name: "default", Reference: "reg/noop:1", SourceURL: "https://example.com"},
			wantIn: "incomplete",
		},
		{
			name:   "missing source_url",
			entry:  lockfile.LockedAdapter{Type: "noop", Name: "default", Reference: "reg/noop:1", ResolvedDigest: "sha256:aa"},
			wantIn: "incomplete",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lf := &lockfile.Lockfile{Adapters: []lockfile.LockedAdapter{tc.entry}}
			ociAdapters := map[string]*workflowAdapter{
				"noop.default": {Type: "noop", Name: "default", Source: "reg/noop"},
			}
			workflowDir := t.TempDir()
			err := assertLockfileCoversAdapters(lf, workflowDir, ociAdapters)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantIn)
			assert.Contains(t, err.Error(), "noop.default")
		})
	}
}

// TestPruneUnattributedOnly verifies that `criteria adapter prune --unattributed-only`
// removes cached entries lacking provenance annotations while leaving attributed
// entries in place.
func TestPruneUnattributedOnly(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	cacheRoot, err := defaultCacheRoot()
	require.NoError(t, err)
	layout, err := oci.Open(cacheRoot)
	require.NoError(t, err)

	bin := []byte("#!/bin/sh\necho ok\n")
	attributedDG, _ := fakeArtifactFixture(t, layout, "attributed", bin)
	unattributedDG, _ := fakeArtifactFixture(t, layout, "unattributed", bin)

	require.NoError(t, layout.Annotate(attributedDG, map[string]string{
		oci.AnnotationReference: "ghcr.io/brokenbots/criteria-adapter-attributed:1.0.0",
		oci.AnnotationSourceURL: "https://github.com/brokenbots/criteria-adapter-attributed",
	}))

	require.NoError(t, runPrune("", 0, true, nil))

	assert.False(t, layout.HasBlob(unattributedDG), "unattributed blob should be pruned")
	assert.True(t, layout.HasBlob(attributedDG), "attributed blob should remain")
}

// TestCollectUnpinnedAdaptersNamesDirectoryAndAdapter verifies that the tree-wide
// coverage checker reports the workflow directory and adapter name for an
// unpinned adapter.
func TestCollectUnpinnedAdaptersNamesDirectoryAndAdapter(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	writeWorkflowHCL(t, root, `
workflow {
  name = "root"
  version = "0.1"
  initial_state = "done"
  target_state = "done"
}
adapter "noop" "root" {
  source = "ghcr.io/brokenbots/criteria-adapter-noop"
}
subworkflow "sub" {
  source = "./sub"
}
state "done" {
  terminal = true
}
`)
	writeWorkflowHCL(t, sub, `
workflow {
  name = "sub"
  version = "0.1"
  initial_state = "done"
  target_state = "done"
}
adapter "copilot" "sub" {
  source = "ghcr.io/brokenbots/criteria-adapter-copilot"
}
state "done" {
  terminal = true
}
`)

	// Lock only the root adapter.
	require.NoError(t, lockfile.Write(filepath.Join(root, workflow.LockfileName), &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{{
			Type:               "noop",
			Name:               "root",
			Reference:          "ghcr.io/brokenbots/criteria-adapter-noop:0.5.4",
			Version:            "0.5.4",
			ResolvedDigest:     "sha256:" + strings.Repeat("a", 64),
			SourceURL:          "https://github.com/brokenbots/criteria-adapter-noop",
			SDKProtocolVersion: 2,
		}},
	}))

	errs, err := collectUnpinnedAdapters(ctx, root)
	require.NoError(t, err)
	require.Len(t, errs, 1)
	msg := errs[0].Error()
	assert.Contains(t, msg, sub)
	assert.Contains(t, msg, "copilot.sub")
	assert.Contains(t, msg, "criteria adapter lock")
}

func writeWorkflowHCL(t *testing.T, dir, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflow.hcl"), []byte(content), 0o644))
}
