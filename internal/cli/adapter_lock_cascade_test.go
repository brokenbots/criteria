package cli

// M4.2 (CRI-228): recursive remote lock for cascaded workflows (ADR-0005 D4).
//
// `criteria adapter lock --recursive` pins remote workflow refs for the whole
// cascade into the parent's .criteria.lock.hcl: each fetched child contributes
// its direct pin plus, transitively, every pin recorded in the child's own
// lockfile, renamed with the declaring chain ("inner/inner"). Frozen fetched
// trees keep their authored pins byte-for-byte; they only contribute. The
// startup gate already merges the tree's lockfiles into one PinSet, so the
// propagated pins are consumed at startup. The lock diff reports workflow_ref
// changes (added/removed/changed) with sources redacted.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

// createCascadeGitFixture builds a bare repository whose main branch holds the
// given workflow content and lockfile content. Unlike createPinGitFixture it
// allows authored workflow_ref pins, which is what a fetched cascade child
// ships so its own nested pins can be propagated to the parent.
func createCascadeGitFixture(t *testing.T, workflowContent, lockContent string) workflowGitFixture {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(src, 0o755))
	runGit(t, src, "init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(src, "workflow.hcl"), []byte(workflowContent), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, lockfile.LockfileName), []byte(lockContent), 0o644))
	runGit(t, src, "add", ".")
	runGit(t, src, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-m", "initial")

	bare := filepath.Join(t.TempDir(), "cascade-fixture.git")
	runGit(t, src, "clone", "--bare", "--quiet", src, bare)
	return workflowGitFixture{
		path:    bare,
		headSHA: runGitBare(t, bare, "rev-parse", "main"),
	}
}

// workflowRefPinHCL renders a workflow_ref lockfile block.
func workflowRefPinHCL(name, source, resolvedRef, kind string) string {
	return "workflow_ref \"" + name + "\" {\n" +
		"  source = \"" + source + "\"\n" +
		"  resolved_ref = \"" + resolvedRef + "\"\n" +
		"  kind = \"" + kind + "\"\n" +
		"}\n"
}

// writeCascadeRootDir creates a plain workflow directory that declares one
// subworkflow "inner" pointing at source. It starts without a lockfile.
func writeCascadeRootDir(t *testing.T, source string) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"),
		[]byte(cascadeWorkflowHCL("cascade_root", source, "")), 0o644))
	return root
}

func findWorkflowRef(lf *lockfile.Lockfile, name string) *lockfile.LockedWorkflowRef {
	if lf == nil {
		return nil
	}
	for i := range lf.WorkflowRefs {
		if lf.WorkflowRefs[i].Name == name {
			return &lf.WorkflowRefs[i]
		}
	}
	return nil
}

func readLockfileFrom(t *testing.T, dir string) *lockfile.Lockfile {
	t.Helper()
	lf, err := lockfile.ReadFromDir(dir)
	require.NoError(t, err)
	return lf
}

// writeDepthTwoCascade builds the real-git depth-2 cascade used by the
// recursive-lock tests: leaf callee B (no adapters, empty lockfile), middle A
// declaring B with B's pin authored in A's lockfile, and a lockless root
// declaring A. It returns the root directory and both fixtures.
func writeDepthTwoCascade(t *testing.T) (root string, middle, child workflowGitFixture) {
	t.Helper()
	child = createCascadeGitFixture(t, pinCalleeWorkflowHCL, "schema_version = 1\n")
	childPinHCL := "schema_version = 1\n" + workflowRefPinHCL("inner", gitFileSource(child), child.headSHA, "git")
	middle = createCascadeGitFixture(t, cascadeWorkflowHCL("cascade_middle", gitFileSource(child), ""), childPinHCL)
	return writeCascadeRootDir(t, gitFileSource(middle)), middle, child
}

// fetchedTopLevelDir returns the flat cache directory where a top-level fetch
// of the fixture materialises: cache/workflows/<slug>/<resolved>.
func fetchedTopLevelDir(t *testing.T, fx workflowGitFixture) string {
	t.Helper()
	return filepath.Join(cascadeCacheRoot(t), parentSlugFor(t, fx.path), fx.headSHA)
}

func readRawLockfileBytes(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, lockfile.LockfileName))
	require.NoError(t, err)
	return string(b)
}

func TestRunLock_RecursiveCascade_DepthTwoPinsParentLockfile(t *testing.T) {
	setWorkflowCacheHome(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	ctx := context.Background()

	root, middle, child := writeDepthTwoCascade(t)

	var out bytes.Buffer
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &out, nil, nil))

	rootLF := readLockfileFrom(t, root)
	require.NotNil(t, rootLF)

	// Direct pin for the fetched middle workflow, named after the declaring
	// subworkflow.
	direct := findWorkflowRef(rootLF, "inner")
	require.NotNil(t, direct, "recursive lock must record a pin for the fetched child")
	assert.Equal(t, "inner", direct.Name)
	assert.Equal(t, gitFileSource(middle), direct.Source)
	assert.Equal(t, middle.headSHA, direct.ResolvedRef)
	assert.Equal(t, "git", direct.Kind)

	// Propagated depth-2 pin from the child's own lockfile.
	propagated := findWorkflowRef(rootLF, "inner/inner")
	require.NotNil(t, propagated, "recursive lock must propagate the child's authored pin to the parent lockfile")
	assert.Equal(t, "inner/inner", propagated.Name)
	assert.Equal(t, gitFileSource(child), propagated.Source)
	assert.Equal(t, child.headSHA, propagated.ResolvedRef)
	assert.Equal(t, "git", propagated.Kind)
	assert.Empty(t, rootLF.Adapters)

	// The fetched child's lockfile is frozen: it must not be rewritten.
	middleLockfileBytes := readRawLockfileBytes(t, fetchedTopLevelDir(t, middle))

	// Re-locking is idempotent: all pins already recorded, nothing rewritten.
	var second bytes.Buffer
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &second, nil, nil))
	assert.Contains(t, second.String(), "lockfile tree up to date")
	assert.Equal(t, middleLockfileBytes, readRawLockfileBytes(t, fetchedTopLevelDir(t, middle)),
		"fetched child lockfile must stay byte-identical")
}

// fakeCascadeFetcher maps sources to pre-materialised directories and returns
// a per-source pin, standing in for remote fetches without any git or cache
// interaction.
type fakeCascadeFetcher struct {
	callers map[string]string                     // source -> materialised directory
	pins    map[string]lockfile.LockedWorkflowRef // source -> resolved pin
}

func (f *fakeCascadeFetcher) Fetch(_ context.Context, _, source string) (string, *lockfile.LockedWorkflowRef, error) {
	dir, ok := f.callers[source]
	if !ok {
		return "", nil, os.ErrNotExist
	}
	pin, ok := f.pins[source]
	if !ok {
		return "", nil, os.ErrNotExist
	}
	pinCopy := pin
	return dir, &pinCopy, nil
}

func TestRunLock_RecursiveCascade_FakeFetcherDepthTwo(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	ctx := context.Background()

	const sourceChild = "git::https://fixtures.example/inner?ref=main"
	const sourceMiddle = "git::https://fixtures.example/middle?ref=main"

	// Middle workflow ships an authored pin for its child (frozen tree).
	middleDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(middleDir, "workflow.hcl"),
		[]byte(cascadeWorkflowHCL("cascade_middle", sourceChild, "")), 0o644))
	middleLock := "schema_version = 1\n" + workflowRefPinHCL("inner", sourceChild, "shaB", "git")
	require.NoError(t, os.WriteFile(filepath.Join(middleDir, lockfile.LockfileName), []byte(middleLock), 0o644))

	childDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "workflow.hcl"), []byte(pinCalleeWorkflowHCL), 0o644))

	fetcher := &fakeCascadeFetcher{
		callers: map[string]string{
			sourceMiddle: middleDir,
			sourceChild:  childDir,
		},
		pins: map[string]lockfile.LockedWorkflowRef{
			sourceMiddle: {Source: sourceMiddle, ResolvedRef: "shaA", Kind: "git"},
			sourceChild:  {Source: sourceChild, ResolvedRef: "shaB", Kind: "git"},
		},
	}

	// The root declares the middle workflow under a different name than the
	// middle's own pin label, so the propagated rename ("remote/inner") is
	// actually exercised.
	rootWorkflow := "workflow {\n" +
		"  name = \"cascade_root\"\n" +
		"  version = \"0.1\"\n" +
		"  initial_state = \"run_inner\"\n" +
		"  target_state  = \"done\"\n" +
		"}\n\n" +
		"subworkflow \"remote\" {\n" +
		"  source = \"" + sourceMiddle + "\"\n" +
		"}\n\n" +
		"step \"run_inner\" {\n" +
		"  target = subworkflow.remote\n" +
		"  outcome \"success\" { next = step.done }\n" +
		"}\n\n" +
		"state \"done\" {\n" +
		"  terminal = true\n" +
		"  success  = true\n" +
		"}\n"
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"), []byte(rootWorkflow), 0o644))

	var out bytes.Buffer
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &out, nil, fetcher))

	rootLF := readLockfileFrom(t, root)
	direct := findWorkflowRef(rootLF, "remote")
	require.NotNil(t, direct, "the direct pin must be named after the declaring subworkflow, not left empty")
	assert.Equal(t, "remote", direct.Name)
	assert.Equal(t, sourceMiddle, direct.Source)
	assert.Equal(t, "shaA", direct.ResolvedRef)

	propagated := findWorkflowRef(rootLF, "remote/inner")
	require.NotNil(t, propagated, "the fetched child's authored pin must be propagated and renamed")
	assert.Equal(t, "remote/inner", propagated.Name)
	assert.Equal(t, sourceChild, propagated.Source)
	assert.Equal(t, "shaB", propagated.ResolvedRef)
}

func TestCompile_RecursiveCascadePinAcceptedByStartupGate(t *testing.T) {
	setWorkflowCacheHome(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	ctx := context.Background()

	root, middle, child := writeDepthTwoCascade(t)

	var out bytes.Buffer
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &out, nil, nil))

	// The startup gate (apply path): the tree's lockfiles — including the
	// root's propagated pins — merge into the PinSet consumed by the engine.
	_, graph, loader, err := compileForExecution(ctx, root, nil, false, false)
	require.NoError(t, err)
	if loader != nil {
		_ = loader.Shutdown(ctx)
	}
	require.NotNil(t, graph)

	pinSet := graph.PinSet
	require.NotNil(t, pinSet, "compile must produce a merged PinSet")

	gateDirect := findWorkflowRef(pinSet, "inner")
	require.NotNil(t, gateDirect)
	assert.Equal(t, gitFileSource(middle), gateDirect.Source)
	assert.Equal(t, middle.headSHA, gateDirect.ResolvedRef)

	gatePropagated := findWorkflowRef(pinSet, "inner/inner")
	require.NotNil(t, gatePropagated, "the propagated depth-2 pin must be part of the merged PinSet")
	assert.Equal(t, gitFileSource(child), gatePropagated.Source)
	assert.Equal(t, child.headSHA, gatePropagated.ResolvedRef)

	// Verification against CRI-227's nested layout: the fetched middle nests
	// at cache/workflows/<middle-slug>/<sha>, its child under
	// <middle-slug>/subworkflows/<child-slug>/<sha>.
	parentSW := graph.Subworkflows["inner"]
	require.NotNil(t, parentSW)
	cacheRoot := cascadeCacheRoot(t)
	assert.Equal(t,
		filepath.Join(cacheRoot, parentSlugFor(t, middle.path), middle.headSHA),
		parentSW.SourcePath)
	childSW := parentSW.Body.Subworkflows["inner"]
	require.NotNil(t, childSW)
	assert.Equal(t,
		filepath.Join(cacheRoot, parentSlugFor(t, middle.path), "subworkflows", parentSlugFor(t, child.path), child.headSHA),
		childSW.SourcePath)
}

func TestRunLock_RecursiveCascade_LockDiffReportsWorkflowRefChange(t *testing.T) {
	setWorkflowCacheHome(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	ctx := context.Background()

	root, middle, _ := writeDepthTwoCascade(t)

	var first bytes.Buffer
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &first, nil, nil))
	assert.NotContains(t, first.String(), "workflow ref changed")

	// Point the declared source at the same repo by commit SHA: the diff key
	// (subworkflow name) is unchanged but the pin's source moves, which the
	// diff must report as WorkflowRefChanged.
	pinnedSource := "git::file://" + middle.path + "?ref=" + middle.headSHA
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"),
		[]byte(cascadeWorkflowHCL("cascade_root", pinnedSource, "")), 0o644))

	var second bytes.Buffer
	require.NoError(t, runLock(ctx, root, false, true, true, nil, &second, nil, nil))
	output := second.String()
	assert.Contains(t, output, "~ workflow_ref.inner workflow ref changed:")
	assert.Contains(t, output, gitFileSource(middle), "the diff must show the previous source (redacted)")

	rootLF := readLockfileFrom(t, root)
	updated := findWorkflowRef(rootLF, "inner")
	require.NotNil(t, updated)
	assert.Equal(t, pinnedSource, updated.Source)
	assert.Equal(t, middle.headSHA, updated.ResolvedRef)

	// The propagated depth-2 pin is unchanged and must not be reported.
	assert.NotContains(t, output, "workflow_ref.inner/inner")
}

func TestPrintLockDiff_WorkflowRefChanged(t *testing.T) {
	const secretSource = "git::https://user:secret@old.example/moved?ref=v1"
	oldLF := &lockfile.Lockfile{
		SchemaVersion: 1,
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "gone", Source: "git::https://old.example/gone?ref=v1", ResolvedRef: "aaa", Kind: "git"},
			{Name: "moved", Source: secretSource, ResolvedRef: "111", Kind: "git"},
			{Name: "stay", Source: "git::https://old.example/stay?ref=v1", ResolvedRef: "333", Kind: "git"},
		},
	}
	newLF := &lockfile.Lockfile{
		SchemaVersion: 1,
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "moved", Source: secretSource, ResolvedRef: "222", Kind: "git"},
			{Name: "fresh", Source: "git::https://new.example/fresh?ref=v2", ResolvedRef: "bbb", Kind: "git"},
			{Name: "stay", Source: "git::https://old.example/stay?ref=v1", ResolvedRef: "333", Kind: "git"},
		},
	}

	var out bytes.Buffer
	printLockDiff(oldLF, newLF, &out, 0, "/workflows/root")

	line := out.String()
	assert.Contains(t, line, "- workflow_ref.gone (stale)")
	assert.Contains(t, line, "+ workflow_ref.fresh")
	assert.Contains(t, line, "~ workflow_ref.moved workflow ref changed:")
	assert.Contains(t, line, "111")
	assert.Contains(t, line, "222")
	assert.NotContains(t, line, "secret", "lock diff output must redact credentials embedded in sources")
	assert.False(t, strings.Contains(line, "workflow_ref.stay"), "unchanged pins must not appear in the diff")
}

func TestPrintLockDiff_WorkflowRefUpToDate(t *testing.T) {
	lf := &lockfile.Lockfile{
		SchemaVersion: 1,
		WorkflowRefs: []lockfile.LockedWorkflowRef{
			{Name: "stay", Source: "git::https://old.example/stay?ref=v1", ResolvedRef: "333", Kind: "git"},
		},
	}

	var out bytes.Buffer
	printLockDiff(lf, lf, &out, 0, "/workflows/root")

	assert.Equal(t, "lockfile up to date, 0 adapter(s)\n", out.String())
}
