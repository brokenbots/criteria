package adapterhost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// FileAuditWriter appends DecisionLogEntry records as JSON-lines to a file.
// Writes are serialized with a mutex to prevent interleaved output.
type FileAuditWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File
	enc  *json.Encoder
}

// NewFileAuditWriter creates a writer that will append to the given path.
// The file and its parent directory are created on the first Write call.
func NewFileAuditWriter(path string) *FileAuditWriter {
	return &FileAuditWriter{path: path}
}

// Write marshals entry as a single JSON line and appends it to the file.
// If the file has not been opened yet, it attempts to create the parent
// directory and open the file for append. Errors during setup or write are
// silently dropped to keep the permission path non-blocking.
func (w *FileAuditWriter) Write(entry *DecisionLogEntry) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.f == nil {
		if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
			return
		}
		f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		w.f = f
		w.enc = json.NewEncoder(f)
	}

	_ = w.enc.Encode(entry)
}

// Close flushes and closes the underlying file.
func (w *FileAuditWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	return w.f.Close()
}
