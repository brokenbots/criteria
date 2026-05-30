package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

func TestWriteSnapshot_CreatesFiles(t *testing.T) {
	tmp := t.TempDir()
	snap := &adapterhost.SessionSnapshot{
		AdapterState:  []byte("blob"),
		SchemaVersion: 1,
		HostArch:      "linux/amd64",
		CreatedAt:     time.Now(),
		SecretOriginRefs: map[string]secrets.OriginRef{
			"token": {Kind: "literal", Ref: "sekrit"},
		},
	}

	seq, err := WriteSnapshot(tmp, snap)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if seq != 1 {
		t.Fatalf("seq = %d; want 1", seq)
	}

	binPath := filepath.Join(tmp, "0000000001.bin")
	jsonPath := filepath.Join(tmp, "0000000001.json")

	if _, err := os.Stat(binPath); err != nil {
		t.Fatalf(".bin missing: %v", err)
	}
	if _, err := os.Stat(jsonPath); err != nil {
		t.Fatalf(".json missing: %v", err)
	}

	blob, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read bin: %v", err)
	}
	if string(blob) != "blob" {
		t.Fatalf("bin = %q; want blob", blob)
	}

	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read json: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("json empty")
	}
}

func TestWriteSnapshot_MultipleCallsIncreaseSeq(t *testing.T) {
	tmp := t.TempDir()
	snap := &adapterhost.SessionSnapshot{
		AdapterState:  []byte("v1"),
		SchemaVersion: 1,
		HostArch:      "linux/amd64",
		CreatedAt:     time.Now(),
	}

	seq1, err := WriteSnapshot(tmp, snap)
	if err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if seq1 != 1 {
		t.Fatalf("seq1 = %d; want 1", seq1)
	}

	snap.AdapterState = []byte("v2")
	seq2, err := WriteSnapshot(tmp, snap)
	if err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if seq2 != 2 {
		t.Fatalf("seq2 = %d; want 2", seq2)
	}
}

func TestReadLatestSnapshot_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	original := &adapterhost.SessionSnapshot{
		AdapterState:     []byte("roundtrip-blob"),
		SchemaVersion:    1,
		PermissionState:  []byte("perm"),
		SecretOriginRefs: map[string]secrets.OriginRef{"k": {Kind: "env", Ref: "VAR"}},
		HostArch:         "darwin/arm64",
		CreatedAt:        time.Now().Truncate(time.Second),
	}

	if _, err := WriteSnapshot(tmp, original); err != nil {
		t.Fatalf("write: %v", err)
	}

	restored, err := ReadLatestSnapshot(tmp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(restored.AdapterState) != "roundtrip-blob" {
		t.Fatalf("adapter state = %q; want roundtrip-blob", restored.AdapterState)
	}
	if restored.SchemaVersion != 1 {
		t.Fatalf("schema version = %d; want 1", restored.SchemaVersion)
	}
	if string(restored.PermissionState) != "perm" {
		t.Fatalf("permission state = %q; want perm", restored.PermissionState)
	}
	if len(restored.SecretOriginRefs) != 1 || restored.SecretOriginRefs["k"].Ref != "VAR" {
		t.Fatalf("secret origin refs mismatch")
	}
	if restored.HostArch != "darwin/arm64" {
		t.Fatalf("host arch = %q; want darwin/arm64", restored.HostArch)
	}
}

func TestReadLatestSnapshot_EmptyDir(t *testing.T) {
	tmp := t.TempDir()
	_, err := ReadLatestSnapshot(tmp)
	if err == nil {
		t.Fatal("expected error for empty dir")
	}
}

func TestListSnapshotSessions(t *testing.T) {
	base := t.TempDir()
	runID := "run-1"

	// Create two session snapshot dirs.
	for _, sid := range []string{"sess-a", "sess-b"} {
		dir := SnapshotDir(base, runID, sid)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	sessions, err := ListSnapshotSessions(base, runID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
}

func TestListSnapshotSessions_MissingDir(t *testing.T) {
	base := t.TempDir()
	sessions, err := ListSnapshotSessions(base, "no-such-run")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}
}
