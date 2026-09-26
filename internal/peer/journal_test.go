package peer

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func spawnedKind(binary string) *criteriav1.SupervisionEvent_Spawned {
	return &criteriav1.SupervisionEvent_Spawned{Spawned: &criteriav1.ProcessSpawned{Binary: binary}}
}

func TestEventJournal_AppendReplay(t *testing.T) {
	j := NewEventJournal(0) // non-positive limit falls back to the default

	ev1, err := j.Append(spawnedKind("a"), "noopa", "scope/inst", "sess-1")
	if err != nil {
		t.Fatalf("append 1: %v", err)
	}
	ev2, err := j.Append(&criteriav1.SupervisionEvent_Exited{Exited: &criteriav1.ProcessExited{ExitCode: 0, Graceful: true}}, "noopa", "scope/inst", "sess-1")
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	ev3, err := j.Append(&criteriav1.SupervisionEvent_Crash{Crash: &criteriav1.CrashClassified{Reason: "x"}}, "noopa", "scope/inst", "sess-1")
	if err != nil {
		t.Fatalf("append 3: %v", err)
	}

	if ev1.GetEventSeq() != 1 || ev2.GetEventSeq() != 2 || ev3.GetEventSeq() != 3 {
		t.Fatalf("sequence numbers not gapless: %d %d %d", ev1.GetEventSeq(), ev2.GetEventSeq(), ev3.GetEventSeq())
	}
	if ev1.GetSpawned().GetBinary() != "a" {
		t.Errorf("spawned payload lost: %+v", ev1.GetSpawned())
	}
	if ev1.GetAdapterType() != "noopa" || ev1.GetScope() != "scope/inst" || ev1.GetSessionId() != "sess-1" {
		t.Errorf("subject fields lost: %+v", ev1)
	}
	if ev1.GetAt() == nil || ev1.GetAt().AsTime().IsZero() {
		t.Errorf("timestamp missing: %+v", ev1)
	}

	all := j.Replay(0)
	if len(all) != 3 {
		t.Fatalf("replay(0) returned %d events, want 3", len(all))
	}
	if all[0].GetEventSeq() != 1 || all[2].GetEventSeq() != 3 {
		t.Errorf("replay(0) order wrong: %d, %d, %d", all[0].GetEventSeq(), all[1].GetEventSeq(), all[2].GetEventSeq())
	}

	tail := j.Replay(1)
	if len(tail) != 2 || tail[0].GetEventSeq() != 2 || tail[1].GetEventSeq() != 3 {
		t.Fatalf("replay(1) got seqs %d,%d, want 2,3", tail[0].GetEventSeq(), tail[1].GetEventSeq())
	}
	if none := j.Replay(3); len(none) != 0 {
		t.Fatalf("replay(3) = %d events, want empty", len(none))
	}
}

func TestEventJournal_Bounded(t *testing.T) {
	j := NewEventJournal(3)
	for i := 0; i < 5; i++ {
		if _, err := j.Append(spawnedKind(fmt.Sprintf("bin-%d", i)), "noopa", "", ""); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if j.Len() != 3 {
		t.Fatalf("journal len = %d, want 3 (bounded)", j.Len())
	}
	// Oldest evicted; sequence numbers stay gapless.
	events := j.Replay(0)
	if len(events) != 3 {
		t.Fatalf("replay(0) = %d events, want 3", len(events))
	}
	for i, ev := range events {
		if want := uint64(i + 3); ev.GetEventSeq() != want {
			t.Errorf("event %d seq = %d, want %d", i, ev.GetEventSeq(), want)
		}
	}
	if got := events[0].GetSpawned().GetBinary(); got != "bin-2" {
		t.Errorf("oldest retained = %q, want bin-2 (bin-0 and bin-1 evicted)", got)
	}
}

func TestEventJournal_NilKindRejected(t *testing.T) {
	j := NewEventJournal(4)
	if _, err := j.Append(nil, "noopa", "", ""); err == nil {
		t.Fatal("want error for nil kind")
	}
	if j.Len() != 0 {
		t.Fatalf("journal retained %d events after rejected append", j.Len())
	}
}

func TestEventJournal_UnknownPayloadRejected(t *testing.T) {
	j := NewEventJournal(4)
	if _, err := j.Append("spawned", "noopa", "", ""); err == nil {
		t.Fatal("want error for non-oneof payload")
	}
	if _, err := j.Append(&criteriav1.ProcessSpawned{}, "noopa", "", ""); err == nil {
		t.Fatal("want error for bare message payload (oneof wrapper required)")
	}
	if j.Len() != 0 {
		t.Fatalf("journal retained %d events after rejected appends", j.Len())
	}
}

func TestEventJournal_ConcurrentAppend(t *testing.T) {
	j := NewEventJournal(8)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if _, err := j.Append(spawnedKind("bin"), "noopa", "", ""); err != nil {
					t.Errorf("append: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if j.Len() != 8 {
		t.Fatalf("journal len = %d, want 8", j.Len())
	}
	events := j.Replay(0)
	// Gapless monotonic seq: 40 appends happened, oldest 32 evicted.
	if last := events[len(events)-1].GetEventSeq(); last != 40 {
		t.Errorf("last seq = %d, want 40 (40 appends total)", last)
	}
	for i, ev := range events {
		if want := uint64(33 + i); ev.GetEventSeq() != want {
			t.Fatalf("gap in retained sequence: event %d seq = %d, want %d", i, ev.GetEventSeq(), want)
		}
	}
}

func TestChildEnv_ScrubsAllRemoteVars(t *testing.T) {
	parent := []string{
		"CRITERIA_REMOTE_HOST=127.0.0.1:7999",
		"CRITERIA_REMOTE_TOKEN=secret-token",
		"CRITERIA_REMOTE_SCOPE=scope/inst",
		"CRITERIA_REMOTE_DIGEST=sha256:abc",
		"CRITERIA_REMOTE_TLS_CERT=/tls/cert.pem",
		"CRITERIA_REMOTE_TLS_KEY=/tls/key.pem",
		"CRITERIA_REMOTE_CA=/tls/ca.pem",
		"CRITERIA_REMOTE_FUTURE_KNOB=sniffed", // new remote var: prefix scrub covers it
		"CRITERIA_ADAPTER_NAME=noopx",
		"CRITERIA_ADAPTER_BINARY=/bin/criteria-adapter-noopx",
		"PATH=/usr/bin:/bin",
		"HOME=/home/op",
		"CRITERIA_REMOTE=x", // prefix match with nothing after the underscore
		"malformed-no-equals",
	}

	child := ChildEnv(parent)
	childSet := map[string]string{}
	for _, kv := range child {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed entry survived scrub: %q", kv)
		}
		childSet[name] = value
	}

	for _, name := range []string{
		"CRITERIA_REMOTE_HOST", "CRITERIA_REMOTE_TOKEN", "CRITERIA_REMOTE_SCOPE",
		"CRITERIA_REMOTE_DIGEST", "CRITERIA_REMOTE_TLS_CERT", "CRITERIA_REMOTE_TLS_KEY",
		"CRITERIA_REMOTE_CA", "CRITERIA_REMOTE_FUTURE_KNOB", "CRITERIA_REMOTE",
	} {
		if v, ok := childSet[name]; ok {
			t.Errorf("%s leaked into child env (value %q)", name, v)
		}
	}
	for name, want := range map[string]string{
		"CRITERIA_ADAPTER_NAME":   "noopx",
		"CRITERIA_ADAPTER_BINARY": "/bin/criteria-adapter-noopx",
		"PATH":                    "/usr/bin:/bin",
		"HOME":                    "/home/op",
	} {
		if got := childSet[name]; got != want {
			t.Errorf("%s = %q, want %q (must be preserved)", name, got, want)
		}
	}
}

// TestEventJournal_LastSeqAndRingEviction checks that LastSeq keeps counting
// past ring eviction (the replay cursor stays monotonic even when the ring
// no longer retains the oldest events).
func TestEventJournal_LastSeqAndRingEviction(t *testing.T) {
	j := NewEventJournal(2)
	for i := 0; i < 4; i++ {
		if _, err := j.Append(spawnedKind("b"), "noopa", "", ""); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if got := j.LastSeq(); got != 4 {
		t.Errorf("LastSeq = %d, want 4", got)
	}
}

// TestEventJournal_WaitFor is the live-notification contract for the
// Supervise stream: WaitFor returns an immediately-closed channel when
// events past the cursor exist, blocks until the next append otherwise, and
// cannot miss an append that races with the subscribe (lost wakeup).
func TestEventJournal_WaitFor(t *testing.T) {
	j := NewEventJournal(0)

	// Nothing retained: WaitFor(0) must block until an append lands.
	if _, err := j.Append(spawnedKind("a"), "noopa", "", ""); err != nil {
		t.Fatalf("append 1: %v", err)
	}

	// Events past the cursor exist: immediate closed channel.
	select {
	case <-j.WaitFor(0):
	default:
		t.Fatal("WaitFor(0) should be closed immediately when event 1 is retained")
	}

	// Cursor at the top: blocks until the next append.
	ch := j.WaitFor(1)
	done := make(chan struct{})
	go func() {
		<-ch
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("WaitFor(top) closed before the next append")
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := j.Append(spawnedKind("b"), "noopa", "", ""); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitFor(top) did not wake on append")
	}

	// A consumed broadcast fires once: after the wake, the fresh WaitFor
	// call blocks again until another append (no spurious re-fires).
	ch2 := j.WaitFor(2)
	select {
	case <-ch2:
		t.Fatal("WaitFor(fresh cursor) should block until a new append")
	case <-time.After(20 * time.Millisecond):
	}
}
