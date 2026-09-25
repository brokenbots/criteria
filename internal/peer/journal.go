package peer

import (
	"errors"
	"fmt"
	"sync"

	"google.golang.org/protobuf/types/known/timestamppb"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// EventKind is the typed lifecycle payload of a SupervisionEvent: one of the
// generated SupervisionEvent_* oneof wrapper pointers (e.g.
// &criteriav1.SupervisionEvent_Spawned{Spawned: &criteriav1.ProcessSpawned{…}}).
type EventKind any

// EventJournal is the peer's bounded in-process supervision journal
// (ADR-0007 Stage A). Sequence numbers are global, monotonic, and gapless per
// peer process; the ring evicts the oldest events beyond the configured
// limit so terminal crash evidence stays bounded in memory.
//
// Events are stored by pointer: proto message values carry internal mutex
// state and must not be copied around.
type EventJournal struct {
	mu      sync.Mutex
	nextSeq uint64
	ring    []*criteriav1.SupervisionEvent
	limit   int
}

// NewEventJournal returns a journal bounded to limit events. A non-positive
// limit falls back to DefaultJournalLimit.
func NewEventJournal(limit int) *EventJournal {
	if limit <= 0 {
		limit = DefaultJournalLimit
	}
	return &EventJournal{limit: limit}
}

// Append records one lifecycle fact, assigning the next gapless sequence
// number and timestamp. The subject fields identify the supervised adapter;
// kind carries the typed payload. Unknown payload types are rejected — a
// journal entry must carry one of the SupervisionEvent oneof arms.
func (j *EventJournal) Append(kind EventKind, adapterType, scope, sessionID string) (*criteriav1.SupervisionEvent, error) {
	if kind == nil {
		return nil, errors.New("supervision event kind is required")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	ev := &criteriav1.SupervisionEvent{
		EventSeq:    j.nextSeq + 1,
		At:          timestamppb.Now(),
		AdapterType: adapterType,
		Scope:       scope,
		SessionId:   sessionID,
	}
	switch k := kind.(type) {
	case *criteriav1.SupervisionEvent_Spawned:
		ev.Kind = k
	case *criteriav1.SupervisionEvent_Exited:
		ev.Kind = k
	case *criteriav1.SupervisionEvent_Crash:
		ev.Kind = k
	case *criteriav1.SupervisionEvent_Flushed:
		ev.Kind = k
	case *criteriav1.SupervisionEvent_Heartbeat:
		ev.Kind = k
	default:
		return nil, fmt.Errorf("unsupported supervision event payload %T", kind)
	}
	j.nextSeq++
	j.ring = append(j.ring, ev)
	if over := len(j.ring) - j.limit; over > 0 {
		j.ring = j.ring[over:]
	}
	return ev, nil
}

// Replay returns journal events strictly after since (exclusive), in append
// order. A host re-opening Supervise passes the last sequence it saw; 0
// replays the whole journal.
func (j *EventJournal) Replay(since uint64) []*criteriav1.SupervisionEvent {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]*criteriav1.SupervisionEvent, 0, len(j.ring))
	for _, ev := range j.ring {
		if ev.GetEventSeq() > since {
			out = append(out, ev)
		}
	}
	return out
}

// Len returns the number of events currently retained.
func (j *EventJournal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.ring)
}
