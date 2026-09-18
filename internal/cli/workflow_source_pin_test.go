package cli

// workflow_source_pin_test.go — CRI-226: caller-declared expected pins
// ("ref") on workflow sources must be enforced at resolve time, before any
// execution. Covers top-level sources (resolveWorkflowSource), cascaded
// subworkflow sources (compile-time enforcement through the fetching
// resolver), and both pin kinds: git commit SHA and archive sha256 digest.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

// pinCalleeWorkflowHCL is a runnable callee workflow that needs no adapter:
// it starts in its terminal success state.
const pinCalleeWorkflowHCL = `
workflow {
  name = "pin_callee"
  version = "0.1"
  initial_state = "done"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}
`

// ---------------------------------------------------------------------------
// Fixtures: git and archive sources carrying custom callee content
// ---------------------------------------------------------------------------

// createPinGitFixture builds a bare repository whose main branch holds the
// given workflow content and returns the source paths/resolved refs. The
// callee ships an empty adapter lockfile because fetched workflow trees must
// carry one to pass the adapter-pin checks at run setup.
func createPinGitFixture(t *testing.T, content string) workflowGitFixture {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(src, 0o755))
	runGit(t, src, "init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(src, "workflow.hcl"), []byte(content), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, lockfile.LockfileName), []byte("schema_version = 1\n"), 0o644))
	runGit(t, src, "add", ".")
	runGit(t, src, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-m", "initial")

	bare := filepath.Join(t.TempDir(), "pin-fixture.git")
	runGit(t, src, "clone", "--bare", "--quiet", src, bare)
	return workflowGitFixture{
		path:    bare,
		headSHA: runGitBare(t, bare, "rev-parse", "main"),
	}
}

type pinArchiveFixture struct {
	tarGzURL string
	digest   string // sha256:<hex> of the served archive bytes
}

// createPinArchiveFixture serves a workflow archive with the given content
// over an httptest server and returns its source URL and sha256 digest ref.
func createPinArchiveFixture(t *testing.T, content string) pinArchiveFixture {
	t.Helper()
	tarBody := buildTarGz(t, map[string][]byte{
		"workflow.hcl":        []byte(content),
		lockfile.LockfileName: []byte("schema_version = 1\n"),
	})
	sum := sha256.Sum256(tarBody)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarBody)
	}))
	t.Cleanup(srv.Close)
	return pinArchiveFixture{
		tarGzURL: srv.URL + "/callee.tar.gz",
		digest:   "sha256:" + hex.EncodeToString(sum[:]),
	}
}

// assertNoRunEvents asserts a run never started: the events file must not
// exist, or must contain no RunStarted/RunCompleted envelopes.
func assertNoRunEvents(t *testing.T, eventsPath string) {
	t.Helper()
	b, err := os.ReadFile(eventsPath)
	if os.IsNotExist(err) {
		return
	}
	require.NoError(t, err)
	for _, line := range splitNDJSONLines(string(b)) {
		assert.NotContains(t, line, "RunStarted", "a pin mismatch must fail before the run starts")
		assert.NotContains(t, line, "RunCompleted", "a pin mismatch must fail before the run starts")
	}
}

// ---------------------------------------------------------------------------
// Top-level sources: resolveWorkflowSource enforces the declared pin
// ---------------------------------------------------------------------------

func TestResolveWorkflowSource_ExpectedPin_Git(t *testing.T) {
	setWorkflowCacheHome(t)
	fx := createWorkflowGitFixture(t)

	// Match: expected equals the resolved SHA; resolution proceeds.
	_, origin, err := resolveWorkflowSource(context.Background(), "git::file://"+fx.path+"?ref=main", fx.headSHA)
	require.NoError(t, err)
	require.NotNil(t, origin)
	assert.Equal(t, fx.headSHA, origin.ResolvedRef)

	// Mismatch: declared pin disagrees with the resolved SHA.
	wrongRef := "sha256:" + strings.Repeat("be", 32)
	_, _, err = resolveWorkflowSource(context.Background(), "git::file://"+fx.path+"?ref=main", wrongRef)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected-pin mismatch")
	assert.Contains(t, err.Error(), fx.headSHA, "error must name the resolved ref")
	assert.Contains(t, err.Error(), wrongRef, "error must name the expected ref")
}

func TestResolveWorkflowSource_ExpectedPin_Archive(t *testing.T) {
	setWorkflowCacheHome(t)
	fx := createWorkflowArchiveFixture(t)

	// Match on the tar.gz digest.
	_, origin, err := resolveWorkflowSource(context.Background(), fx.tarGzURL, fx.tarGzRef)
	require.NoError(t, err)
	require.NotNil(t, origin)
	assert.Equal(t, fx.tarGzRef, origin.ResolvedRef)

	// Match on the zip digest.
	_, _, err = resolveWorkflowSource(context.Background(), fx.zipURL, fx.zipRef)
	require.NoError(t, err)

	// Mismatch: declared digest disagrees with the archive bytes.
	wrongDigest := "sha256:" + strings.Repeat("ab", 32)
	_, _, err = resolveWorkflowSource(context.Background(), fx.tarGzURL, wrongDigest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected-pin mismatch")
	assert.Contains(t, err.Error(), fx.tarGzRef, "error must name the resolved digest")
	assert.Contains(t, err.Error(), wrongDigest, "error must name the expected digest")
}

func TestResolveWorkflowSource_ExpectedPin_LocalSourceRefused(t *testing.T) {
	installStubFetcher(t) // a local source must never reach the fetcher
	fx := createWorkflowGitFixture(t)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflow.hcl"), []byte(runnableWorkflowHCL), 0o644))

	// A declared pin cannot be verified for a local source: fail closed.
	_, _, err := resolveWorkflowSource(context.Background(), dir, fx.headSHA)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local")
	assert.Contains(t, err.Error(), fx.headSHA, "error must name the declared pin")

	// A whitespace-only pin is treated as absent: unchanged behavior.
	_, _, err = resolveWorkflowSource(context.Background(), dir, " \t ")
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Run-time subworkflow resolver: fetchingSubWorkflowResolver
// ---------------------------------------------------------------------------

func TestFetchingSubWorkflowResolver_RemoteReturnsFetcherPin(t *testing.T) {
	fake := &fakeWorkflowFetcher{
		callers:  map[string]string{"https://example/flow.tar.gz": "/materialised"},
		resolved: "sha256:cafe",
	}
	resolver := &fetchingSubWorkflowResolver{fetcher: fake}

	dir, pin, err := resolver.ResolveSource(context.Background(), t.TempDir(), "https://example/flow.tar.gz")
	require.NoError(t, err)
	assert.Equal(t, "/materialised", dir)
	require.NotNil(t, pin, "the fetcher pin must be handed to the compiler for pin enforcement (CRI-226)")
	assert.Equal(t, "sha256:cafe", pin.ResolvedRef)
}

func TestFetchingSubWorkflowResolver_RemoteRequiresPin(t *testing.T) {
	// A remote source that resolves without a pin cannot be verified against
	// a declared ref, so the resolver refuses it rather than dropping the pin.
	resolver := &fetchingSubWorkflowResolver{
		fetcher: nilPinFetcher{},
	}
	_, _, err := resolver.ResolveSource(context.Background(), t.TempDir(), "https://example/flow.tar.gz")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolved without a pin")
}

// nilPinFetcher materialises a remote tree but resolves without a pin.
type nilPinFetcher struct{}

func (nilPinFetcher) Fetch(_ context.Context, _, _ string) (string, *lockfile.LockedWorkflowRef, error) {
	return "/materialised", nil, nil
}

func TestFetchingSubWorkflowResolver_FetchErrorPropagates(t *testing.T) {
	resolver := &fetchingSubWorkflowResolver{
		fetcher: &fakeWorkflowFetcher{callers: map[string]string{}},
	}
	_, _, err := resolver.ResolveSource(context.Background(), t.TempDir(), "https://example/flow.tar.gz")
	require.Error(t, err)
}

func TestFetchingSubWorkflowResolver_NoFetcherConfigured(t *testing.T) {
	resolver := &fetchingSubWorkflowResolver{}
	_, _, err := resolver.ResolveSource(context.Background(), t.TempDir(), "https://example/flow.tar.gz")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no fetcher configured")
}

func TestFetchingSubWorkflowResolver_LocalDelegatesAndSkipsFetcher(t *testing.T) {
	stub := installStubFetcher(t)
	root := t.TempDir()
	inner := filepath.Join(root, "inner")
	require.NoError(t, os.MkdirAll(inner, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(inner, "workflow.hcl"), []byte(pinCalleeWorkflowHCL), 0o644))

	resolver := newFetchingSubWorkflowResolver([]string{root})
	dir, pin, err := resolver.ResolveSource(context.Background(), root, "./inner")
	require.NoError(t, err)
	assert.Equal(t, inner, dir)
	assert.Nil(t, pin, "local sources carry no pin")
	assert.Empty(t, stub.fetchCalls(), "local sources must not reach the fetcher")

	// AllowedRoots are still enforced for local delegation.
	_, _, err = resolver.ResolveSource(context.Background(), t.TempDir(), "./inner")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allowed root")
}

// ---------------------------------------------------------------------------
// End to end: cascaded subworkflow pins through runApply
// ---------------------------------------------------------------------------

// cascadedPinParentHCL builds a parent workflow that cascades a remote
// subworkflow with an operator-declared expected ref ("" = no ref attr).
func cascadedPinParentHCL(source, ref string) string {
	refAttr := ""
	if ref != "" {
		refAttr = fmt.Sprintf("  ref    = %q\n", ref)
	}
	return "workflow {\n" +
		"  name = \"pin_cascade\"\n" +
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

func writeCascadedParentDir(t *testing.T, source, ref string) string {
	t.Helper()
	parent := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(parent, "workflow.hcl"), []byte(cascadedPinParentHCL(source, ref)), 0o644))
	return parent
}

func TestApply_CascadedSubworkflowPin_Git(t *testing.T) {
	setWorkflowCacheHome(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fx := createPinGitFixture(t, pinCalleeWorkflowHCL)
	source := "git::file://" + fx.path + "?ref=main"

	// Match: the declared ref equals the resolved commit SHA and the run proceeds.
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	require.NoError(t, runApply(context.Background(), applyOptions{
		workflowPath: writeCascadedParentDir(t, source, fx.headSHA),
		eventsPath:   eventsFile,
	}))
	types, err := readPayloadTypes(eventsFile)
	require.NoError(t, err)
	assert.Equal(t, 1, countPayloadType(types, "RunStarted"))
	assert.Equal(t, 1, countPayloadType(types, "RunCompleted"))

	// Mismatch: the run fails closed before execution.
	mismatchEvents := filepath.Join(t.TempDir(), "events.ndjson")
	err = runApply(context.Background(), applyOptions{
		workflowPath: writeCascadedParentDir(t, source, "sha256:"+strings.Repeat("cd", 32)),
		eventsPath:   mismatchEvents,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected-pin mismatch")
	assert.Contains(t, err.Error(), fx.headSHA, "error must name the resolved ref")
	assert.Contains(t, err.Error(), "cdcd", "error must name the expected ref")
	assertNoRunEvents(t, mismatchEvents)
}

func TestApply_CascadedSubworkflowPin_Archive(t *testing.T) {
	setWorkflowCacheHome(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fx := createPinArchiveFixture(t, pinCalleeWorkflowHCL)

	// Match: the declared digest equals the archive's sha256 digest.
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	require.NoError(t, runApply(context.Background(), applyOptions{
		workflowPath: writeCascadedParentDir(t, fx.tarGzURL, fx.digest),
		eventsPath:   eventsFile,
	}))
	types, err := readPayloadTypes(eventsFile)
	require.NoError(t, err)
	assert.Equal(t, 1, countPayloadType(types, "RunStarted"))
	assert.Equal(t, 1, countPayloadType(types, "RunCompleted"))

	// Mismatch on the digest: the run fails closed before execution.
	wrongDigest := "sha256:" + strings.Repeat("ab", 32)
	mismatchEvents := filepath.Join(t.TempDir(), "events.ndjson")
	err = runApply(context.Background(), applyOptions{
		workflowPath: writeCascadedParentDir(t, fx.tarGzURL, wrongDigest),
		eventsPath:   mismatchEvents,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected-pin mismatch")
	assert.Contains(t, err.Error(), fx.digest, "error must name the resolved digest")
	assert.Contains(t, err.Error(), wrongDigest, "error must name the expected digest")
	assertNoRunEvents(t, mismatchEvents)
}

// TestApply_CascadedSubworkflowPin_AbsentRefUnchanged pins the pin-absent
// contract end to end: a remote cascaded subworkflow with no declared ref
// resolves and runs exactly as before CRI-226.
func TestApply_CascadedSubworkflowPin_AbsentRefUnchanged(t *testing.T) {
	setWorkflowCacheHome(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fx := createPinGitFixture(t, pinCalleeWorkflowHCL)
	source := "git::file://" + fx.path + "?ref=main"

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	require.NoError(t, runApply(context.Background(), applyOptions{
		workflowPath: writeCascadedParentDir(t, source, ""),
		eventsPath:   eventsFile,
	}))
	types, err := readPayloadTypes(eventsFile)
	require.NoError(t, err)
	assert.Equal(t, 1, countPayloadType(types, "RunStarted"))
	assert.Equal(t, 1, countPayloadType(types, "RunCompleted"))
}

// ---------------------------------------------------------------------------
// End to end: top-level source pin through runApply (--workflow-ref)
// ---------------------------------------------------------------------------

func TestApply_TopLevelPin_Git(t *testing.T) {
	setWorkflowCacheHome(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fx := createWorkflowGitFixture(t)
	source := "git::file://" + fx.path + "?ref=main"

	// Match: the run proceeds.
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	require.NoError(t, runApply(context.Background(), applyOptions{
		workflowPath: source,
		workflowRef:  fx.headSHA,
		eventsPath:   eventsFile,
	}))
	types, err := readPayloadTypes(eventsFile)
	require.NoError(t, err)
	assert.Equal(t, 1, countPayloadType(types, "RunStarted"))
	assert.Equal(t, 1, countPayloadType(types, "RunCompleted"))

	// Mismatch: the run fails closed before execution.
	wrongRef := "sha256:" + strings.Repeat("be", 32)
	mismatchEvents := filepath.Join(t.TempDir(), "events.ndjson")
	err = runApply(context.Background(), applyOptions{
		workflowPath: source,
		workflowRef:  wrongRef,
		eventsPath:   mismatchEvents,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected-pin mismatch")
	assert.Contains(t, err.Error(), fx.headSHA)
	assert.Contains(t, err.Error(), wrongRef)
	assertNoRunEvents(t, mismatchEvents)
}

// TestApply_TopLevelPin_AbsentUnchanged pins the pin-absent contract for the
// top level: no --workflow-ref, unchanged behavior.
func TestApply_TopLevelPin_AbsentUnchanged(t *testing.T) {
	setWorkflowCacheHome(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fx := createWorkflowGitFixture(t)
	source := "git::file://" + fx.path + "?ref=main"

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	require.NoError(t, runApply(context.Background(), applyOptions{
		workflowPath: source,
		eventsPath:   eventsFile,
	}))
	types, err := readPayloadTypes(eventsFile)
	require.NoError(t, err)
	assert.Equal(t, 1, countPayloadType(types, "RunStarted"))
	assert.Equal(t, 1, countPayloadType(types, "RunCompleted"))
}
