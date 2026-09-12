package cli

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// runIdentityFingerprint computes a stable digest identifying one logical CLI
// invocation: the workflow being executed, the orchestrator it targets, and
// the variable inputs supplied on the command line.
//
// CRI-125: when a runner pod restarts mid-run, the restarted process must not
// silently start a second run (with a fresh run_id) against the same
// workflow. Crash-recovery checkpoints store this fingerprint, so a restart
// matches it and resumes — or keeps failed — the original run instead of
// forking a zombie run that breaks adapter state.
//
// The digest covers var-file contents rather than their paths: the operator
// materialises per-pod scratch paths (mktemp), so paths differ across
// restarts while the file bytes stay identical. Override strings are sorted
// so flag order does not affect identity. Raw secret values are never
// persisted anywhere — only the one-way digest is written to checkpoints.
//
// It returns "" when identity cannot be computed (unreadable var file, empty
// workflow path). "" never matches a checkpoint fingerprint, so callers fall
// back to the pre-CRI-125 behavior (start a fresh run) rather than
// suppressing.
func runIdentityFingerprint(workflowPath, serverURL string, varFiles, varOverrides []string) string {
	h := sha256.New()
	writeField := func(field string) error {
		var lenBuf [4]byte
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(field)))
		if _, err := h.Write(lenBuf[:]); err != nil {
			return err
		}
		_, err := h.Write([]byte(field))
		return err
	}
	fields := make([]string, 0, 4+len(varOverrides)+len(varFiles))
	fields = append(fields, "criteria-run-identity/v1")

	path := strings.TrimSpace(workflowPath)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return ""
	}
	fields = append(fields, abs, strings.TrimRight(strings.TrimSpace(serverURL), "/"))

	sortedOverrides := make([]string, len(varOverrides))
	for i, v := range varOverrides {
		sortedOverrides[i] = strings.TrimSpace(v)
	}
	sort.Strings(sortedOverrides)
	fields = append(fields, fmt.Sprint(len(sortedOverrides)))
	fields = append(fields, sortedOverrides...)

	fields = append(fields, fmt.Sprint(len(varFiles)))
	for _, vf := range varFiles {
		raw, err := os.ReadFile(strings.TrimSpace(vf))
		if err != nil {
			return ""
		}
		fields = append(fields, string(raw))
	}

	for _, field := range fields {
		if err := writeField(field); err != nil {
			return ""
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
