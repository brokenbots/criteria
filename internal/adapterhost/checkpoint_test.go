package adapterhost

// checkpoint_test.go — CRI-202: adapter-side checkpoint save and restore
// surfaces. A stateful adapter's declared policy decides when its state is
// captured (never for stateless or on-demand adapters); a save failure blocks
// step completion (the engine turns it into a fatal run error) so no
// step-done event exists without its checkpoint; an over-cap blob is refused
// loudly, never truncated; restore-time schema verification refuses
// mismatched state in both directions while keeping pre-CRI-202 snapshots
// usable.

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// ckptHandle is a stateful Handle for checkpoint tests: its Info declares a
// configurable state surface, Snapshot reports the current blob, Execute
// mutates the blob, and Restore records replays.
type ckptHandle struct {
	decl *workflow.StateDeclaration

	mu      sync.Mutex
	state   []byte
	open    int
	restore []ckptRestoreRecord
}

// ckptRestoreRecord captures one Restore RPC the adapter received.
type ckptRestoreRecord struct {
	blob          []byte
	schemaVersion uint32
}

func (h *ckptHandle) Info(context.Context) (Info, error) {
	return Info{AdapterInfo: workflow.AdapterInfo{State: h.decl}}, nil
}
func (h *ckptHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	h.mu.Lock()
	h.open++
	h.mu.Unlock()
	return nil
}
func (h *ckptHandle) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, _ *v2.ExecutionRejection) (adapter.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = append(h.state, '.')
	return adapter.Result{}, nil
}
func (h *ckptHandle) CloseSession(context.Context, string) error { return nil }
func (h *ckptHandle) Kill()                                      {}
func (h *ckptHandle) Pause(context.Context, string) error        { return nil }
func (h *ckptHandle) Resume(context.Context, string) error       { return nil }
func (h *ckptHandle) Snapshot(_ context.Context, _ string) (*v2.SnapshotResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return &v2.SnapshotResponse{State: append([]byte(nil), h.state...)}, nil
}
func (h *ckptHandle) Restore(_ context.Context, _ string, state []byte, schemaVersion uint32) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.restore = append(h.restore, ckptRestoreRecord{
		blob:          append([]byte(nil), state...),
		schemaVersion: schemaVersion,
	})
	h.state = append([]byte(nil), state...)
	return nil
}
func (h *ckptHandle) restoredCalls() []ckptRestoreRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ckptRestoreRecord(nil), h.restore...)
}
func (h *ckptHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}

// ckptLoader hands every Resolve the same handle.
type ckptLoader struct{ handle Handle }

func (l *ckptLoader) Resolve(context.Context, string) (Handle, error) { return l.handle, nil }
func (l *ckptLoader) Shutdown(context.Context) error                  { return nil }

// ckptRecorder records CheckpointSave calls.
type ckptRecorder struct {
	mu      sync.Mutex
	saves   []SessionSnapshot
	err     error
	gotName []string
}

func (r *ckptRecorder) save(name string, snap *SessionSnapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.saves = append(r.saves, *snap)
	r.gotName = append(r.gotName, name)
	return nil
}

func (r *ckptRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.saves)
}

// openStatefulSession opens a session against the handle and returns the
// owning manager and session.
func openStatefulSession(t *testing.T, h *ckptHandle, rec *ckptRecorder) (*SessionManager, *Session) {
	t.Helper()
	m := NewSessionManager(&ckptLoader{handle: h})
	m.CheckpointSave = rec.save
	if err := m.Open(context.Background(), "s1", "stateful", OnCrashFail, nil, nil); err != nil {
		t.Fatalf("Open: %v", err)
	}
	sess := m.sessions["s1"]
	if sess == nil {
		t.Fatal("Open left no session s1")
	}
	return m, sess
}

func TestAutoCheckpoints_GranularityMatrix(t *testing.T) {
	cases := []struct {
		name    string
		decl    *workflow.StateDeclaration
		perStep bool
		perTurn bool
	}{
		{name: "nil-declaration", decl: nil},
		{name: "explicit-none", decl: &workflow.StateDeclaration{Mode: workflow.StateModeNone}},
		{name: "per-step", decl: &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "h.v1", Granularity: workflow.StateGranularityPerStep},
			perStep: true},
		{name: "empty-granularity-defaults-to-per-step", decl: &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "h.v1"},
			perStep: true},
		{name: "unknown-granularity-defaults-to-per-step", decl: &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "h.v1", Granularity: "hourly"},
			perStep: true},
		{name: "per-turn-saves-at-both-boundaries", decl: &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "h.v1", Granularity: workflow.StateGranularityPerTurn},
			perStep: true, perTurn: true},
		{name: "on-demand-opts-out", decl: &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "h.v1", Granularity: workflow.StateGranularityOnDemand}},
		{name: "ref-mode-per-step", decl: &workflow.StateDeclaration{Mode: workflow.StateModeRef, Schema: "h.v1", Granularity: workflow.StateGranularityPerStep},
			perStep: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			perStep, perTurn := autoCheckpointsWithDecl(tc.decl)
			if perStep != tc.perStep || perTurn != tc.perTurn {
				t.Fatalf("autoCheckpointsWithDecl = (%v, %v); want (%v, %v)", perStep, perTurn, tc.perStep, tc.perTurn)
			}
		})
	}
}

func TestOpen_CapturesDeclaredStateForCheckpointing(t *testing.T) {
	decl := &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "harness.v1", Granularity: workflow.StateGranularityPerTurn}
	h := &ckptHandle{decl: decl}
	_, sess := openStatefulSession(t, h, &ckptRecorder{})
	if sess.stateSchema != "harness.v1" || sess.stateMode != workflow.StateModeBlob {
		t.Fatalf("session state stamp = (%q, %q); want schema harness.v1 under mode blob", sess.stateSchema, sess.stateMode)
	}
}

func TestCheckpointAfterExecute_SavesDeclaredStateAtStepBoundary(t *testing.T) {
	h := &ckptHandle{decl: &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "harness.v1"}}
	rec := &ckptRecorder{}
	m, sess := openStatefulSession(t, h, rec)

	if _, err := sess.handle.Execute(context.Background(), sess.Name, &workflow.StepNode{}, nil, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := ckptAfter(m, sess); err != nil {
		t.Fatalf("checkpointAfterExecute: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("saves = %d; want 1 per step boundary", rec.count())
	}
	snap := rec.saves[0]
	if string(snap.AdapterState) != "." {
		t.Fatalf("saved blob = %q; want the adapter's state", snap.AdapterState)
	}
	if snap.StateSchema != "harness.v1" {
		t.Fatalf("saved StateSchema = %q; want the declared schema stamped", snap.StateSchema)
	}
	if want := ComputeStateDigest(snap.AdapterState); snap.StateDigest != want {
		t.Fatalf("saved StateDigest = %q; want %q", snap.StateDigest, want)
	}
}

// ckptAfter wraps checkpointAfterExecute for readability in tests.
func ckptAfter(m *SessionManager, sess *Session) error {
	return m.checkpointAfterExecute(context.Background(), sess)
}

func TestCheckpointAfterExecute_SaveFailureBlocksStepCompletion(t *testing.T) {
	h := &ckptHandle{decl: &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "harness.v1"}}
	rec := &ckptRecorder{err: errors.New("disk full")}
	m, sess := openStatefulSession(t, h, rec)

	err := ckptAfter(m, sess)
	if err == nil {
		t.Fatal("checkpointAfterExecute = nil; a failed save must block step completion")
	}
	if !strings.Contains(err.Error(), "checkpointing is required before the step may complete") {
		t.Fatalf("error = %q; want the step-blocking diagnostic", err.Error())
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %q; want it to name the underlying save failure", err.Error())
	}
	if !strings.Contains(err.Error(), `"s1"`) {
		t.Fatalf("error = %q; want it to name the session", err.Error())
	}
}

func TestCheckpointAfterExecute_CanceledContextSkipsSave(t *testing.T) {
	h := &ckptHandle{decl: &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "harness.v1"}}
	rec := &ckptRecorder{}
	m, sess := openStatefulSession(t, h, rec)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.checkpointAfterExecute(ctx, sess); err != nil {
		t.Fatalf("checkpointAfterExecute under canceled context: %v; the run is stopping and no step-outcome follows", err)
	}
	if rec.count() != 0 {
		t.Fatalf("saves = %d; want 0", rec.count())
	}
}

func TestCheckpointAfterExecute_StatelessAndOnDemandSkipSave(t *testing.T) {
	rec := &ckptRecorder{}

	t.Run("stateless", func(t *testing.T) {
		m, sess := openStatefulSession(t, &ckptHandle{decl: nil}, rec)
		if err := ckptAfter(m, sess); err != nil {
			t.Fatalf("checkpointAfterExecute: %v", err)
		}
	})
	t.Run("on-demand", func(t *testing.T) {
		m, sess := openStatefulSession(t, &ckptHandle{decl: &workflow.StateDeclaration{
			Mode: workflow.StateModeBlob, Schema: "harness.v1", Granularity: workflow.StateGranularityOnDemand,
		}}, rec)
		if err := ckptAfter(m, sess); err != nil {
			t.Fatalf("checkpointAfterExecute: %v", err)
		}
	})
	if rec.count() != 0 {
		t.Fatalf("saves = %d; want 0 for stateless and on-demand adapters", rec.count())
	}
}

func TestCheckpointAfterExecute_MissingCheckpointSaveSurfaceIsNoOp(t *testing.T) {
	h := &ckptHandle{decl: &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "harness.v1"}}
	m := NewSessionManager(&ckptLoader{handle: h})
	if err := m.Open(context.Background(), "s1", "stateful", OnCrashFail, nil, nil); err != nil {
		t.Fatalf("Open: %v", err)
	}
	sess := m.sessions["s1"]
	// nil CheckpointSave (no engine wiring) must not turn execution off.
	if err := m.checkpointAfterExecute(context.Background(), sess); err != nil {
		t.Fatalf("checkpointAfterExecute: %v", err)
	}
}

func TestCheckpointAfterExecute_OverCapBlobRefusedLoudly(t *testing.T) {
	h := &ckptHandle{decl: &workflow.StateDeclaration{
		Mode: workflow.StateModeBlob, Schema: "harness.v1", MaxBytes: 8,
	}}
	rec := &ckptRecorder{}
	m, sess := openStatefulSession(t, h, rec)

	for i := 0; i < 10; i++ {
		if _, err := sess.handle.Execute(context.Background(), sess.Name, &workflow.StepNode{}, nil, nil); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	err := ckptAfter(m, sess)
	if err == nil {
		t.Fatal("checkpointAfterExecute = nil; an over-cap save must be refused")
	}
	if !strings.Contains(err.Error(), "refusing to truncate") {
		t.Fatalf("error = %q; want the no-truncation diagnostic", err.Error())
	}
	if !strings.Contains(err.Error(), "max_bytes 8") {
		t.Fatalf("error = %q; want it to name the declared cap", err.Error())
	}
	if rec.count() != 0 {
		t.Fatalf("saves = %d; want 0 — an over-cap blob must never be persisted", rec.count())
	}
}

func TestTurnCheckpoint_FiresOnTurnBoundaryAndSerializesSaves(t *testing.T) {
	h := &ckptHandle{decl: &workflow.StateDeclaration{
		Mode: workflow.StateModeBlob, Schema: "harness.v1", Granularity: workflow.StateGranularityPerTurn,
	}}
	rec := &ckptRecorder{}
	_, sess := openStatefulSession(t, h, rec)

	if sess.PermissionState == nil || sess.PermissionState.turnCheckpoint == nil {
		t.Fatalf("per-turn adapter must carry a turn checkpoint hook (PermissionState=%v)", sess.PermissionState)
	}

	for i := 0; i < 3; i++ {
		sess.PermissionState.turnCheckpoint()
	}
	deadline := time.Now().Add(2 * time.Second)
	for rec.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if rec.count() != 1 {
		t.Fatalf("saves = %d; want exactly 1 serialized save for 3 racing turn hooks", rec.count())
	}
}

func TestTurnCheckpoint_AbsentForPerStepAdapters(t *testing.T) {
	h := &ckptHandle{decl: &workflow.StateDeclaration{
		Mode: workflow.StateModeBlob, Schema: "harness.v1", Granularity: workflow.StateGranularityPerStep,
	}}
	rec := &ckptRecorder{}
	_, sess := openStatefulSession(t, h, rec)
	if sess.PermissionState == nil || sess.PermissionState.turnCheckpoint != nil {
		t.Fatalf("per-step adapter must not carry a turn checkpoint hook (PermissionState=%v)", sess.PermissionState)
	}
}

func TestValidateRestoredStateSchema(t *testing.T) {
	decl := &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "harness.v1"}
	none := &workflow.StateDeclaration{Mode: workflow.StateModeNone}

	cases := []struct {
		name     string
		decl     *workflow.StateDeclaration
		schema   string
		stateLen int
		wantErr  string
	}{
		{name: "matching-schema-accepts", decl: decl, schema: "harness.v1", stateLen: 4},
		{name: "checkpoint-newer-schema-refuses", decl: decl, schema: "harness.v2", stateLen: 4,
			wantErr: `state schema mismatch: checkpoint carries "harness.v2", adapter declares "harness.v1"`},
		{name: "checkpoint-older-schema-refuses", decl: decl, schema: "harness.v0", stateLen: 4,
			wantErr: `state schema mismatch: checkpoint carries "harness.v0", adapter declares "harness.v1"`},
		{name: "stateful-checkpoint-stateless-adapter-refuses", decl: nil, schema: "harness.v1", stateLen: 4,
			wantErr: `checkpoint carries state (schema "harness.v1") but the adapter declares no checkpointable state`},
		{name: "stateful-checkpoint-none-adapter-refuses", decl: none, schema: "harness.v1", stateLen: 4,
			wantErr: `checkpoint carries state (schema "harness.v1") but the adapter declares no checkpointable state`},
		{name: "pre-cri202-untagged-proceeds", decl: decl, schema: "", stateLen: 4},
		{name: "untagged-empty-state-proceeds-silently", decl: decl, schema: "", stateLen: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := &SessionSnapshot{StateSchema: tc.schema}
			if tc.stateLen > 0 {
				snap.AdapterState = make([]byte, tc.stateLen)
			}
			err := validateRestoredStateSchema("stateful", tc.decl, snap)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateRestoredStateSchema: %v; want acceptance", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v; want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestSessionBound_DistinguishesVerifiedRecords(t *testing.T) {
	h := &ckptHandle{decl: &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "harness.v1"}}
	m := NewSessionManager(&ckptLoader{handle: h})

	if m.SessionBound("s1") {
		t.Fatal("SessionBound on a fresh manager = true; want false")
	}
	if err := m.Open(context.Background(), "s1", "stateful", OnCrashFail, nil, nil); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !m.SessionBound("s1") {
		t.Fatal("SessionBound after Open = false; want true")
	}
	if !m.SessionOpen("s1") {
		t.Fatal("SessionOpen after Open = false; want true")
	}

	// A verified-but-unbound record (resume re-verify) must NOT look bound:
	// bootstrapSessionsForResume restores it through the full Restore path
	// instead of RestoreIntoLiveSession.
	m2 := NewSessionManager(&ckptLoader{handle: h})
	if err := m2.Verify(context.Background(), "s1", "stateful", OnCrashFail, nil, nil, nil, "", "", ""); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !m2.SessionOpen("s1") {
		t.Fatal("SessionOpen after verify = false; want true (a verified record counts as open)")
	}
	if m2.SessionBound("s1") {
		t.Fatal("SessionBound after verify = true; want false — the record is verified, not bound")
	}
}

// liveSnap builds a restore-ready SessionSnapshot for live-session tests:
// schema-stamped, digest-verified, and compatible with the current host.
// The checkpoint payload is always "prior-state" — refusal tests below rely
// on it being replaced, not varied.
func liveSnap(schema string) *SessionSnapshot {
	state := []byte("prior-state")
	return &SessionSnapshot{
		AdapterState:  state,
		SchemaVersion: currentSnapshotSchemaVersion,
		HostArch:      runtime.GOOS + "/" + runtime.GOARCH,
		StateSchema:   schema,
		StateMode:     workflow.StateModeBlob,
		StateDigest:   ComputeStateDigest(state),
	}
}

// TestRestoreIntoLiveSession_ReplaysCheckpointIntoBoundSession: a remote
// adapter that phone-homed during initialization binds its session before
// the resume pass runs, so the pass must replay the checkpoint into the LIVE
// session — the adapter receives Restore with the checkpoint's exact bytes
// and schema version, and the session stays bound (never relaunched).
func TestRestoreIntoLiveSession_ReplaysCheckpointIntoBoundSession(t *testing.T) {
	h := &ckptHandle{decl: &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "harness.v1"}}
	m, _ := openStatefulSession(t, h, &ckptRecorder{})

	snap := liveSnap("harness.v1")
	if err := m.RestoreIntoLiveSession(context.Background(), "s1", snap); err != nil {
		t.Fatalf("RestoreIntoLiveSession: %v", err)
	}

	calls := h.restoredCalls()
	if len(calls) != 1 {
		t.Fatalf("adapter received %d restore calls; want 1", len(calls))
	}
	if string(calls[0].blob) != "prior-state" {
		t.Errorf("restored blob = %q; want prior-state", calls[0].blob)
	}
	if calls[0].schemaVersion != snap.SchemaVersion {
		t.Errorf("restored schemaVersion = %d; want %d", calls[0].schemaVersion, snap.SchemaVersion)
	}
	if !m.SessionBound("s1") {
		t.Error("session no longer bound after live restore; want the same live session retained")
	}
}

// TestRestoreIntoLiveSession_RefusesWhenSessionNotOpen: a restore routed to
// a live session that does not exist fails loudly with the session named —
// never a silent fresh start on the mis-routed checkpoint.
func TestRestoreIntoLiveSession_RefusesWhenSessionNotOpen(t *testing.T) {
	m := NewSessionManager(&ckptLoader{handle: &ckptHandle{}})
	err := m.RestoreIntoLiveSession(context.Background(), "s1", liveSnap("harness.v1"))
	if err == nil {
		t.Fatal("expected a loud refusal for a restore into a non-open session")
	}
	if !strings.Contains(err.Error(), `session "s1" is not open`) {
		t.Errorf("refusal does not name the session: %v", err)
	}
}

// TestRestoreIntoLiveSession_RefusesSchemaContradiction: the relaunched
// (phone-homed) adapter's declared state surface must agree with the
// checkpoint stamp. Every contradiction refuses loudly without calling
// Restore — no path returns nil and continues fresh.
func TestRestoreIntoLiveSession_RefusesSchemaContradiction(t *testing.T) {
	t.Run("schema-mismatch", func(t *testing.T) {
		h := &ckptHandle{decl: &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "harness.v1"}}
		m, _ := openStatefulSession(t, h, &ckptRecorder{})
		err := m.RestoreIntoLiveSession(context.Background(), "s1", liveSnap("other.v1"))
		if err == nil {
			t.Fatal("expected a loud refusal on schema mismatch")
		}
		if !strings.Contains(err.Error(), "state schema mismatch") || !strings.Contains(err.Error(), "harness.v1") || !strings.Contains(err.Error(), "other.v1") {
			t.Errorf("refusal lacks the schema-mismatch diagnostic: %v", err)
		}
		if len(h.restoredCalls()) != 0 {
			t.Errorf("adapter received %d restore calls on a refused restore; want 0", len(h.restoredCalls()))
		}
	})
	t.Run("adapter-declares-no-state", func(t *testing.T) {
		h := &ckptHandle{decl: &workflow.StateDeclaration{Mode: workflow.StateModeNone}}
		m, _ := openStatefulSession(t, h, &ckptRecorder{})
		err := m.RestoreIntoLiveSession(context.Background(), "s1", liveSnap("harness.v1"))
		if err == nil {
			t.Fatal("expected a loud refusal when the adapter dropped its state declaration")
		}
		if !strings.Contains(err.Error(), "declares no checkpointable state") {
			t.Errorf("refusal lacks the dropped-state diagnostic: %v", err)
		}
		if len(h.restoredCalls()) != 0 {
			t.Errorf("adapter received %d restore calls on a refused restore; want 0", len(h.restoredCalls()))
		}
	})
	t.Run("no-cached-declaration", func(t *testing.T) {
		h := &ckptHandle{decl: &workflow.StateDeclaration{Mode: workflow.StateModeBlob, Schema: "harness.v1"}}
		m, _ := openStatefulSession(t, h, &ckptRecorder{})
		m.mu.Lock()
		delete(m.adapterInfos, "s1")
		m.mu.Unlock()
		err := m.RestoreIntoLiveSession(context.Background(), "s1", liveSnap("harness.v1"))
		if err == nil {
			t.Fatal("expected a loud refusal when no declared surface is cached and the checkpoint is schema-stamped")
		}
		if !strings.Contains(err.Error(), "declares no checkpointable state") {
			t.Errorf("refusal lacks the no-declaration diagnostic: %v", err)
		}
		if len(h.restoredCalls()) != 0 {
			t.Errorf("adapter received %d restore calls on a refused restore; want 0", len(h.restoredCalls()))
		}
	})
}
