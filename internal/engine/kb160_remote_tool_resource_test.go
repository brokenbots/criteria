package engine

// kb160_remote_tool_resource_test.go — KB-160: the remote-environment half of
// tool-resource host routing. The KB-58 borrow path deliberately skips
// adapters whose resolved environment is remote (BorrowToolResourceSessionsFrom
// carries host-local records only), and in a parallel-iteration SessionManager
// nothing else ever verifies the root-declared remote callee, so the nested
// tool call misses lookupOrBind and dies with typed unknown_adapter.
//
// Under the peer model (ADR-0008 cards 93-95) a remote environment runs a
// criteria peer that dials the main process and hosts ALL of that
// environment's adapters — the peer is that environment's host-of-record. The
// fix routes the nested tool call to the OWNER SessionManager that provisioned
// the environment (the root scope, which verified the peer-hosted session),
// executes it over the peer's ONE shared phone-home session, and keeps every
// borrowed iteration manager from binding or closing anything.
//
// The fixture runs the REAL engine path on both ends: a real phone-home shim
// started by Run, and the real production peer phone-home dial/serve loop
// (peer.NewServeAdapterServer) whose adapter surface is an injected fake
// recorder, so the assertions observe the exact shared-session traffic a real
// peer would see.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2/hclparse"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapterhost"
	peerpkg "github.com/brokenbots/criteria/internal/peer"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

const (
	kb160RemoteEnvKey = "remote.cell"
	// kb160PeerDigest pairs with the mcp pin in kb160PinSet: the fake peer
	// presents this runtime digest, the shim's lockfile verifier accepts it.
	kb160PeerDigest = "sha256:abcd1234"
)

func kb160PinSet() *lockfile.Lockfile {
	return &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "mcp", ResolvedDigest: kb160PeerDigest},
		},
	}
}

// kb160ReservePort binds a loopback port and releases it: the remote env
// declares the reserved address so the test knows the phone-home address
// before Run starts the shim on it.
func kb160ReservePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	listen := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	return listen
}

func kb160RemoteEnv(t *testing.T, listen string) *workflow.EnvironmentNode {
	t.Helper()
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL([]byte(fmt.Sprintf("listen_address = %q\ninsecure = true\n", listen)), "remote.hcl")
	if diags.HasErrors() {
		t.Fatalf("parse remote body: %s", diags)
	}
	return &workflow.EnvironmentNode{
		Type:    "remote",
		Name:    "cell",
		Process: &workflow.ProcessPolicy{Exec: []string{"*"}},
		RawBody: file.Body,
	}
}

// kb160RemoteGraph builds the KB-160 reproduction topology on the KB-58
// fixture: the ROOT graph declares the remote-environment callee "mcp.probe"
// (peer-hosted, with adapter secrets) and a parallel subworkflow step; each
// iteration body declares only its own claude caller whose step allowlists
// the probe tool. The mcp loader builtin is deliberately NOT registered — a
// host-local resolution of the callee is itself the bug and fails the test.
func kb160RemoteGraph(t *testing.T, listen string) *workflow.FSMGraph {
	t.Helper()
	g, _, _ := kb58Graph(t, []string{"adapter.mcp.probe.tools.*"}, workflow.DefaultPolicy)
	g.Environments = map[string]*workflow.EnvironmentNode{
		kb160RemoteEnvKey: kb160RemoteEnv(t, listen),
	}
	g.PinSet = kb160PinSet()
	g.Adapters[kb58CalleeRef].Environment = kb160RemoteEnvKey
	return g
}

// ─── fake peer-hosted adapter ────────────────────────────────────────────────

// kb160PeerCallee is the mcp-like callee implemented in the peer process: a
// full adapterhost.Client over the v2 wire shape, recording every session
// open (with secrets), execute, and close the peer serves for its
// environment. The shared-session assertions read its state.
type kb160PeerCallee struct {
	mu       sync.Mutex
	opens    []string
	openSecs []map[string]string
	closes   []string
	executes []kb160Execute
}

type kb160Execute struct {
	session string
	step    string
	task    string
}

func (c *kb160PeerCallee) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: "mcp", Version: "test", Capabilities: []string{"execute"}}, nil
}

func (c *kb160PeerCallee) OpenSession(_ context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opens = append(c.opens, req.GetSessionId())
	secs := map[string]string{}
	for k, v := range req.GetSecrets() {
		secs[k] = v
	}
	c.openSecs = append(c.openSecs, secs)
	return &v2.OpenSessionResponse{}, nil
}

func (c *kb160PeerCallee) Execute(_ context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	c.mu.Lock()
	c.executes = append(c.executes, kb160Execute{
		session: req.GetSessionId(),
		step:    req.GetStepName(),
		task:    req.GetInput()["task"],
	})
	c.mu.Unlock()
	out, err := json.Marshal(map[string]any{"report": "probed:" + req.GetInput()["task"]})
	if err != nil {
		return err
	}
	return sink.Emit(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{
			Outcome:     "success",
			OutputsJson: out,
		}},
	})
}

func (c *kb160PeerCallee) CloseSession(_ context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes = append(c.closes, req.GetSessionId())
	return &v2.CloseSessionResponse{}, nil
}

func (c *kb160PeerCallee) Log(_ context.Context, _ *v2.LogRequest, sink adapterhost.LogEventSink) error {
	return sink.Emit(&v2.LogEvent{StreamName: "stdout", Line: []byte("kb160 callee log")})
}

func (c *kb160PeerCallee) Permissions(_ context.Context, requests <-chan *v2.PermissionEvent) error {
	for range requests {
	}
	return nil
}

func (c *kb160PeerCallee) Pause(context.Context, *v2.PauseRequest) (*v2.PauseResponse, error) {
	return &v2.PauseResponse{}, nil
}

func (c *kb160PeerCallee) Resume(context.Context, *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	return &v2.ResumeResponse{}, nil
}

func (c *kb160PeerCallee) Snapshot(context.Context, *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}

func (c *kb160PeerCallee) Restore(context.Context, *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	return &v2.RestoreResponse{}, nil
}

func (c *kb160PeerCallee) Inspect(context.Context, *v2.InspectRequest) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}

func (c *kb160PeerCallee) Prompt(context.Context, *adapterhost.PromptRequest) (*adapterhost.PromptResponse, error) {
	return &adapterhost.PromptResponse{}, nil
}

func (c *kb160PeerCallee) openCount(session string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, id := range c.opens {
		if id == session {
			n++
		}
	}
	return n
}

func (c *kb160PeerCallee) secretsSeen(session string) (map[string]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, id := range c.opens {
		if id == session {
			return c.openSecs[i], true
		}
	}
	return nil, false
}

func (c *kb160PeerCallee) executeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.executes)
}

func (c *kb160PeerCallee) closeCount(session string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, id := range c.closes {
		if id == session {
			n++
		}
	}
	return n
}

// ─── fixture harness ─────────────────────────────────────────────────────────

// kb160StartPeer boots the production peer phone-home server for adapter
// "mcp" against callee, dialing listen with the pinned digest. The phone-home
// retry loop keeps dialing until Run starts the shim on the reserved address.
func kb160StartPeer(t *testing.T, listen string, callee *kb160PeerCallee) *peerpkg.Server {
	t.Helper()
	cfg := &peerpkg.Config{
		Host:        listen,
		AdapterName: "mcp",
		Digest:      kb160PeerDigest,
		BackoffMin:  10 * time.Millisecond,
		BackoffMax:  50 * time.Millisecond,
	}
	srv := peerpkg.NewServeAdapterServer(cfg, callee, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	return srv
}

// kb160StartRun drives the engine Run the way the KB-58 harness does, on a
// goroutine so the test can poll the terminal state.
func kb160StartRun(t *testing.T, g *workflow.FSMGraph, callers *kb58CallerProbe) (*loopOutputSink, *engineAuditCollector, chan error) {
	t.Helper()
	loader := adapterhost.NewLoaderWithDiscovery(func(string) (string, error) { return "", os.ErrNotExist })
	loader.RegisterBuiltin("claude", func() adapterhost.Handle { return callers.newFake() })

	sink := &loopOutputSink{}
	audit := &engineAuditCollector{}
	done := make(chan error, 1)
	go func() {
		done <- New(g, loader, sink, WithAuditWriter(audit)).Run(context.Background())
	}()
	return sink, audit, done
}

// kb160AwaitRun polls the sink until the run recorded its terminal state,
// then returns Run's error.
func kb160AwaitRun(t *testing.T, sink *loopOutputSink, done chan error, timeout time.Duration) error {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			// Poll through the locked accessor: the engine goroutine writes
			// the terminal fields while this goroutine reads them.
			if state, _ := sink.terminalState(); state == "" {
				continue
			}
			// The engine records the terminal state before Run returns;
			// give teardown bookkeeping a beat to finish.
			select {
			case err := <-done:
				return err
			case <-time.After(250 * time.Millisecond):
				return nil
			}
		case <-deadline:
			state, _ := sink.terminalState()
			t.Fatalf("run never reached a terminal state (sink.terminal=%q)", state)
		}
	}
}

// ─── the reproduction ────────────────────────────────────────────────────────

// TestKB160_ParallelIterationRoutesRemoteToolResourceThroughPeer is the KB-160
// reproduction: parallel iterations tool-call a REMOTE-environment adapter
// declared only in the root graph. Before the fix every nested call fails
// with typed unknown_adapter (the borrowed manager never sees a session for
// it). After the fix the calls route to the owner SessionManager that holds
// the environment's peer-hosted shared session: exactly one dial per
// environment, exactly one session opened inside the peer (with the root
// adapter's secrets), one execution per tool call, one close at owner
// teardown — and the callee's outputs reach every caller.
func TestKB160_ParallelIterationRoutesRemoteToolResourceThroughPeer(t *testing.T) {
	listen := kb160ReservePort(t)
	g := kb160RemoteGraph(t, listen)
	callers := &kb58CallerProbe{}
	callee := &kb160PeerCallee{}
	kb160StartPeer(t, listen, callee)
	sink, audit, done := kb160StartRun(t, g, callers)

	if err := kb160AwaitRun(t, sink, done, 60*time.Second); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Every issued call resolves and succeeds: both iterations, both calls.
	for _, id := range []string{"call-0", "call-1"} {
		res := callers.gotResult(id)
		if res == nil {
			t.Fatalf("%s: caller never received a tool_call_result", id)
		}
		if res.CallError != "" {
			t.Fatalf("%s: call_error = %q (outputs=%s); want empty (the remote callee must resolve through the peer)", id, res.CallError, string(res.OutputsJson))
		}
		if !strings.Contains(string(res.OutputsJson), "probed:probe-thing") {
			t.Errorf("%s: outputs %s missing the callee report", id, string(res.OutputsJson))
		}
	}

	// Exactly ONE session is opened inside the peer, for the shared
	// environment session — one dial serves both parallel iterations and all
	// four nested calls.
	if got, want := callee.openCount(kb58CalleeSess), 1; got != want {
		t.Errorf("peer session opens for %q = %d; want %d (one shared session per environment)", kb58CalleeSess, got, want)
	}

	// The shared session is opened with the ROOT adapter's secrets.
	if secs, ok := callee.secretsSeen(kb58CalleeSess); !ok {
		t.Errorf("peer session %q never opened", kb58CalleeSess)
	} else if secs[kb58SecretVar] != kb58ProbeSecret {
		t.Errorf("peer session opened without the root adapter secret: got %v; want %s=%q", secs, kb58SecretVar, kb58ProbeSecret)
	}

	// The secret never leaks into results returned to the caller.
	for _, id := range []string{"call-0", "call-1"} {
		if res := callers.gotResult(id); res != nil && strings.Contains(string(res.OutputsJson), kb58ProbeSecret) {
			t.Errorf("%s: probe secret leaked into results returned to the caller", id)
		}
	}

	// Four executions flow through the shared session (2 iterations × 2
	// calls); they may arrive in any order across the two iterations.
	if got, want := callee.executeCount(), 4; got != want {
		t.Errorf("peer executes = %d; want %d (2 iterations × 2 calls over the shared session)", got, want)
	}

	// Lifecycle: the OWNER closes the shared session exactly once at root
	// scope teardown; the borrowed iteration managers never close it.
	if got, want := callee.closeCount(kb58CalleeSess), 1; got != want {
		t.Errorf("peer closes for %q = %d; want %d (owner closes the shared session once)", kb58CalleeSess, got, want)
	}

	if state, ok := sink.terminalState(); state != "done" || !ok {
		t.Errorf("terminal state: got %q (ok=%v); want \"done\" (true)", state, ok)
	}

	// Audit hygiene: no unknown_adapter deny may be recorded.
	if reasons := auditReasons(audit.all(), "unknown_adapter"); len(reasons) != 0 {
		t.Errorf("audit recorded unknown_adapter denials: %v", reasons)
	}
}
