package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

func TestResolveFromManifest(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "adapter.yaml")
	data := `schema_version: 1
name: demo
version: 0.2.0
source_url: https://example.com
`
	if err := os.WriteFile(manifestPath, []byte(data), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cfg := remoteConfig{Manifest: manifestPath, Host: "host:7778"}
	if err := cfg.resolve(slogDiscard()); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Name != "demo" {
		t.Errorf("name = %q, want demo", cfg.Name)
	}
	if cfg.Version != "0.2.0" {
		t.Errorf("version = %q, want 0.2.0", cfg.Version)
	}
	if cfg.Binary != "/usr/local/bin/criteria-adapter-demo" {
		t.Errorf("binary = %q, want /usr/local/bin/criteria-adapter-demo", cfg.Binary)
	}
}

func TestNameFromBinary(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/usr/local/bin/criteria-adapter-shell", "shell"},
		{"criteria-adapter-copilot", "copilot"},
		{"/foo/bar", "bar"},
	}
	for _, c := range cases {
		if got := nameFromBinary(c.in); got != c.want {
			t.Errorf("nameFromBinary(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAdapterEnvFiltersRemoteVars(t *testing.T) {
	for _, name := range remoteEnvVars {
		t.Setenv(name, "leaked-"+name)
	}
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("CRITERIA_KEEP_ME", "still-here")

	got := adapterEnv()
	for _, kv := range got {
		name, _, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		if slicesContains(remoteEnvVars, name) {
			t.Errorf("adapterEnv leaked remote variable %s", name)
		}
	}

	var hasKeep bool
	for _, kv := range got {
		if kv == "CRITERIA_KEEP_ME=still-here" {
			hasKeep = true
			break
		}
	}
	if !hasKeep {
		t.Error("adapterEnv removed unrelated variable CRITERIA_KEEP_ME")
	}
}

// TestStartAdapterFiltersRemoteEnvFromChild is a regression test for CRI-109.
// The remote runner must not let its own CRITERIA_REMOTE_* configuration leak
// into the child adapter process. Real adapters detect CRITERIA_REMOTE_HOST and
// switch into phone-home/ServeRemote mode, which abandons the local go-plugin
// handshake and causes the runner's StartTimeout to expire. The fixture child
// fails fast if any remote variable leaks through; with the fix, the local
// handshake completes and Info succeeds well within the 30-second timeout.
func TestStartAdapterFiltersRemoteEnvFromChild(t *testing.T) {
	mockBin := buildMockChildAdapter(t)

	for _, name := range remoteEnvVars {
		t.Setenv(name, "runner-"+name)
	}
	t.Setenv("PATH", os.Getenv("PATH"))

	client, kill, err := startAdapter(mockBin)
	if err != nil {
		t.Fatalf("startAdapter: %v", err)
	}
	defer kill()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := client.Info(ctx, &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.GetName() != "remote-detecting-mock" {
		t.Errorf("adapter name = %q, want remote-detecting-mock", info.GetName())
	}
	if info.GetVersion() != "0.1.0" {
		t.Errorf("adapter version = %q, want 0.1.0", info.GetVersion())
	}
}

// buildMockChildAdapter compiles the remote-detecting mock adapter used by
// TestStartAdapterFiltersRemoteEnvFromChild. The source lives in testdata so
// the build inherits the root module's dependencies.
func buildMockChildAdapter(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "remote-detecting-mock")

	moduleRoot, err := findModuleRoot()
	if err != nil {
		t.Fatalf("find module root: %v", err)
	}

	cmd := exec.Command("go", "build", "-o", bin, "./cmd/criteria-adapter-remote-runner/testdata/remote-detecting-mock")
	cmd.Dir = moduleRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mock child adapter: %v\n%s", err, string(out))
	}
	return bin
}

// findModuleRoot returns the repository root by walking up from the current
// working directory until it finds a go.mod file.
func findModuleRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := cwd; dir != "/"; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
	}
	return "", errors.New("go.mod not found")
}

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
