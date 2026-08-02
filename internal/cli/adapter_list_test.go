package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestListInstalledIncludesFilesystemAdapters is the regression test for the
// adapter-list change. It builds the in-tree noop fixture, installs it as a
// filesystem adapter under a fresh CRITERIA_HOME, and asserts that
// `criteria adapter list` reports it instead of printing "(no cached adapters)".
func TestListInstalledIncludesFilesystemAdapters(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRITERIA_HOME", root)
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("CRITERIA_ADAPTERS", "")

	adaptersDir := filepath.Join(root, "adapters")
	require.NoError(t, os.MkdirAll(adaptersDir, 0o755))

	binPath := filepath.Join(adaptersDir, "criteria-adapter-noop")
	buildNoopFixture(t, binPath)

	var out bytes.Buffer
	require.NoError(t, runList(&out, true, false))

	got := out.String()
	require.Contains(t, got, "noop")
	require.Contains(t, got, "0.1.0")
	require.Contains(t, got, binPath)
	require.NotContains(t, got, "(no cached adapters)")
}

// TestListInstalled_MarksUnresponsiveAndContinues verifies that a broken
// installed adapter is listed as "name (unresponsive)" and does not prevent
// working adapters from appearing. A regression here would crash the listing
// or silently drop the broken entry.
func TestListInstalled_MarksUnresponsiveAndContinues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-style executable script not portable to windows")
	}

	root := t.TempDir()
	t.Setenv("CRITERIA_HOME", root)
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("CRITERIA_ADAPTERS", "")

	adaptersDir := filepath.Join(root, "adapters")
	require.NoError(t, os.MkdirAll(adaptersDir, 0o755))

	noopPath := filepath.Join(adaptersDir, "criteria-adapter-noop")
	buildNoopFixture(t, noopPath)

	brokenPath := filepath.Join(adaptersDir, "criteria-adapter-broken")
	require.NoError(t, os.WriteFile(brokenPath, []byte("#!/bin/sh\nexit 1\n"), 0o755))

	var out bytes.Buffer
	require.NoError(t, runList(&out, true, false))

	got := out.String()
	require.Contains(t, got, "noop")
	require.Contains(t, got, "0.1.0")
	require.Contains(t, got, noopPath)
	require.Contains(t, got, "broken (unresponsive)")
	require.Contains(t, got, brokenPath)
	require.NotContains(t, got, "(no cached adapters)")
}

func buildNoopFixture(t *testing.T, outPath string) {
	t.Helper()
	repoRoot, err := filepath.Abs("../..")
	require.NoError(t, err)

	cmd := exec.Command("go", "build", "-o", outPath, "./internal/adapter/conformance/testdata/noop/main.go")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build noop fixture: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
