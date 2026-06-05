package manifest_test

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
)

// TestEmitManifest_RoundTripsThroughHostParser builds the in-tree noop adapter
// (which uses criteria-go-adapter-sdk/adapterhost.Serve), runs it with --emit-manifest, and parses
// the output with the host manifest parser. This proves the cross-module
// contract: the JSON the sdk emits is accepted as adapter.yaml by the host,
// even though the sdk module cannot import the host's manifest package.
func TestEmitManifest_RoundTripsThroughHostParser(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))

	bin := filepath.Join(t.TempDir(), "noop-adapter")
	build := exec.Command("go", "build", "-o", bin, "./internal/adapter/conformance/testdata/noop")
	build.Dir = moduleRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build noop adapter: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--emit-manifest").Output()
	if err != nil {
		t.Fatalf("run --emit-manifest: %v", err)
	}

	m, err := manifest.Parse(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("host parser rejected emitted manifest: %v\n%s", err, out)
	}

	// The emitted manifest must also pass full validation — otherwise the host
	// would reject it at pull time (this is the publish→pull contract).
	if err := m.Validate(); err != nil {
		t.Fatalf("emitted manifest failed Validate(): %v\n%s", err, out)
	}

	if m.Name != "noop" {
		t.Errorf("name = %q, want noop", m.Name)
	}
	if m.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", m.SchemaVersion)
	}
	if m.SDKProtocolVersion != 2 {
		t.Errorf("sdk_protocol_version = %d, want 2", m.SDKProtocolVersion)
	}
	if len(m.Capabilities) == 0 {
		t.Error("expected capabilities to be carried through")
	}
}
