package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/engine"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
	"github.com/brokenbots/criteria/workflow"
)

// pauseAckTimeout bounds the wait for a boundary-pause request to land. The
// in-flight step (adapter turn, for_each body, ...) completes first; longer
// steps than this make the RPC caller wait instead of blocking apply forever.
const pauseAckTimeout = 2 * time.Minute

// pauseLandingRetryWindow bounds the service-layer retry for verbs that
// race the run's start-up: control.json is published before the engine is
// registered and before the first pause lands, so a fast local control
// client can see transient errRunNotRunning / run_not_paused answers that
// the next step boundary refutes.
const pauseLandingRetryWindow = 2 * time.Second

// errRunNotRunning reports that no run loop is active in the owned engine:
// the run has already reached a terminal state or paused at a node.
var errRunNotRunning = errors.New("run is not running (already terminal or paused at a node)")

// localRunControl is the in-process bus between the loopback control listener
// (CRI-255: JSON seam verbs + the Connect LocalControlService) and the apply
// process's drain loop driving Engine.Pause/Resume semantics. apply owns the
// listener, so the bus is the only authority for the owner run.
//
// Channel semantics: resumeReq / payloadReq are persistent buffered(1)
// mailbox channels (created once, reused across pause cycles) so a decision
// delivered in the window between the pause landing (tracker updated) and
// the drain loop starting to wait is accepted and parked, not refused; at
// the top of every fresh pause cycle the tracker's OnNewPause hook clears
// stale parked entries. ResolveResume / resume park under c.mu
// (drain-then-replace), so repeat deliveries are idempotent and nothing
// consumed by an earlier pause cycle can leak into a later one.
type localRunControl struct {
	runID   string
	graph   *workflow.FSMGraph
	tracker *pauseTracker

	mu  sync.Mutex
	eng *engine.Engine
	// resumeReq / payloadReq are the mailbox channels for boundary-resume
	// tokens and approval/signal decisions (capacity 1 each). They are
	// created once and persist across pause cycles so a decision delivered
	// in the window between the pause landing (tracker updated) and the
	// drain loop taking over is accepted and parked, not refused; each new
	// pause cycle drains stale parked entries via the tracker's OnNewPause
	// hook.
	resumeReq  chan struct{}
	payloadReq chan map[string]string
	// finished flips when the owning apply function exits; the service
	// layer's landing retry treats a not-yet-paused answer as final from
	// then on (a run that exited can never pause).
	finished bool
	// listenerUp reports whether the run-state/control listener attached
	// (bind succeeded). When false, an approval pause without a configured
	// resumer can never resolve: the drain loop fails the run loudly
	// (CRI-256) instead of waiting forever on a dead surface.
	listenerUp bool
}

func newLocalRunControl(runID string, graph *workflow.FSMGraph, tracker *pauseTracker, eng *engine.Engine) *localRunControl {
	c := &localRunControl{
		runID:      runID,
		graph:      graph,
		tracker:    tracker,
		eng:        eng,
		resumeReq:  make(chan struct{}, 1),
		payloadReq: make(chan map[string]string, 1),
	}
	tracker.OnNewPause = c.drainPendingResumes
	return c
}

// drainPendingResumes drops stale parked resume tokens and decisions at the
// start of a new pause cycle: anything parked while the previous pause was
// being resolved (e.g. a racing decision that lost to a file-mode resumer)
// must not leak into this cycle.
func (c *localRunControl) drainPendingResumes() {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.resumeReq:
	default:
	}
	select {
	case <-c.payloadReq:
	default:
	}
}

// setEngine swaps the engine the control RPCs address after a resume cycle
// constructed a fresh engine instance.
func (c *localRunControl) setEngine(eng *engine.Engine) {
	c.mu.Lock()
	c.eng = eng
	c.mu.Unlock()
}

// pause lands the PauseRun verb: ask the active run loop to pause at the next
// checkpoint point (top of the next iteration) and wait until the pause
// machinery has made its durable state (step checkpoint + adapter session
// snapshots) — the ack is the engine's pause-landed channel.
func (c *localRunControl) pause(ctx context.Context) error {
	c.mu.Lock()
	tracker, eng := c.tracker, c.eng
	c.mu.Unlock()
	if node := tracker.PausedAt(); node != "" {
		return fmt.Errorf("run is already paused at node %q; deliver its decision via ResolveResume or resume it via ResumeRun", node)
	}
	if eng == nil {
		return errRunNotRunning
	}
	ack, ok := eng.RequestPause()
	if !ok {
		return errRunNotRunning
	}
	select {
	case <-ack:
		// The ack channel closes on a real landing (after the pause tracker
		// recorded the node and the checkpoint was written) and on a runLoop
		// exit that dropped the request (the run finished or failed before
		// the latch could be honored). Re-read the tracker to tell them
		// apart: no parked node means the run ended without pausing.
		if node := tracker.PausedAt(); node == "" {
			return fmt.Errorf("run ended without pausing; it is no longer running")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(pauseAckTimeout):
		return fmt.Errorf("pause request still pending after %s (the in-flight step has not reached a checkpoint boundary); poll the run status or retry", pauseAckTimeout)
	}
}

// resume lands the boundary-pause ResumeRun verb. The token is parked in the
// mailbox and consumed by the drain loop's awaitBoundaryResume, which then
// drives the engine to the next pause point or terminal state — the response
// returns as soon as the resume was accepted, not when the run completes.
func (c *localRunControl) resume() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.eng == nil {
		return errRunNotRunning
	}
	if node := c.tracker.PausedAt(); isApprovalOrSignalNode(c.graph, node) {
		return fmt.Errorf("run is paused at node %q awaiting an approval or signal decision: deliver it via ResolveResume", node)
	}
	// Replace any parked token: a repeated ResumeRun is idempotent.
	select {
	case <-c.resumeReq:
	default:
	}
	c.resumeReq <- struct{}{}
	return nil
}

// resolveResume delivers an approval decision or signal outcome. The decision
// is validated against the paused node's contract before it is parked in the
// mailbox for the drain loop: an invalid payload would otherwise fail the run
// inside the engine when the resume re-evaluates the node. A decision
// delivered in the window between the pause landing and the drain loop taking
// over is parked and accepted too (the mailbox persists across that gap).
func (c *localRunControl) resolveResume(signal string, payload map[string]string) (accepted bool, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pausedNode := c.tracker.PausedAt()
	if pausedNode == "" {
		return false, "run_not_paused"
	}
	if signal == "" {
		return false, "no_pending_signal"
	}
	target, ok := resolveSignalTarget(c.graph, signal)
	if !ok {
		return false, "no_pending_signal"
	}
	if target != pausedNode {
		return false, "signal_mismatch"
	}
	if reason := validatePausePayload(c.graph, pausedNode, payload); reason != "" {
		return false, reason
	}
	// Replace any parked decision: a repeated ResolveResume is idempotent.
	select {
	case <-c.payloadReq:
	default:
	}
	c.payloadReq <- payload
	return true, "ok"
}

// resolveSignalTarget maps a signal name to the paused node that satisfies
// it: an approval node by its (own) name, a signal wait by its declared
// signal (a wait node's name is accepted too).
func resolveSignalTarget(graph *workflow.FSMGraph, signal string) (string, bool) {
	if a, ok := graph.Approvals[signal]; ok {
		return a.Name, true
	}
	if signal == "" {
		return "", false
	}
	for _, w := range graph.Waits {
		if w.Signal == signal || w.Name == signal {
			return w.Name, true
		}
	}
	return "", false
}

// validatePausePayload checks the decision payload against the paused node
// before acceptance: an invalid decision would otherwise fail the run inside
// the engine's node evaluation on resume. Returns "" when valid.
func validatePausePayload(graph *workflow.FSMGraph, node string, payload map[string]string) string {
	if a, ok := graph.Approvals[node]; ok {
		decision := payload["decision"]
		if decision == "" {
			return "signal_mismatch"
		}
		if _, ok := a.Outcomes[decision]; !ok {
			return "signal_mismatch"
		}
		return ""
	}
	w, ok := graph.Waits[node]
	if !ok || w.Signal == "" {
		// A non-resolvable pause waits for a boundary ResumeRun; a delivered
		// payload has no meaning there.
		return "signal_mismatch"
	}
	if len(w.Outcomes) > 1 {
		// Multi-outcome signal waits require payload["outcome"].
		if _, ok := w.Outcomes[payload["outcome"]]; !ok {
			return "signal_mismatch"
		}
		return ""
	}
	// Single-outcome waits tolerate a payload-free resume (documented
	// fallback); when a selector is provided it must still be valid.
	if outcome := payload["outcome"]; outcome != "" {
		if _, ok := w.Outcomes[outcome]; !ok {
			return "signal_mismatch"
		}
	}
	return ""
}

// awaitBoundaryResume blocks until a boundary-resume token arrives (parked by
// ResumeRun) or the context is canceled. ok reports whether a resume (rather
// than a context cancellation) arrived.
func (c *localRunControl) awaitBoundaryResume(ctx context.Context) bool {
	c.mu.Lock()
	ch := c.resumeReq
	c.mu.Unlock()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

// awaitResolveResume blocks until an approval/signal decision arrives (parked
// by ResolveResume, possibly before this call) or the context is canceled.
func (c *localRunControl) awaitResolveResume(ctx context.Context) (map[string]string, bool) {
	c.mu.Lock()
	ch := c.payloadReq
	c.mu.Unlock()
	select {
	case payload := <-ch:
		return payload, true
	case <-ctx.Done():
		return nil, false
	}
}

// pollResolveResume non-blockingly consumes a parked approval/signal decision,
// if one is waiting. resolveApprovalPause uses it so a decision delivered
// while the pause was landing wins the resolution race before any configured
// resumer (CRI-256) gets the chance to answer first.
func (c *localRunControl) pollResolveResume() (map[string]string, bool) {
	c.mu.Lock()
	ch := c.payloadReq
	c.mu.Unlock()
	select {
	case payload := <-ch:
		return payload, true
	default:
		return nil, false
	}
}

// resolveResumeChan exposes the decision mailbox so a racing consumer can
// select over it; the channel is created once and stable for the run's
// lifetime.
func (c *localRunControl) resolveResumeChan() <-chan map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.payloadReq
}

// setListenerUp records whether the run's control listener actually attached
// (bind succeeded). The drain loop needs this to distinguish a pause that
// awaits a live control surface from one that can never be resolved.
func (c *localRunControl) setListenerUp(up bool) {
	c.mu.Lock()
	c.listenerUp = up
	c.mu.Unlock()
}

func (c *localRunControl) isListenerUp() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.listenerUp
}

// isApprovalOrSignalNode reports whether node pauses inside its evaluation
// (approval and signal-wait nodes) rather than at a run-loop checkpoint
// boundary.
func isApprovalOrSignalNode(graph *workflow.FSMGraph, node string) bool {
	if _, ok := graph.Approvals[node]; ok {
		return true
	}
	w, ok := graph.Waits[node]
	return ok && w.Signal != ""
}

// localControlService implements criteria.v1.LocalControlServiceHandler for
// the local run's loopback listener (CRI-255). All methods drive the same
// Engine.Pause/Resume semantics the castle Control stream exercises.
type localControlService struct {
	ctrl  *localRunControl
	runID string
}

func (s *localControlService) PauseRun(ctx context.Context, req *pb.PauseRunRequest) (*pb.PauseRunResponse, error) {
	if err := s.checkRun(req.RunId); err != nil {
		return nil, err
	}
	if err := s.ctrlPause(ctx); err != nil {
		return nil, pauseErrorToConnect(err)
	}
	return &pb.PauseRunResponse{}, nil
}

// ctrlPause retries across the pause-landing window: the listener publishes
// control.json before the run's engine is registered and before the first
// pause lands, so a fast control client can observe a transient
// errRunNotRunning that the very next step boundary refutes. Wait briefly
// instead of failing the verb on that window (not on a genuinely terminal
// run, which keeps failing fast).
func (s *localControlService) ctrlPause(ctx context.Context) error {
	deadline := time.Now().Add(pauseLandingRetryWindow)
	for {
		err := s.ctrl.pause(ctx)
		if err == nil || !errors.Is(err, errRunNotRunning) || ctx.Err() != nil {
			return err
		}
		if s.ctrl.isFinished() || time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *localControlService) ResumeRun(_ context.Context, req *pb.ResumeRunRequest) (*pb.ResumeRunResponse, error) {
	if err := s.checkRun(req.RunId); err != nil {
		return nil, err
	}
	if err := s.ctrlResume(); err != nil {
		return nil, pauseErrorToConnect(err)
	}
	return &pb.ResumeRunResponse{}, nil
}

// ctrlResume retries across the pause-landing window (see ctrlPause): the
// engine pointer registers moments after the listener comes up, and an
// early resume request must not fail the verb.
func (s *localControlService) ctrlResume() error {
	deadline := time.Now().Add(pauseLandingRetryWindow)
	for {
		err := s.ctrl.resume()
		if err == nil || !errors.Is(err, errRunNotRunning) {
			return err
		}
		if s.ctrl.isFinished() || time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *localControlService) ResolveResume(_ context.Context, req *pb.ResumeRequest) (*pb.ResumeResponse, error) {
	if err := s.checkRun(req.RunId); err != nil {
		return nil, err
	}
	accepted, reason := s.ctrlResolve(req.Signal, req.Payload)
	return &pb.ResumeResponse{Accepted: accepted, Reason: reason}, nil
}

// ctrlResolve retries run_not_paused across the pause-landing window
// (see ctrlPause): approval/signal decisions may arrive while the run is
// still traveling toward its pause. Other reasons (signal mismatch, invalid
// payload) are answered immediately.
func (s *localControlService) ctrlResolve(signal string, payload map[string]string) (accepted bool, reason string) {
	deadline := time.Now().Add(pauseLandingRetryWindow)
	for {
		accepted, reason := s.ctrl.resolveResume(signal, payload)
		if accepted || reason != "run_not_paused" {
			return accepted, reason
		}
		if s.ctrl.isFinished() || time.Now().After(deadline) {
			return accepted, reason
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// isFinished reports whether the owning run reached a terminal state (or
// otherwise exited its owning function): a finished run never pauses, so a
// not-yet-paused answer is final rather than transient.
func (c *localRunControl) isFinished() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finished
}

// markFinished flips the terminal latch used by the service-layer landing
// retry; called when the owning apply function exits (the run can no longer
// pause after that point).
func (c *localRunControl) markFinished() {
	c.mu.Lock()
	c.finished = true
	c.mu.Unlock()
}

// checkRun rejects requests for a foreign run id: the local control listener
// serves exactly the apply process's own run.
func (s *localControlService) checkRun(runID string) error {
	if runID != s.runID {
		return connect.Errorf(connect.CodeNotFound, "run %q is not served by this control listener", runID)
	}
	return nil
}

func pauseErrorToConnect(err error) error {
	return connect.NewError(connect.CodeFailedPrecondition, err.Error()).WithCause(err)
}

// mountLocalControlService builds the HTTP handler for the local run control
// listener's LocalControlService (v2 connect server/register/mount shape) and
// returns the service subtree pattern plus handler for
// runstate.Server.WithLocalService.
func mountLocalControlService(svc criteriav1connect.LocalControlServiceHandler) (string, http.Handler) {
	server := connect.NewServer()
	criteriav1connect.RegisterLocalControlServiceHandler(server, svc)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	return criteriav1connect.LocalControlServiceName + "/", mux
}

// controlEndpoint is the discovery record a local apply writes next to its
// run-state.json while the run's control listener is up. Consumers discover
// the endpoint by run id instead of scraping an address from logs.
type controlEndpoint struct {
	Protocol    string `json:"protocol"`
	ControlAddr string `json:"control_addr"`
	RunID       string `json:"run_id"`
	PID         int    `json:"pid"`
}

const controlEndpointProtocol = "criteria.v1.LocalControlService/connect"

func controlEndpointPath(runID string) (string, error) {
	d, err := runDataDir(runID)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "control.json"), nil
}

func writeControlEndpoint(runID, addr string) error {
	p, err := controlEndpointPath(runID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create run dir for control endpoint: %w", err)
	}
	ep := controlEndpoint{Protocol: controlEndpointProtocol, ControlAddr: addr, RunID: runID, PID: os.Getpid()}
	b, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		return fmt.Errorf("encode control endpoint: %w", err)
	}
	return atomicReplaceFile(p, b, nil)
}

func removeControlEndpoint(runID string) {
	if p, err := controlEndpointPath(runID); err == nil {
		_ = os.Remove(p)
	}
}

// LocalControlEndpoint resolves the control endpoint a local run published in
// its state dir. Returns "", nil when the run has no listener record (it may
// still be a castle run; callers fall back to the server surface).
func LocalControlEndpoint(runID string) (string, error) {
	if runID == "" {
		return "", errors.New("run_id required")
	}
	p, err := controlEndpointPath(runID)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read control endpoint: %w", err)
	}
	var ep controlEndpoint
	if err := json.Unmarshal(b, &ep); err != nil {
		return "", fmt.Errorf("decode control endpoint for run %s: %w", runID, err)
	}
	if ep.Protocol != controlEndpointProtocol {
		return "", fmt.Errorf("run %s publishes control protocol %q; this CLI speaks %q", runID, ep.Protocol, controlEndpointProtocol)
	}
	if strings.TrimSpace(ep.RunID) != runID || strings.TrimSpace(ep.ControlAddr) == "" {
		return "", fmt.Errorf("control endpoint for run %s is incomplete", runID)
	}
	if err := validateControlHostPort(ep.ControlAddr); err != nil {
		return "", fmt.Errorf("control endpoint for run %s is invalid: %w", runID, err)
	}
	return ep.ControlAddr, nil
}

// localControlClient builds a Connect LocalControlService client for a
// loopback control listener address.
func localControlClient(addr string) criteriav1connect.LocalControlServiceClient {
	return criteriav1connect.NewLocalControlServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{}, "http://"+addr)))
}

// localControlServiceClientFor is the client seam used by the pause/resume/
// approve verbs; tests inject a client pointed at an httptest server.
var localControlServiceClientFor = localControlClient

// controlSurfaceOverridden reports whether the pause/resume caller explicitly
// targeted the castle control surface: a --server flag on the command or a
// CRITERIA_SERVER_URL environment value. Otherwise local runs resolve their
// listener from the run's state dir (LocalControlEndpoint).
func controlSurfaceOverridden(cmd *cobra.Command) bool {
	if cmd.Flags().Changed("server") {
		return true
	}
	return os.Getenv("CRITERIA_SERVER_URL") != ""
}

// resolveControlAddr parses --control-addr ("host:port"). The empty flag
// binds a random loopback port. localhost maps to 127.0.0.1 (the runstate
// listener binds IP literals only); a non-loopback host is refused.
func resolveControlAddr(flag string) (host string, port int, err error) {
	raw := strings.TrimSpace(flag)
	if raw == "" {
		return "127.0.0.1", 0, nil
	}
	host, portRaw, err := net.SplitHostPort(raw)
	if err != nil {
		return "", 0, fmt.Errorf("invalid --control-addr %q: %w", flag, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	port, aErr := strconv.Atoi(portRaw)
	if aErr != nil {
		return "", 0, fmt.Errorf("invalid --control-addr %q: %w", flag, aErr)
	}
	if jErr := validateControlHostPort(net.JoinHostPort(host, portRaw)); jErr != nil {
		return "", 0, fmt.Errorf("invalid --control-addr %q: %w", flag, jErr)
	}
	return host, port, nil
}

// validateControlHostPort enforces the locked CRI-255 rule: the control
// listener never binds wider than loopback. Both the apply-side bind and the
// client-side endpoint record go through it.
func validateControlHostPort(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address %q lacks a port: %w", addr, err)
	}
	h := host
	if h == "" {
		h = "127.0.0.1"
	}
	ip := net.ParseIP(h)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("refusing non-loopback control address %q: the local control listener is loopback-only", addr)
	}
	return nil
}
