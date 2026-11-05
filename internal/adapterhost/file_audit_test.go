package adapterhost

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureLogger returns a slog.Logger plus a func collecting
// level-prefixed rendered records written through it.
func captureLogger() (logger *slog.Logger, records func() []string) {
	var mu sync.Mutex
	var recs []string
	h := slog.NewTextHandler(&writerFn{fn: func(b []byte) {
		mu.Lock()
		recs = append(recs, string(b))
		mu.Unlock()
	}, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), recs...)
	}
}

type writerFn struct {
	fn func([]byte)
	mu *sync.Mutex
}

func (w *writerFn) Write(p []byte) (int, error) {
	w.fn(p)
	return len(p), nil
}

func TestFileAuditWriter_CountsDroppedOnUnavailablePath(t *testing.T) {
	// A blocker file on the path makes both the parent directory creation
	// and the file open fail permanently; every Write must be accounted as
	// a drop without an error escaping.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := NewFileAuditWriter(filepath.Join(blocker, "audit.log"))

	entry := &DecisionLogEntry{Tool: "exec", Decision: "allow"}
	w.Write(entry)
	w.Write(entry)
	w.Write(entry)

	dropped, attempted, lastErr := w.AuditLoss()
	if dropped != 3 || attempted != 3 {
		t.Fatalf("want 3 dropped of 3 attempted, got dropped=%d attempted=%d", dropped, attempted)
	}
	if lastErr == "" {
		t.Fatal("expected the last write error to be reported")
	}
}

func TestFileAuditWriter_WritesAndCountsOnHealthyPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	w := NewFileAuditWriter(path)
	defer func() { _ = w.Close() }()

	entry := &DecisionLogEntry{Tool: "exec", Decision: "allow", Reason: "ok"}
	w.Write(entry)
	w.Write(entry)

	dropped, attempted, lastErr := w.AuditLoss()
	if dropped != 0 {
		t.Fatalf("expected zero drops, got %d (last error %q)", dropped, lastErr)
	}
	if attempted != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempted)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	if lines := strings.Count(string(b), "\n"); lines != 2 {
		t.Fatalf("expected 2 JSON lines, got %d in %q", lines, string(b))
	}
}

func TestFileAuditWriter_DropLogsRateLimited(t *testing.T) {
	log, recs := captureLogger()
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The blocker file defeats every setup attempt for the duration.
	w := NewFileAuditWriter(filepath.Join(blocker, "audit.log"))
	w.log = log
	w.dropLogInterval = time.Hour // nothing drops within the test window

	entry := &DecisionLogEntry{Tool: "exec", Decision: "deny"}
	for i := 0; i < 20; i++ {
		w.Write(entry)
	}

	got := recs()
	if n := countLevel(got, "ERROR"); n != 1 {
		t.Fatalf("expected exactly one rate-limited error log in a %v window, got %d", w.dropLogInterval, n)
	}
}

func TestFileAuditWriter_WarnsOncePastThreshold(t *testing.T) {
	log, recs := captureLogger()
	// The parent directory exists but the audit file's open fails: the
	// path itself is a directory.
	poisoned := filepath.Join(t.TempDir(), "audit.log")
	if err := os.MkdirAll(poisoned, 0o700); err != nil {
		t.Fatal(err)
	}
	w := NewFileAuditWriter(poisoned)
	w.log = log
	w.dropLogInterval = 0 // log every drop
	w.warnThreshold = 3

	entry := &DecisionLogEntry{Tool: "exec", Decision: "deny"}
	for i := 0; i < 10; i++ {
		w.Write(entry)
	}

	got := recs()
	if n := countLevel(got, "WARN"); n != 1 {
		t.Fatalf("expected exactly one threshold WARN, got %d", n)
	}
	if n := countLevel(got, "ERROR"); n < 1 {
		t.Fatal("expected per-drop error logs alongside the WARN")
	}
	if dropped, attempted, _ := w.AuditLoss(); dropped != 10 || attempted != 10 {
		t.Fatalf("expected 10 dropped of 10 attempted, got %d/%d", dropped, attempted)
	}
}

func TestFileAuditWriter_RecoversAfterSetupFailure(t *testing.T) {
	dir := t.TempDir()
	w := NewFileAuditWriter(filepath.Join(dir, "audit.log"))
	defer func() { _ = w.Close() }()

	// Make the first setup attempts fail by reserving the audit path as a
	// directory, then release it so a later Write succeeds through the
	// retry path.
	poisoned := filepath.Join(dir, "audit.log")
	if err := os.MkdirAll(poisoned, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := &DecisionLogEntry{Tool: "exec", Decision: "allow"}
	w.Write(entry)
	if dropped, _, lastErr := w.AuditLoss(); dropped != 1 || lastErr == "" {
		t.Fatalf("expected one accounted drop, got dropped=%d lastErr=%q", dropped, lastErr)
	}

	if err := os.Remove(poisoned); err != nil {
		t.Fatal(err)
	}
	w.Write(entry)

	dropped, attempted, _ := w.AuditLoss()
	if dropped != 1 {
		t.Fatalf("expected the recovery write to not count as a drop, got dropped=%d", dropped)
	}
	if attempted != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempted)
	}
	b, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil || !strings.Contains(string(b), `"allow"`) {
		t.Fatalf("expected a recovered audit line, err=%v data=%q", err, string(b))
	}
}

func TestFileAuditWriter_WriteNeverBlocksThePermissionPath(t *testing.T) {
	// An injected write failure must not surface an error and must not
	// take longer than the accounting itself; the caller (the permission
	// decision path) owns no audit retry.
	w := NewFileAuditWriter(filepath.Join(t.TempDir(), "audit.log"))
	// A blocker file on the path forces every setup attempt to fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.path = filepath.Join(blocker, "audit.log")
	w.Write(&DecisionLogEntry{Tool: "exec", Decision: "deny", Reason: "boom"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		start := time.Now()
		for i := 0; i < 100; i++ {
			w.Write(&DecisionLogEntry{Tool: "exec", Decision: "deny", Reason: "boom"})
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("100 failing writes took %s; the permission path must not block", elapsed)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Write blocked on a persistent failure")
	}
	dropped, attempted, lastErr := w.AuditLoss()
	if dropped != attempted {
		t.Errorf("dropped %d must track attempted %d writes exactly", dropped, attempted)
	}
	if dropped < 100 {
		t.Errorf("expected the 100 failing writes to be accounted, got %d", dropped)
	}
	if lastErr == "" && dropped > 0 {
		t.Errorf("dropped %d entries but reported no last error", dropped)
	}
}

func TestFileAuditWriter_AuditLossIsRaceSafe(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := NewFileAuditWriter(filepath.Join(blocker, "audit.log"))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry := &DecisionLogEntry{Tool: "exec", Decision: "deny"}
			for j := 0; j < 25; j++ {
				w.Write(entry)
				w.AuditLoss()
			}
		}()
	}
	wg.Wait()

	dropped, attempted, _ := w.AuditLoss()
	if dropped != 200 || attempted != 200 {
		t.Fatalf("expected 200/200 from concurrent writes, got %d/%d", dropped, attempted)
	}
}

// countLevel counts rendered records whose level=name.
func countLevel(recs []string, level string) int {
	n := 0
	for _, r := range recs {
		if strings.Contains(r, "level="+level) {
			n++
		}
	}
	return n
}
