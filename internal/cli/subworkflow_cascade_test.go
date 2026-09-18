package cli

// M4.1 (CRI-227): remote subworkflow cascade during graph compilation.
//
// Subworkflow sources with remote schemes resolve through the same fetcher
// (defaultWorkflowFetcher) used for top-level workflow sources, recursively
// during compilation. Cascaded resolutions nest under the cascade root's slug
// (cache/workflows/<root-slug>/subworkflows/<child-slug>/<version>) so cycle
// detection on resolved cache paths stays deterministic; top-level fetches
// keep the flat layout (ADR-0005 D4). The resolved pin of every cascaded
// source is returned to the compiler, where CRI-226's expected-pin machinery
// enforces it fail-closed, and the engine compatibility constraint is
// rechecked for each resolved subworkflow.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/dirs"
)

// gitFileSource renders a workflowGitFixture as a git::file:// source.
func gitFileSource(fx workflowGitFixture) string {
	return "git::file://" + fx.path + "?ref=main"
}

// cascadeWorkflowHCL builds a workflow named name that declares one
// subworkflow "inner" pointing at source, with the operator-declared expected
// ref (refAttr may be empty), and a step that enters it.
func cascadeWorkflowHCL(name, source, ref string) string {
	refAttr := ""
	if ref != "" {
		refAttr = "  ref    = \"" + ref + "\"\n"
	}
	return "workflow {\n" +
		"  name = \"" + name + "\"\n" +
		"  version = \"0.1\"\n" +
		"  initial_state = \"run_inner\"\n" +
		"  target_state  = \"done\"\n" +
		"}\n\n" +
		"subworkflow \"inner\" {\n" +
		"  source = \"" + source + "\"\n" +
		refAttr +
		"}\n\n" +
		"step \"run_inner\" {\n" +
		"  target = subworkflow.inner\n" +
		"  outcome \"success\" { next = step.done }\n" +
		"}\n\n" +
		"state \"done\" {\n" +
		"  terminal = true\n" +
		"  success  = true\n" +
		"}\n"
}

// rewriteGitFixtureMain pushes a second commit to the fixture's main branch
// with the given workflow content (used to close cycles between fixtures
// whose URLs are only known after creation).
func rewriteGitFixtureMain(t *testing.T, fx workflowGitFixture, content string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "rewrite")
	require.NoError(t, os.MkdirAll(src, 0o755))
	runGit(t, src, "clone", "--quiet", fx.path, src)
	require.NoError(t, os.WriteFile(filepath.Join(src, "workflow.hcl"), []byte(content), 0o644))
	runGit(t, src, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-a", "-m", "rewrite")
	runGit(t, src, "push", "--quiet", "origin", "HEAD:main")
}

// relFromCacheRoot returns caller-relative path parts of dir below cacheRoot.
func relFromCacheRoot(t *testing.T, cacheRoot, dir string) string {
	t.Helper()
	rel, err := filepath.Rel(cacheRoot, dir)
	require.NoError(t, err)
	return rel
}

func TestCompile_RemoteCascade_NestedCacheLayout(t *testing.T) {
	setWorkflowCacheHome(t)
	cacheRoot := cascadeCacheRoot(t)
	child := createPinGitFixture(t, pinCalleeWorkflowHCL)
	parent := createPinGitFixture(t, cascadeWorkflowHCL("cascade_parent", gitFileSource(child), ""))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"),
		[]byte(cascadeWorkflowHCL("cascade_root", gitFileSource(parent), "")), 0o644))

	_, graph, err := parseCompileForCli(context.Background(), root, nil, false, false)
	require.NoError(t, err, "depth-2 remote cascade must compile")

	parentSW := graph.Subworkflows["inner"]
	require.NotNil(t, parentSW)
	// Top-level caller (the local root workflow) keeps the flat layout.
	assert.Equal(t,
		filepath.Join(parentSlugFor(t, parent.path), parent.headSHA),
		relFromCacheRoot(t, cacheRoot, parentSW.SourcePath),
		"parent fetched from a local caller stays flat under its own slug")

	childSW := parentSW.Body.Subworkflows["inner"]
	require.NotNil(t, childSW)
	// Cascaded caller (the fetched parent tree) nests under the parent slug.
	assert.Equal(t,
		filepath.Join(parentSlugFor(t, parent.path), "subworkflows", parentSlugFor(t, child.path), child.headSHA),
		relFromCacheRoot(t, cacheRoot, childSW.SourcePath),
		"child fetched from inside the cache nests under the parent slug")
	assert.Equal(t, child.headSHA, childSW.SourcePath[strings.LastIndex(childSW.SourcePath, "/")+1:])

	// The parent and child bodies are wired into the graph: the parent enters
	// its own subworkflow step, the leaf callee starts terminal.
	assert.Equal(t, "run_inner", parentSW.BodyEntry)
	assert.Equal(t, "done", childSW.BodyEntry)
}

// cascadeCacheRoot returns the cache/workflows dir for the current
// CRITERIA_HOME, mirroring dirs.CacheWorkflows.
func cascadeCacheRoot(t *testing.T) string {
	t.Helper()
	home, err := dirs.Home()
	require.NoError(t, err)
	return filepath.Join(home, "cache", "workflows")
}

// parentSlugFor derives the cache slug for a fixture repo path, mirroring
// slugForSource applied to the repo URL the fetcher derives from the
// git::file:// source form (splitGitSource strips the git:: force prefix).
func parentSlugFor(t *testing.T, repoPath string) string {
	t.Helper()
	return slugForSource("file://" + repoPath)
}

func TestCompile_RemoteCascade_ArchiveChild_NestedCacheLayout(t *testing.T) {
	setWorkflowCacheHome(t)
	cacheRoot := cascadeCacheRoot(t)
	child := createPinArchiveFixture(t, pinCalleeWorkflowHCL)
	parent := createPinGitFixture(t, cascadeWorkflowHCL("archive_parent", child.tarGzURL, child.digest))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"),
		[]byte(cascadeWorkflowHCL("archive_root", gitFileSource(parent), "")), 0o644))

	_, graph, err := parseCompileForCli(context.Background(), root, nil, false, false)
	require.NoError(t, err, "git parent cascading an archive child must compile")

	parentSW := graph.Subworkflows["inner"]
	require.NotNil(t, parentSW)
	childSW := parentSW.Body.Subworkflows["inner"]
	require.NotNil(t, childSW)
	assert.Equal(t,
		filepath.Join(parentSlugFor(t, parent.path), "subworkflows", slugForSource(child.tarGzURL), child.digest),
		relFromCacheRoot(t, cacheRoot, childSW.SourcePath),
		"archive child nests under the parent slug with its digest directory")
}

func TestCompile_RemoteCascade_CycleDetected(t *testing.T) {
	setWorkflowCacheHome(t)
	// A declares B; B is then rewritten to declare A. The follow-up commit to
	// A's main closes the cycle.
	a := createPinGitFixture(t, cascadeWorkflowHCL("cascade_a", "https://placeholder.invalid/b.git", ""))
	b := createPinGitFixture(t, cascadeWorkflowHCL("cascade_b", gitFileSource(a), ""))
	rewriteGitFixtureMain(t, a, cascadeWorkflowHCL("cascade_a", gitFileSource(b), ""))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"),
		[]byte(cascadeWorkflowHCL("cascade_root", gitFileSource(a), "")), 0o644))

	_, _, err := parseCompileForCli(context.Background(), root, nil, false, false)
	require.Error(t, err, "remote subworkflow cycle must be rejected at compile time")
	assert.Contains(t, err.Error(), "subworkflow cycle detected")
	assert.Contains(t, err.Error(), " -> ", "the diagnostic must show the cycle chain")
}

func TestCompile_RemoteCascade_SelfCycle(t *testing.T) {
	setWorkflowCacheHome(t)
	a := createPinGitFixture(t, cascadeWorkflowHCL("cascade_self", "https://placeholder.invalid/a.git", ""))
	rewriteGitFixtureMain(t, a, cascadeWorkflowHCL("cascade_self", gitFileSource(a), ""))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"),
		[]byte(cascadeWorkflowHCL("cascade_root", gitFileSource(a), "")), 0o644))

	_, _, err := parseCompileForCli(context.Background(), root, nil, false, false)
	require.Error(t, err, "a self-referential remote subworkflow must be rejected")
	assert.Contains(t, err.Error(), "subworkflow cycle detected")
}

func TestCompile_RemoteCascade_PinEnforcement(t *testing.T) {
	t.Run("matching_refs_compile", func(t *testing.T) {
		setWorkflowCacheHome(t)
		child := createPinGitFixture(t, pinCalleeWorkflowHCL)
		parent := createPinGitFixture(t, cascadeWorkflowHCL("pin_parent", gitFileSource(child), child.headSHA))

		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"),
			[]byte(cascadeWorkflowHCL("pin_root", gitFileSource(parent), parent.headSHA)), 0o644))

		_, graph, err := parseCompileForCli(context.Background(), root, nil, false, false)
		require.NoError(t, err, "declared refs matching the resolved pins must compile")
		require.NotNil(t, graph.Subworkflows["inner"])
		require.NotNil(t, graph.Subworkflows["inner"].Body.Subworkflows["inner"])
	})

	t.Run("root_level_mismatch_fails_closed", func(t *testing.T) {
		setWorkflowCacheHome(t)
		child := createPinGitFixture(t, pinCalleeWorkflowHCL)
		parent := createPinGitFixture(t, cascadeWorkflowHCL("pin_parent", gitFileSource(child), child.headSHA))

		root := t.TempDir()
		expected := strings.Repeat("cd", 20)
		require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"),
			[]byte(cascadeWorkflowHCL("pin_root", gitFileSource(parent), expected)), 0o644))

		_, _, err := parseCompileForCli(context.Background(), root, nil, false, false)
		require.Error(t, err, "an expected-pin mismatch on a cascaded source must fail closed at compile time")
		assert.Contains(t, err.Error(), "expected-pin mismatch")
		assert.Contains(t, err.Error(), parent.headSHA, "error must name the resolved ref")
		assert.Contains(t, err.Error(), expected, "error must name the expected ref")
	})

	t.Run("child_level_mismatch_fails_closed", func(t *testing.T) {
		setWorkflowCacheHome(t)
		child := createPinGitFixture(t, pinCalleeWorkflowHCL)
		expected := strings.Repeat("ab", 20)
		parent := createPinGitFixture(t, cascadeWorkflowHCL("pin_parent", gitFileSource(child), expected))

		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"),
			[]byte(cascadeWorkflowHCL("pin_root", gitFileSource(parent), parent.headSHA)), 0o644))

		_, _, err := parseCompileForCli(context.Background(), root, nil, false, false)
		require.Error(t, err, "an expected-pin mismatch inside the cascade must fail closed")
		assert.Contains(t, err.Error(), "expected-pin mismatch")
		assert.Contains(t, err.Error(), child.headSHA)
		assert.Contains(t, err.Error(), expected)
	})
}

func TestCompile_RemoteCascade_EngineCompatRecheck(t *testing.T) {
	t.Setenv("CRITERIA_OVERRIDE_VERSION", "0.5.8")
	setWorkflowCacheHome(t)
	child := createPinGitFixture(t, incompatibleChildWorkflowHCL)
	parent := createPinGitFixture(t, cascadeWorkflowHCL("compat_parent", gitFileSource(child), ""))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"),
		[]byte(cascadeWorkflowHCL("compat_root", gitFileSource(parent), "")), 0o644))

	_, _, err := parseCompileForCli(context.Background(), root, nil, false, false)
	require.Error(t, err, "an incompatible engine constraint on a cascaded subworkflow must fail")
	assert.Contains(t, err.Error(), "requires Criteria >=99.0.0")
	assert.Contains(t, err.Error(), "running engine is v0.5.8")
}

// incompatibleChildWorkflowHCL is a runnable callee that requires an engine
// far newer than the override version used in the compat test.
const incompatibleChildWorkflowHCL = `
workflow {
  name             = "compat_callee"
  version          = "0.1"
  criteria_version = ">=99.0.0"
  initial_state    = "done"
  target_state     = "done"
}

state "done" {
  terminal = true
  success  = true
}
`

func TestCompile_RemoteCascade_CachedHit(t *testing.T) {
	setWorkflowCacheHome(t)
	child := createPinGitFixture(t, pinCalleeWorkflowHCL)

	// Simulate a fetched parent tree inside the cache as the cascaded caller.
	callerDir := filepath.Join(cascadeCacheRoot(t), "cached-parent", "0123456789abcdef0123456789abcdef01234567")
	require.NoError(t, os.MkdirAll(callerDir, 0o755))

	f := newWorkflowFetcherFunc()
	dir1, pin1, err := f.Fetch(context.Background(), callerDir, gitFileSource(child))
	require.NoError(t, err)
	require.NotNil(t, pin1)
	assert.Equal(t, child.headSHA, pin1.ResolvedRef)

	// A second fetch from the same cascaded caller resolves to the same
	// nested directory (warm cache hit; no re-materialization).
	dir2, pin2, err := f.Fetch(context.Background(), callerDir, gitFileSource(child))
	require.NoError(t, err)
	assert.Equal(t, dir1, dir2)
	assert.Equal(t, pin1.ResolvedRef, pin2.ResolvedRef)
	assert.Equal(t,
		filepath.Join("cached-parent", "subworkflows", parentSlugFor(t, child.path), child.headSHA),
		relFromCacheRoot(t, cascadeCacheRoot(t), dir2))
}

func TestCascadeSlugDir(t *testing.T) {
	cacheRoot := "/data/cache/workflows"
	f := &defaultWorkflowFetcher{cacheRoot: cacheRoot}

	tests := []struct {
		name      string
		callerDir string
		slug      string
		want      string
	}{
		{"caller outside cache stays flat", "/workflows/root", "child", "/data/cache/workflows/child"},
		{"caller at cache root stays flat", cacheRoot, "child", "/data/cache/workflows/child"},
		{"fetched caller nests under its slug", cacheRoot + "/parent-slug/abc123", "child", cacheRoot + "/parent-slug/subworkflows/child"},
		{"nested caller stays under root slug", cacheRoot + "/parent-slug/subworkflows/mid/def456", "child", cacheRoot + "/parent-slug/subworkflows/child"},
		{"prefix collision outside cache stays flat", "/data/cache/workflows-extra/abc", "child", "/data/cache/workflows/child"},
		{"escaping caller stays flat", cacheRoot + "/../evil/abc", "child", "/data/cache/workflows/child"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, f.cascadeSlugDir(tt.callerDir, tt.slug))
		})
	}
}
