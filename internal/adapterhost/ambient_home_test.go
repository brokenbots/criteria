package adapterhost

// Regression tests for KB-224 (moved from Linear CRI-292, mirrored in
// criteria-adapter-copilot#31): locally launched copilot adapters must get a
// fresh ambient home per criteria invocation, while shell-bound and
// sandbox-bound launches keep their current environment handling.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// envMap turns a command environment slice into a lookup map.
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	return m
}

// copilotGraph returns a graph with the given adapter instances plus optional
// environments, wired for buildCommandCustomizer tests.
func copilotGraph(adapters map[string]string, envs map[string]*workflow.EnvironmentNode) *workflow.FSMGraph {
	g := &workflow.FSMGraph{
		Adapters:     map[string]*workflow.AdapterNode{},
		Environments: envs,
	}
	for id, envKey := range adapters {
		node := &workflow.AdapterNode{Type: "copilot", Name: id}
		if envKey != "" {
			node.Name = strings.TrimPrefix(id, "copilot.")
			node.Environment = envKey
		}
		g.Adapters[id] = node
	}
	return g
}

func TestBuildCommandCustomizer_CopilotAmbientHomeIsolated(t *testing.T) {
	sm := NewSessionManager(nil)
	sm.graph = copilotGraph(map[string]string{"copilot.main": ""}, nil)

	customizer, cleanup, err := sm.buildCommandCustomizer("copilot.main", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if customizer == nil {
		t.Fatal("expected non-nil customizer: copilot launches must be isolated (KB-224)")
	}
	cmd := exec.Command("true")
	customizer("copilot.main", cmd)

	env := envMap(cmd.Env)
	home, ok := env["HOME"]
	if !ok {
		t.Fatal("expected HOME in launch environment")
	}
	if hostHome := os.Getenv("HOME"); hostHome != "" && home == hostHome {
		t.Fatalf("launch HOME %q unchanged: expected a fresh per-invocation scratch home", home)
	}
	if !strings.Contains(filepath.Base(filepath.Dir(home)), "criteria-ambient-home-") {
		t.Errorf("HOME %q: expected a scratch home under a criteria-ambient-home- root", home)
	}
	if instance := env[ambientHomeInstanceIDEnv]; instance != "copilot.main" {
		t.Errorf("ambientHomeInstanceIDEnv = %q, want %q", instance, "copilot.main")
	}
	if path := env["PATH"]; path != os.Getenv("PATH") {
		t.Errorf("PATH not preserved from host environment: %q", path)
	}
	info, err := os.Stat(home)
	if err != nil {
		t.Fatalf("stat scratch home: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("scratch home mode = %o, want 700", got)
	}

	// The per-launch cleanup is a no-op by contract: the same instance's
	// scratch home outlives launches within the invocation and must still
	// exist here.
	if cleanup == nil {
		t.Fatal("expected non-nil cleanup")
	}
	cleanup()
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("scratch home removed by per-launch cleanup: %v", err)
	}

	// Shutdown removes the invocation's scratch root.
	if err := sm.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(home)); !os.IsNotExist(err) {
		t.Errorf("scratch root still present after shutdown: %v", err)
	}
}

func TestBuildAmbientHomeCustomizer_AllocatorDisabled(t *testing.T) {
	sm := &SessionManager{}
	if customizer, _ := sm.buildAmbientHomeCustomizer("copilot.main"); customizer != nil {
		t.Fatal("expected nil customizer when allocator is disabled")
	}
}

func TestBuildCommandCustomizer_AmbientHomeStablePerInstance(t *testing.T) {
	sm := NewSessionManager(nil)
	sm.graph = copilotGraph(map[string]string{"copilot.main": "", "copilot.helper": ""}, nil)

	customizer, _, err := sm.buildCommandCustomizer("copilot.main", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cmd1 := exec.Command("true")
	customizer("copilot.main", cmd1)

	// The same instance keeps the same home across steps; a different
	// instance gets a distinct directory.
	for _, id := range []string{"copilot.main", "copilot.main", "copilot.helper"} {
		cust, _, err := sm.buildCommandCustomizer(id, "")
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", id, err)
		}
		cmd := exec.Command("true")
		cust(id, cmd)
		if got, want := envMap(cmd.Env)["HOME"], envMap(cmd1.Env)["HOME"]; id == "copilot.main" && got != want {
			t.Errorf("HOME for %q changed between launches: %q then %q", id, want, got)
		}
		if envMap(cmd.Env)["HOME"] == envMap(cmd1.Env)["HOME"] && id != "copilot.main" {
			t.Errorf("different instances share one scratch home")
		}
	}
}

func TestAmbientHomeFreshAcrossManagers(t *testing.T) {
	// The regression: before KB-224 the second criteria invocation reused the
	// copilot CLI's ambient state because both invocations shared $HOME.
	// Two SessionManager invocations must therefore never hand out the same
	// scratch home.
	makeHome := func(t *testing.T) string {
		t.Helper()
		sm := NewSessionManager(nil)
		sm.graph = copilotGraph(map[string]string{"copilot.main": ""}, nil)
		customizer, _, err := sm.buildCommandCustomizer("copilot.main", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		cmd := exec.Command("true")
		customizer("copilot.main", cmd)
		home := envMap(cmd.Env)["HOME"]
		if err := sm.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		return home
	}
	if first, second := makeHome(t), makeHome(t); first == second {
		t.Fatalf("two invocations share one ambient home %q: ambient session state would cross invocations", first)
	}
}

func TestBuildCommandCustomizer_ShellAdapterKeepsHostHome(t *testing.T) {
	// Shell-type adapters keep the host environment: their steps rely on
	// operator configuration (git/gh credentials, caches) living in the real
	// home, and the KB-224 incident never affected them.
	sm := NewSessionManager(nil)
	sm.graph = &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"shell.default": {Type: "shell", Name: "default", Environment: "shell.ci"},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"shell.ci": {Type: "shell", Name: "ci"},
		},
	}
	customizer, cleanup, err := sm.buildCommandCustomizer("shell.default", "/tmp/worktree")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cleanup != nil {
		t.Fatal("expected nil cleanup for non-sandbox env")
	}
	cmd := exec.Command("true")
	customizer("shell", cmd)
	if cmd.Dir != "/tmp/worktree" {
		t.Errorf("cmd.Dir = %q, want %q", cmd.Dir, "/tmp/worktree")
	}
	if got, want := envMap(cmd.Env)["HOME"], os.Getenv("HOME"); got != want {
		t.Errorf("shell launch HOME = %q, want host HOME %q", got, want)
	}
}

func TestBuildCommandCustomizer_CopilotWithShellEnvIsolated(t *testing.T) {
	// A copilot adapter bound to a shell-typed environment is still locally
	// launched and still isolated; the environment type only gates sandbox-
	// and container-style environments out.
	sm := NewSessionManager(nil)
	sm.graph = copilotGraph(
		map[string]string{"copilot.main": "shell.ci"},
		map[string]*workflow.EnvironmentNode{"shell.ci": {Type: "shell", Name: "ci"}},
	)
	customizer, _, err := sm.buildCommandCustomizer("copilot.main", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if customizer == nil {
		t.Fatal("expected isolation for a copilot adapter bound to a shell environment")
	}
	cmd := exec.Command("true")
	customizer("copilot.main", cmd)
	if home := envMap(cmd.Env)["HOME"]; home == os.Getenv("HOME") {
		t.Errorf("copilot launch reuses host HOME despite shell binding")
	}
}

func TestBuildAmbientHomeCustomizer_NonIsolatedAdapterTypes(t *testing.T) {
	sm := NewSessionManager(nil)
	sm.graph = &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"noop.default": {Type: "noop", Name: "default"},
			"mcp.default":  {Type: "mcp", Name: "default"},
		},
	}
	for _, id := range []string{"noop.default", "mcp.default"} {
		if customizer, _ := sm.buildAmbientHomeCustomizer(id); customizer != nil {
			t.Errorf("adapter %q: expected no ambient-home isolation", id)
		}
	}
}

func TestBuildAmbientHomeCustomizer_SandboxBoundCopilotNotAmbient(t *testing.T) {
	// A sandbox-bound copilot adapter is excluded: the sandbox customizer owns
	// the launch environment entirely, and compose must not let the ambient
	// customizer run after the sandbox scrub.
	sm := NewSessionManager(nil)
	sm.graph = &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"copilot.default": {Type: "copilot", Name: "default", Environment: "sandbox.default"},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"sandbox.default": {Type: "sandbox", Name: "default"},
		},
	}
	if customizer, _ := sm.buildAmbientHomeCustomizer("copilot.default"); customizer != nil {
		t.Fatal("expected no ambient-home isolation for a sandbox-bound copilot adapter")
	}
}

func TestBuildAmbientHomeCustomizer_ContainerBoundCopilotNotAmbient(t *testing.T) {
	sm := NewSessionManager(nil)
	sm.graph = copilotGraph(
		map[string]string{"copilot.main": "docker.default"},
		map[string]*workflow.EnvironmentNode{"docker.default": {Type: "container", Name: "default"}},
	)
	if customizer, _ := sm.buildAmbientHomeCustomizer("copilot.main"); customizer != nil {
		t.Fatal("expected no ambient-home isolation for a container-bound copilot adapter")
	}
}

func TestAmbientHomeAllocator_CleanupIsIdempotent(t *testing.T) {
	a := &ambientHomeAllocator{}
	home, ok, err := a.homeFor("copilot.main", "copilot")
	if err != nil || !ok {
		t.Fatalf("homeFor: %q, ok=%v, err=%v", home, ok, err)
	}
	a.cleanup()
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("scratch home still present after cleanup: %v", err)
	}
	a.cleanup() // second call must not panic or remove anything unexpected
}

func TestAmbientHomeAllocator_OtherAdapterTypesDisabled(t *testing.T) {
	a := &ambientHomeAllocator{}
	if _, ok, err := a.homeFor("shell.default", "shell"); ok || err != nil {
		t.Errorf("shell adapter: ok=%v, err=%v, want disabled", ok, err)
	}
	if _, ok, err := a.homeFor("noop.default", "noop"); ok || err != nil {
		t.Errorf("noop adapter: ok=%v, err=%v, want disabled", ok, err)
	}
}

func TestAmbientHomeAllocator_InstanceIDFallbackType(t *testing.T) {
	// Callers without a graph-verified type rely on the instance ID prefix.
	a := &ambientHomeAllocator{}
	if _, ok, err := a.homeFor("copilot.main", ""); err != nil || !ok {
		t.Fatalf("instance-ID fallback: ok=%v, err=%v, want enabled", ok, err)
	}
	customRoot := t.TempDir()
	b := &ambientHomeAllocator{root: customRoot}
	home, ok, err := b.homeFor("copilot.alt", "")
	if err != nil || !ok {
		t.Fatalf("explicit root: ok=%v, err=%v", ok, err)
	}
	if home != filepath.Join(customRoot, "copilot.alt") {
		t.Errorf("home = %q, want %q", home, filepath.Join(customRoot, "copilot.alt"))
	}
}

func TestSanitizeAmbientHomeInstanceID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"copilot.main", "copilot.main"},
		{"copilot/a b", "copilot_a_b"},
		{"", "adapter"},
	}
	for _, tt := range tests {
		if got := sanitizeAmbientHomeInstanceID(tt.in); got != tt.want {
			t.Errorf("sanitizeAmbientHomeInstanceID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestReplaceEnvValue(t *testing.T) {
	env := []string{"PATH=/usr/bin", "HOME=/home/criteria", "OTHER=x"}
	got := envMap(replaceEnvValue(env, "HOME", "/scratch/home"))
	if got["HOME"] != "/scratch/home" || got["PATH"] != "/usr/bin" || got["OTHER"] != "x" {
		t.Errorf("replaceEnvValue = %v", got)
	}
	appended := envMap(replaceEnvValue([]string{"PATH=/usr/bin"}, "HOME", "/scratch"))
	if appended["HOME"] != "/scratch" {
		t.Errorf("absent key not appended: %v", appended)
	}
}

// fakeOperatorHome builds an operator home with every credential/config entry
// the scratch home preserves, and returns its path.
func fakeOperatorHome(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(source, rel), []byte(content), mode); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	mkdir := func(rel string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(source, rel), mode); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
	}
	write(".gitconfig", "[user]\n\tname = Operator\n", 0o600)
	write(".git-credentials", "https://token@example.com\n", 0o600)
	write(".netrc", "machine example.com\n", 0o600)
	mkdir(".ssh", 0o700)
	write(".ssh/id_ed25519", "KEY", 0o600)
	mkdir(".config/git", 0o700)
	write(".config/git/config", "[alias]\n", 0o600)
	mkdir(".config/gh", 0o700)
	write(".config/gh/hosts.yml", "github.com:\n", 0o600)
	return source
}

func TestBuildCommandCustomizer_CopilotAmbientHomePreservesCredentials(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	source := fakeOperatorHome(t)
	sm := NewSessionManager(nil)
	sm.ambientHomes.sourceHome = source
	sm.graph = copilotGraph(map[string]string{"copilot.main": ""}, nil)

	customizer, _, err := sm.buildCommandCustomizer("copilot.main", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if customizer == nil {
		t.Fatal("expected non-nil customizer")
	}
	cmd := exec.Command("true")
	customizer("copilot.main", cmd)

	env := envMap(cmd.Env)
	// The credential pointers preserve operator identity without copying
	// content: git reads the operator's global config, gh its config dir.
	if expected := filepath.Join(source, ".gitconfig"); os.Getenv("GIT_CONFIG_GLOBAL") == "" {
		if env["GIT_CONFIG_GLOBAL"] != expected {
			t.Errorf("GIT_CONFIG_GLOBAL = %q, want operator path %q", env["GIT_CONFIG_GLOBAL"], expected)
		}
	} else if env["GIT_CONFIG_GLOBAL"] != os.Getenv("GIT_CONFIG_GLOBAL") {
		t.Errorf("GIT_CONFIG_GLOBAL = %q: operator-provided value must not be overridden", env["GIT_CONFIG_GLOBAL"])
	}
	if expected := filepath.Join(source, ".config", "gh"); os.Getenv("GH_CONFIG_DIR") == "" {
		if env["GH_CONFIG_DIR"] != expected {
			t.Errorf("GH_CONFIG_DIR = %q, want operator path %q", env["GH_CONFIG_DIR"], expected)
		}
	} else if env["GH_CONFIG_DIR"] != os.Getenv("GH_CONFIG_DIR") {
		t.Errorf("GH_CONFIG_DIR = %q: operator-provided value must not be overridden", env["GH_CONFIG_DIR"])
	}

	// HOME-relative tool state is linked (never copied) into the scratch home.
	home := env["HOME"]
	for _, rel := range []string{".git-credentials", ".netrc", ".ssh", filepath.Join(".config", "git")} {
		link, err := os.Readlink(filepath.Join(home, rel))
		if err != nil {
			t.Fatalf("scratch home entry %s: %v", rel, err)
		}
		if want := filepath.Join(source, rel); link != want {
			t.Errorf("scratch %s -> %q, want %q", rel, link, want)
		}
	}
	// Ambient session state itself must stay fresh: no ~/.copilot anywhere.
	if _, err := os.Lstat(filepath.Join(home, ".copilot")); !os.IsNotExist(err) {
		t.Errorf("scratch home contains .copilot: isolation would leak session state")
	}
	if _, err := os.Lstat(filepath.Join(source, ".ssh", "id_ed25519")); err != nil {
		t.Fatalf("operator home must be linked, not copied: %v", err)
	}
}

func TestBuildCommandCustomizer_CopilotWithWorkingDirStaysIsolated(t *testing.T) {
	// The primary local-launch composition: an eligible copilot adapter with a
	// non-empty environment working_directory. The launch must keep the
	// working directory AND the fresh scratch home with a fully populated
	// environment (skip its host-env re-addition because a customizer is
	// active; an empty cmd.Env here would launch the adapter with none).
	workingDir := t.TempDir()
	sm := NewSessionManager(nil)
	sm.graph = copilotGraph(map[string]string{"copilot.main": ""}, nil)

	customizer, _, err := sm.buildCommandCustomizer("copilot.main", workingDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if customizer == nil {
		t.Fatal("expected non-nil customizer for a non-empty working directory")
	}
	var firstHome string
	for launch := 0; launch < 2; launch++ {
		cmd := exec.Command("true")
		customizer("copilot.main", cmd)
		if cmd.Dir != workingDir {
			t.Errorf("launch %d: cmd.Dir = %q, want working directory %q", launch, cmd.Dir, workingDir)
		}
		env := envMap(cmd.Env)
		if len(env) == 0 {
			t.Fatalf("launch %d: empty cmd.Env: the adapter would launch with an empty environment", launch)
		}
		home := env["HOME"]
		if home == "" || home == os.Getenv("HOME") {
			t.Fatalf("launch %d: HOME = %q, want a fresh scratch home", launch, home)
		}
		if want := env["PATH"]; want != os.Getenv("PATH") {
			t.Errorf("launch %d: PATH not preserved: %q", launch, want)
		}
		if launch == 0 {
			firstHome = home
			continue
		}
		if home != firstHome {
			t.Errorf("per-instance home not stable across launches: %q then %q", firstHome, home)
		}
	}
}

func TestAmbientHomeAllocator_FailOpenOnUnwritableScratchRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: a read-only parent does not block scratch root creation")
	}
	readonly := t.TempDir()
	if err := os.Chmod(readonly, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(readonly, 0o755) })

	sm := &SessionManager{ambientHomes: &ambientHomeAllocator{rootDir: readonly}}
	sm.graph = copilotGraph(map[string]string{"copilot.main": ""}, nil)

	// Fail-open: no isolation on allocation failure, and today's host-env
	// behavior is preserved rather than the run wedged.
	if customizer, _ := sm.buildAmbientHomeCustomizer("copilot.main"); customizer != nil {
		t.Fatal("expected nil customizer when the scratch root cannot be created")
	}
	workingDir := t.TempDir()
	composed, _, err := sm.buildCommandCustomizer("copilot.main", workingDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if composed == nil {
		t.Fatal("expected the composition fallback customizer")
	}
	cmd := exec.Command("true")
	composed("copilot.main", cmd)
	if cmd.Dir != workingDir {
		t.Errorf("cmd.Dir = %q, want %q", cmd.Dir, workingDir)
	}
	env := envMap(cmd.Env)
	if got := env["HOME"]; got != os.Getenv("HOME") {
		t.Errorf("fallback HOME = %q, want the host home", got)
	}
	if _, ok := env[ambientHomeInstanceIDEnv]; ok {
		t.Errorf("fallback environment carries %s", ambientHomeInstanceIDEnv)
	}
}

func TestAppendEnvIfAbsent(t *testing.T) {
	env := []string{"PATH=/usr/bin"}
	got := envMap(appendEnvIfAbsent(env, "GIT_CONFIG_GLOBAL", "/real/home/.gitconfig"))
	if got["GIT_CONFIG_GLOBAL"] != "/real/home/.gitconfig" || got["PATH"] != "/usr/bin" {
		t.Errorf("appendEnvIfAbsent = %v", got)
	}
	kept := appendEnvIfAbsent([]string{"GIT_CONFIG_GLOBAL=/operator"}, "GIT_CONFIG_GLOBAL", "/source")
	if envMap(kept)["GIT_CONFIG_GLOBAL"] != "/operator" {
		t.Errorf("appendEnvIfAbsent overrode an existing value: %v", kept)
	}
}
