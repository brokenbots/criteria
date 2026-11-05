package adapterhost

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brokenbots/criteria/internal/tunables"
)

// FileAuditWriter appends DecisionLogEntry records as JSON-lines to a file.
// Writes are serialized with a mutex to prevent interleaved output.
type FileAuditWriter struct {
	path string
	log  *slog.Logger

	// Timing/state knobs. Unexported so production stays on the tunables
	// defaults; in-package tests inject short values.
	dropLogInterval time.Duration
	warnThreshold   int64

	// Counters are atomic so the loss snapshot can be read without the
	// write mutex.
	written   atomic.Int64
	dropped   atomic.Int64
	attempted atomic.Int64

	// Write-path state protected by mu.
	mu              sync.Mutex
	f               *os.File
	enc             *json.Encoder
	lastErr         error
	lastErrLoggedAt time.Time
	warnedThreshold bool
}

// NewFileAuditWriter creates a writer that will append to the given path.
// The file and its parent directory are created on the first Write call.
func NewFileAuditWriter(path string) *FileAuditWriter {
	return &FileAuditWriter{
		path:            path,
		dropLogInterval: tunables.DefaultFileAuditDropLogInterval,
		warnThreshold:   int64(tunables.DefaultFileAuditLossWarnThreshold),
	}
}

// Write marshals entry as a single JSON line and appends to the file,
// creating the file and parent directory on first use. Write never blocks
// the caller: any setup or write failure is absorbed and accounted for as a
// dropped audit entry (counted, rate-limited into the log, and reported
// through AuditLoss) instead of failing the permission path.
func (w *FileAuditWriter) Write(entry *DecisionLogEntry) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.attempted.Add(1)

	if err := w.ensureOpen(); err != nil {
		w.recordDropLocked(err)
		return
	}

	if err := w.enc.Encode(entry); err != nil {
		w.recordDropLocked(err)
		return
	}
	w.written.Add(1)
}

// AuditLoss reports the cumulative audit-write loss: how many entries were
// dropped, how many writes were attempted, and the most recent error. It
// implements the engine-side AuditLossReporter probe so a run can surface
// audit loss in its record.
func (w *FileAuditWriter) AuditLoss() (dropped, attempted int64, lastError string) {
	dropped = w.dropped.Load()
	attempted = w.attempted.Load()
	w.mu.Lock()
	lastError = errText(w.lastErr)
	w.mu.Unlock()
	return dropped, attempted, lastError
}

// ensureOpen lazily creates the parent directory and opens the audit file
// for append. A nil file means the next Write retries the setup.
func (w *FileAuditWriter) ensureOpen() error {
	if w.f != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w.f = f
	w.enc = json.NewEncoder(f)
	return nil
}

// recordDropLocked accounts an absorbed write failure. It logs a per-drop
// error immediately on the first drop and then at most once per drop-log
// interval, and escalates to a one-time WARN once cumulative drops exceed
// the threshold.
func (w *FileAuditWriter) recordDropLocked(err error) {
	w.dropped.Add(1)
	w.lastErr = err
	now := time.Now()

	log := w.log
	if log == nil {
		log = slog.Default()
	}

	dropped := w.dropped.Load()
	if dropped > w.warnThreshold && !w.warnedThreshold {
		w.warnedThreshold = true
		log.Warn("permission audit file writer dropping entries",
			"path", w.path, "dropped", dropped,
			"attempted", w.attempted.Load(), "error", err.Error())
		return
	}

	if w.dropLogInterval <= 0 ||
		w.lastErrLoggedAt.IsZero() || now.Sub(w.lastErrLoggedAt) >= w.dropLogInterval {
		w.lastErrLoggedAt = now
		log.Error("permission audit entry dropped",
			"path", w.path, "dropped", dropped, "error", err.Error())
	}
}

// Close flushes and closes the underlying file. Subsequent Writes retry the
// setup path.
func (w *FileAuditWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	f := w.f
	w.f = nil
	w.enc = nil
	return f.Close()
}

// errText renders err for the loss snapshot; empty when nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
