package cli

import (
	"fmt"
	"io/fs"
	"path"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

// validManifest renders an adapter.yaml that passes Manifest.Validate.
func validManifest(name string) string {
	return fmt.Sprintf(`schema_version: 1
name: %s
version: 0.5.1
source_url: https://github.com/brokenbots/example
sdk_protocol_version: 2
platforms:
  - os: %s
    arch: %s
`, name, runtime.GOOS, runtime.GOARCH)
}

func hostBinDir() string { return path.Join("bin", runtime.GOOS, runtime.GOARCH) }

// The workflow's adapter label is an alias chosen by the workflow author. It
// must not be used to address the binary inside the artifact — the artifact
// publishes its own name. This is the regression: a workflow declaring
// `adapter "claude" "main"` against an artifact publishing "claude-agent" used
// to fail with `open bin/<os>/<arch>/criteria-adapter-claude: file does not exist`.
func TestArtifactBinaryPath_IgnoresWorkflowLabel(t *testing.T) {
	artifact := fstest.MapFS{
		"adapter.yaml": {Data: []byte(validManifest("claude-agent"))},
		path.Join(hostBinDir(), "criteria-adapter-claude-agent"): {Data: []byte("ELF")},
	}

	got, err := artifactBinaryPath(artifact)
	if err != nil {
		t.Fatalf("artifactBinaryPath: %v", err)
	}
	want := path.Join(hostBinDir(), "criteria-adapter-claude-agent")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// An artifact whose binary basename follows no convention still resolves, so
// long as it publishes exactly one file for this platform.
func TestArtifactBinaryPath_SoleFileWithoutManifestMatch(t *testing.T) {
	artifact := fstest.MapFS{
		"adapter.yaml": {Data: []byte(validManifest("greeter"))},
		path.Join(hostBinDir(), "some-oddly-named-binary"): {Data: []byte("ELF")},
	}

	got, err := artifactBinaryPath(artifact)
	if err != nil {
		t.Fatalf("artifactBinaryPath: %v", err)
	}
	want := path.Join(hostBinDir(), "some-oddly-named-binary")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// When several files ship for a platform, the manifest name disambiguates.
func TestArtifactBinaryPath_ManifestNameDisambiguates(t *testing.T) {
	artifact := fstest.MapFS{
		"adapter.yaml": {Data: []byte(validManifest("greeter"))},
		path.Join(hostBinDir(), "criteria-adapter-greeter"): {Data: []byte("ELF")},
		path.Join(hostBinDir(), "README"):                   {Data: []byte("notes")},
	}

	got, err := artifactBinaryPath(artifact)
	if err != nil {
		t.Fatalf("artifactBinaryPath: %v", err)
	}
	want := path.Join(hostBinDir(), "criteria-adapter-greeter")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Ambiguity the manifest cannot resolve is an error, not an arbitrary pick.
func TestArtifactBinaryPath_AmbiguousIsError(t *testing.T) {
	artifact := fstest.MapFS{
		"adapter.yaml":                        {Data: []byte(validManifest("greeter"))},
		path.Join(hostBinDir(), "binary-one"): {Data: []byte("ELF")},
		path.Join(hostBinDir(), "binary-two"): {Data: []byte("ELF")},
	}

	_, err := artifactBinaryPath(artifact)
	if err == nil {
		t.Fatal("expected an error for an ambiguous artifact")
	}
	if !strings.Contains(err.Error(), "must select one") {
		t.Errorf("error should tell the publisher how to fix it, got: %v", err)
	}
}

// A platform miss should say which platforms the artifact does ship.
func TestArtifactBinaryPath_MissingPlatformListsPublished(t *testing.T) {
	artifact := fstest.MapFS{
		"adapter.yaml": {Data: []byte(validManifest("greeter"))},
		"bin/plan9/mips/criteria-adapter-greeter": {Data: []byte("ELF")},
	}

	_, err := artifactBinaryPath(artifact)
	if err == nil {
		t.Fatal("expected an error when the host platform is not published")
	}
	if !strings.Contains(err.Error(), "plan9/mips") {
		t.Errorf("error should list published platforms, got: %v", err)
	}
	if !strings.Contains(err.Error(), runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("error should name the platform we wanted, got: %v", err)
	}
}

// A manifest name is adapter-controlled input that we join onto a path, so a
// traversing name must not be honoured. The "criteria-adapter-" prefix absorbs
// one ".." level, so three escape the platform directory: the name below cleans
// to bin/<os>/evil, a real file here. The test therefore fails if the guard is
// removed, rather than merely resolving to a path that happens not to exist.
const traversalName = "../../../evil"

func TestArtifactBinaryPath_RejectsTraversalInManifestName(t *testing.T) {
	escapeTarget := path.Join("bin", runtime.GOOS, "evil")
	if got := path.Join(hostBinDir(), "criteria-adapter-"+traversalName); got != escapeTarget {
		t.Fatalf("premise broken: traversal cleans to %q, not %q", got, escapeTarget)
	}

	evil := strings.Replace(validManifest("greeter"), "name: greeter", "name: "+traversalName, 1)
	artifact := fstest.MapFS{
		"adapter.yaml": {Data: []byte(evil)},
		escapeTarget:   {Data: []byte("PWNED")},
		path.Join(hostBinDir(), "criteria-adapter-greeter"): {Data: []byte("ELF")},
	}

	got, err := artifactBinaryPath(artifact)
	if err != nil {
		t.Fatalf("artifactBinaryPath: %v", err)
	}
	if got == escapeTarget {
		t.Fatalf("manifest name escaped its platform directory to %q", got)
	}
	// The invalid manifest name is discarded, and the sole-file scan wins.
	want := path.Join(hostBinDir(), "criteria-adapter-greeter")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !fs.ValidPath(got) {
		t.Errorf("resolved path %q is not a valid artifact path", got)
	}
}
