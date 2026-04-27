package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/brokenbots/overseer/internal/adapter/conformance"
)

var (
	testPluginBin string
	testEchoBin   string
)

func TestMain(m *testing.M) {
	testPluginBin, testEchoBin = buildPluginAndFixtureBinaries()
	os.Exit(m.Run())
}

func TestMCPPluginConformance(t *testing.T) {
	conformance.RunPlugin(
		t,
		"mcp",
		testPluginBin,
		conformance.Options{
			OpenConfig: map[string]string{
				"command": testEchoBin,
			},
			StepConfig: map[string]string{
				"tool":            "echo",
				"message":         "hello from conformance",
				"success_outcome": "success",
			},
			AllowedOutcomes: []string{"success", "failure", "needs_review"},
		},
	)
}

func buildPluginAndFixtureBinaries() (string, string) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	tmpDir := os.TempDir()
	pluginBin := filepath.Join(tmpDir, "overseer-adapter-mcp-test")
	echoBin := filepath.Join(tmpDir, "echo-mcp-test")

	buildPlugin := exec.Command("go", "build", "-o", pluginBin, "./cmd/overseer-adapter-mcp")
	buildPlugin.Dir = moduleRoot
	if out, err := buildPlugin.CombinedOutput(); err != nil {
		panic("build mcp plugin: " + err.Error() + "\n" + string(out))
	}

	buildFixture := exec.Command("go", "build", "-o", echoBin, "./cmd/overseer-adapter-mcp/testfixtures/echo-mcp")
	buildFixture.Dir = moduleRoot
	if out, err := buildFixture.CombinedOutput(); err != nil {
		panic("build echo fixture: " + err.Error() + "\n" + string(out))
	}

	return pluginBin, echoBin
}
