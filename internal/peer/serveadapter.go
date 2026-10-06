package peer

// This file hosts the serve-adapter flavor of the phone-home server
// (ADR-0008 child role): instead of fronting a spawned adapter child, the
// criteria process itself implements the v2 AdapterService and serves it
// directly over the peer phone-home connection. There is no child runtime
// to supervise, so the journal is supplied by the caller (the serve-adapter
// CLI command journals the child-run arms for the hosted run) and control
// actions that would have targeted a child process are answered by the
// in-process adapter where it supports them.

import (
	"fmt"
	"log/slog"
	"math/rand"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	adapterhost "github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/tunables"
)

// ChildRunCanceler is the optional extra surface an in-process served
// adapter may implement so the host can cancel a live child run out of band
// (ADR-0008 child role): the host sends PeerService.Control with
// CancelChildRun while the adapter's Execute stream is running, and the
// adapter's engine-stop machinery cancels that run cooperatively. The
// in-flight Execute stream then resolves with a typed canceled failure.
type ChildRunCanceler interface {
	// CancelChildRun cancels the in-flight child run. A non-empty runID
	// selects that specific run; empty means "the current one". It returns
	// the matched run id and whether a matching in-flight run was found
	// (accepted). Cancellation is cooperative: the adapter does not block
	// the RPC on the run settling — the Execute stream carries the result.
	CancelChildRun(runID string) (matched string, accepted bool)
}

// NewServeAdapterServer builds a phone-home server for the serve-adapter
// child role: impl is the workflow-serving AdapterService implemented in
// this process (the CLI command owns engine wiring around it), journal
// records the supervision facts the host replay-streams (nil means an empty
// in-memory journal).
//
// Unlike NewServer, there is no child runtime: the adapter is registered
// unwrapped on the phone-home server, serve-adapter-specific control verbs
// delegate to impl when it implements ChildRunCanceler, and process exit
// after CloseSession teardown is driven by RequestExit.
func NewServeAdapterServer(cfg *Config, impl adapterhost.Client, journal *EventJournal, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if journal == nil {
		journal = NewEventJournal(0)
	}
	s := &Server{
		cfg:                 cfg,
		impl:                impl,
		serveAdapterJournal: journal,
		log:                 log,
		heartbeat:           tunables.FromEnv().HeartbeatInterval,
		capabilities:        defaultPeerCapabilities(),
		exitSignal:          make(chan struct{}),
	}
	s.dialFunc = s.dial
	s.childClient = func() (adapterhost.Client, bool) { return impl, true }
	s.rand = rand.Float64
	s.sleep = sleepCtx
	return s
}

// RequestExit stops the phone-home loop without reconnecting, so the
// process exits once the host's CloseSession teardown is complete
// (ADR-0008 child role: CloseSession cancels the in-flight child run, then
// the child exits). Safe to call repeatedly; subsequent calls are no-ops.
func (s *Server) RequestExit() {
	if s.exitSignal == nil {
		return
	}
	s.exitOnce.Do(func() { close(s.exitSignal) })
}

// ExitRequested reports whether RequestExit has been called.
func (s *Server) ExitRequested() bool {
	select {
	case <-s.exitSignal:
		return true
	default:
		return false
	}
}

// Journal exposes the serve-adapter journal so the CLI command can append
// child-run arms while the phone-home server streams them.
func (s *Server) Journal() *EventJournal {
	return s.journalFor()
}

// journalFor returns the journal supervision streams from: the child
// runtime's journal in the legacy peer role, the caller-supplied journal in
// the serve-adapter role.
func (s *Server) journalFor() *EventJournal {
	if s.rt != nil {
		return s.rt.Journal()
	}
	return s.serveAdapterJournal
}

// controlServeAdapter answers Control verbs when there is no child runtime:
// a child run lives inside this process, so CancelChildRun routes to the
// optional canceler and KillChild — which targets a spawned process — has
// nothing to act on (host-initiated teardown rides CloseSession).
func (s *Server) controlServeAdapter(req *criteriav1.ControlRequest) *criteriav1.ControlResponse {
	if cancel := req.GetCancelChildRun(); cancel != nil {
		canceler, ok := s.impl.(ChildRunCanceler)
		if cancel == nil || !ok {
			return &criteriav1.ControlResponse{
				Accepted: false,
				Detail:   "workflow adapter does not support child-run cancellation",
			}
		}
		matched, accepted := canceler.CancelChildRun(cancel.GetRunId())
		if !accepted {
			return &criteriav1.ControlResponse{
				Accepted: false,
				Detail:   fmt.Sprintf("no in-flight child run %q", cancel.GetRunId()),
			}
		}
		return &criteriav1.ControlResponse{
			Accepted: true,
			Detail:   fmt.Sprintf("cancelling child run %q", matched),
		}
	}
	return &criteriav1.ControlResponse{Accepted: false, Detail: "serve-adapter mode serves no spawned child"}
}
