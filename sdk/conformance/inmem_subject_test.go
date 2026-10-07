// Package conformance_test — in-memory Subject for the SDK conformance suite.
//
// This file provides an in-memory implementation of the conformance.Subject
// interface. It runs the full conformance suite without a server instance,
// proving the SDK contract is implementable by any compliant orchestrator.
package conformance_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"golang.org/x/net/http2"

	criteria "github.com/brokenbots/criteria/sdk"
	"github.com/brokenbots/criteria/sdk/conformance"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// TestConformance runs the full SDK conformance suite against the in-memory
// Subject implementation, proving the contract is implementable without a server.
func TestConformance(t *testing.T) {
	conformance.Run(t, &inMemSubject{handlers: make(map[string]*inMemHandler)})
}

// ---------------------------------------------------------------------------
// inMemSubject — conformance.Subject
// ---------------------------------------------------------------------------

type inMemSubject struct {
	mu       sync.Mutex
	handlers map[string]*inMemHandler
}

func (s *inMemSubject) SetUp(t *testing.T) (baseURL string, client *http.Client, teardown func()) {
	t.Helper()
	h := newInMemHandler()

	s.mu.Lock()
	s.handlers[t.Name()] = h
	s.mu.Unlock()
	t.Cleanup(func() {
		s.mu.Lock()
		delete(s.handlers, t.Name())
		s.mu.Unlock()
	})

	mux := http.NewServeMux()
	oPath, oHandler := criteria.NewServiceHandler(h)
	mux.Handle(oPath, oHandler)
	cServer := connect.NewServer()
	criteriav1connect.RegisterServerServiceHandler(cServer, h)
	connecthttp.Mount(mux, cServer)

	srv := httptest.NewUnstartedServer(mux)
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = &protocols
	srv.Start()
	httpClient := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return net.Dial(network, addr)
			},
		},
	}
	t.Cleanup(srv.Close)
	return srv.URL, httpClient, srv.Close
}

// handlerForTest walks up the test-name hierarchy to find the handler created
// by the nearest ancestor SetUp call.
func (s *inMemSubject) handlerForTest(t *testing.T) *inMemHandler {
	t.Helper()
	name := t.Name()
	for name != "" {
		s.mu.Lock()
		h, ok := s.handlers[name]
		s.mu.Unlock()
		if ok {
			return h
		}
		idx := strings.LastIndex(name, "/")
		if idx < 0 {
			break
		}
		name = name[:idx]
	}
	return nil
}

func (s *inMemSubject) RegisterAgent(t *testing.T, name, token string) string {
	t.Helper()
	h := s.handlerForTest(t)
	if h == nil {
		t.Fatal("RegisterAgent: no handler found; call SetUp first")
	}
	return h.registerAgent(name, token)
}

func (s *inMemSubject) ListRunEvents(t *testing.T, baseURL string, client *http.Client, token, runID string, sinceSeq uint64) []*pb.Envelope {
	t.Helper()
	cClient := criteriav1connect.NewServerServiceClient(connect.NewClient(connecthttp.NewTransport(client, baseURL)))
	ctx, info := connect.NewClientContext(context.Background())
	info.RequestHeader().Set("Authorization", "Bearer "+token)
	resp, err := cClient.ListRunEvents(ctx, &pb.ListRunEventsRequest{RunId: runID, SinceSeq: sinceSeq})
	if err != nil {
		t.Fatalf("ListRunEvents: %v", err)
	}
	return resp.Events
}

func (s *inMemSubject) StopRun(t *testing.T, baseURL string, client *http.Client, token, runID string) error {
	t.Helper()
	cClient := criteriav1connect.NewServerServiceClient(connect.NewClient(connecthttp.NewTransport(client, baseURL)))
	ctx, info := connect.NewClientContext(context.Background())
	info.RequestHeader().Set("Authorization", "Bearer "+token)
	_, err := cClient.StopRun(ctx, &pb.StopRunRequest{RunId: runID})
	return err
}

// ---------------------------------------------------------------------------
// Run state machine
// ---------------------------------------------------------------------------

type runState int

const (
	runStatePending  runState = iota // created, not yet paused or terminal
	runStatePaused                   // paused at a wait/approval node
	runStateTerminal                 // completed or failed
)

type pauseKind int

const (
	pauseKindNone     pauseKind = iota
	pauseKindWait               // paused at a wait{signal=...} node
	pauseKindApproval           // paused at an approval{} node
)

type runRecord struct {
	runID      string
	criteriaID string
	workflow   string

	state  runState
	pause  pauseKind
	signal string // current wait-signal or approval-node name

	events     []*pb.Envelope
	corrIndex  map[string]uint64 // correlation_id → assigned seq (for dedup)
	seqCounter atomic.Uint64
}

func newRunRecord(runID, criteriaID, workflow string) *runRecord {
	return &runRecord{
		runID:      runID,
		criteriaID: criteriaID,
		workflow:   workflow,
		corrIndex:  make(map[string]uint64),
	}
}

// ---------------------------------------------------------------------------
// inMemHandler — CriteriaService + ServerService
// ---------------------------------------------------------------------------

type controlSubscription struct {
	criteriaID string
	ch         chan *pb.ControlMessage
}

type agentRecord struct {
	name  string
	token string
}

type inMemHandler struct {
	criteriav1connect.UnimplementedCriteriaServiceHandler
	criteriav1connect.UnimplementedServerServiceHandler

	mu            sync.Mutex
	agents        map[string]*agentRecord // id → record
	tokenToID     map[string]string       // token → id
	runs          map[string]*runRecord   // run_id → record
	subscriptions []*controlSubscription
	runCounter    atomic.Uint64
}

func newInMemHandler() *inMemHandler {
	return &inMemHandler{
		agents:    make(map[string]*agentRecord),
		tokenToID: make(map[string]string),
		runs:      make(map[string]*runRecord),
	}
}

func (h *inMemHandler) registerAgent(name, token string) string {
	id := "inmem-" + name
	h.mu.Lock()
	h.agents[id] = &agentRecord{name: name, token: token}
	h.tokenToID[token] = id
	h.mu.Unlock()
	return id
}

func (h *inMemHandler) authAgent(authHeader string) (string, error) {
	token := strings.TrimPrefix(authHeader, "Bearer ")
	h.mu.Lock()
	id, ok := h.tokenToID[token]
	h.mu.Unlock()
	if !ok {
		return "", connect.NewError(connect.CodeUnauthenticated, "")
	}
	return id, nil
}

// serverAuthHeader extracts the Authorization header from a server-side call
// context.
func serverAuthHeader(ctx context.Context) string {
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		return info.RequestHeader().Get("Authorization")
	}
	return ""
}

func assertOwnsAgent(callerID, ownerID string) error {
	if callerID != ownerID {
		return connect.NewError(connect.CodePermissionDenied, "")
	}
	return nil
}

func (h *inMemHandler) assertOwnsRun(callerID, runID string) (*runRecord, error) {
	h.mu.Lock()
	run, ok := h.runs[runID]
	h.mu.Unlock()
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "")
	}
	if run.criteriaID != callerID {
		return nil, connect.NewError(connect.CodePermissionDenied, "")
	}
	return run, nil
}

// CriteriaService handlers

func (h *inMemHandler) Register(_ context.Context, _ *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, "")
}

func (h *inMemHandler) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	callerID, err := h.authAgent(serverAuthHeader(ctx))
	if err != nil {
		return nil, err
	}
	if err := assertOwnsAgent(callerID, req.CriteriaId); err != nil {
		return nil, err
	}
	return &pb.HeartbeatResponse{}, nil
}

func (h *inMemHandler) CreateRun(ctx context.Context, req *pb.CreateRunRequest) (*pb.Run, error) {
	callerID, err := h.authAgent(serverAuthHeader(ctx))
	if err != nil {
		return nil, err
	}
	if err := assertOwnsAgent(callerID, req.CriteriaId); err != nil {
		return nil, err
	}
	c := h.runCounter.Add(1)
	runID := fmt.Sprintf("run-%s-%d", req.WorkflowName, c)
	run := newRunRecord(runID, callerID, req.WorkflowName)
	h.mu.Lock()
	h.runs[runID] = run
	h.mu.Unlock()
	return &pb.Run{RunId: runID, WorkflowName: req.WorkflowName}, nil
}

func (h *inMemHandler) ReattachRun(ctx context.Context, req *pb.ReattachRunRequest) (*pb.ReattachRunResponse, error) {
	callerID, err := h.authAgent(serverAuthHeader(ctx))
	if err != nil {
		return nil, err
	}
	if err := assertOwnsAgent(callerID, req.CriteriaId); err != nil {
		return nil, err
	}
	run, err := h.assertOwnsRun(callerID, req.RunId)
	if err != nil {
		return nil, err
	}
	return &pb.ReattachRunResponse{
		Status:    "created",
		CanResume: run.state == runStatePaused,
	}, nil
}

func (h *inMemHandler) Resume(ctx context.Context, req *pb.ResumeRequest) (*pb.ResumeResponse, error) {
	callerID, err := h.authAgent(serverAuthHeader(ctx))
	if err != nil {
		return nil, err
	}
	run, err := h.assertOwnsRun(callerID, req.RunId)
	if err != nil {
		return nil, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if run.state != runStatePaused {
		return &pb.ResumeResponse{Accepted: false, Reason: "run_not_paused"}, nil
	}
	if run.signal != req.Signal {
		return &pb.ResumeResponse{Accepted: false, Reason: "signal_mismatch"}, nil
	}

	// Persist the resume event, then clear paused state.
	seq := run.seqCounter.Add(1)
	env := &pb.Envelope{RunId: req.RunId, Seq: seq}
	switch run.pause {
	case pauseKindApproval:
		env.Payload = &pb.Envelope_ApprovalDecision{ApprovalDecision: &pb.ApprovalDecision{
			Node:     run.signal,
			Decision: req.Payload["decision"],
			Actor:    req.Payload["actor"],
		}}
	default: // pauseKindWait
		env.Payload = &pb.Envelope_WaitResumed{WaitResumed: &pb.WaitResumed{
			Node:   run.signal,
			Signal: run.signal,
		}}
	}
	run.events = append(run.events, env)
	run.state = runStatePending
	run.pause = pauseKindNone
	run.signal = ""

	return &pb.ResumeResponse{Accepted: true}, nil
}

func (h *inMemHandler) SubmitEvents(ctx context.Context, stream criteriav1connect.CriteriaServiceSubmitEventsServerStream) error {
	callerID, err := h.authAgent(serverAuthHeader(ctx))
	if err != nil {
		return err
	}
	for {
		env, err := stream.Receive()
		if err != nil {
			return nil //nolint:nilerr // EOF is normal end-of-stream
		}
		if env.SchemaVersion != 0 && env.SchemaVersion != criteria.SchemaVersion {
			return connect.NewError(connect.CodeFailedPrecondition, "")
		}

		h.mu.Lock()
		run, ok := h.runs[env.RunId]
		if !ok {
			h.mu.Unlock()
			return connect.NewError(connect.CodeNotFound, "")
		}
		if run.criteriaID != callerID {
			h.mu.Unlock()
			return connect.NewError(connect.CodePermissionDenied, "")
		}

		// Idempotency: same correlation_id → return the same seq without re-persisting.
		if env.CorrelationId != "" {
			if prev, seen := run.corrIndex[env.CorrelationId]; seen {
				h.mu.Unlock()
				if err := stream.Send(&pb.Ack{Seq: prev, CorrelationId: env.CorrelationId}); err != nil {
					return err
				}
				continue
			}
		}

		seq := run.seqCounter.Add(1)
		env.Seq = seq
		if env.CorrelationId != "" {
			run.corrIndex[env.CorrelationId] = seq
		}
		run.events = append(run.events, env)

		// Update run state based on event type.
		switch p := env.Payload.(type) {
		case *pb.Envelope_WaitEntered:
			if p.WaitEntered.Mode == "signal" {
				run.state = runStatePaused
				run.pause = pauseKindWait
				run.signal = p.WaitEntered.Signal
			}
		case *pb.Envelope_ApprovalRequested:
			run.state = runStatePaused
			run.pause = pauseKindApproval
			run.signal = p.ApprovalRequested.Node
		case *pb.Envelope_RunCompleted:
			run.state = runStateTerminal
		case *pb.Envelope_RunFailed:
			run.state = runStateTerminal
		}
		h.mu.Unlock()

		if err := stream.Send(&pb.Ack{Seq: seq, CorrelationId: env.CorrelationId}); err != nil {
			return err
		}
	}
}

func (h *inMemHandler) Control(ctx context.Context, req *pb.ControlSubscribeRequest, stream criteriav1connect.CriteriaServiceControlServerStream) error {
	criteriaID, err := h.authAgent(serverAuthHeader(ctx))
	if err != nil {
		return err
	}
	if req.CriteriaId != "" {
		if err := assertOwnsAgent(criteriaID, req.CriteriaId); err != nil {
			return err
		}
	}

	sub := &controlSubscription{
		criteriaID: criteriaID,
		ch:         make(chan *pb.ControlMessage, 16),
	}
	h.mu.Lock()
	h.subscriptions = append(h.subscriptions, sub)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		for i, s := range h.subscriptions {
			if s == sub {
				h.subscriptions = append(h.subscriptions[:i], h.subscriptions[i+1:]...)
				break
			}
		}
		h.mu.Unlock()
	}()

	// First message must be ControlReady.
	if err := stream.Send(&pb.ControlMessage{
		Command: &pb.ControlMessage_ControlReady{ControlReady: &pb.ControlReady{}},
	}); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-sub.ch:
			if !ok {
				return nil
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

// ServerService handlers

func (h *inMemHandler) ListAgents(_ context.Context, _ *pb.ListAgentsRequest) (*pb.ListAgentsResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*pb.Agent, 0, len(h.agents))
	for id, rec := range h.agents {
		out = append(out, &pb.Agent{CriteriaId: id, Name: rec.name, Status: "online"})
	}
	return &pb.ListAgentsResponse{Agents: out}, nil
}

func (h *inMemHandler) GetAgent(_ context.Context, req *pb.GetAgentRequest) (*pb.Agent, error) {
	h.mu.Lock()
	rec, ok := h.agents[req.CriteriaId]
	h.mu.Unlock()
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "")
	}
	return &pb.Agent{CriteriaId: req.CriteriaId, Name: rec.name}, nil
}

func (h *inMemHandler) ListRuns(_ context.Context, _ *pb.ListRunsRequest) (*pb.ListRunsResponse, error) {
	return &pb.ListRunsResponse{}, nil
}

func (h *inMemHandler) GetRun(_ context.Context, req *pb.GetRunRequest) (*pb.Run, error) {
	h.mu.Lock()
	run, ok := h.runs[req.RunId]
	h.mu.Unlock()
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "")
	}
	return &pb.Run{RunId: run.runID, WorkflowName: run.workflow}, nil
}

func (h *inMemHandler) ListRunEvents(_ context.Context, req *pb.ListRunEventsRequest) (*pb.ListRunEventsResponse, error) {
	h.mu.Lock()
	run, ok := h.runs[req.RunId]
	if !ok {
		h.mu.Unlock()
		return nil, connect.NewError(connect.CodeNotFound, "")
	}
	var events []*pb.Envelope
	for _, e := range run.events {
		if e.Seq > req.SinceSeq {
			events = append(events, e)
		}
	}
	h.mu.Unlock()
	return &pb.ListRunEventsResponse{Events: events}, nil
}

func (h *inMemHandler) WatchRun(ctx context.Context, req *pb.WatchRunRequest, stream criteriav1connect.ServerServiceWatchRunServerStream) error {
	h.mu.Lock()
	run, ok := h.runs[req.RunId]
	if !ok {
		h.mu.Unlock()
		return connect.NewError(connect.CodeNotFound, "")
	}
	var lastSent uint64
	for _, e := range run.events {
		if e.Seq > req.SinceSeq {
			if err := stream.Send(e); err != nil {
				h.mu.Unlock()
				return err
			}
			lastSent = e.Seq
		}
	}
	h.mu.Unlock()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			h.mu.Lock()
			var toSend []*pb.Envelope
			for _, e := range run.events {
				if e.Seq > lastSent {
					toSend = append(toSend, e)
				}
			}
			h.mu.Unlock()
			for _, e := range toSend {
				if err := stream.Send(e); err != nil {
					return err
				}
				lastSent = e.Seq
			}
		}
	}
}

func (h *inMemHandler) StopRun(_ context.Context, req *pb.StopRunRequest) (*pb.StopRunResponse, error) {
	h.mu.Lock()
	run, ok := h.runs[req.RunId]
	if !ok {
		h.mu.Unlock()
		return nil, connect.NewError(connect.CodeNotFound, "")
	}
	ownerID := run.criteriaID
	var target *controlSubscription
	for _, sub := range h.subscriptions {
		if sub.criteriaID == ownerID {
			target = sub
			break
		}
	}
	h.mu.Unlock()

	if target == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "")
	}
	select {
	case target.ch <- &pb.ControlMessage{Command: &pb.ControlMessage_RunCancel{
		RunCancel: &pb.RunCancel{RunId: req.RunId, Reason: req.Reason},
	}}:
	default:
	}
	return &pb.StopRunResponse{}, nil
}
