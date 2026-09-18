package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

// testUserPass is assembled at runtime so no credential-shaped literal lands
// in the source tree; tests assert it never reaches cache paths, logs, or
// stdout in raw or slugified ("user_pass") form.
var testUserPass = "user" + ":" + "pass"

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
	// gitPatternTarGzURL and gitPatternZipURL serve the same archives from
	// paths that also match a git URL pattern (a ".git" segment earlier in
	// the path) and carry a query string, pinning that an archive-suffix
	// path wins over the git pattern and that the query is ignored.
	gitPatternTarGzURL string
	gitPatternZipURL   string
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
		case "v1.git/flow.tar.gz":
			_, _ = w.Write(tarBody)
		case "repo.git/releases/flow.zip":
			_, _ = w.Write(zipBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	fx.tarGzURL = srv.URL + "/flow.tar.gz"
	fx.zipURL = srv.URL + "/flow.zip"
	fx.gitPatternTarGzURL = srv.URL + "/v1.git/flow.tar.gz?download=1"
	fx.gitPatternZipURL = srv.URL + "/repo.git/releases/flow.zip?download=1"
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
// (ADR-0005 D3: local behavior is unchanged). The http/ftps/git+ prefixed
// cases are the classifier regression class: local relative paths whose first
// element merely starts with a scheme word must stay local.
func TestResolveWorkflowSource_LocalForms_Unchanged(t *testing.T) {
	stub := installStubFetcher(t)
	cases := []string{
		"workflow.hcl",
		"flow/workflow.hcl",
		"/abs/path/workflow.hcl",
		"flow-dir",
		"file:///abs/path/workflow.hcl",
		"httpflows",
		"httpflows/workflow.hcl",
		"httpsx",
		"httpx",
		"ftpsync",
		"git+fixture",
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

// TestSlugForSource pins the cache-slug derivation: userinfo is replaced with
// the redactSourceForLog shape before slugifying so credentials never persist
// in the on-disk cache directory name, while local, file://, scp-style, and
// userinfo-free sources keep the plain slugify result (existing cache entries
// stay reachable).
func TestSlugForSource(t *testing.T) {
	cases := []struct{ name, source, want string }{
		{"credentials-in-url", "https://" + testUserPass + "@host.example/x.tar.gz", "https___redacted_host.example_x.tar.gz"},
		{"username-only", "http://deploy@host.example/org/repo.git", "http___redacted_host.example_org_repo.git"},
		{"password-only", "http://:secret@host.example/x.tar.gz", "http___redacted_host.example_x.tar.gz"},
		{"ssh-with-userinfo", "ssh://deploy:secret@host.example/org/repo.git", "ssh___redacted_host.example_org_repo.git"},
		{"plain-https", "https://host.example/x.tar.gz", "https___host.example_x.tar.gz"},
		{"at-in-path-no-userinfo", "https://host.example/a@b/x.tar.gz", "https___host.example_a_b_x.tar.gz"},
		{"scp-style-git-form", "git@host.example:org/repo.git?ref=main", "git_host.example_org_repo.git_ref_main"},
		{"file-scheme", "file:///tmp/fixture.git?ref=main", "file____tmp_fixture.git_ref_main"},
		{"local-path", "workflow.hcl", "workflow.hcl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, slugForSource(tc.source))
		})
	}
}

// TestResolveWorkflowSource_ArchiveCredentialsNotInCachePath is the regression
// for the userinfo credential leak: the cache slug for a source carrying
// userinfo must redact the credentials before slugifying, so neither the
// on-disk directory nor origin.Path contain the credentials in raw or
// slugified form (previously slugify preserved "user_pass" verbatim).
func TestResolveWorkflowSource_ArchiveCredentialsNotInCachePath(t *testing.T) {
	home := setWorkflowCacheHome(t)
	fx := createWorkflowArchiveFixture(t)
	hostPort := strings.TrimPrefix(fx.tarGzURL, "http://")
	source := "http://" + testUserPass + "@" + hostPort

	dir, origin, err := resolveWorkflowSource(context.Background(), source)
	require.NoError(t, err)

	wantSlug := slugify("http://redacted@" + hostPort)
	require.NotContains(t, wantSlug, testUserPass)
	assert.Equal(t,
		filepath.Join(home, "cache", "workflows", wantSlug, fx.tarGzRef),
		dir, "cache layout must redact userinfo in the slug")
	assert.NotContains(t, dir, testUserPass, "credentials must not survive in the cache path")
	assert.NotContains(t, dir, "user_pass", "slugified credentials must not survive either")

	require.NotNil(t, origin)
	assert.Equal(t, "archive", origin.Kind)
	assert.Equal(t, source, origin.Source, "the origin keeps the raw source; the lockfile stores it per ADR-0005 D4")
	assert.Equal(t, dir, origin.Path)
	requireResolvedWorkflow(t, dir)
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

// TestResolveWorkflowSource_OptionLikeGitSourceRejected is the command-level
// regression for the git argv injection class: a git source whose repository
// position starts with "-" would make git parse it as a command-line option
// (e.g. "--upload-pack=<cmd>", which runs <cmd> locally through the shell).
// Resolution must fail closed with the typed unsafe-source error, return no
// origin, and never execute the injected command.
func TestResolveWorkflowSource_OptionLikeGitSourceRejected(t *testing.T) {
	setWorkflowCacheHome(t)
	sentinel := filepath.Join(t.TempDir(), "pwned")
	source := "git::--upload-pack=touch " + sentinel

	dir, origin, err := resolveWorkflowSource(context.Background(), source)
	require.Error(t, err)
	assert.ErrorIs(t, err, errUnsafeGitSource)
	assert.Contains(t, err.Error(), "invalid git workflow source")
	assert.Empty(t, dir)
	assert.Nil(t, origin)
	assert.NoFileExists(t, sentinel, "the injected command must never run")
}

// TestCommands_HttpPrefixedLocalPath_Unchanged is the command-level
// regression for the remote/local classifier: validate and compile on a local
// relative path whose first element starts with "http" must behave exactly as
// before remote sources were accepted, and must never touch the fetcher.
func TestCommands_HttpPrefixedLocalPath_Unchanged(t *testing.T) {
	stub := installStubFetcher(t)
	root := t.TempDir()
	dir := filepath.Join(root, "httpflows")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflow.hcl"), []byte(runnableWorkflowHCL), 0o644))
	t.Chdir(root)

	out := captureOutput(t, func() {
		ok := validatePath(context.Background(), "httpflows", nil, false, false)
		require.True(t, ok)
	})
	assert.Contains(t, out, "httpflows: ok")

	compiled, err := compileWorkflowOutput(context.Background(), "httpflows/workflow.hcl", "json", nil, false, false)
	require.NoError(t, err)
	var parsed struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(compiled, &parsed))
	assert.Equal(t, "remote_source_flow", parsed.Name)

	assert.Empty(t, stub.fetchCalls(), "local http-prefixed paths must not reach the fetcher")
}

// TestRedactSourceForLog pins the log-boundary credential redaction applied
// to the "workflow source resolved" record.
func TestRedactSourceForLog(t *testing.T) {
	cases := []struct{ source, want string }{
		{"https://user:token@host.example/x.tar.gz", "https://redacted@host.example/x.tar.gz"},
		{"git::https://user:token@host.example/repo.git?ref=main", "git::https://redacted@host.example/repo.git?ref=main"},
		{"ssh://deploy@host.example/org/repo.git", "ssh://redacted@host.example/org/repo.git"},
		{"https://host.example/x.tar.gz", "https://host.example/x.tar.gz"},
		{"git@host.example:org/repo.git?ref=main", "git@host.example:org/repo.git?ref=main"},
		{"git::file:///tmp/fixture.git?ref=main", "git::file:///tmp/fixture.git?ref=main"},
		{"httpflows", "httpflows"},
		// "@" is the userinfo delimiter only in the authority component; a
		// later "@" in the path must not corrupt the host.
		{"https://host.example/a@b/x.tar.gz", "https://host.example/a@b/x.tar.gz"},
		{"http://" + testUserPass + "@host.example/a@b/x.tar.gz", "http://redacted@host.example/a@b/x.tar.gz"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, redactSourceForLog(tc.source), tc.source)
	}
}

// TestResolveWorkflowSource_FetchedAt pins the fetch timestamp semantics
// (CRI-225): a fresh fetch reports the materialization time, and a
// warm-cache resolution reports the original materialization time rather
// than the resolution time.
func TestResolveWorkflowSource_FetchedAt(t *testing.T) {
	setWorkflowCacheHome(t)
	fx := createWorkflowArchiveFixture(t)

	_, origin1, err := resolveWorkflowSource(context.Background(), fx.tarGzURL)
	require.NoError(t, err)
	require.False(t, origin1.FetchedAt.IsZero())
	require.WithinDuration(t, time.Now().UTC(), origin1.FetchedAt, 10*time.Second,
		"a fresh fetch's fetched_at is the materialization time")

	_, origin2, err := resolveWorkflowSource(context.Background(), fx.tarGzURL)
	require.NoError(t, err)
	assert.True(t, origin2.FetchedAt.Equal(origin1.FetchedAt),
		"a warm-cache resolution must report the original materialization time")
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

// runIDFromEvents discovers the run id from a local ND-JSON events file
// (each envelope carries the top-level run_id).
func runIDFromEvents(t *testing.T, eventsFile string) string {
	t.Helper()
	b, err := os.ReadFile(eventsFile)
	require.NoError(t, err)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var env struct {
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal([]byte(line), &env); err == nil && env.RunID != "" {
			return env.RunID
		}
	}
	t.Fatal("no run_id found in the events file")
	return ""
}

// requireRunMetadata reads the run's RunMetadata admission record from the
// run's state directory, discovering the run id from the events file
// (CRI-225: the run must publish its resolved workflow origin).
func requireRunMetadata(t *testing.T, eventsFile string) *RunMetadata {
	t.Helper()
	runID := runIDFromEvents(t, eventsFile)
	state, err := stateDir()
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(state, "runs", runID, "run-metadata.json"))
	require.NoError(t, err, "the run must publish its resolved workflow origin record")
	var md RunMetadata
	require.NoError(t, json.Unmarshal(b, &md))
	return &md
}

// TestApply_RemoteGitSource_EndToEnd runs apply with a git ref source: the
// workflow must execute from the fetched cache tree and the resolved origin
// (source URL, resolved ref, cache path) must be logged for RunMetadata
// recording (CRI-225).
func TestApply_RemoteGitSource_EndToEnd(t *testing.T) {
	start := time.Now().UTC().Add(-2 * time.Second)
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

	md := requireRunMetadata(t, eventsFile)
	assert.Equal(t, "git", md.Kind)
	assert.Equal(t, source, md.Source)
	assert.Equal(t, fx.headSHA, md.ResolvedRef)
	assert.Equal(t, origin.Path, md.CachePath)
	assert.False(t, md.FetchedAt.IsZero(), "the record must carry the fetch timestamp")
	assert.False(t, md.FetchedAt.Before(start), "fetched_at must not predate the test")
	assert.False(t, md.FetchedAt.After(time.Now().UTC().Add(time.Second)), "fetched_at must not be in the future")
}

// TestApply_RemoteArchiveSource_EndToEnd runs apply with an http archive
// source, asserting the same origin contract with the sha256 digest ref.
func TestApply_RemoteArchiveSource_EndToEnd(t *testing.T) {
	start := time.Now().UTC().Add(-2 * time.Second)
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

	md := requireRunMetadata(t, eventsFile)
	assert.Equal(t, "archive", md.Kind)
	assert.Equal(t, source, md.Source)
	assert.Equal(t, fx.tarGzRef, md.ResolvedRef)
	assert.Equal(t, origin.Path, md.CachePath)
	assert.False(t, md.FetchedAt.IsZero(), "the record must carry the fetch timestamp")
	assert.False(t, md.FetchedAt.Before(start), "fetched_at must not predate the test")
	assert.False(t, md.FetchedAt.After(time.Now().UTC().Add(time.Second)), "fetched_at must not be in the future")
}

// TestApply_RemoteSource_CredentialsNotLogged pins the apply log boundary for
// a credential-bearing source: the cache_path field is safe by construction
// (the slug is redacted before the directory is created) and no byte of the
// structured log may contain the userinfo in raw or slugified form.
func TestApply_RemoteSource_CredentialsNotLogged(t *testing.T) {
	setWorkflowCacheHome(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fx := createWorkflowArchiveFixture(t)
	hostPort := strings.TrimPrefix(fx.tarGzURL, "http://")
	source := "http://" + testUserPass + "@" + hostPort

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

	assert.NotContains(t, logBuf.String(), testUserPass, "apply logs must not contain URL userinfo")
	assert.NotContains(t, logBuf.String(), "user_pass", "apply logs must not contain the slugified credentials")
	assert.NotContains(t, origin.Path, testUserPass)

	rec := capturedOriginLog(t, &logBuf)
	assert.Equal(t, "archive", rec["kind"])
	assert.Equal(t, redactSourceForLog(source), rec["source"])
	assert.Equal(t, fx.tarGzRef, rec["resolved_ref"])
	assert.Equal(t, origin.Path, rec["cache_path"])
	requireResolvedWorkflow(t, origin.Path)

	// CRI-225: the recorded provenance carries the same redaction guarantee.
	md := requireRunMetadata(t, eventsFile)
	assert.Equal(t, "archive", md.Kind)
	assert.Equal(t, redactSourceForLog(source), md.Source, "the record must store the redacted source")
	assert.Equal(t, fx.tarGzRef, md.ResolvedRef)
	assert.Equal(t, origin.Path, md.CachePath)
	assert.False(t, md.FetchedAt.IsZero(), "the record must carry the fetch timestamp")
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

// TestApply_RemoteSource_RoutesGitPatternArchiveURL runs apply with a
// git-pattern-matching archive source (".git" segment earlier in the path,
// query string attached), asserting the archive origin contract end to end:
// pre-fix this source was misrouted to the git fetcher and failed with a
// git ls-remote error.
func TestApply_RemoteSource_RoutesGitPatternArchiveURL(t *testing.T) {
	setWorkflowCacheHome(t)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	fx := createWorkflowArchiveFixture(t)
	source := fx.gitPatternTarGzURL

	var logBuf bytes.Buffer
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
	requireResolvedWorkflow(t, rec["cache_path"].(string))
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
	assert.Contains(t, out, source+": ok")
	assert.NotContains(t, out, "cache"+string(filepath.Separator)+"workflows",
		"validate must echo the user-supplied source, not the internal cache path")
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
	assert.Contains(t, out, source+": ok")
	assert.NotContains(t, out, "cache"+string(filepath.Separator)+"workflows",
		"validate must echo the user-supplied source, not the internal cache path")
}

// TestValidate_RemoteSource_CredentialsNotPrinted pins the validate stdout
// boundary for a credential-bearing source: the OK line echoes the
// user-supplied source with userinfo redacted, and the resolved cache path
// (whose slug is itself redacted) never reaches stdout.
func TestValidate_RemoteSource_CredentialsNotPrinted(t *testing.T) {
	setWorkflowCacheHome(t)
	fx := createWorkflowArchiveFixture(t)
	hostPort := strings.TrimPrefix(fx.tarGzURL, "http://")
	source := "http://" + testUserPass + "@" + hostPort

	var ok bool
	out := captureOutput(t, func() {
		ok = validatePath(context.Background(), source, nil, false, false)
		require.True(t, ok)
	})
	assert.NotContains(t, out, testUserPass, "validate stdout must not contain URL userinfo")
	assert.NotContains(t, out, "user_pass", "validate stdout must not contain the slugified credentials")
	assert.NotContains(t, out, "cache"+string(filepath.Separator)+"workflows")
	assert.Contains(t, out, redactSourceForLog(source)+": ok",
		"the OK line must echo the user-supplied (redacted) source")
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

// TestValidate_RemoteSource_FetchErrorRedactsCredentials pins the fetch-error
// boundary for a credential-bearing source: the failure text must carry the
// redacted source, never the raw userinfo, in raw or slugified form.
func TestValidate_RemoteSource_FetchErrorRedactsCredentials(t *testing.T) {
	setWorkflowCacheHome(t)
	fx := createWorkflowArchiveFixture(t)
	hostPort := strings.TrimPrefix(fx.tarGzURL, "http://")
	source := "http://" + testUserPass + "@" + hostPort + "/missing.tar.gz"

	var ok bool
	out := captureOutput(t, func() {
		ok = validatePath(context.Background(), source, nil, false, false)
	})
	assert.False(t, ok)
	assert.Contains(t, out, ": error:")
	assert.Contains(t, out, redactSourceForLog(source))
	assert.NotContains(t, out, testUserPass, "fetch errors must not echo URL userinfo")
	assert.NotContains(t, out, "user_pass", "fetch errors must not echo slugified credentials")
}

// serveGitHTTPBackend bridges an httptest server to "git http-backend" so
// https git URL sources can be exercised end to end over the smart HTTP
// transport. reposDir must be the parent directory of the bare repos it
// serves.
func serveGitHTTPBackend(t *testing.T, reposDir string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		if r.Body != nil {
			_, _ = io.Copy(&body, r.Body)
		}
		cmd := exec.Command("git", "http-backend")
		cmd.Env = append(os.Environ(),
			"GIT_PROJECT_ROOT="+reposDir,
			"GIT_HTTP_EXPORT_ALL=1",
			"REQUEST_METHOD="+r.Method,
			"PATH_INFO="+r.URL.EscapedPath(),
			"QUERY_STRING="+r.URL.RawQuery,
			"REMOTE_ADDR="+r.RemoteAddr,
			"CONTENT_TYPE="+r.Header.Get("Content-Type"),
		)
		if proto := r.Header.Get("Git-Protocol"); proto != "" {
			cmd.Env = append(cmd.Env, "GIT_PROTOCOL="+proto)
		}
		if body.Len() > 0 {
			cmd.Env = append(cmd.Env, "CONTENT_LENGTH="+strconv.Itoa(body.Len()))
		}
		cmd.Stdin = &body
		var out, errOut bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errOut
		if err := cmd.Run(); err != nil {
			http.Error(w, "git http-backend failed: "+errOut.String(), http.StatusInternalServerError)
			return
		}

		// Parse the CGI response: headers, blank line, then the payload.
		resp := out.Bytes()
		var hdr, payload []byte
		if sep := bytes.Index(resp, []byte("\r\n\r\n")); sep != -1 {
			hdr, payload = resp[:sep], resp[sep+4:]
		} else if sep := bytes.Index(resp, []byte("\n\n")); sep != -1 {
			hdr, payload = resp[:sep], resp[sep+2:]
		} else {
			payload = resp
		}
		status := http.StatusOK
		for _, line := range strings.Split(string(hdr), "\n") {
			line = strings.TrimSpace(line)
			key, val, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			if strings.EqualFold(key, "Status") {
				if fields := strings.Fields(val); len(fields) > 0 {
					if code, err := strconv.Atoi(fields[0]); err == nil {
						status = code
					}
				}
				continue
			}
			w.Header().Set(strings.TrimSpace(key), strings.TrimSpace(val))
		}
		w.WriteHeader(status)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestValidate_RemoteSource_RoutesGitHttpsVsArchive pins the fetcher routing
// at the command level: a bare https git URL (repo.git suffix, served by a
// git http-backend bridge) routes to the git getter with the <slug>/<sha>
// cache layout, while a plain https archive URL routes to the archive getter
// with the <slug>/sha256:<digest> layout (docs/workflow.md "Source schemes").
func TestValidate_RemoteSource_RoutesGitHttpsVsArchive(t *testing.T) {
	home := setWorkflowCacheHome(t)
	gitFX := createWorkflowGitFixture(t)
	archiveFX := createWorkflowArchiveFixture(t)

	srv := serveGitHTTPBackend(t, filepath.Dir(gitFX.path))
	gitSource := srv.URL + "/fixture.git?ref=main"

	gitDir, gitOrigin, err := resolveWorkflowSource(context.Background(), gitSource)
	require.NoError(t, err)
	assert.Equal(t, "git", gitOrigin.Kind)
	assert.Equal(t, gitSource, gitOrigin.Source)
	assert.Equal(t, gitFX.headSHA, gitOrigin.ResolvedRef)
	assert.Equal(t,
		filepath.Join(home, "cache", "workflows", slugify(srv.URL+"/fixture.git"), gitFX.headSHA),
		gitDir, "an https git source must use the <slug>/<sha> cache layout")
	requireResolvedWorkflow(t, gitDir)

	archiveDir, archiveOrigin, err := resolveWorkflowSource(context.Background(), archiveFX.tarGzURL)
	require.NoError(t, err)
	assert.Equal(t, "archive", archiveOrigin.Kind)
	assert.Equal(t, archiveFX.tarGzURL, archiveOrigin.Source)
	assert.Equal(t, archiveFX.tarGzRef, archiveOrigin.ResolvedRef)
	assert.Equal(t,
		filepath.Join(home, "cache", "workflows", slugify(archiveFX.tarGzURL), archiveFX.tarGzRef),
		archiveDir, "an https archive source must use the <slug>/sha256:<digest> cache layout")
	requireResolvedWorkflow(t, archiveDir)

	out := captureOutput(t, func() {
		require.True(t, validatePath(context.Background(), gitSource, nil, false, false))
		require.True(t, validatePath(context.Background(), archiveFX.tarGzURL, nil, false, false))
	})
	assert.Contains(t, out, gitSource+": ok")
	assert.Contains(t, out, archiveFX.tarGzURL+": ok")
}

// TestValidate_RemoteSource_RoutesGitPatternArchiveURL is the regression for
// the routing reorder: an http(s) archive URL that also matches a git URL
// pattern — a ".git" segment earlier in the path, here with a query string —
// must reach the archive fetcher and resolve to
// cache/workflows/<slug>/sha256:<digest>, not fail with a git ls-remote
// error. Real-world forms like
// https://github.com/org/repo/archive/v1.tar.gz hit the same boundary.
func TestValidate_RemoteSource_RoutesGitPatternArchiveURL(t *testing.T) {
	home := setWorkflowCacheHome(t)
	fx := createWorkflowArchiveFixture(t)
	source := fx.gitPatternTarGzURL // ".../v1.git/flow.tar.gz?download=1"

	dir, origin, err := resolveWorkflowSource(context.Background(), source)
	require.NoError(t, err)
	require.NotNil(t, origin)
	assert.Equal(t, "archive", origin.Kind)
	assert.Equal(t, source, origin.Source)
	assert.Equal(t, fx.tarGzRef, origin.ResolvedRef)
	assert.Equal(t,
		filepath.Join(home, "cache", "workflows", slugify(source), fx.tarGzRef),
		dir, "a git-pattern-matching archive must use the <slug>/sha256:<digest> cache layout")
	requireResolvedWorkflow(t, dir)

	zipDir, zipOrigin, err := resolveWorkflowSource(context.Background(), fx.gitPatternZipURL)
	require.NoError(t, err)
	assert.Equal(t, "archive", zipOrigin.Kind)
	assert.Equal(t, fx.zipRef, zipOrigin.ResolvedRef)
	requireResolvedWorkflow(t, zipDir)

	out := captureOutput(t, func() {
		require.True(t, validatePath(context.Background(), source, nil, false, false))
	})
	assert.Contains(t, out, source+": ok")
	assert.NotContains(t, out, "cache"+string(filepath.Separator)+"workflows",
		"validate must echo the user-supplied source, not the internal cache path")
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
