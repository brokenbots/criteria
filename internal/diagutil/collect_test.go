package diagutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/opencontainers/go-digest"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

const testAdapterType = "test"
const testDigest = "sha256:38f23c92a11548ce57c54e9312c567558d3ad017fd632adfba305b058988703d"

// setHermeticAdapterRoot points CRITERIA_ADAPTERS at a temp directory for the
// duration of the test so InstallRoot()/AdaptersRoots() are deterministic and
// do not depend on $HOME.
func setHermeticAdapterRoot(t *testing.T) {
	t.Helper()
	t.Setenv("CRITERIA_ADAPTERS", t.TempDir())
}

// mockHandle implements adapterhost.Handle for testing schema collection.
type mockHandle struct {
	info    adapterhost.Info
	infoErr error
	killed  bool
}

func (m *mockHandle) Info(context.Context) (adapterhost.Info, error) { return m.info, m.infoErr }
func (m *mockHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (m *mockHandle) Execute(context.Context, string, *workflow.StepNode, adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{}, nil
}
func (m *mockHandle) CloseSession(context.Context, string) error { return nil }
func (m *mockHandle) Kill()                                      { m.killed = true }
func (m *mockHandle) Pause(context.Context, string) error        { return nil }
func (m *mockHandle) Resume(context.Context, string) error       { return nil }
func (m *mockHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return nil, errors.New("not implemented")
}
func (m *mockHandle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return nil, errors.New("not implemented")
}
func (m *mockHandle) Restore(context.Context, string, []byte, uint32) error { return nil }

var _ adapterhost.Handle = (*mockHandle)(nil)

// mockDiscoveryLoader records how it was invoked and implements discoveryLoader.
type mockDiscoveryLoader struct {
	resolveFunc              func(ctx context.Context, name string) (adapterhost.Handle, error)
	resolveWithDiscoveryFunc func(ctx context.Context, name string, discover adapterhost.DiscoveryFunc, customizer func(string, *exec.Cmd)) (adapterhost.Handle, error)
	shutdownCalled           bool
}

func (m *mockDiscoveryLoader) Resolve(ctx context.Context, name string) (adapterhost.Handle, error) {
	return m.resolveFunc(ctx, name)
}

func (m *mockDiscoveryLoader) ResolveWithDiscovery(ctx context.Context, name string, discover adapterhost.DiscoveryFunc, customizer func(string, *exec.Cmd)) (adapterhost.Handle, error) {
	return m.resolveWithDiscoveryFunc(ctx, name, discover, customizer)
}

func (m *mockDiscoveryLoader) Shutdown(context.Context) error {
	m.shutdownCalled = true
	return nil
}

var _ adapterhost.Loader = (*mockDiscoveryLoader)(nil)
var _ discoveryLoader = (*mockDiscoveryLoader)(nil)

func writeLockfile(t *testing.T, dir string) {
	t.Helper()
	content := fmt.Sprintf(`schema_version = 1

adapter "%s" "default" {
  reference          = "ghcr.io/example/%s"
  version            = "1.0.0"
  resolved_digest    = "%s"
  source_url         = "https://github.com/example/%s"
  sdk_protocol_version = 2
}
`, testAdapterType, testAdapterType, testDigest, testAdapterType)
	if err := os.WriteFile(filepath.Join(dir, ".criteria.lock.hcl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write lockfile: %v", err)
	}
}

func makeSpec() *workflow.Spec {
	return &workflow.Spec{
		Adapters: []workflow.AdapterDeclSpec{
			{Type: testAdapterType, Name: "default"},
		},
	}
}

func schemafulHandle() *mockHandle {
	return &mockHandle{
		info: adapterhost.Info{
			AdapterInfo: workflow.AdapterInfo{
				ConfigSchema: map[string]workflow.ConfigField{"foo": {Type: workflow.ConfigFieldString}},
				InputSchema:  map[string]workflow.ConfigField{"bar": {Type: workflow.ConfigFieldString}},
				OutputSchema: map[string]workflow.ConfigField{"baz": {Type: workflow.ConfigFieldString}},
			},
		},
	}
}

func schemalessHandle() *mockHandle {
	return &mockHandle{
		info: adapterhost.Info{
			AdapterInfo: workflow.AdapterInfo{
				ConfigSchema: map[string]workflow.ConfigField{"foo": {Type: workflow.ConfigFieldString}},
			},
		},
	}
}

// expectedDigestPath returns the path the discovery function should consult for
// the locked digest under the current adapter install root.
func expectedDigestPath(t *testing.T) string {
	t.Helper()
	root, err := adapterhost.InstallRoot()
	if err != nil {
		t.Fatalf("InstallRoot: %v", err)
	}
	return filepath.Join(root, adapterhost.EncodeDigest(digest.Digest(testDigest)), adapterhost.AdapterBinaryName(testAdapterType))
}

// TestCollectSchemas_LockfileOnlyResolution asserts that an adapter referenced
// only by source + version, with a lockfile present, is resolved via the locked
// digest and its output schema becomes available to the compiler. It must fail
// without the fix because the old code only called Resolve (by-name).
func TestCollectSchemas_LockfileOnlyResolution(t *testing.T) {
	setHermeticAdapterRoot(t)

	dir := t.TempDir()
	writeLockfile(t, dir)

	resolveWithDiscoveryCalled := false
	loader := &mockDiscoveryLoader{
		resolveFunc: func(_ context.Context, name string) (adapterhost.Handle, error) {
			return nil, fmt.Errorf("by-name resolution not available for %q", name)
		},
		resolveWithDiscoveryFunc: func(_ context.Context, name string, _ adapterhost.DiscoveryFunc, _ func(string, *exec.Cmd)) (adapterhost.Handle, error) {
			if name != testAdapterType {
				t.Errorf("ResolveWithDiscovery called with %q, want %q", name, testAdapterType)
			}
			resolveWithDiscoveryCalled = true
			return schemafulHandle(), nil
		},
	}

	schemas, diags := CollectSchemas(context.Background(), loader, dir, makeSpec(), nil)

	if !resolveWithDiscoveryCalled {
		t.Errorf("ResolveWithDiscovery was not called; the lockfile digest was not used for resolution")
	}

	info, ok := schemas[testAdapterType]
	if !ok {
		t.Fatalf("schemas missing %q; got %v", testAdapterType, schemas)
	}
	if _, ok := info.OutputSchema["baz"]; !ok {
		t.Errorf("output schema 'baz' not available; got %v", info.OutputSchema)
	}

	for _, d := range diags {
		t.Errorf("unexpected diagnostic: %s - %s", d.Summary, d.Detail)
	}
}

// TestCollectSchemas_ResolvedDigestMatchesLockfile asserts that the digest
// actually consulted during schema resolution is the one pinned in the lockfile.
func TestCollectSchemas_ResolvedDigestMatchesLockfile(t *testing.T) {
	setHermeticAdapterRoot(t)

	dir := t.TempDir()
	writeLockfile(t, dir)

	loader := &mockDiscoveryLoader{
		resolveFunc: func(_ context.Context, name string) (adapterhost.Handle, error) {
			return nil, fmt.Errorf("by-name resolution not available for %q", name)
		},
		resolveWithDiscoveryFunc: func(_ context.Context, name string, discover adapterhost.DiscoveryFunc, _ func(string, *exec.Cmd)) (adapterhost.Handle, error) {
			if name != testAdapterType {
				t.Errorf("ResolveWithDiscovery called with %q, want %q", name, testAdapterType)
			}
			path, err := discover(name)
			if err == nil {
				return nil, fmt.Errorf("expected discovery to fail for missing binary, got path %q", path)
			}
			var notFound *adapterhost.ErrAdapterNotFound
			if !errors.As(err, &notFound) {
				return nil, fmt.Errorf("expected ErrAdapterNotFound, got %T: %w", err, err)
			}
			wantPath := expectedDigestPath(t)
			found := false
			for _, searched := range notFound.Searched {
				if searched == wantPath {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("discovery searched %v, expected to include %q", notFound.Searched, wantPath)
			}
			return schemafulHandle(), nil
		},
	}

	schemas, diags := CollectSchemas(context.Background(), loader, dir, makeSpec(), nil)
	if _, ok := schemas[testAdapterType]; !ok {
		t.Fatalf("schemas missing %q", testAdapterType)
	}
	for _, d := range diags {
		t.Errorf("unexpected diagnostic: %s - %s", d.Summary, d.Detail)
	}
}

// TestCollectSchemas_NoOutputSchemaWarning asserts that an adapter that resolves
// but declares no output_schema produces a distinct warning.
func TestCollectSchemas_NoOutputSchemaWarning(t *testing.T) {
	setHermeticAdapterRoot(t)

	dir := t.TempDir()
	writeLockfile(t, dir)

	loader := &mockDiscoveryLoader{
		resolveFunc: func(_ context.Context, name string) (adapterhost.Handle, error) {
			return nil, fmt.Errorf("by-name resolution not available for %q", name)
		},
		resolveWithDiscoveryFunc: func(_ context.Context, name string, _ adapterhost.DiscoveryFunc, _ func(string, *exec.Cmd)) (adapterhost.Handle, error) {
			if name != testAdapterType {
				t.Errorf("ResolveWithDiscovery called with %q, want %q", name, testAdapterType)
			}
			return schemalessHandle(), nil
		},
	}

	schemas, diags := CollectSchemas(context.Background(), loader, dir, makeSpec(), nil)
	if _, ok := schemas[testAdapterType]; !ok {
		t.Fatalf("schemas missing %q", testAdapterType)
	}

	var found bool
	for _, d := range diags {
		if d.Severity != hcl.DiagWarning {
			t.Errorf("expected warning, got %v: %s - %s", d.Severity, d.Summary, d.Detail)
			continue
		}
		if !strings.Contains(d.Summary, "resolved but declares no output schema") {
			t.Errorf("unexpected warning summary: %s", d.Summary)
			continue
		}
		if !strings.Contains(d.Detail, testDigest) {
			t.Errorf("warning detail should name the resolved digest %q, got %q", testDigest, d.Detail)
		}
		found = true
	}
	if !found {
		t.Fatalf("expected 'declares no output schema' warning, got diagnostics: %v", diags)
	}
}

// TestCollectSchemas_UnverifiedWarningNamesSources asserts that a genuinely
// unresolvable adapter warning names the consulted sources and does not repeat
// the misleading "run criteria adapter lock" advice when a lockfile exists.
func TestCollectSchemas_UnverifiedWarningNamesSources(t *testing.T) {
	setHermeticAdapterRoot(t)

	dir := t.TempDir()
	writeLockfile(t, dir)

	loader := &mockDiscoveryLoader{
		resolveFunc: func(_ context.Context, name string) (adapterhost.Handle, error) {
			return nil, fmt.Errorf("adapter %q not found", name)
		},
		resolveWithDiscoveryFunc: func(_ context.Context, name string, _ adapterhost.DiscoveryFunc, _ func(string, *exec.Cmd)) (adapterhost.Handle, error) {
			return nil, fmt.Errorf("adapter %q not found at digest path", name)
		},
	}

	_, diags := CollectSchemas(context.Background(), loader, dir, makeSpec(), nil)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %v", len(diags), diags)
	}
	d := diags[0]
	if d.Severity != hcl.DiagWarning {
		t.Fatalf("expected warning, got %v", d.Severity)
	}
	if !strings.Contains(d.Summary, fmt.Sprintf("adapter %q schema unverified", testAdapterType)) {
		t.Errorf("summary %q should contain 'schema unverified'", d.Summary)
	}
	if !strings.Contains(d.Detail, "lockfile") {
		t.Errorf("detail should mention lockfile, got %q", d.Detail)
	}
	if !strings.Contains(d.Detail, testDigest) {
		t.Errorf("detail should name the locked digest, got %q", d.Detail)
	}
	if !strings.Contains(d.Detail, "OCI cache") {
		t.Errorf("detail should mention the OCI cache path for the lockfile digest, got %q", d.Detail)
	}
	if !strings.Contains(d.Detail, "attempted, not found") {
		t.Errorf("detail should note the digest-addressed cache path was attempted but not found, got %q", d.Detail)
	}
	if strings.Contains(d.Detail, "criteria adapter lock") {
		t.Errorf("detail should not suggest re-running lock when lockfile exists; got %q", d.Detail)
	}
}
