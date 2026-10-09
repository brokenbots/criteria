// Static guard over the golang builder pins of the images/remote-adapters
// Dockerfiles (KB-229).
package smoke

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// The official golang builder images set GOTOOLCHAIN=local, so a module whose
// `go` directive is newer than the builder's toolchain fails the docker build
// outright ("requires go >= 1.26.9 (running go 1.26.6; GOTOOLCHAIN=local)").
// Every `FROM golang:*` builder in this file set must therefore be exact-pinned
// (no floating tags) at or above the strictest module it has to install.
//
// copilotModuleFloor is the toolchain floor demanded by
// github.com/brokenbots/criteria-adapter-copilot as of v0.5.16 (KB-229). When
// the adapter module raises its floor, re-verify its @v<version>.mod on the Go
// module proxy and raise this constant together with the builder FROM pin.
var copilotModuleFloor = goVersion{major: 1, minor: 26, patch: 9}

const copilotFloorReason = "criteria-adapter-copilot v0.5.16 requires go >= 1.26.9 (KB-229)"

// goVersion is a (major, minor, patch) go toolchain version.
type goVersion struct{ major, minor, patch int }

func (v goVersion) String() string { return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch) }

func (v goVersion) atLeast(o goVersion) bool {
	if v.major != o.major {
		return v.major > o.major
	}
	if v.minor != o.minor {
		return v.minor > o.minor
	}
	return v.patch >= o.patch
}

// floorSpec is one toolchain floor: the version and why it applies.
type floorSpec struct {
	version goVersion
	reason  string
}

// goFloorsFromRepoRoot derives the repo module floor from go.mod's `go`
// directive. Files not listed in the table below default to that floor.
func goFloorsFromRepoRoot(t *testing.T) map[string][]floorSpec {
	t.Helper()
	repoRoot := findModuleRoot(t)
	repoFloor := floorSpec{
		version: goFloorFromGoMod(t, filepath.Join(repoRoot, "go.mod")),
		reason:  "root go.mod `go` directive",
	}

	return map[string][]floorSpec{
		// Both images run `go install criteria-adapter-<name>@v*` (Dockerfile
		// again via the publish-remote-adapter-images copilot matrix leg), so
		// the builders must also satisfy the copilot module floor.
		"images/remote-adapters/Dockerfile.peer": {repoFloor, {copilotModuleFloor, copilotFloorReason}},
		"images/remote-adapters/Dockerfile":      {repoFloor, {copilotModuleFloor, copilotFloorReason}},
		// claude-agent's builder compiles only the in-repo remote-runner and
		// installs no external adapter module.
		"images/remote-adapters/Dockerfile.claude-agent": {repoFloor},
	}
}

// TestRemoteAdapterImageBuilderToolchain asserts that every `FROM golang:*`
// builder under images/remote-adapters is exact-pinned to a patch version
// that satisfies the toolchain floors of the modules each image installs.
func TestRemoteAdapterImageBuilderToolchain(t *testing.T) {
	repoRoot := findModuleRoot(t)
	floorsByFile := goFloorsFromRepoRoot(t)

	paths, err := filepath.Glob(filepath.Join(repoRoot, "images", "remote-adapters", "Dockerfile*"))
	if err != nil {
		t.Fatalf("glob remote-adapter Dockerfiles: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no Dockerfiles found under images/remote-adapters/")
	}
	for _, path := range paths {
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			t.Fatalf("rel %q: %v", path, err)
		}
		t.Run(rel, func(t *testing.T) {
			floors, ok := floorsByFile[rel]
			if !ok {
				// A new Dockerfile in this set defaults to the repo floor;
				// extend the table when it installs adapter modules.
				floors = floorsByFile["images/remote-adapters/Dockerfile.claude-agent"]
			}
			testBuilderPins(t, rel, floors)
		})
	}
}

const (
	floatingTag = ":floating:" // marker for a golang tag without an exact version
	digestPin   = ":digest:"   // golang@sha256:<digest> form
)

// goBuilder is one `FROM golang:<tag> [AS <stage>]` line in a Dockerfile.
type goBuilder struct {
	tagFull string // everything after "golang:", e.g. "1.26.9-bookworm"
	image   string // "golang:<tag>" as written
	stage   string
	tag     string
}

// goBuilderPins extracts every `FROM golang:*` line from a Dockerfile.
func goBuilderPins(dockerfile string) []goBuilder {
	versionRe := regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-[a-z0-9.-]+)?$`)
	digestRe := regexp.MustCompile(`^@sha256:[0-9a-f]{64}$`)
	re := regexp.MustCompile(`(?m)^FROM golang:(\S+)(?:\s+AS\s+(\S+))?\s*$`)

	matches := re.FindAllStringSubmatch(dockerfile, -1)
	pins := make([]goBuilder, 0, len(matches))
	for _, m := range matches {
		pin := goBuilder{tagFull: m[1], image: "golang:" + m[1], stage: m[2]}
		if pin.stage == "" {
			pin.stage = "(unnamed)"
		}
		switch {
		case versionRe.MatchString(pin.tagFull):
			pin.tag = pin.tagFull
		case digestRe.MatchString(pin.tagFull):
			pin.tag = digestPin // exact pin, but the floor cannot be checked
		default:
			pin.tag = floatingTag
		}
		pins = append(pins, pin)
	}
	return pins
}

// effectiveFloor returns the strictest floor and the reason it applies.
func effectiveFloor(floors []floorSpec) (effective goVersion, why string) {
	effective, why = goVersion{}, "no floors declared"
	for _, f := range floors {
		if f.version.atLeast(effective) {
			effective, why = f.version, f.reason
		}
	}
	return effective, why
}

func testBuilderPins(t *testing.T, rel string, floors []floorSpec) {
	t.Helper()
	repoRoot := findModuleRoot(t)

	raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}

	pins := goBuilderPins(string(raw))
	if len(pins) == 0 {
		t.Fatalf("%s has no `FROM golang:*` builder stage; if the image no longer builds from source, drop its entry from the floor table", rel)
	}

	effective, why := effectiveFloor(floors)
	for _, pin := range pins {
		switch pin.tag {
		case digestPin:
			continue
		case floatingTag:
			t.Errorf("%s: builder stage %q uses floating golang tag %s; floating tags are forbidden — pin an exact patch version (e.g. golang:%s-bookworm)", rel, pin.stage, pin.image, effective)
		default:
			ver, ok := parseExactGoVersion(pin.tag)
			if !ok || !ver.atLeast(effective) {
				t.Errorf("%s: builder stage %q pins golang %s, not the required exact pin at or above floor %s (%s); bump the builder with the pin discipline", rel, pin.stage, pin.image, effective, why)
			}
		}
	}
}

// parseExactGoVersion parses "1.26.9-bookworm" ("1.26.9") and rejects
// floating or malformed tags ("1.26", "latest", digest form, ...).
func parseExactGoVersion(tag string) (goVersion, bool) {
	m := regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-[a-z0-9.-]+)?$`).FindStringSubmatch(tag)
	if m == nil {
		return goVersion{}, false
	}
	v, err := newGoVersion(m[1], m[2], m[3])
	return v, err == nil
}

// goFloorFromGoMod parses the `go` directive of a go.mod ("go 1.26.6");
// a missing patch component means patch 0.
func goFloorFromGoMod(t *testing.T, path string) goVersion {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := regexp.MustCompile(`(?m)^go (\d+)\.(\d+)(?:\.(\d+))?\s*$`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s: no `go <major>.<minor>[.<patch>]` directive found", path)
	}
	patch := "0"
	if len(m[3]) > 0 {
		patch = string(m[3])
	}
	v, err := newGoVersion(string(m[1]), string(m[2]), patch)
	if err != nil {
		t.Fatalf("%s: parse go directive: %v", path, err)
	}
	return v
}

func newGoVersion(major, minor, patch string) (goVersion, error) {
	majorI, err := strconv.Atoi(major)
	if err != nil {
		return goVersion{}, fmt.Errorf("invalid go major %q", major)
	}
	minorI, err := strconv.Atoi(minor)
	if err != nil {
		return goVersion{}, fmt.Errorf("invalid go minor %q", minor)
	}
	patchI, err := strconv.Atoi(patch)
	if err != nil {
		return goVersion{}, fmt.Errorf("invalid go patch %q", patch)
	}
	return goVersion{major: majorI, minor: minorI, patch: patchI}, nil
}
