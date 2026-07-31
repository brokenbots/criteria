package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// writeRecursiveLockFixture builds a root workflow directory and a local-path
// subworkflow directory, each declaring a distinct OCI adapter. It returns the
// root directory.
func writeRecursiveLockFixture(t *testing.T) (root, sub string) {
	t.Helper()
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	rootDir := t.TempDir()
	subDir := filepath.Join(rootDir, "pair_programming_loop")
	require.NoError(t, os.MkdirAll(subDir, 0o755))

	rootWorkflow := `workflow {
  name    = "root"
  version = "0.1"
  initial_state = "start"
  target_state = "done"
}

adapter "copilot" "coordinator" {
  source  = "ghcr.io/brokenbots/criteria-adapter-copilot"
  version = "0.5.4"
}

subworkflow "pair_programming_loop" {
  source = "./pair_programming_loop"
}

step "start" {
  target = adapter.copilot.coordinator
  input {}
  outcome "success" { next = state.done }
}
`
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "workflow.hcl"), []byte(rootWorkflow), 0o600))

	subWorkflow := `workflow {
  name    = "pair"
  version = "0.1"
  initial_state = "start"
  target_state = "done"
}

adapter "copilot" "driver" {
  source  = "ghcr.io/brokenbots/criteria-adapter-copilot"
  version = "0.5.4"
}

step "start" {
  target = adapter.copilot.driver
  input {}
  outcome "success" { next = state.done }
}
`
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "workflow.hcl"), []byte(subWorkflow), 0o600))

	return rootDir, subDir
}

func TestRunLock_Recursive_LocksTree(t *testing.T) {
	ctx := context.Background()
	root, sub := writeRecursiveLockFixture(t)

	fake := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     digest.FromString("copilot:0.5.4").String(),
			SDKProtocolVersion: 2,
		})

	var out bytes.Buffer
	err := runLock(ctx, root, false, true, true, nil, &out, fake, nil)
	require.NoError(t, err)

	assert.Contains(t, out.String(), "copilot.coordinator")
	assert.Contains(t, out.String(), "copilot.driver")

	rootLF, err := lockfile.ReadFromDir(root)
	require.NoError(t, err)
	require.NotNil(t, rootLF)
	require.Len(t, rootLF.Adapters, 1)
	assert.Equal(t, "copilot", rootLF.Adapters[0].Type)
	assert.Equal(t, "coordinator", rootLF.Adapters[0].Name)
	assert.NotEmpty(t, rootLF.Adapters[0].ResolvedDigest)

	subLF, err := lockfile.ReadFromDir(sub)
	require.NoError(t, err)
	require.NotNil(t, subLF)
	require.Len(t, subLF.Adapters, 1)
	assert.Equal(t, "copilot", subLF.Adapters[0].Type)
	assert.Equal(t, "driver", subLF.Adapters[0].Name)
	assert.NotEmpty(t, subLF.Adapters[0].ResolvedDigest)
}

func TestRunLock_Recursive_RestoresDeletedSubworkflowLockfile(t *testing.T) {
	ctx := context.Background()
	root, sub := writeRecursiveLockFixture(t)

	fake := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     digest.FromString("copilot:0.5.4").String(),
			SDKProtocolVersion: 2,
		})

	require.NoError(t, runLock(ctx, root, false, true, true, nil, &bytes.Buffer{}, fake, nil))
	require.NoError(t, os.Remove(filepath.Join(sub, workflow.LockfileName)))

	var out bytes.Buffer
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &out, fake, nil))
	assert.Contains(t, out.String(), filepath.Base(sub))

	subLF, err := lockfile.ReadFromDir(sub)
	require.NoError(t, err)
	require.NotNil(t, subLF)
	require.Len(t, subLF.Adapters, 1)
}

func TestRunLock_Recursive_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	root, _ := writeRecursiveLockFixture(t)

	fake := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     digest.FromString("copilot:0.5.4").String(),
			SDKProtocolVersion: 2,
		})

	require.NoError(t, runLock(ctx, root, false, true, true, nil, &bytes.Buffer{}, fake, nil))

	var out bytes.Buffer
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &out, fake, nil))
	assert.Contains(t, out.String(), "lockfile tree up to date")
}

func TestRunLock_NoRecursive_KeepsSingleDirectoryBehaviour(t *testing.T) {
	ctx := context.Background()
	root, sub := writeRecursiveLockFixture(t)

	fake := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     digest.FromString("copilot:0.5.4").String(),
			SDKProtocolVersion: 2,
		})

	var out bytes.Buffer
	err := runLock(ctx, root, false, false, true, nil, &out, fake, nil)
	require.NoError(t, err)

	rootLF, err := lockfile.ReadFromDir(root)
	require.NoError(t, err)
	require.NotNil(t, rootLF)
	assert.Len(t, rootLF.Adapters, 1)

	_, err = lockfile.ReadFromDir(sub)
	require.NoError(t, err)
	// The subworkflow lockfile may be created if ReadFromDir returns nil? No,
	// ReadFromDir returns (nil,nil) when missing. We stat directly.
	_, statErr := os.Stat(filepath.Join(sub, workflow.LockfileName))
	require.True(t, os.IsNotExist(statErr), "--no-recursive must not create the subworkflow lockfile")
}

// captureStderr temporarily replaces os.Stderr with a pipe, runs fn, then
// restores os.Stderr and returns everything written to it. It must not be used
// concurrently with other goroutines that write to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w
	defer func() { os.Stderr = oldStderr }()

	fn()

	require.NoError(t, w.Close())
	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	return buf.String()
}

func TestValidate_FailsOnUnpinnedAdapterInTree(t *testing.T) {
	ctx := context.Background()
	root, sub := writeRecursiveLockFixture(t)

	// Lock recursively so the root is pinned, then delete the subworkflow
	// lockfile to simulate the confirmed-broken state.
	fake := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     digest.FromString("copilot:0.5.4").String(),
			SDKProtocolVersion: 2,
		})
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &bytes.Buffer{}, fake, nil))
	require.NoError(t, os.Remove(filepath.Join(sub, workflow.LockfileName)))

	workflowPath := filepath.Join(root, "workflow.hcl")
	out := captureStderr(t, func() {
		_ = validatePath(ctx, workflowPath, nil, false, false)
	})
	require.Contains(t, out, sub, "validate stderr must name the workflow directory")
	require.Contains(t, out, "copilot", "validate stderr must name the adapter")
	require.Contains(t, out, "criteria adapter lock", "validate stderr must state the fix command")
}

func TestApply_FailsOnUnpinnedAdapterInTree(t *testing.T) {
	ctx := context.Background()
	root, sub := writeRecursiveLockFixture(t)

	fake := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     digest.FromString("copilot:0.5.4").String(),
			SDKProtocolVersion: 2,
		})
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &bytes.Buffer{}, fake, nil))
	require.NoError(t, os.Remove(filepath.Join(sub, workflow.LockfileName)))

	workflowPath := filepath.Join(root, "workflow.hcl")
	var err error
	out := captureStderr(t, func() {
		_, _, _, err = compileForExecution(ctx, workflowPath, nil, false, false)
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unpinned")
	assert.Contains(t, err.Error(), root, "apply top-level error must name the workflow directory")
	require.Contains(t, out, sub, "apply stderr must name the workflow directory")
	require.Contains(t, out, "copilot", "apply stderr must name the adapter")
	require.Contains(t, out, "criteria adapter lock", "apply stderr must state the fix command")
}

// fakeWorkflowFetcher is a test double that materialises a pre-built remote
// workflow tree and returns a deterministic pin.
type fakeWorkflowFetcher struct {
	callers  map[string]string // source -> materialised directory
	resolved string            // resolved identifier to record in the lockfile
}

func (f *fakeWorkflowFetcher) Fetch(ctx context.Context, callerDir, source string) (string, *lockfile.LockedWorkflowRef, error) {
	dir, ok := f.callers[source]
	if !ok {
		return "", nil, fmt.Errorf("unknown source %q", source)
	}
	return dir, &lockfile.LockedWorkflowRef{Name: "", Source: source, ResolvedRef: f.resolved, Kind: "git"}, nil
}

// writeFetchedWorkflowFixture creates a root workflow that references a remote
// subworkflow source, plus a materialised remote directory in the caller's temp
// space. It returns the root directory, the remote directory, and the source
// string used in the parent.
func writeFetchedWorkflowFixture(t *testing.T, source string) (root, remoteDir string) {
	t.Helper()
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	root = t.TempDir()
	remoteDir = t.TempDir()

	rootWorkflow := `workflow {
  name    = "root"
  version = "0.1"
  initial_state = "start"
  target_state = "done"
}

adapter "copilot" "coordinator" {
  source  = "ghcr.io/brokenbots/criteria-adapter-copilot"
  version = "0.5.4"
}

subworkflow "remote" {
  source = "` + source + `"
}

state "start" {
  terminal = true
}
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"), []byte(rootWorkflow), 0o600))

	remoteWorkflow := `workflow {
  name    = "remote"
  version = "0.1"
  initial_state = "start"
  target_state = "done"
}

adapter "copilot" "driver" {
  source  = "ghcr.io/brokenbots/criteria-adapter-copilot"
  version = "0.5.4"
}

state "start" {
  terminal = true
}
`
	require.NoError(t, os.WriteFile(filepath.Join(remoteDir, "workflow.hcl"), []byte(remoteWorkflow), 0o600))

	return root, remoteDir
}

func TestRunLock_FetchedWorkflowCompleteLockfileAccepted(t *testing.T) {
	ctx := context.Background()
	source := "https://github.com/example/remote-workflow.git"
	root, remoteDir := writeFetchedWorkflowFixture(t, source)

	// Pre-populate a complete lockfile in the fetched tree.
	resolvedDg := digest.FromString("copilot:0.5.4").String()
	require.NoError(t, lockfile.Write(filepath.Join(remoteDir, workflow.LockfileName), &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{{
			Type: "copilot", Name: "driver",
			Reference: "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4",
			Version:   "0.5.4", ResolvedDigest: resolvedDg,
			SourceURL:          "https://github.com/brokenbots/criteria-adapter-copilot",
			SDKProtocolVersion: 2,
		}},
	}))

	fake := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     resolvedDg,
			SDKProtocolVersion: 2,
		})

	fetcher := &fakeWorkflowFetcher{callers: map[string]string{source: remoteDir}, resolved: "abc123"}

	var out bytes.Buffer
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &out, fake, fetcher))

	rootLF, err := lockfile.ReadFromDir(root)
	require.NoError(t, err)
	require.NotNil(t, rootLF)
	require.Len(t, rootLF.WorkflowRefs, 1)
	assert.Equal(t, source, rootLF.WorkflowRefs[0].Source)
	assert.Equal(t, "abc123", rootLF.WorkflowRefs[0].ResolvedRef)
}

func TestRunLock_FetchedWorkflowMissingLockfileFails(t *testing.T) {
	ctx := context.Background()
	source := "https://github.com/example/remote-workflow.git"
	root, remoteDir := writeFetchedWorkflowFixture(t, source)

	fake := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     digest.FromString("copilot:0.5.4").String(),
			SDKProtocolVersion: 2,
		})

	fetcher := &fakeWorkflowFetcher{callers: map[string]string{source: remoteDir}, resolved: "abc123"}

	var out bytes.Buffer
	err := runLock(ctx, root, false, true, true, nil, &out, fake, fetcher)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no lockfile")
	assert.Contains(t, err.Error(), source)
	assert.Contains(t, err.Error(), "criteria adapter lock")
}

func TestRunLock_FetchedWorkflowPartialLockfileFails(t *testing.T) {
	ctx := context.Background()
	source := "https://github.com/example/remote-workflow.git"
	root, remoteDir := writeFetchedWorkflowFixture(t, source)

	// Lockfile exists but does not cover the declared driver adapter.
	require.NoError(t, lockfile.Write(filepath.Join(remoteDir, workflow.LockfileName), &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters:      []lockfile.LockedAdapter{},
	}))

	fake := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     digest.FromString("copilot:0.5.4").String(),
			SDKProtocolVersion: 2,
		})

	fetcher := &fakeWorkflowFetcher{callers: map[string]string{source: remoteDir}, resolved: "abc123"}

	var out bytes.Buffer
	err := runLock(ctx, root, false, true, true, nil, &out, fake, fetcher)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unpinned")
	assert.Contains(t, err.Error(), "copilot.driver")
}

func TestRunLock_FetchedWorkflowCompleteLockfilePinsPreserved(t *testing.T) {
	ctx := context.Background()
	source := "https://github.com/example/remote-workflow.git"
	root, remoteDir := writeFetchedWorkflowFixture(t, source)

	// Pre-seed the fetched tree with a complete lockfile whose digest differs
	// from anything the resolver would return. The author's pins must win.
	authorDg := digest.FromString("author:pin").String()
	require.NoError(t, lockfile.Write(filepath.Join(remoteDir, workflow.LockfileName), &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{{
			Type: "copilot", Name: "driver",
			Reference: "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4",
			Version:   "0.5.4", ResolvedDigest: authorDg,
			SourceURL:          "https://github.com/brokenbots/criteria-adapter-copilot",
			SDKProtocolVersion: 2,
		}},
	}))
	before, err := os.ReadFile(filepath.Join(remoteDir, workflow.LockfileName))
	require.NoError(t, err)

	// The resolver would produce a different digest if it were called.
	resolverDg := digest.FromString("resolver:tag").String()
	fake := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     resolverDg,
			SDKProtocolVersion: 2,
		})

	fetcher := &fakeWorkflowFetcher{callers: map[string]string{source: remoteDir}, resolved: "abc123"}

	var out bytes.Buffer
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &out, fake, fetcher))

	rootLF, err := lockfile.ReadFromDir(root)
	require.NoError(t, err)
	require.Len(t, rootLF.WorkflowRefs, 1)
	assert.Equal(t, source, rootLF.WorkflowRefs[0].Source)
	assert.Equal(t, "abc123", rootLF.WorkflowRefs[0].ResolvedRef)

	after, err := os.ReadFile(filepath.Join(remoteDir, workflow.LockfileName))
	require.NoError(t, err)
	assert.Equal(t, before, after, "fetched workflow lockfile must be preserved byte-for-byte")
}

func TestRunLock_FetchedWorkflowStaleVersionConstraintFails(t *testing.T) {
	ctx := context.Background()
	source := "https://github.com/example/remote-workflow.git"
	root, remoteDir := writeFetchedWorkflowFixture(t, source)

	// The workflow declares version = "0.5.4" (exact) but the shipped lockfile
	// pins 0.6.0. Re-locking must reject the stale pin.
	require.NoError(t, lockfile.Write(filepath.Join(remoteDir, workflow.LockfileName), &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{{
			Type: "copilot", Name: "driver",
			Reference: "ghcr.io/brokenbots/criteria-adapter-copilot:0.6.0",
			Version:   "0.6.0", ResolvedDigest: digest.FromString("copilot:0.6.0").String(),
			SourceURL:          "https://github.com/brokenbots/criteria-adapter-copilot",
			SDKProtocolVersion: 2,
		}},
	}))

	fake := newFakeResolver().withTag("0.5.4").withTag("0.6.0")
	fetcher := &fakeWorkflowFetcher{callers: map[string]string{source: remoteDir}, resolved: "abc123"}

	err := runLock(ctx, root, false, true, true, nil, &bytes.Buffer{}, fake, fetcher)
	require.Error(t, err)
	assert.Contains(t, err.Error(), source)
	assert.Contains(t, err.Error(), "copilot.driver")
	assert.Contains(t, err.Error(), "version changed")
	assert.Contains(t, err.Error(), "criteria adapter lock")
}

func TestRunLock_FetchedWorkflowBranchRefPinned(t *testing.T) {
	ctx := context.Background()
	source := "https://github.com/example/remote-workflow.git?ref=main"
	root, remoteDir := writeFetchedWorkflowFixture(t, source)

	resolvedDg := digest.FromString("copilot:0.5.4").String()
	require.NoError(t, lockfile.Write(filepath.Join(remoteDir, workflow.LockfileName), &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{{
			Type: "copilot", Name: "driver",
			Reference: "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4",
			Version:   "0.5.4", ResolvedDigest: resolvedDg,
			SourceURL:          "https://github.com/brokenbots/criteria-adapter-copilot",
			SDKProtocolVersion: 2,
		}},
	}))

	fake := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     resolvedDg,
			SDKProtocolVersion: 2,
		})

	// The fetcher returns a fixed commit SHA for the mutable branch.
	fetcher := &fakeWorkflowFetcher{callers: map[string]string{source: remoteDir}, resolved: "deadbeef1234"}

	require.NoError(t, runLock(ctx, root, false, true, true, nil, &bytes.Buffer{}, fake, fetcher))

	rootLF, err := lockfile.ReadFromDir(root)
	require.NoError(t, err)
	require.Len(t, rootLF.WorkflowRefs, 1)
	assert.Equal(t, "deadbeef1234", rootLF.WorkflowRefs[0].ResolvedRef)

	// A second run re-uses the same pin; the fetcher should not re-resolve the branch.
	fetcher2 := &fakeWorkflowFetcher{callers: map[string]string{"https://github.com/example/remote-workflow.git?ref=deadbeef1234": remoteDir}, resolved: "different"}
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &bytes.Buffer{}, fake, fetcher2))

	rootLF, err = lockfile.ReadFromDir(root)
	require.NoError(t, err)
	require.Len(t, rootLF.WorkflowRefs, 1)
	assert.Equal(t, "deadbeef1234", rootLF.WorkflowRefs[0].ResolvedRef)
}

func TestRunLock_UpgradeRecursesAndAcceptsDriftUnderImmutablePins(t *testing.T) {
	ctx := context.Background()
	root, sub := writeRecursiveLockFixture(t)

	firstDg := digest.FromString("copilot:0.5.4:1").String()
	secondDg := digest.FromString("copilot:0.5.4:2").String()

	fake := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     firstDg,
			SDKProtocolVersion: 2,
		})

	require.NoError(t, runLock(ctx, root, false, true, true, nil, &bytes.Buffer{}, fake, nil))

	// Simulate registry drift: the tag now resolves to a new digest.
	fake2 := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     secondDg,
			SDKProtocolVersion: 2,
		})

	// Plain lock re-verifies the existing pin and rejects drift.
	var plainOut bytes.Buffer
	err := runLock(ctx, root, false, true, true, nil, &plainOut, fake2, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drift")

	// Upgrade re-evaluates constraints and accepts the new digest under the
	// immutable version pin 0.5.4.
	require.NoError(t, runLock(ctx, root, true, true, true, nil, &bytes.Buffer{}, fake2, nil))
	subLF, err := lockfile.ReadFromDir(sub)
	require.NoError(t, err)
	assert.Equal(t, secondDg, subLF.Adapters[0].ResolvedDigest)
}

// runPathFetcher builds a fetcher that materialises the remote workflow
// directory used by the run-path tests. It accepts both the original source and
// the source qualified by the resolved ref so re-fetch by SHA works.
func runPathFetcher(remoteDir, source string) workflowFetcher {
	const resolved = "abc123"
	keys := map[string]string{
		source:                      remoteDir,
		source + "?ref=" + resolved: remoteDir,
	}
	return &fakeWorkflowFetcher{callers: keys, resolved: resolved}
}

func TestCollectUnpinnedAdapters_FetchedWorkflowComplete(t *testing.T) {
	ctx := context.Background()
	source := "https://github.com/example/remote-workflow.git"
	root, remoteDir := writeFetchedWorkflowFixture(t, source)

	dg := digest.FromString("copilot:0.5.4").String()
	require.NoError(t, lockfile.Write(filepath.Join(remoteDir, workflow.LockfileName), &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{{
			Type: "copilot", Name: "driver",
			Reference: "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4",
			Version:   "0.5.4", ResolvedDigest: dg,
			SourceURL:          "https://github.com/brokenbots/criteria-adapter-copilot",
			SDKProtocolVersion: 2,
		}},
	}))
	require.NoError(t, lockfile.Write(filepath.Join(root, workflow.LockfileName), &lockfile.Lockfile{
		SchemaVersion: 1,
		WorkflowRefs: []lockfile.LockedWorkflowRef{{
			Source: source, ResolvedRef: "abc123", Kind: "git",
		}},
		Adapters: []lockfile.LockedAdapter{{
			Type: "copilot", Name: "coordinator",
			Reference: "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4",
			Version:   "0.5.4", ResolvedDigest: dg,
			SourceURL:          "https://github.com/brokenbots/criteria-adapter-copilot",
			SDKProtocolVersion: 2,
		}},
	}))

	old := newWorkflowFetcherFunc
	newWorkflowFetcherFunc = func() workflowFetcher { return runPathFetcher(remoteDir, source) }
	defer func() { newWorkflowFetcherFunc = old }()

	errs, err := collectUnpinnedAdapters(ctx, root)
	require.NoError(t, err)
	assert.Empty(t, errs)
}

func TestCollectUnpinnedAdapters_FetchedWorkflowMissingLockfile(t *testing.T) {
	ctx := context.Background()
	source := "https://github.com/example/remote-workflow.git"
	root, remoteDir := writeFetchedWorkflowFixture(t, source)

	require.NoError(t, lockfile.Write(filepath.Join(root, workflow.LockfileName), &lockfile.Lockfile{
		SchemaVersion: 1,
		WorkflowRefs: []lockfile.LockedWorkflowRef{{
			Source: source, ResolvedRef: "abc123", Kind: "git",
		}},
	}))

	old := newWorkflowFetcherFunc
	newWorkflowFetcherFunc = func() workflowFetcher { return runPathFetcher(remoteDir, source) }
	defer func() { newWorkflowFetcherFunc = old }()

	_, err := collectUnpinnedAdapters(ctx, root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), source)
	assert.Contains(t, err.Error(), "no lockfile")
	assert.Contains(t, err.Error(), "criteria adapter lock")
}

func TestCollectUnpinnedAdapters_FetchedWorkflowPartialLockfile(t *testing.T) {
	ctx := context.Background()
	source := "https://github.com/example/remote-workflow.git"
	root, remoteDir := writeFetchedWorkflowFixture(t, source)

	require.NoError(t, lockfile.Write(filepath.Join(remoteDir, workflow.LockfileName), &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters:      []lockfile.LockedAdapter{},
	}))
	require.NoError(t, lockfile.Write(filepath.Join(root, workflow.LockfileName), &lockfile.Lockfile{
		SchemaVersion: 1,
		WorkflowRefs: []lockfile.LockedWorkflowRef{{
			Source: source, ResolvedRef: "abc123", Kind: "git",
		}},
	}))

	old := newWorkflowFetcherFunc
	newWorkflowFetcherFunc = func() workflowFetcher { return runPathFetcher(remoteDir, source) }
	defer func() { newWorkflowFetcherFunc = old }()

	_, err := collectUnpinnedAdapters(ctx, root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), source)
	assert.Contains(t, err.Error(), "copilot.driver")
	assert.Contains(t, err.Error(), "criteria adapter lock")
}

func TestAutoPullCompileAdapters_FetchedWorkflowComplete(t *testing.T) {
	ctx := context.Background()
	source := "https://github.com/example/remote-workflow.git"
	root, remoteDir := writeFetchedWorkflowFixture(t, source)
	t.Setenv("CRITERIA_ADAPTERS", t.TempDir())

	cacheRoot, err := defaultCacheRoot()
	require.NoError(t, err)
	layout, err := oci.Open(cacheRoot)
	require.NoError(t, err)

	binContent := []byte("#!/bin/sh\necho remote\n")
	manifestDigest, _ := fakeArtifactFixture(t, layout, "copilot", binContent)

	resolvedDg := manifestDigest.String()
	fake := newFakeResolver().
		withTag("0.5.4").
		withEntry("ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4", &lockfile.LockedAdapter{
			ResolvedDigest:     resolvedDg,
			SDKProtocolVersion: 2,
		})

	// The fetched workflow ships a complete lockfile with the author's tested pins.
	require.NoError(t, lockfile.Write(filepath.Join(remoteDir, workflow.LockfileName), &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{{
			Type: "copilot", Name: "driver",
			Reference: "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.4",
			Version:   "0.5.4", ResolvedDigest: resolvedDg,
			SourceURL:          "https://github.com/brokenbots/criteria-adapter-copilot",
			SDKProtocolVersion: 2,
		}},
	}))

	fetcher := runPathFetcher(remoteDir, source)
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &bytes.Buffer{}, fake, fetcher))

	old := newWorkflowFetcherFunc
	newWorkflowFetcherFunc = func() workflowFetcher { return runPathFetcher(remoteDir, source) }
	defer func() { newWorkflowFetcherFunc = old }()

	// Sanity-check that the fetched lockfile is still present after runLock.
	remoteLF, err := lockfile.ReadFromDir(remoteDir)
	require.NoError(t, err)
	require.NotNil(t, remoteLF, "remote lockfile should exist before auto-pull")

	spec, diags := workflow.ParseDir(root)
	require.False(t, diags.HasErrors())
	pinSet, err := loadTreePinSet(ctx, root)
	require.NoError(t, err)
	require.NoError(t, autoPullCompileAdapters(ctx, root, spec, pinSet, true))
}
