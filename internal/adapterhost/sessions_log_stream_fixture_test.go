package adapterhost

import (
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/workflow"
)

// TestSessionManager_RealEarlyLogAdapter_SurvivesIdleStall uses the real
// nonheartbeating fixture binary to prove the host-side heartbeat-stall defense
// works against an actual adapter whose Log stream returns immediately. A
// correct host must disarm the stall detector and let Execute succeed after
// idling past the threshold.
func TestSessionManager_RealEarlyLogAdapter_SurvivesIdleStall(t *testing.T) {
	rec := &recordingSlogHandler{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(rec))
	defer slog.SetDefault(oldLogger)

	adapterBin := buildNonHeartbeatingAdapter(t)

	loader := NewLoaderWithDiscovery(func(requested string) (string, error) {
		if requested != "nonheartbeating" {
			return "", nil
		}
		return adapterBin, nil
	})
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	sm := NewSessionManager(loader)
	sm.HeartbeatStallThreshold = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sm.Open(ctx, "agent", "nonheartbeating", "fail", nil, nil); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() { _ = sm.Close(context.Background(), "agent") }()

	// Idle past the stall threshold. A host that did not disarm the stall
	// detector after the early Log return would declare the session crashed.
	time.Sleep(250 * time.Millisecond)

	step := &workflow.StepNode{Name: "run"}
	_, err := sm.Execute(ctx, "agent", step, &logEventCollector{})
	if err != nil {
		t.Fatalf("expected Execute to succeed after idle past stall threshold, got %v", err)
	}

	var found int
	for _, r := range rec.all() {
		if strings.Contains(r, "broke the log-stream contract") {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one contract-breaker diagnostic from the real fixture, got %d", found)
	}
}

func buildNonHeartbeatingAdapter(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	adapterBin := filepath.Join(t.TempDir(), "criteria-adapter-nonheartbeating")

	cmd := exec.Command("go", "build", "-o", adapterBin, "./internal/adapter/conformance/testfixtures/nonheartbeating")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build nonheartbeating adapter: %v\n%s", err, string(output))
	}
	return adapterBin
}
