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

	"connectrpc.com/connect"
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

func (s *localControlService) PauseRun(ctx context.Context, req *connect.Request[pb.PauseRunRequest]) (*connect.Response[pb.PauseRunResponse], error) {
	if err := s.checkRun(req.Msg.RunId); err != nil {
		return nil, err
	}
	if err := s.ctrl.pause(ctx); err != nil {
		return nil, pauseErrorToConnect(err)
	}
	return connect.NewResponse(&pb.PauseRunResponse{}), nil
}

func (s *localControlService) ResumeRun(_ context.Context, req *connect.Request[pb.ResumeRunRequest]) (*connect.Response[pb.ResumeRunResponse], error) {
	if err := s.checkRun(req.Msg.RunId); err != nil {
		return nil, err
	}
	if err := s.ctrl.resume(); err != nil {
		return nil, pauseErrorToConnect(err)
	}
	return connect.NewResponse(&pb.ResumeRunResponse{}), nil
}

func (s *localControlService) ResolveResume(_ context.Context, req *connect.Request[pb.ResumeRequest]) (*connect.Response[pb.ResumeResponse], error) {
	if err := s.checkRun(req.Msg.RunId); err != nil {
		return nil, err
	}
	accepted, reason := s.ctrl.resolveResume(req.Msg.Signal, req.Msg.Payload)
	return connect.NewResponse(&pb.ResumeResponse{Accepted: accepted, Reason: reason}), nil
}

// checkRun rejects requests for a foreign run id: the local control listener
// serves exactly the apply process's own run.
func (s *localControlService) checkRun(runID string) error {
	if runID != s.runID {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("run %q is not served by this control listener", runID))
	}
	return nil
}

func pauseErrorToConnect(err error) error {
	return connect.NewError(connect.CodeFailedPrecondition, err)
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
	hc := &http.Client{}
	return criteriav1connect.NewLocalControlServiceClient(hc, "http://"+addr)
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
