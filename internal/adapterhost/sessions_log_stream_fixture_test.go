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
// does not falsely crash an adapter whose Log handler returns immediately.
// With criteria-go-adapter-sdk v0.5.3 the SDK keeps the log stream alive by
// emitting heartbeats on the adapter's behalf, so the host must accept those
// heartbeats and let Execute succeed after idling past the threshold.
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
	sm.HeartbeatStallThreshold = 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := sm.Open(ctx, "agent", "nonheartbeating", "fail", nil, nil); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() { _ = sm.Close(context.Background(), "agent") }()

	// With criteria-go-adapter-sdk v0.5.3 the SDK itself keeps the log stream
	// alive by emitting heartbeats on the adapter's behalf even when the
	// adapter's Log handler returns early. Idle past one full heartbeat
	// interval so the host has received a heartbeat, then confirm Execute is
	// allowed to proceed without a false stall crash.
	time.Sleep(35 * time.Second)

	step := &workflow.StepNode{Name: "run"}
	_, err := sm.Execute(ctx, "agent", step, &logEventCollector{})
	if err != nil {
		t.Fatalf("expected Execute to succeed after idle past stall threshold, got %v", err)
	}

	// The stream is still being kept alive by SDK heartbeats, so the host
	// should not have logged a contract-breaker diagnostic.
	for _, r := range rec.all() {
		if strings.Contains(r, "broke the log-stream contract") {
			t.Fatalf("unexpected contract-breaker diagnostic while SDK heartbeats keep stream alive: %s", r)
		}
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
