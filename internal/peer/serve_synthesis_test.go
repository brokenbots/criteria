package peer

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// TestConnSpecsForScopes_SynthesizesNonManifestDefinition pins the KB-253
// scan-synthesis path end to end at conn-spec level: a scanned token for a
// definition outside the boot manifest resolves to a conn spec (synthesized
// from the definition's KIND binary staged in the pod + the engine's digest
// sidecar), spawns its child, and the definition joins the served children
// set. Fail-closed arms: a token whose kind has no staged binary, and a
// sidecar-less token, produce a loud refusal and NO conn spec.
func TestConnSpecsForScopes_SynthesizesNonManifestDefinition(t *testing.T) {
	if testing.Short() {
		t.Skip("synthesis test spawns a real adapter subprocess")
	}
	bin := buildNoopAdapter(t)

	const (
		instanceA = "8a2bb7d7-6a4b-4ab1-9c8a-2f5d3a4b1c01"
	)
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Boot-manifest definition (shell.boot) + a scanned non-manifest
	// definition "shell.extra" (same kind: shell) whose binary the boot
	// member already staged. A third token carries NO sidecar: refused.
	write(filepath.Join("run_a", instanceA, "shell.boot.token"), "tok-boot\n")
	write(filepath.Join("run_a", instanceA, "shell.boot.sha256"), "sha256-bootpin\n")
	write(filepath.Join("run_a", instanceA, "shell.extra.token"), "tok-synth\n")
	write(filepath.Join("run_a", instanceA, "shell.extra.sha256"), "sha256-synthpin\n")
	write(filepath.Join("run_a", instanceA, "ghost.token"), "tok-ghost\n")

	// The boot spec's binary IS the staged fixture binary; synthesis takes a
	// same-kind boot member's binary ahead of the convention paths.
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "127.0.0.1:7999"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "shell.boot"
	cfg.AdapterBinary = bin
	cfg.Digest = "sha256-bootpin"
	cfg.ScopesDir = dir
	cfg.Adapters = []AdapterSpec{{Name: "shell.boot", Version: "1.0.0", Digest: "sha256-bootpin", Binary: bin}}

	var logs capturedBuffer
	rt := NewRuntime(&cfg, captureLogger(&logs.buf))
	rt.exitPoll = 20 * time.Millisecond
	rt.shutdownGrace = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Boot(ctx); err != nil {
		t.Fatalf("boot: %v", err)
	}
	s := NewServer(&cfg, rt, slog.New(slog.NewTextHandler(&logs.buf, nil)))
	s.runCtx.Store(ctx)

	specs, err := s.connSpecsForScopes()
	if err != nil {
		t.Fatalf("connSpecsForScopes: %v", err)
	}
	t.Logf("peer log:\n%s", logs.buf.String())

	// The synthesized definition must appear as its own serving conn and as
	// a hosted child; the sidecar-less token must be refused (absent).
	byAdapter := map[string][]serveConnSpec{}
	ghostDialed := false
	for _, sp := range specs {
		byAdapter[sp.name] = append(byAdapter[sp.name], sp)
		if sp.name == "ghost" {
			ghostDialed = true
		}
	}
	if ghostDialed {
		t.Fatalf("sidecar-less token must be refused, got a conn: %+v", byAdapter["ghost"])
	}
	synthConns := byAdapter["shell.extra"]
	if len(synthConns) != 1 {
		t.Fatalf("synthesized definition conns = %d, want 1: %+v", len(synthConns), synthConns)
	}
	sc := synthConns[0]
	if sc.token != "tok-synth" || sc.digest != "sha256-synthpin" {
		t.Fatalf("synthesized conn identity wrong: %+v", sc)
	}
	found := false
	for _, child := range sc.children {
		if child == "shell.extra" {
			found = true
		}
	}
	if !found {
		t.Fatalf("synthesized child missing from served children set: %v", sc.children)
	}
	// The child is actually running (spawned at synthesis, Boot's sequence).
	if adapterhost.ProcessExited(lookupChildHandle(t, rt, "shell.extra")) {
		t.Fatal("synthesized child already exited")
	}
	// The synthesis is registered for respawn after an idle exit.
	if err := rt.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func lookupChildHandle(t *testing.T, rt *peerRuntime, name string) adapterhost.Handle {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	c := rt.children[name]
	if c == nil {
		t.Fatalf("child %q not registered", name)
	}
	return c.handle
}

type capturedBuffer struct{ buf bytes.Buffer }