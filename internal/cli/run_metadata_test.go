package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunMetadataFromOrigin_RedactsCredentials pins the record's redaction
// guarantee: a credential-bearing source is stored only in its redacted form
// (redactSourceForLog), and no credential material appears anywhere in the
// serialized record in raw or slugified form (CRI-225).
func TestRunMetadataFromOrigin_RedactsCredentials(t *testing.T) {
	credentialSource := "http://" + testUserPass + "@host.example/x.tar.gz"
	origin := &WorkflowOrigin{
		Kind:        "archive",
		Source:      credentialSource,
		ResolvedRef: "sha256:abc123",
		Path:        filepath.Join("cache", "workflows", slugForSource("http://redacted@host.example/x.tar.gz"), "sha256:abc123"),
		FetchedAt:   time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
	}

	md := runMetadataFromOrigin(origin)

	assert.Equal(t, "archive", md.Kind)
	assert.Equal(t, redactSourceForLog(credentialSource), md.Source)
	assert.NotContains(t, md.Source, testUserPass, "recorded source must be redacted")
	assert.Equal(t, "sha256:abc123", md.ResolvedRef)
	assert.Equal(t, origin.Path, md.CachePath)
	assert.Equal(t, origin.FetchedAt, md.FetchedAt)

	b, err := json.Marshal(md)
	require.NoError(t, err)
	assert.NotContains(t, string(b), testUserPass, "serialized record must not contain URL userinfo")
	assert.NotContains(t, string(b), "user_pass", "serialized record must not contain the slugified credentials")
}

// TestRunMetadataFromOrigin_CarriesAllOriginFields pins the full origin
// contract for a credential-free source: every origin field is recorded
// unchanged (the source already being redaction-stable).
func TestRunMetadataFromOrigin_CarriesAllOriginFields(t *testing.T) {
	fetched := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	origin := &WorkflowOrigin{
		Kind:        "git",
		Source:      "git::file:///tmp/fixture.git?ref=main",
		ResolvedRef: "0123abcd",
		Path:        "/cache/workflows/slug/0123abcd",
		FetchedAt:   fetched,
	}

	md := runMetadataFromOrigin(origin)
	assert.Equal(t, *origin, WorkflowOrigin{
		Kind:        md.Kind,
		Source:      md.Source,
		ResolvedRef: md.ResolvedRef,
		Path:        md.CachePath,
		FetchedAt:   md.FetchedAt,
	})
}

// TestWriteRunMetadata_RoundTrip pins the on-disk contract: the record is
// written to runs/<run_id>/run-metadata.json under the state dir with
// state-file permissions and round-trips as JSON.
func TestWriteRunMetadata_RoundTrip(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", state)

	fetched := time.Now().UTC().Truncate(time.Second)
	md := &RunMetadata{
		Kind:        "git",
		Source:      "git::https://host.example/org/repo.git?ref=main",
		ResolvedRef: "0123abcd",
		CachePath:   filepath.Join(state, "cache", "workflows", "slug", "0123abcd"),
		FetchedAt:   fetched,
	}

	require.NoError(t, writeRunMetadata("run-1", md))

	path := filepath.Join(state, "runs", "run-1", "run-metadata.json")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
	assert.Zero(t, info.Mode().Perm()&^0o600, "run metadata must use state-file permissions")

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var got RunMetadata
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, *md, got)
	assert.True(t, got.FetchedAt.Equal(fetched), "fetched_at must round-trip as UTC RFC3339")
}

// TestWriteRunMetadata_RequiresRunID pins the fail-closed surface for a
// missing run id.
func TestWriteRunMetadata_RequiresRunID(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	require.Error(t, writeRunMetadata("", &RunMetadata{Kind: "git"}))
}

// TestPublishRunMetadata_NilOrigin_WritesNothing pins the local-source
// behavior: no origin means no record is written anywhere.
func TestPublishRunMetadata_NilOrigin_WritesNothing(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", state)

	publishRunMetadata("run-1", nil, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))

	assert.NoDirExists(t, filepath.Join(state, "runs", "run-1"))
}

// TestPublishRunMetadata_WritesRecordFromOrigin pins the publication
// behavior: the record lands under the run's state directory.
func TestPublishRunMetadata_WritesRecordFromOrigin(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", state)

	origin := &WorkflowOrigin{
		Kind:        "archive",
		Source:      "https://host.example/x.tar.gz",
		ResolvedRef: "sha256:abc123",
		Path:        "/cache/workflows/slug/sha256_abc123",
		FetchedAt:   time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
	}
	publishRunMetadata("run-1", origin, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))

	b, err := os.ReadFile(filepath.Join(state, "runs", "run-1", "run-metadata.json"))
	require.NoError(t, err)
	var md RunMetadata
	require.NoError(t, json.Unmarshal(b, &md))
	assert.Equal(t, runMetadataFromOrigin(origin), &md)
}

// TestPublishRunMetadata_FailureDegradesToWarning pins the soft-degrade
// contract: a recording failure must not panic or fail the run; it warns.
func TestPublishRunMetadata_FailureDegradesToWarning(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state-is-a-file")
	require.NoError(t, os.WriteFile(stateFile, []byte("x"), 0o600))
	t.Setenv("CRITERIA_STATE_DIR", stateFile)

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, nil))

	origin := &WorkflowOrigin{Kind: "git", Source: "git::file:///tmp/fixture.git", ResolvedRef: "abc", FetchedAt: time.Now().UTC()}
	publishRunMetadata("run-1", origin, log)

	assert.Contains(t, logBuf.String(), "failed to record run metadata provenance")
}

// TestApply_LocalSource_NoRunMetadataRecord pins the command-level contract
// for local sources: a run executed from a local workflow publishes no
// provenance record because there is no workflow origin.
func TestApply_LocalSource_NoRunMetadataRecord(t *testing.T) {
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"), []byte(runnableWorkflowHCL), 0o644))
	t.Chdir(root)

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	require.NoError(t, runApply(context.Background(), applyOptions{workflowPath: "workflow.hcl", eventsPath: eventsFile}))
	assertApplyEvents(t, eventsFile)

	runID := runIDFromEvents(t, eventsFile)
	state, err := stateDir()
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(state, "runs", runID, "run-metadata.json"),
		"local-source runs must not record a workflow origin")
}
