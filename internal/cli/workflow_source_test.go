package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

// runnableWorkflowHCL is a workflow that compiles, validates, and executes
// end to end using the noop adapter binary built by TestMain (see
// apply_test.go for the same shape).
const runnableWorkflowHCL = `
workflow {
  name = "remote_source_flow"
  version = "0.1"
  initial_state = "run_adapter"
  target_state  = "done"
}

adapter "noop" "demo" {
  config {
    bootstrap = "true"
  }
}

step "run_adapter" {
  target = adapter.noop.demo
  input {
    prompt = "hello"
  }
  outcome "success" { next = step.done }
  outcome "failure" { next = step.failed }
}

state "done" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`

// stubFetcher records Fetch calls and always fails, proving local sources
// never reach the fetcher.
type stubFetcher struct {
	mu    sync.Mutex
	calls []string
}

func (s *stubFetcher) Fetch(_ context.Context, _, source string) (string, *lockfile.LockedWorkflowRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, source)
	return "", nil, errors.New("local sources must not reach the workflow fetcher")
}

func (s *stubFetcher) fetchCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// installStubFetcher replaces the workflow fetcher factory for the duration of
// the test so any fetch attempt fails loudly.
func installStubFetcher(t *testing.T) *stubFetcher {
	t.Helper()
	stub := &stubFetcher{}
	prev := newWorkflowFetcherFunc
	newWorkflowFetcherFunc = func() workflowFetcher { return stub }
	t.Cleanup(func() { newWorkflowFetcherFunc = prev })
	return stub
}

// setWorkflowCacheHome isolates the real workflow cache under CRITERIA_HOME.
func setWorkflowCacheHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "criteria-home")
	t.Setenv("CRITERIA_HOME", home)
	return home
}

type workflowGitFixture struct {
	path    string // bare repo path usable as file:///<path> or scp-style source
	headSHA string
	tagSHA  string
}

// createWorkflowGitFixture builds a bare repository whose main branch holds a
// runnable workflow and tags the commit v1.
func createWorkflowGitFixture(t *testing.T) workflowGitFixture {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(src, 0o755))
	runGit(t, src, "init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(src, "workflow.hcl"), []byte(runnableWorkflowHCL), 0o644))
	runGit(t, src, "add", ".")
	runGit(t, src, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-m", "initial")

	bare := filepath.Join(t.TempDir(), "fixture.git")
	runGit(t, src, "clone", "--bare", "--quiet", src, bare)
	runGitBare(t, bare, "tag", "v1", "main")

	return workflowGitFixture{
		path:    bare,
		headSHA: runGitBare(t, bare, "rev-parse", "main"),
		tagSHA:  runGitBare(t, bare, "rev-parse", "v1"),
	}
}

type archiveFixture struct {
	tarGzURL string
	zipURL   string
	tarGzRef string
	zipRef   string
}

// createWorkflowArchiveFixture serves runnable workflow archives (tar.gz and
// zip) over an httptest server and returns their source URLs and sha256
// digest refs.
func createWorkflowArchiveFixture(t *testing.T) archiveFixture {
	t.Helper()
	entries := map[string][]byte{"workflow.hcl": []byte(runnableWorkflowHCL)}
	tarBody := buildTarGz(t, entries)
	zipBody := buildZip(t, entries)
	tarSum := sha256.Sum256(tarBody)
	zipSum := sha256.Sum256(zipBody)

	fx := archiveFixture{
		tarGzRef: "sha256:" + hex.EncodeToString(tarSum[:]),
		zipRef:   "sha256:" + hex.EncodeToString(zipSum[:]),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case "flow.tar.gz":
			_, _ = w.Write(tarBody)
		case "flow.zip":
			_, _ = w.Write(zipBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	fx.tarGzURL = srv.URL + "/flow.tar.gz"
	fx.zipURL = srv.URL + "/flow.zip"
	return fx
}

func requireResolvedWorkflow(t *testing.T, dir string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, "workflow.hcl"))
	require.NoError(t, err)
	assert.Equal(t, runnableWorkflowHCL, string(got))
}

// ---------------------------------------------------------------------------
// Resolver
// ---------------------------------------------------------------------------

// TestResolveWorkflowSource_LocalForms_Unchanged verifies local sources pass
// through byte-for-byte with a nil origin and never reach the fetcher
// (ADR-0005 D3: local behavior is unchanged).
func TestResolveWorkflowSource_LocalForms_Unchanged(t *testing.T) {
	stub := installStubFetcher(t)
	cases := []string{
		"workflow.hcl",
		"flow/workflow.hcl",
		"/abs/path/workflow.hcl",
		"flow-dir",
		"file:///abs/path/workflow.hcl",
	}
	for _, source := range cases {
		t.Run(source, func(t *testing.T) {
			dir, origin, err := resolveWorkflowSource(context.Background(), source)
			require.NoError(t, err)
			assert.Equal(t, source, dir, "local source must be returned unchanged")
			assert.Nil(t, origin, "local sources must not produce an origin")
		})
	}
	assert.Empty(t, stub.fetchCalls(), "local sources must not reach the fetcher")
}

// TestResolveWorkflowSource_GitRefForms covers git ref forms through the real
// fetcher: branch, tag, HEAD, an explicit commit SHA, and the default HEAD
// form, asserting the returned origin (ADR-0005 D1/D4/D6).
func TestResolveWorkflowSource_GitRefForms(t *testing.T) {
	home := setWorkflowCacheHome(t)
	fx := createWorkflowGitFixture(t)
	cases := []struct {
		name     string
		source   string
		expected string
	}{
		{"branch", "git::file://" + fx.path + "?ref=main", fx.headSHA},
		{"tag", "git::file://" + fx.path + "?ref=v1", fx.tagSHA},
		{"head", "git::file://" + fx.path + "?ref=HEAD", fx.headSHA},
		{"commit-sha", "git::file://" + fx.path + "?ref=" + fx.tagSHA, fx.tagSHA},
		{"default-head", "git::file://" + fx.path, fx.headSHA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, origin, err := resolveWorkflowSource(context.Background(), tc.source)
			require.NoError(t, err)

			require.NotNil(t, origin)
			assert.Equal(t, "git", origin.Kind)
			assert.Equal(t, tc.source, origin.Source)
			assert.Equal(t, tc.expected, origin.ResolvedRef)
			assert.Equal(t, dir, origin.Path)
			assert.Equal(t,
				filepath.Join(home, "cache", "workflows", slugify("file://"+fx.path), tc.expected),
				dir, "cache layout must be cache/workflows/<slug>/<version>")
			requireResolvedWorkflow(t, dir)
		})
	}
}

// TestResolveWorkflowSource_ArchiveForms covers http(s) archive sources
// (tar.gz and zip) through the real fetcher, asserting the sha256 digest ref
// in the origin.
func TestResolveWorkflowSource_ArchiveForms(t *testing.T) {
	home := setWorkflowCacheHome(t)
	fx := createWorkflowArchiveFixture(t)
	cases := []struct {
		name     string
		source   string
		expected string
	}{
		{"tar.gz", fx.tarGzURL, fx.tarGzRef},
		{"zip", fx.zipURL, fx.zipRef},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, origin, err := resolveWorkflowSource(context.Background(), tc.source)
			require.NoError(t, err)

			require.NotNil(t, origin)
			assert.Equal(t, "archive", origin.Kind)
			assert.Equal(t, tc.source, origin.Source)
			assert.Equal(t, tc.expected, origin.ResolvedRef)
			assert.Equal(t, dir, origin.Path)
			assert.Equal(t,
				filepath.Join(home, "cache", "workflows", slugify(tc.source), tc.expected),
				dir, "cache layout must be cache/workflows/<slug>/<version>")
			requireResolvedWorkflow(t, dir)
		})
	}
}

// TestResolveWorkflowSource_BareScpStyleForm covers the scp-style
// "git@host:path?ref=..." form without the git:: force prefix, which url.Parse
// rejects; the fetcher must still route it to the git getter (fake ssh
// transport serves the fixture).
func TestResolveWorkflowSource_BareScpStyleForm(t *testing.T) {
	home := setWorkflowCacheHome(t)
	fx := createWorkflowGitFixture(t)
	installFakeSSH(t)

	source := "git@127.0.0.1:" + fx.path + "?ref=main"
	dir, origin, err := resolveWorkflowSource(context.Background(), source)
	require.NoError(t, err)

	require.NotNil(t, origin)
	assert.Equal(t, "git", origin.Kind)
	assert.Equal(t, source, origin.Source)
	assert.Equal(t, fx.headSHA, origin.ResolvedRef)
	assert.Equal(t, dir, origin.Path)
	assert.Equal(t,
		filepath.Join(home, "cache", "workflows", slugify("git@127.0.0.1:"+fx.path), fx.headSHA),
		dir)
	requireResolvedWorkflow(t, dir)
}

// TestResolveWorkflowSource_UnsupportedRemoteForm pins the error surface for
// remote source forms the fetcher does not support.
func TestResolveWorkflowSource_UnsupportedRemoteForm(t *testing.T) {
	setWorkflowCacheHome(t)
	source := "ftp://example.com/workflow.tar.gz"

	dir, origin, err := resolveWorkflowSource(context.Background(), source)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported workflow source scheme")
	assert.Empty(t, dir)
	assert.Nil(t, origin)
}

// TestResolveWorkflowSource_MissingGitSourceFails verifies fetch failures
// surface as resolution errors without an origin.
func TestResolveWorkflowSource_MissingGitSourceFails(t *testing.T) {
	setWorkflowCacheHome(t)
	source := "git::file:///nonexistent/repo.git?ref=main"

	dir, origin, err := resolveWorkflowSource(context.Background(), source)
	require.Error(t, err)
	assert.Empty(t, dir)
	assert.Nil(t, origin)
}

// ---------------------------------------------------------------------------
// apply
// ---------------------------------------------------------------------------

func assertApplyEvents(t *testing.T, eventsFile string) {
	t.Helper()
	types, err := readPayloadTypes(eventsFile)
	require.NoError(t, err)
	assert.Equal(t, 1, countPayloadType(types, "RunStarted"))
	assert.Equal(t, 1, countPayloadType(types, "RunCompleted"))
	got := filterPayloadTypes(types, map[string]bool{
		"StepEntered": true, "StepOutcome": true, "StepTransition": true,
	})
	assert.Equal(t, []string{"StepEntered", "StepOutcome", "StepTransition"}, got)
}

// capturedOriginLog extracts the "workflow source resolved" log record from an
// apply run using an injectable slog handler.
func capturedOriginLog(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "workflow source resolved" {
			found = rec
		}
	}
	require.NotNil(t, found, "apply must log the resolved workflow origin; got:\n%s", buf.String())
	return found
}

// TestApply_RemoteGitSource_EndToEnd runs apply with a git ref source: the
// workflow must execute from the fetched cache tree and the resolved origin
// (source URL, resolved ref, cache path) must be logged for RunMetadata
// recording (CRI-225).
func TestApply_RemoteGitSource_EndToEnd(t *testing.T) {
	setWorkflowCacheHome(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fx := createWorkflowGitFixture(t)
	source := "git::file://" + fx.path + "?ref=main"

	var logBuf bytes.Buffer
	_, origin, err := resolveWorkflowSource(context.Background(), source)
	require.NoError(t, err)

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	require.NoError(t, runApply(context.Background(), applyOptions{
		workflowPath: source,
		eventsPath:   eventsFile,
		log:          slog.New(slog.NewJSONHandler(&logBuf, nil)),
	}))
	assertApplyEvents(t, eventsFile)

	rec := capturedOriginLog(t, &logBuf)
	assert.Equal(t, "git", rec["kind"])
	assert.Equal(t, source, rec["source"])
	assert.Equal(t, fx.headSHA, rec["resolved_ref"])
	assert.Equal(t, origin.Path, rec["cache_path"])
	requireResolvedWorkflow(t, origin.Path)
}

// TestApply_RemoteArchiveSource_EndToEnd runs apply with an http archive
// source, asserting the same origin contract with the sha256 digest ref.
func TestApply_RemoteArchiveSource_EndToEnd(t *testing.T) {
	setWorkflowCacheHome(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fx := createWorkflowArchiveFixture(t)
	source := fx.tarGzURL

	var logBuf bytes.Buffer
	_, origin, err := resolveWorkflowSource(context.Background(), source)
	require.NoError(t, err)

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	require.NoError(t, runApply(context.Background(), applyOptions{
		workflowPath: source,
		eventsPath:   eventsFile,
		log:          slog.New(slog.NewJSONHandler(&logBuf, nil)),
	}))
	assertApplyEvents(t, eventsFile)

	rec := capturedOriginLog(t, &logBuf)
	assert.Equal(t, "archive", rec["kind"])
	assert.Equal(t, source, rec["source"])
	assert.Equal(t, fx.tarGzRef, rec["resolved_ref"])
	assert.Equal(t, origin.Path, rec["cache_path"])
	requireResolvedWorkflow(t, origin.Path)
}

// TestApply_RemoteSource_CacheReuse demonstrates cache reuse: the second
// apply with a pinned commit SHA source must serve from the existing cache
// entry without touching git (the fixture is deleted) and without
// re-materializing the tree (a sentinel file survives).
func TestApply_RemoteSource_CacheReuse(t *testing.T) {
	setWorkflowCacheHome(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fx := createWorkflowGitFixture(t)
	source := "git::file://" + fx.path + "?ref=" + fx.tagSHA

	_, origin1, err := resolveWorkflowSource(context.Background(), source)
	require.NoError(t, err)
	requireResolvedWorkflow(t, origin1.Path)

	sentinel := filepath.Join(origin1.Path, "sentinel.txt")
	require.NoError(t, os.WriteFile(sentinel, []byte("hit"), 0o644))

	// Remove the fixture: with a pinned SHA the fetcher must not perform any
	// git operation for a cache hit.
	require.NoError(t, os.RemoveAll(fx.path))

	_, origin2, err := resolveWorkflowSource(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, origin1.Path, origin2.Path, "second resolution must hit the cache")
	assert.Equal(t, fx.tagSHA, origin2.ResolvedRef)
	got, err := os.ReadFile(sentinel)
	require.NoError(t, err, "cache entry must not be re-materialized")
	assert.Equal(t, "hit", string(got))

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	require.NoError(t, runApply(context.Background(), applyOptions{workflowPath: source, eventsPath: eventsFile}))
	assertApplyEvents(t, eventsFile)
}

// ---------------------------------------------------------------------------
// validate
// ---------------------------------------------------------------------------

// TestValidate_RemoteGitSource validates a git ref source end to end.
func TestValidate_RemoteGitSource(t *testing.T) {
	setWorkflowCacheHome(t)
	fx := createWorkflowGitFixture(t)
	source := "git::file://" + fx.path + "?ref=main"

	out := captureOutput(t, func() {
		ok := validatePath(context.Background(), source, nil, false, false)
		require.True(t, ok)
	})
	assert.Contains(t, out, ": ok")
	assert.Contains(t, out, "cache"+string(filepath.Separator)+"workflows", "validate must report the resolved cache path")
}

// TestValidate_RemoteArchiveSource validates an http archive source end to end.
func TestValidate_RemoteArchiveSource(t *testing.T) {
	setWorkflowCacheHome(t)
	fx := createWorkflowArchiveFixture(t)
	source := fx.zipURL

	out := captureOutput(t, func() {
		ok := validatePath(context.Background(), source, nil, false, false)
		require.True(t, ok)
	})
	assert.Contains(t, out, ": ok")
	assert.Contains(t, out, "cache"+string(filepath.Separator)+"workflows", "validate must report the resolved cache path")
}

// TestValidate_RemoteSource_FetchErrorFailsValidation verifies that a failed
// fetch fails validation with an actionable error instead of a parse error.
func TestValidate_RemoteSource_FetchErrorFailsValidation(t *testing.T) {
	setWorkflowCacheHome(t)
	source := "git::file:///nonexistent/repo.git?ref=main"

	var ok bool
	out := captureOutput(t, func() {
		ok = validatePath(context.Background(), source, nil, false, false)
	})
	assert.False(t, ok)
	assert.Contains(t, out, source+": error:")
}

// TestValidate_LocalSource_DoesNotFetch proves local validation is unchanged:
// local sources are validated in place and never reach the fetcher.
func TestValidate_LocalSource_DoesNotFetch(t *testing.T) {
	stub := installStubFetcher(t)
	workflowPath := writeWorkflowFile(t, runnableWorkflowHCL)

	out := captureOutput(t, func() {
		ok := validatePath(context.Background(), workflowPath, nil, false, false)
		require.True(t, ok)
	})
	assert.Contains(t, out, workflowPath+": ok")
	assert.Empty(t, stub.fetchCalls())
}

// ---------------------------------------------------------------------------
// compile
// ---------------------------------------------------------------------------

// TestCompile_RemoteGitSource compiles a git ref source end to end.
func TestCompile_RemoteGitSource(t *testing.T) {
	setWorkflowCacheHome(t)
	fx := createWorkflowGitFixture(t)
	source := "git::file://" + fx.path + "?ref=main"

	out, err := compileWorkflowOutput(context.Background(), source, "json", nil, false, false)
	require.NoError(t, err)
	var compiled struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(out, &compiled))
	assert.Equal(t, "remote_source_flow", compiled.Name)
}

// TestCompile_RemoteArchiveSource compiles an http archive source end to end.
func TestCompile_RemoteArchiveSource(t *testing.T) {
	setWorkflowCacheHome(t)
	fx := createWorkflowArchiveFixture(t)
	source := fx.tarGzURL

	out, err := compileWorkflowOutput(context.Background(), source, "json", nil, false, false)
	require.NoError(t, err)
	var compiled struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(out, &compiled))
	assert.Equal(t, "remote_source_flow", compiled.Name)
}

// TestCompile_RemoteSource_FetchErrorFails pins compile's failure surface for
// an unresolvable remote source.
func TestCompile_RemoteSource_FetchErrorFails(t *testing.T) {
	setWorkflowCacheHome(t)
	source := "git::file:///nonexistent/repo.git?ref=main"

	_, err := compileWorkflowOutput(context.Background(), source, "json", nil, false, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonexistent"+string(filepath.Separator)+"repo.git")
}

// TestCompile_LocalSource_DoesNotFetch proves local compilation is unchanged.
func TestCompile_LocalSource_DoesNotFetch(t *testing.T) {
	stub := installStubFetcher(t)
	workflowPath := writeWorkflowFile(t, runnableWorkflowHCL)

	out, err := compileWorkflowOutput(context.Background(), workflowPath, "json", nil, false, false)
	require.NoError(t, err)
	var compiled struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(out, &compiled))
	assert.Equal(t, "remote_source_flow", compiled.Name)
	assert.Empty(t, stub.fetchCalls())
}
