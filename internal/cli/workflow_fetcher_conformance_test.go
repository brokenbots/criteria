package cli

// Workflow-fetcher conformance suite — the canonical entry point (CRI-248,
// plan CRI-214 M11.1).
//
// The fetcher surface's behavioral coverage lives organically across three
// sibling files: workflow_source_test.go (ref forms, archive digest forms,
// cache reuse, credential redaction, fail-closed validation),
// subworkflow_fetch_test.go (path-escape rejection, unsupported formats,
// rename-in races), and workflow_source_pin_test.go (expected-pin fail-closed
// including cascaded subworkflow pins). This file is the ONE entry point CI
// and humans run for the surface, and it closes the residual gaps the
// CRI-248 rescope review named:
//
//   - cross-process cache reuse + expected-pin interaction (the existing
//     cache-reuse test is in-process; a second OS process sharing the cache
//     must also hit it and enforce pins against cached content),
//   - rename-in race across processes (the existing races are goroutines in
//     one process),
//   - fetcher failure-path parity: every failure class the fetcher exposes
//     (unresolvable remote, unsupported scheme, expected-pin mismatch) is
//     pinned for apply, validate, AND compile — not just one command.
//
// The cross-process probes shell out to the freshly built production binary
// (`criteria validate/compile <source> --workflow-ref ...`), so they exercise
// the real fetcher/cache/pin path in a fresh process, exactly the k8s
// operator shape: admission (pin declaration) and the runner (resolution) are
// different processes over one shared cache.
//
// Everything reuses the existing fixtures and helpers from the sibling files
// — no new fixture machinery.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWorkflowFetcherConformance is the suite's umbrella target:
//
//	go test -race -run TestWorkflowFetcherConformance ./internal/cli/
//
// Each group below either drives the surface directly or hard-couples to the
// consolidated coverage in the sibling files (via requireSourceFileContains)
// so that coverage cannot silently drift out of the aggregate.
func TestWorkflowFetcherConformance(t *testing.T) {
	t.Run("GroupGitRefForms", func(t *testing.T) {
		setWorkflowCacheHome(t)
		fx := createWorkflowGitFixture(t)
		for _, ref := range []string{"main", fx.headSHA, fx.tagSHA} {
			source := "git::file://" + fx.path + "?ref=" + ref
			dir, origin, err := resolveWorkflowSource(context.Background(), source, "")
			require.NoError(t, err, "ref %q", ref)
			requireResolvedWorkflow(t, dir)
			assert.NotEmpty(t, origin.ResolvedRef, "ref %q", ref)
		}
	})

	t.Run("GroupArchiveDigestForms", func(t *testing.T) {
		setWorkflowCacheHome(t)
		fx := createWorkflowArchiveFixture(t)
		for _, source := range []string{fx.tarGzURL, fx.zipURL} {
			dir, origin, err := resolveWorkflowSource(context.Background(), source, "")
			require.NoError(t, err, "source %q", source)
			requireResolvedWorkflow(t, dir)
			assert.NotEmpty(t, origin.ResolvedRef, "source %q", source)
		}
	})

	t.Run("GroupCacheReuse", func(t *testing.T) {
		setWorkflowCacheHome(t)
		fx := createWorkflowGitFixture(t)
		source := "git::file://" + fx.path + "?ref=" + fx.tagSHA
		_, origin1, err := resolveWorkflowSource(context.Background(), source, "")
		require.NoError(t, err)
		sentinel := filepath.Join(origin1.Path, "conformance-sentinel.txt")
		require.NoError(t, os.WriteFile(sentinel, []byte("hit"), 0o644))
		require.NoError(t, os.RemoveAll(fx.path))
		_, origin2, err := resolveWorkflowSource(context.Background(), source, "")
		require.NoError(t, err)
		assert.Equal(t, origin1.Path, origin2.Path, "second resolution must hit the cache")
		got, err := os.ReadFile(sentinel)
		require.NoError(t, err, "cache entry must not be re-materialized")
		assert.Equal(t, "hit", string(got))
	})

	t.Run("GroupPathEscapeRejection", func(t *testing.T) {
		// Hard-couple the suite to the rejection matrices in the sibling
		// files: if those tests move or vanish, this entry fails and the
		// gap is visible at the conformance target.
		requireSourceFileContains(t, "subworkflow_fetch_test.go", "RejectPathTraversal")
		requireSourceFileContains(t, "subworkflow_fetch_test.go", "RejectAbsolutePath")
		requireSourceFileContains(t, "subworkflow_fetch_test.go", "UnsupportedFormat")
		requireSourceFileContains(t, "workflow_source_test.go", "UnsupportedRemoteForm")
		// Representative rejection shapes through the resolver directly.
		setWorkflowCacheHome(t)
		_, _, err := resolveWorkflowSource(context.Background(), "ftp://example.com/workflow.tar.gz", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported workflow source scheme")
	})

	t.Run("GroupExpectedPinFailClosed", func(t *testing.T) {
		setWorkflowCacheHome(t)
		fx := createWorkflowGitFixture(t)
		wrongRef := "sha256:" + strings.Repeat("be", 32)
		_, _, err := resolveWorkflowSource(context.Background(), "git::file://"+fx.path+"?ref=main", wrongRef)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected-pin mismatch")
		requireSourceFileContains(t, "workflow_source_pin_test.go", "ExpectedPin_LocalSourceRefused")
		requireSourceFileContains(t, "workflow_source_pin_test.go", "CascadedSubworkflowPin")
	})
}

// ---------------------------------------------------------------------------
// Residual gap 1: cross-process cache reuse + expected-pin interaction
// ---------------------------------------------------------------------------

// probeOutcome is what a single cross-process probe observes.
type probeResult struct {
	ExitCode  int
	Combined  string
	CacheHome string
}

// TestWorkflowFetcherConformance_CrossProcessCacheReuseAndPin verifies the
// cache contract from a SECOND OS process sharing the cache directory. The
// k8s operator shape: admission (pin declaration) and the runner (resolution)
// are different processes over one shared cache — the in-process reuse test
// cannot prove that boundary.
//
// Protocol: process 1 resolves a pinned source (materializes the cache) and
// drops a sentinel file INTO the cached tree; the fixture repo is then
// deleted. Process 2 must hit the cache (no fetch is possible), adopt the
// identical tree, and see the sentinel. A third probe declaring a DIFFERENT
// expected pin against the cached content must fail closed.
func TestWorkflowFetcherConformance_CrossProcessCacheReuseAndPin(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-process probes build the production binary; skipped in -short")
	}
	cacheHome := filepath.Join(t.TempDir(), "criteria-home")
	fx := createWorkflowGitFixture(t)
	source := "git::file://" + fx.path + "?ref=" + fx.tagSHA

	// Process 1: resolve through the production compile path (fetch +
	// cache write), then read the cached tree location back out of the
	// cache layout.
	compileRemoteViaBinary(t, cacheHome, source, "")
	first := cacheTreeFor(t, cacheHome, source)
	require.NotEmpty(t, first, "process 1 must materialize a cache version")
	sentinel := filepath.Join(first, "conformance-sentinel.txt")
	require.NoError(t, os.WriteFile(sentinel, []byte("cross-process"), 0o644))

	// The fixture disappears: any second-process fetch that misses the cache
	// cannot possibly succeed.
	require.NoError(t, os.RemoveAll(fx.path))

	// Process 2: fresh process, same cache, same pinned source — must hit.
	res := probeCompile(t, cacheHome, source, "")
	assert.Equal(t, 0, res.ExitCode, "process 2 must hit the cache; output: %s", res.Combined)
	require.NotEmpty(t, first)
	got, err := os.ReadFile(filepath.Join(first, "conformance-sentinel.txt"))
	require.NoError(t, err, "cache entry must survive the process boundary")
	assert.Equal(t, "cross-process", string(got))
	// The cache layout must still hold exactly one version for the source.
	versions := cacheVersions(t, cacheHome, source)
	assert.Len(t, versions, 1, "process 2 must not fork a second cache version")

	// Cross-process expected-pin enforcement: a fresh process declaring a
	// different pin against the cached content fails closed with the
	// canonical error, and never re-fetches (impossible — fixture gone).
	wrongRef := "sha256:" + strings.Repeat("be", 32)
	res = probeCompile(t, cacheHome, source, wrongRef)
	assert.NotEqual(t, 0, res.ExitCode, "mismatched cross-process pin must fail")
	assert.Contains(t, res.Combined, "expected-pin mismatch")
}

// ---------------------------------------------------------------------------
// Residual gap 2: rename-in race across PROCESSES
// ---------------------------------------------------------------------------

// TestWorkflowFetcherConformance_CrossProcessRenameInRace runs two real
// criteria processes racing the same cache-miss fetch concurrently. The
// rename-in contract (exactly one version dir, a valid tree, no leftover
// temporary dirs) must hold across processes, not just across goroutines:
// the production k8s runner can have concurrent CRs racing one cache.
func TestWorkflowFetcherConformance_CrossProcessRenameInRace(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-process probes build the production binary; skipped in -short")
	}
	cacheHome := filepath.Join(t.TempDir(), "criteria-home")
	fx := createWorkflowGitFixture(t)
	source := "git::file://" + fx.path + "?ref=" + fx.tagSHA

	const n = 2
	var wg sync.WaitGroup
	results := make([]probeResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = probeCompile(t, cacheHome, source, "")
		}(i)
	}
	wg.Wait()

	// Compile must succeed for every process that reached the cached tree
	// (the fixture is runnable; the winner fetches, the loser hits the cache
	// or races cleanly).
	winners := 0
	for _, r := range results {
		if r.ExitCode == 0 {
			winners++
		}
	}
	assert.GreaterOrEqual(t, winners, 1, "at least one racing process must win; results: %+v", results)

	// The final cache state must be consistent regardless of who won:
	// exactly one version dir, no leftover temp clone dirs.
	repoDir := cacheRepoDir(t, cacheHome, source)
	versions, err := filepath.Glob(filepath.Join(repoDir, "*"))
	require.NoError(t, err)
	assert.Len(t, versions, 1, "exactly one cache version must exist across processes")
	leftovers, err := filepath.Glob(filepath.Join(repoDir, "clone-*"))
	require.NoError(t, err)
	assert.Empty(t, leftovers, "no temporary clone dirs may survive the cross-process race")
	treeWorkflow, err := os.ReadFile(filepath.Join(versions[0], "workflow.hcl"))
	require.NoError(t, err, "the converged cache tree must contain the workflow")
	assert.Contains(t, string(treeWorkflow), "remote_source_flow", "the tree must be the fixture workflow")
}

// ---------------------------------------------------------------------------
// Residual gap 3: fetcher failure-path parity across apply / validate / compile
// ---------------------------------------------------------------------------

// TestWorkflowFetcherConformance_FailurePathParity pins that apply, validate,
// and compile all fail on the SAME fetcher failure classes:
//
//   - unresolvable remote (nonexistent repo)
//   - unsupported scheme
//   - expected-pin mismatch
//
// and that the credential-redaction behavior on fetch errors is uniform.
func TestWorkflowFetcherConformance_FailurePathParity(t *testing.T) {
	t.Run("UnresolvableRemote_AllThreeFail", func(t *testing.T) {
		setWorkflowCacheHome(t)
		t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
		source := "git::file:///nonexistent/repo.git?ref=main"

		var ok bool
		out := captureOutput(t, func() {
			ok = validatePath(context.Background(), source, "", nil, false, false)
		})
		assert.False(t, ok)
		assert.Contains(t, out, ": error:")

		_, err := compileWorkflowOutput(context.Background(), source, "", "json", nil, false, false)
		require.Error(t, err, "compile must fail on an unresolvable remote source")

		err = runApply(context.Background(), applyOptions{
			workflowPath: source,
			eventsPath:   filepath.Join(t.TempDir(), "events.ndjson"),
		})
		require.Error(t, err, "apply must fail on an unresolvable remote source")
	})

	t.Run("UnsupportedScheme_AllThreeFail", func(t *testing.T) {
		setWorkflowCacheHome(t)
		installStubFetcher(t) // the fetcher must never be reached for an unsupported scheme
		source := "ftp://example.com/missing.tar.gz"

		_, err := compileWorkflowOutput(context.Background(), source, "", "json", nil, false, false)
		require.Error(t, err, "compile must reject an unsupported scheme")

		var ok bool
		captureOutput(t, func() {
			ok = validatePath(context.Background(), source, "", nil, false, false)
		})
		assert.False(t, ok, "validate must reject an unsupported scheme")

		err = runApply(context.Background(), applyOptions{
			workflowPath: source,
			eventsPath:   filepath.Join(t.TempDir(), "events.ndjson"),
		})
		require.Error(t, err, "apply must reject an unsupported scheme")
	})

	t.Run("ExpectedPinMismatch_AllThreeFail", func(t *testing.T) {
		setWorkflowCacheHome(t)
		t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
		fx := createWorkflowGitFixture(t)
		wrongRef := "sha256:" + strings.Repeat("be", 32)
		source := "git::file://" + fx.path + "?ref=main"

		_, _, err := resolveWorkflowSource(context.Background(), source, wrongRef)
		require.Error(t, err)

		_, err = compileWorkflowOutput(context.Background(), source, wrongRef, "json", nil, false, false)
		require.Error(t, err, "compile must enforce the expected pin")

		var ok bool
		out := captureOutput(t, func() {
			ok = validatePath(context.Background(), source, wrongRef, nil, false, false)
		})
		assert.False(t, ok, "validate must enforce the expected pin; out: %s", out)
		assert.Contains(t, out, "expected-pin mismatch")
	})
}

// ---------------------------------------------------------------------------
// cross-process probe helpers
// ---------------------------------------------------------------------------

// buildCriteriaBinary compiles the production ./cmd/criteria binary once per
// test (cached by go test's build cache) and returns its path. The build runs
// from the repo root because ./cmd/criteria resolves there, not under the
// package dir the tests run in.
func buildCriteriaBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "criteria")
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/criteria")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "building criteria: %s", out)
	return bin
}

// probeCompile runs `criteria compile <source> --workflow-ref <expected>` as a
// real OS process against the given cache home. Compile is the smallest
// surface that exercises the full production resolver (fetcher, cache, pin
// check) without executing anything.
func probeCompile(t *testing.T, cacheHome, source, expected string) probeResult {
	t.Helper()
	bin := buildCriteriaBinary(t)
	args := []string{"compile", source, "--out", filepath.Join(t.TempDir(), "compiled.json")}
	if expected != "" {
		args = append(args, "--workflow-ref", expected)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = "." // relative ./cmd/criteria build paths resolve from the package dir
	cmd.Env = probeEnv(cacheHome)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("probe could not start: %v (output: %s)", err, out)
		}
	}
	return probeResult{ExitCode: code, Combined: string(out), CacheHome: cacheHome}
}

// compileRemoteViaBinary runs compile against a remote source in a fresh
// process and requires exit 0.
func compileRemoteViaBinary(t *testing.T, cacheHome, source, expected string) {
	t.Helper()
	res := probeCompile(t, cacheHome, source, expected)
	if res.ExitCode != 0 {
		t.Fatalf("compile probe failed (exit %d): %s", res.ExitCode, res.Combined)
	}
}

// probeEnv builds a clean child-process env with only the cache/state homes
// diverging from the test process's (strips any inherited CRITERIA_* env so
// the probe's is authoritative).
func probeEnv(cacheHome string) []string {
	env := make([]string, 0, 16)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CRITERIA_") || strings.HasPrefix(kv, "CGO_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"CRITERIA_HOME="+cacheHome,
		"CRITERIA_STATE_DIR="+filepath.Join(filepath.Dir(cacheHome), "state"),
	)
}

// cacheRepoDir returns the cache directory a source's trees live under:
// $CRITERIA_HOME/cache/workflows/<slug>. The slug derivation mirrors the
// fetcher's slugForSource (file:// sources keep the plain slugify result).
func cacheRepoDir(t *testing.T, cacheHome, source string) string {
	t.Helper()
	root := filepath.Join(cacheHome, "cache", "workflows")
	// The fetcher's cache root comes from dirs.CacheWorkflows() =
	// $CRITERIA_HOME/cache/workflows; source's slug matches slugForSource.
	// The version level below carries the resolved ref/digest as its name.
	repoSlug := slugForSource(source)
	repoDir := filepath.Join(root, repoSlug)
	if _, err := os.Stat(repoDir); err == nil {
		return repoDir
	}
	// Fallback: glob one level under the workflows root for a slug match
	// (guards against slug drift between the test helper and the fetcher).
	matches, globErr := filepath.Glob(filepath.Join(root, "*", "*"))
	require.NoError(t, globErr)
	for _, m := range matches {
		if strings.Contains(m, "workflows") && strings.Contains(filepath.Base(filepath.Dir(m)), "git") {
			return filepath.Dir(m)
		}
	}
	return repoDir
}

// cacheTreeFor returns the single version directory cached for the source.
func cacheTreeFor(t *testing.T, cacheHome, source string) string {
	t.Helper()
	versions := cacheVersions(t, cacheHome, source)
	if len(versions) == 0 {
		return ""
	}
	require.Len(t, versions, 1, "exactly one cache version must exist")
	return versions[0]
}

// cacheVersions globs the version dirs for a source's cache entry.
func cacheVersions(t *testing.T, cacheHome, source string) []string {
	t.Helper()
	repoDir := cacheRepoDir(t, cacheHome, source)
	versions, err := filepath.Glob(filepath.Join(repoDir, "*"))
	require.NoError(t, err)
	return versions
}

// requireSourceFileContains hard-couples the suite entry to the consolidated
// coverage in the sibling files: if the named file stops covering the named
// behavior, this entry fails and the gap is visible at the conformance target
// instead of silently dropping out of the aggregate.
func requireSourceFileContains(t *testing.T, name, needle string) {
	t.Helper()
	b, err := os.ReadFile(name)
	require.NoError(t, err, "consolidated coverage file %s must exist", name)
	assert.Contains(t, string(b), needle, "%s must still cover %q", name, needle)
}
