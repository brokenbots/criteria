package cli

// Tests for CRI-128: `criteria apply` must wire a per-run engine data dir so
// that workflows using per_scope_sessions on a remote environment work in
// local mode, resume, server, agent, and reattach entry points.
//
// The observable contract asserted here:
//   - a per-scope remote workflow no longer fails with the
//     "per_scope_sessions requires a run data directory (WithDataDir)" gate;
//   - a rotated accept token file is written under the run's own state dir
//     (runs/<runID>/remote-tokens), with 0700 dirs / 0600 files;
//   - the raw token never appears in any emitted event;
//   - non-per-scope runs are unaffected.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	applytest "github.com/brokenbots/criteria/internal/cli/applytest"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/run"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// cri128NoopDigest is the lockfile-pinned digest the fake phone-home adapters
// present. It matches the digest written by writeScopeSessionWorkflow.
const cri128NoopDigest = "sha256:abababababababababababababababababababababababababababababababab"

// scopeSessionWorkflowHCL is a workflow with a single noop adapter bound to a
// per-scope remote environment.
const scopeSessionWorkflowHCL = `
workflow {
  name = "cri128_per_scope"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "noop" "default" {
  environment = remote.prod
}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
  outcome "failure" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`

// scopeSessionTwoAdapterWorkflowHCL uses two noop adapter instances under the
// same per-scope remote environment.
const scopeSessionTwoAdapterWorkflowHCL = `
workflow {
  name = "cri128_per_scope_two"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "noop" "one" {
  environment = remote.prod
}

adapter "noop" "two" {
  environment = remote.prod
}

step "first" {
  target = adapter.noop.one
  outcome "success" { next = step.second }
  outcome "failure" { next = step.done }
}

step "second" {
  target = adapter.noop.two
  outcome "success" { next = step.done }
  outcome "failure" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`

// scopeSessionWaitWorkflowHCL adds a wait/signal node before the per-scope
// step, for resume-path tests.
const scopeSessionWaitWorkflowHCL = `
workflow {
  name = "cri128_per_scope_wait"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "noop" "default" {
  environment = remote.prod
}

step "first" {
  target = adapter.noop.default
  outcome "success" { next = step.gate }
  outcome "failure" { next = step.done }
}

wait "gate" {
  signal = "resume"
  outcome "received" { next = step.work }
}

step "work" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
  outcome "failure" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`

// inertRemoteWorkflowHCL is the same shape as scopeSessionWorkflowHCL but
// without per_scope_sessions, to prove the wiring is inert otherwise.
const inertRemoteWorkflowHCL = `
workflow {
  name = "cri128_inert"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address = "127.0.0.1:0"
}

adapter "noop" "default" {
  environment = remote.prod
}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.done }
  outcome "failure" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`

// writeScopeSessionWorkflow writes the workflow plus a lockfile pinning
// noop/default to cri128NoopDigest (the shim's digest verifier requires the
// pin for phone-home accept).
func writeScopeSessionWorkflow(t *testing.T, contents string) (workflowPath string) {
	t.Helper()
	dir := t.TempDir()
	workflowPath = filepath.Join(dir, "workflow.hcl")
	if err := os.WriteFile(workflowPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	lockContent := fmt.Sprintf(`schema_version = 1

adapter "noop" "default" {
  reference            = "ghcr.io/example/noop"
  version              = "1.0.0"
  resolved_digest      = "%s"
  source_url           = "https://github.com/example/noop"
  sdk_protocol_version = 2
}
`, cri128NoopDigest)
	if err := os.WriteFile(filepath.Join(dir, ".criteria.lock.hcl"), []byte(lockContent), 0o644); err != nil {
		t.Fatalf("write lockfile: %v", err)
	}
	return workflowPath
}

// tokenFilesUnder returns all rotated token files written for a run under
// <home>/runs/<runID>/remote-tokens, walking the tree so the layout can stay
// at the level the evidence establishes (a token file nested under
// remote-tokens, e.g. <scopeName>/<scopeInstanceID>/<type>.token with an empty
// root-scope name collapsing to <scopeInstanceID>/<type>.token).
func tokenFilesUnder(t *testing.T, home, runID string) []string {
	t.Helper()
	return walkTokenFiles(t, filepath.Join(home, "runs", runID, "remote-tokens"))
}

// tokenFilesUnderAnyRun finds rotated token files when the run ID is not
// known (exactly one run dir exists in a fresh state dir).
func tokenFilesUnderAnyRun(t *testing.T, home string) []string {
	t.Helper()
	runDirs, err := filepath.Glob(filepath.Join(home, "runs", "*"))
	if err != nil {
		t.Fatalf("glob runs: %v", err)
	}
	var out []string
	for _, dir := range runDirs {
		out = append(out, walkTokenFiles(t, filepath.Join(dir, "remote-tokens"))...)
	}
	return out
}

func walkTokenFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // no tokens rotated yet
			}
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".token") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// requireTokenFilePerms asserts 0600 on the token file and 0700 on every
// directory from the token file up to and including the remote-tokens dir.
func requireTokenFilePerms(t *testing.T, tokenPath string) {
	t.Helper()
	fi, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file perms: got %o, want 600", perm)
	}
	dir := filepath.Dir(tokenPath)
	for {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("dir %s perms: got %o, want 700", dir, perm)
		}
		if fi.Name() == "remote-tokens" {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
}

// requireNotGateError fails when err still contains the CRI-128 gate error.
func requireNotGateError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "per_scope_sessions requires a run data directory") {
		t.Fatalf("per-scope workflow still hits the run-data-dir gate: %v", err)
	}
}

// requireHexToken asserts the token file holds a 64-hex-character token and
// returns it.
func requireHexToken(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	token := strings.TrimSpace(string(raw))
	if _, err := hex.DecodeString(token); err != nil || len(token) != 64 {
		t.Errorf("token %q is not a 64-char hex accept token (decode err: %v)", token, err)
	}
	return token
}

// cri128FakeAdapter is a minimal v2 adapter served over the phone-home
// connection by the test dialer.
type cri128FakeAdapter struct {
	v2.UnimplementedAdapterServiceServer
}

func (cri128FakeAdapter) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: "noop", Version: "1.0.0", Capabilities: []string{"execute"}}, nil
}

func (cri128FakeAdapter) OpenSession(context.Context, *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}

func (cri128FakeAdapter) Execute(_ *v2.ExecuteRequest, stream v2.AdapterService_ExecuteServer) error {
	return stream.Send(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Result{
		Result: &v2.ExecuteResult{Outcome: "success", OutputsJson: []byte(`{}`)},
	}})
}

// handshakeFrame mirrors the shim's pre-gRPC identity frame.
type handshakeFrame struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Token   string `json:"token"`
	Scope   string `json:"scope,omitempty"`
}

// cri128Envelope mirrors the LocalSink ND-JSON envelope shape, including the
// top-level run_id used to discover the run's state directory.
type cri128Envelope struct {
	PayloadType string          `json:"payload_type"`
	RunID       string          `json:"run_id"`
	Payload     json.RawMessage `json:"payload"`
}

// provisionPayload is the AdapterEvent payload for provision_wanted events.
type provisionPayload struct {
	Kind string `json:"kind"`
	Data struct {
		RunID           string `json:"run_id"`
		ScopeName       string `json:"scope_name"`
		ScopeInstanceID string `json:"scope_instance_id"`
		Adapter         string `json:"adapter"`
		Digest          string `json:"digest"`
		ShimListenAddr  string `json:"shim_listen_address"`
		TokenRef        string `json:"token_ref"`
	} `json:"data"`
}

// phoneHomeDialer watches a run's ND-JSON events file and plays the role of
// the phone-homing adapter: when the engine emits a provision_wanted event the
// dialer dials the shim with the rotated scope token and serves a minimal gRPC
// adapter; when a step enters it dials again for the step's session bind.
type phoneHomeDialer struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu          sync.Mutex
	seen        map[string]bool
	provisioned map[string]scopeProvision
	servers     []*grpc.Server
	stopOnce    sync.Once
	negativeOK  bool
	negativeErr string
	runID       string
}

type scopeProvision struct {
	adapter   string
	scopeKey  string
	shimAddr  string
	tokenPath string
}

// provisionSnapshot copies the current provision state for all adapters.
func (d *phoneHomeDialer) provisionSnapshot() map[string]scopeProvision {
	d.mu.Lock()
	defer d.mu.Unlock()
	snap := make(map[string]scopeProvision, len(d.provisioned))
	for k, p := range d.provisioned {
		snap[k] = p
	}
	return snap
}

// singleConnListener serves exactly one accepted connection (the adapter's
// phone-home connection) to a gRPC server.
type singleConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	return &singleConnListener{conn: conn, done: make(chan struct{})}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var served bool
	l.once.Do(func() {
		served = true
		close(l.done)
	})
	if served {
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error { return nil }

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

func newPhoneHomeDialer(t *testing.T) *phoneHomeDialer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d := &phoneHomeDialer{
		ctx:         ctx,
		cancel:      cancel,
		seen:        make(map[string]bool),
		provisioned: make(map[string]scopeProvision),
	}
	t.Cleanup(d.stop)
	return d
}

// watch polls the events file until the dialer is stopped.
func (d *phoneHomeDialer) watch(eventsPath string) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for d.ctx.Err() == nil {
			raw, err := os.ReadFile(eventsPath)
			if err == nil {
				d.consume(raw)
			}
			select {
			case <-d.ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
}

func (d *phoneHomeDialer) consume(raw []byte) {
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var env cri128Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil || env.PayloadType == "" {
			continue
		}
		d.mu.Lock()
		if env.RunID != "" && d.runID == "" {
			d.runID = env.RunID
		}
		d.mu.Unlock()
		switch env.PayloadType {
		case "RunCompleted", "RunFailed":
			// The run is over: drop the adapter connections so the engine's
			// teardown (adapter Kill) is not blocked waiting for the client.
			d.stopServers()
		case "AdapterEvent":
			var payload provisionPayload
			if err := json.Unmarshal(env.Payload, &payload); err != nil || payload.Kind != "adapter.lifecycle.provision_wanted" {
				continue
			}
			key := "prov:" + payload.Data.Adapter + ":" + payload.Data.ScopeInstanceID
			d.mu.Lock()
			already := d.seen[key]
			d.seen[key] = true
			if !already {
				d.provisioned[payload.Data.Adapter] = scopeProvision{
					adapter:   payload.Data.Adapter,
					scopeKey:  payload.Data.ScopeName + "/" + payload.Data.ScopeInstanceID,
					shimAddr:  payload.Data.ShimListenAddr,
					tokenPath: payload.Data.TokenRef,
				}
			}
			total := len(d.provisioned)
			d.mu.Unlock()
			if !already {
				d.dialAllProvisions()
				if total == 2 {
					d.startNegativeDial()
				}
			}
		case "StepEntered":
			var payload struct {
				Step    string `json:"step"`
				Adapter string `json:"adapter"`
				Attempt int    `json:"attempt"`
			}
			if err := json.Unmarshal(env.Payload, &payload); err != nil || payload.Adapter == "" {
				continue
			}
			key := fmt.Sprintf("step:%s:%s:%d", payload.Step, payload.Adapter, payload.Attempt)
			d.mu.Lock()
			already := d.seen[key]
			d.seen[key] = true
			d.mu.Unlock()
			if !already {
				// StepEntered carries the adapter *type* ("noop") while
				// provision_wanted carries the adapter instance name
				// ("default"); dial every provisioned scope so the step's
				// session bind finds a live connection for its scope key.
				d.dialAllProvisions()
			}
		}
	}
}

// dialAllProvisions dials the shim once per provisioned scope and serves the
// fake adapter over each connection. Dials are retried while the shim listener
// is still coming up.
func (d *phoneHomeDialer) dialAllProvisions() {
	snap := d.provisionSnapshot()
	for _, p := range snap {
		d.wg.Add(1)
		go func(p scopeProvision) {
			defer d.wg.Done()
			token, err := os.ReadFile(p.tokenPath)
			if err != nil {
				return // token vanished: nothing to present
			}
			var conn net.Conn
			for d.ctx.Err() == nil {
				c, err := net.DialTimeout("tcp", p.shimAddr, time.Second)
				if err != nil {
					select {
					case <-d.ctx.Done():
						return
					case <-time.After(30 * time.Millisecond):
					}
					continue
				}
				conn = c
				break
			}
			if conn == nil {
				return
			}
			frame, _ := json.Marshal(handshakeFrame{
				Name:    "noop",
				Version: "1.0.0",
				Digest:  cri128NoopDigest,
				Token:   strings.TrimSpace(string(token)),
				Scope:   p.scopeKey,
			})
			if _, err := conn.Write(append(frame, '\n')); err != nil {
				_ = conn.Close()
				return
			}
			grpcServer := grpc.NewServer()
			v2.RegisterAdapterServiceServer(grpcServer, &cri128FakeAdapter{})
			d.mu.Lock()
			d.servers = append(d.servers, grpcServer)
			d.mu.Unlock()
			_ = grpcServer.Serve(newSingleConnListener(conn))
		}(p)
	}
}

// startNegativeDial proves per-scope token isolation: presenting one scope's
// session key with another scope's token must be rejected (the shim closes
// the connection). Requires exactly two provisioned scopes.
func (d *phoneHomeDialer) startNegativeDial() {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.mu.Lock()
		var first, second scopeProvision
		for _, p := range d.provisioned {
			if first.adapter == "" {
				first = p
				continue
			}
			second = p
		}
		victimKey := first.scopeKey
		wrongTokenPath := second.tokenPath
		shimAddr := first.shimAddr
		d.mu.Unlock()

		token, err := os.ReadFile(wrongTokenPath)
		if err != nil {
			d.setNegative("read wrong token: " + err.Error())
			return
		}
		conn, err := net.DialTimeout("tcp", shimAddr, 2*time.Second)
		if err != nil {
			d.setNegative("negative dial: " + err.Error())
			return
		}
		frame, _ := json.Marshal(handshakeFrame{
			Name:    "noop",
			Version: "1.0.0",
			Digest:  cri128NoopDigest,
			Token:   strings.TrimSpace(string(token)),
			Scope:   victimKey,
		})
		if _, err := conn.Write(append(frame, '\n')); err != nil {
			_ = conn.Close()
			d.setNegative("negative handshake write: " + err.Error())
			return
		}
		// The shim must close the connection on token verification failure.
		buf := make([]byte, 1)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, rerr := conn.Read(buf)
		_ = conn.Close()
		if rerr == nil {
			d.setNegative("shim accepted cross-scope token")
		} else {
			d.mu.Lock()
			d.negativeOK = true
			d.mu.Unlock()
		}
	}()
}

func (d *phoneHomeDialer) setNegative(msg string) {
	d.mu.Lock()
	d.negativeErr = msg
	d.mu.Unlock()
}

// stopServers tears down every gRPC server the dialer started. Safe to call
// multiple times.
func (d *phoneHomeDialer) stopServers() {
	d.stopOnce.Do(func() {
		d.mu.Lock()
		servers := append([]*grpc.Server(nil), d.servers...)
		d.mu.Unlock()
		for _, s := range servers {
			s.Stop()
		}
	})
}

// stop cancels the watch loop and stops every gRPC server so all dialer
// goroutines exit before the test ends.
func (d *phoneHomeDialer) stop() {
	d.cancel()
	d.stopServers()
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

// TestRunDataDirDerivation pins the observable path contract: the engine run
// data dir is the run's own state directory (<home>/runs/<runID>).
func TestRunDataDirDerivation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	got, err := runDataDir("run-abc")
	if err != nil {
		t.Fatalf("runDataDir: %v", err)
	}
	want := filepath.Join(home, "runs", "run-abc")
	if got != want {
		t.Errorf("runDataDir = %q, want %q", got, want)
	}

	if _, err := runDataDir(""); err == nil {
		t.Error("expected error for empty run ID")
	}
}

// TestApplyLocalPerScopeSessionsCompletes is the flagship regression: the
// full `criteria apply` local flow with per_scope_sessions, phone-home
// satisfied by the dialer. It fails against the pre-fix build with
// "per_scope_sessions requires a run data directory (WithDataDir)".
func TestApplyLocalPerScopeSessionsCompletes(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	wfPath := writeScopeSessionWorkflow(t, scopeSessionWorkflowHCL)
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")

	dialer := newPhoneHomeDialer(t)
	dialer.watch(eventsFile)

	runCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runApply(runCtx, applyOptions{
			workflowPath: wfPath,
			eventsPath:   eventsFile,
			log:          discardLogger(),
		})
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runApply: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatalf("runApply did not complete in time; events=%s", mustReadEventsForDiag(t, eventsFile))
	}
	dialer.stop()

	assertCompletedPerScopeRun(t, eventsFile, home, dialer.runID, 1)
}

// assertCompletedPerScopeRun checks the events and token files for a
// completed local per-scope run.
func assertCompletedPerScopeRun(t *testing.T, eventsFile, home, runID string, wantProvision int) {
	t.Helper()
	raw, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if runID == "" {
		t.Fatal("no run_id observed in events")
	}

	provisions := make(map[string]provisionPayload)
	runCompleted := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var env cri128Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if env.PayloadType == "RunCompleted" {
			runCompleted++
		}
		if env.PayloadType != "AdapterEvent" {
			continue
		}
		var payload provisionPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			continue
		}
		if payload.Kind == "adapter.lifecycle.provision_wanted" {
			provisions[payload.Data.Adapter] = payload
			// Root-scope runs have an empty scope name; the scope instance
			// ID identifies the scope instance.
			if payload.Data.ScopeName != "" {
				t.Errorf("provision scope name = %q, want empty root scope", payload.Data.ScopeName)
			}
			if payload.Data.ScopeInstanceID == "" {
				t.Error("provision event missing scope_instance_id")
			}
			if payload.Data.ShimListenAddr == "" {
				t.Error("provision event missing shim_listen_address")
			}
		}
	}
	if runCompleted != 1 {
		t.Errorf("RunCompleted events: got %d, want 1", runCompleted)
	}
	if len(provisions) != wantProvision {
		t.Errorf("provision_wanted events: got %d, want %d", len(provisions), wantProvision)
	}

	// Token files: one per provisioned adapter, under the run's own state dir.
	tokens := tokenFilesUnder(t, home, runID)
	if len(tokens) != wantProvision {
		t.Fatalf("token files under %s: got %v, want %d", filepath.Join(home, "runs", runID, "remote-tokens"), tokens, wantProvision)
	}
	seenTokens := make(map[string]bool, len(tokens))
	for _, p := range tokens {
		requireTokenFilePerms(t, p)
		token := requireHexToken(t, p)
		if seenTokens[token] {
			t.Errorf("token reused across scopes: %s", p)
		}
		seenTokens[token] = true
		if strings.Contains(string(raw), token) {
			t.Errorf("raw accept token leaked into emitted events: %s", p)
		}
	}

	// token_ref in each provision event points at the token file for that
	// adapter under the run data dir (never the workflow dir).
	for adapter := range provisions {
		payload := provisions[adapter]
		ref := payload.Data.TokenRef
		wantPrefix := filepath.Join(home, "runs", runID, "remote-tokens")
		if !strings.HasPrefix(ref, wantPrefix) {
			t.Errorf("token_ref for %s: %q does not start with %q", adapter, ref, wantPrefix)
		}
		if _, err := os.Stat(ref); err != nil {
			t.Errorf("token file from token_ref missing: %v", err)
		}
	}
}

func mustReadEventsForDiag(t *testing.T, eventsFile string) string {
	t.Helper()
	raw, err := os.ReadFile(eventsFile)
	if err != nil {
		return "<unreadable>"
	}
	return string(raw)
}

// TestApplyLocalPerScopeSessionsBlockedWithoutPhoneHome is the focused
// regression variant: without an adapter phone-homing home, the run must now
// block on session establishment (deadline) instead of failing instantly with
// the run-data-dir gate, and the token must already be rotated on disk.
func TestApplyLocalPerScopeSessionsBlockedWithoutPhoneHome(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	wfPath := writeScopeSessionWorkflow(t, scopeSessionWorkflowHCL)
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")

	runCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		errCh <- runApply(runCtx, applyOptions{
			workflowPath: wfPath,
			eventsPath:   eventsFile,
			log:          discardLogger(),
		})
		close(done)
	}()

	// The rotated token must appear on disk while the run is still parked on
	// per-scope session establishment; without phone-home the run cannot
	// finish. If the run returns first, the data-dir gate (or another
	// startup failure) fired, which is the regression this test guards.
	token := waitForRotatedToken(t, home, done, 6*time.Second)
	time.Sleep(300 * time.Millisecond) // let the engine park on the session bind
	cancel()
	runErr := <-errCh
	if token == "" {
		if runErr == nil {
			t.Fatal("run completed without rotating a token or failing")
		}
		requireNotGateError(t, runErr)
		t.Fatalf("run ended before rotating a token: %v", runErr)
	}
	requireNotGateError(t, runErr)

	tokens := tokenFilesUnderAnyRun(t, home)
	if len(tokens) != 1 {
		t.Fatalf("token files: got %v, want exactly one", tokens)
	}
	requireTokenFilePerms(t, tokens[0])
	requireHexToken(t, tokens[0])
}

// TestApplyLocalPerScopeSessionsTwoScopes runs a workflow with two noop
// adapter instances under the same per-scope remote environment: each scope
// gets its own token, and presenting one scope's key with the other scope's
// token is rejected by the shim (the CRI-115 token-reuse contract exercised
// end-to-end through the CLI).
func TestApplyLocalPerScopeSessionsTwoScopes(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	wfPath := writeScopeSessionWorkflow(t, scopeSessionTwoAdapterWorkflowHCL)
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")

	dialer := newPhoneHomeDialer(t)
	dialer.watch(eventsFile)

	runCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runApply(runCtx, applyOptions{
			workflowPath: wfPath,
			eventsPath:   eventsFile,
			log:          discardLogger(),
		})
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runApply: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatalf("runApply did not complete in time; events=%s", mustReadEventsForDiag(t, eventsFile))
	}
	dialer.stop()

	assertCompletedPerScopeRun(t, eventsFile, home, dialer.runID, 2)

	if !dialer.negativeOK {
		t.Fatalf("cross-scope token dial was not rejected: %s", dialer.negativeErr)
	}
}

// TestApplyLocalRemoteWithoutPerScopeSessionsInert proves the wiring is inert
// for workflows without per_scope_sessions: no tokens are rotated, and the
// run still blocks on (and is cancelled by) the remote phone-home flow.
func TestApplyLocalRemoteWithoutPerScopeSessionsInert(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	wfPath := writeScopeSessionWorkflow(t, inertRemoteWorkflowHCL)
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")

	runCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runErr := runApply(runCtx, applyOptions{
		workflowPath: wfPath,
		eventsPath:   eventsFile,
		log:          discardLogger(),
	})
	if runErr == nil {
		t.Fatal("expected the remote run to block without phone-home, got success")
	}
	requireNotGateError(t, runErr)
	// Inertia: the failure is the legacy adapter-verify deadline, unchanged
	// by the data-dir wiring.
	if !strings.Contains(runErr.Error(), "verify adapter") {
		t.Fatalf("expected the legacy verify path to block until the deadline, got: %v", runErr)
	}

	tokens := tokenFilesUnderAnyRun(t, home)
	if len(tokens) != 0 {
		t.Errorf("unexpected token files for non-per-scope run: %v", tokens)
	}
}

// fakeSignalResumer satisfies localresume.LocalResumer with an immediate
// signal payload.
type fakeSignalResumer struct{}

func (fakeSignalResumer) ResumeApproval(context.Context, string, string, []string, string) (map[string]string, error) {
	return map[string]string{"decision": "approved"}, nil
}

func (fakeSignalResumer) ResumeSignal(context.Context, string, string, string, []string) (map[string]string, error) {
	return map[string]string{"outcome": "received"}, nil
}

// TestDrainLocalResumeCyclesPerScopeSessionsWiresDataDir covers the
// apply_resume.go wiring site: a locally resumed per-scope run rotates a
// token under the run's own state dir instead of hitting the gate.
func TestDrainLocalResumeCyclesPerScopeSessionsWiresDataDir(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	wfPath := writeScopeSessionWorkflow(t, scopeSessionWaitWorkflowHCL)

	runCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_, graph, loader, err := compileForExecution(runCtx, wfPath, discardLogger(), false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(runCtx)) }()

	runID := "cri128-resume-1"
	// Use the real local-mode sink (ND-JSON to an events file) so engine
	// events emitted during the resumed run have somewhere to go.
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	eventsFileHandle, err := os.Create(eventsFile)
	if err != nil {
		t.Fatalf("create events file: %v", err)
	}
	defer eventsFileHandle.Close()
	baseSink := &run.LocalSink{RunID: runID, Out: eventsFileHandle}
	tracker := &pauseTracker{Sink: baseSink}
	tracker.mu.Lock()
	tracker.pausedNode = "gate"
	tracker.signalDetail = &signalDetail{signalName: "resume"}
	tracker.mu.Unlock()
	runSink := &terminalSuccessSink{Sink: tracker}
	initialEng := engine.New(graph, loader, runSink)

	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		errCh <- drainLocalResumeCycles(runCtx, discardLogger(), graph, loader, tracker, runSink, fakeSignalResumer{}, runID, applyOptions{workflowPath: wfPath}, initialEng)
		close(done)
	}()

	token := waitForRotatedToken(t, home, done, 6*time.Second)
	time.Sleep(300 * time.Millisecond) // let the engine park on the session bind
	cancel()
	err = <-errCh
	if err == nil {
		t.Fatal("expected the resumed per-scope run to block without phone-home")
	}
	requireNotGateError(t, err)
	if token == "" {
		t.Fatalf("token files under %s: none rotated before the run ended (%v)", runDataDirPath(t, runID), err)
	}

	tokens := tokenFilesUnder(t, home, runID)
	if len(tokens) != 1 {
		t.Fatalf("token files under %s: got %v, want exactly one", runDataDirPath(t, runID), tokens)
	}
	requireTokenFilePerms(t, tokens[0])
}

// runDataDirPath returns the human-readable run data dir for diagnostics.
func runDataDirPath(t *testing.T, runID string) string {
	t.Helper()
	d, err := runDataDir(runID)
	if err != nil {
		t.Fatalf("runDataDir: %v", err)
	}
	return d
}

// waitForRotatedToken polls the state dir until a rotated token file appears
// under any run data dir, the observed run terminates (signalled by
// closing done), or the deadline passes. It returns the token path, or "" if
// none appeared.
func waitForRotatedToken(t *testing.T, home string, done <-chan struct{}, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if tokens := tokenFilesUnderAnyRun(t, home); len(tokens) > 0 {
			return tokens[0]
		}
		select {
		case <-done:
			return ""
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ""
}

// TestExecuteServerRunPerScopeSessionsWiresDataDir covers the
// apply_server.go wiring site for fresh server-mode runs.
func TestExecuteServerRunPerScopeSessionsWiresDataDir(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	fake := applytest.New(t)
	wfPath := writeScopeSessionWorkflow(t, scopeSessionWorkflowHCL)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	log := discardLogger()
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, resumed, err := setupServerRun(ctx, log, graph, src, fake.URL(), "cri128-server", &copts, cancel, nil, "")
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	if resumed {
		t.Fatal("setupServerRun unexpectedly resumed a matching checkpoint")
	}
	defer client.Close()

	state := newLocalRunState(runID, graph.Name, fake.URL())
	opts := applyOptions{workflowPath: wfPath, serverURL: fake.URL()}
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		errCh <- executeServerRun(ctx, log, loader, client, state, graph, opts, nil)
		close(done)
	}()

	token := waitForRotatedToken(t, home, done, 8*time.Second)
	time.Sleep(300 * time.Millisecond) // let the engine park on the session bind
	cancel()
	runErr := <-errCh
	if runErr == nil {
		t.Fatal("expected the server-mode per-scope run to block without phone-home")
	}
	requireNotGateError(t, runErr)
	if token == "" {
		t.Fatalf("token files under %s: none rotated before the run ended (%v)", runDataDirPath(t, runID), runErr)
	}

	tokens := tokenFilesUnder(t, home, runID)
	if len(tokens) != 1 {
		t.Fatalf("token files under %s: got %v, want exactly one", runDataDirPath(t, runID), tokens)
	}
	requireTokenFilePerms(t, tokens[0])
}

// TestDrainResumeCyclesPerScopeSessionsWiresDataDir covers the second
// apply_server.go wiring site: a server-mode resume cycle re-derives the run
// data dir and wires it into the resumed engine.
func TestDrainResumeCyclesPerScopeSessionsWiresDataDir(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	fake := applytest.New(t)
	wfPath := writeScopeSessionWorkflow(t, scopeSessionWaitWorkflowHCL)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	log := discardLogger()
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, resumed, err := setupServerRun(ctx, log, graph, src, fake.URL(), "cri128-resume", &copts, cancel, nil, "")
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	if resumed {
		t.Fatal("setupServerRun unexpectedly resumed a matching checkpoint")
	}
	defer client.Close()

	state := newLocalRunState(runID, graph.Name, fake.URL())

	var eng *engine.Engine
	sink := buildServerSink(ctx, client, client, runID, graph, wfPath, fake.URL(), "", log,
		func() map[string]int {
			if eng != nil {
				return eng.VisitCounts()
			}
			return nil
		})

	// Pre-pause the sink as if the engine had paused at the wait node, then
	// deliver a resume message over a channel mirroring the client's.
	sink.OnRunPaused("gate", "", "")
	initialEng := engine.New(graph, loader, sink, engine.WithWorkflowDir(filepath.Dir(wfPath)))
	eng = initialEng

	resumeCh := make(chan *pb.ResumeRun, 1)
	resumeCh <- &pb.ResumeRun{RunId: runID, Signal: "resume", Payload: map[string]string{"outcome": "received"}}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer drainCancel()
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		errCh <- drainResumeCycles(drainCtx, log, loader, sink, sink, resumeCh, state, graph, filepath.Dir(wfPath), initialEng)
		close(done)
	}()

	token := waitForRotatedToken(t, home, done, 6*time.Second)
	time.Sleep(300 * time.Millisecond) // let the engine park on the session bind
	drainCancel()
	drainErr := <-errCh
	if drainErr == nil {
		t.Fatal("expected the resumed per-scope engine to block without phone-home")
	}
	requireNotGateError(t, drainErr)
	if token == "" {
		t.Fatalf("token files under %s: none rotated before the run ended (%v)", runDataDirPath(t, runID), drainErr)
	}

	tokens := tokenFilesUnder(t, home, runID)
	if len(tokens) != 1 {
		t.Fatalf("token files under %s: got %v, want exactly one", runDataDirPath(t, runID), tokens)
	}
	requireTokenFilePerms(t, tokens[0])
}

// TestBuildAgentRunPerScopeSessionsWiresDataDir covers the agent.go wiring
// site: assignments executed by the agent get the run data dir from the
// assignment's run ID.
func TestBuildAgentRunPerScopeSessionsWiresDataDir(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	fake := applytest.New(t)
	wfPath := writeScopeSessionWorkflow(t, scopeSessionWorkflowHCL)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	log := discardLogger()
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, _, resumed, err := setupServerRun(ctx, log, graph, src, fake.URL(), "cri128-agent", &copts, cancel, nil, "")
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	if resumed {
		t.Fatal("setupServerRun unexpectedly resumed a matching checkpoint")
	}
	defer client.Close()

	runID := "cri128-agent-1"
	publisher, closePublisher, err := newRunPublisher(ctx, client, runID)
	if err != nil {
		t.Fatalf("newRunPublisher: %v", err)
	}
	defer closePublisher()

	assignment := &pb.WorkflowAssignment{RunId: runID, WorkflowSource: scopeSessionWorkflowHCL}
	opts := &agentOptions{serverURL: fake.URL()}
	eng, _, _, _, err := buildAgentRun(ctx, ctx, log, client, assignment, opts, publisher, graph, loader, filepath.Dir(wfPath), wfPath)
	if err != nil {
		t.Fatalf("buildAgentRun: %v", err)
	}

	runErrCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		runErrCh <- eng.Run(ctx)
		close(done)
	}()

	token := waitForRotatedToken(t, home, done, 8*time.Second)
	time.Sleep(300 * time.Millisecond) // let the engine park on the session bind
	cancel()
	runErr := <-runErrCh
	if runErr == nil {
		t.Fatal("expected the agent-mode per-scope run to block without phone-home")
	}
	requireNotGateError(t, runErr)
	if token == "" {
		t.Fatalf("token files under %s: none rotated before the run ended (%v)", runDataDirPath(t, runID), runErr)
	}

	tokens := tokenFilesUnder(t, home, runID)
	if len(tokens) != 1 {
		t.Fatalf("token files under %s: got %v, want exactly one", runDataDirPath(t, runID), tokens)
	}
	requireTokenFilePerms(t, tokens[0])
}

// TestReattachPerScopeSessionsWiresDataDir covers the three reattach.go
// wiring sites: resumePausedRun, serviceResumeSignals, and resumeActiveRun.
// In each the run blocks on per-scope session establishment (deadline), which
// is only reachable when the run data dir was wired.
func TestReattachPerScopeSessionsWiresDataDir(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	wfPath := writeScopeSessionWorkflow(t, scopeSessionWorkflowHCL)

	runCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	_, graph, loader, err := compileForExecution(runCtx, wfPath, discardLogger(), false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(runCtx)) }()

	t.Run("resumePausedRun", func(t *testing.T) {
		runID := "cri128-reattach-paused"
		cp := &StepCheckpoint{RunID: runID, Workflow: "cri128_per_scope", WorkflowPath: wfPath}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ft := &fakeTransport{}
		resp := &pb.ReattachRunResponse{Status: "paused", CurrentStep: "start", Attempt: 0, CanResume: true}
		done := make(chan struct{})
		go func() {
			resumePausedRun(ctx, discardLogger(), ft, cp, graph, resp, nil)
			close(done)
		}()

		token := waitForRotatedToken(t, home, done, 5*time.Second)
		cancel()
		<-done
		if token == "" {
			t.Fatalf("no token files rotated by resumePausedRun under %s", runDataDirPath(t, runID))
		}
		requireTokenFilePerms(t, token)
	})

	t.Run("serviceResumeSignals", func(t *testing.T) {
		runID := "cri128-reattach-signals"
		cp := &StepCheckpoint{RunID: runID, Workflow: "cri128_per_scope", WorkflowPath: wfPath}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ft := &fakeTransport{}
		sink := &run.Sink{RunID: runID, Client: ft, Log: discardLogger(), Ctx: ctx}
		sink.OnRunPaused("start", "", "")
		initialEng := engine.New(graph, loader, sink, engine.WithWorkflowDir(filepath.Dir(wfPath)))

		ft.resumeCh = make(chan *pb.ResumeRun, 1)
		ft.resumeCh <- &pb.ResumeRun{RunId: runID, Signal: "criteria"}

		done := make(chan struct{})
		go func() {
			serviceResumeSignals(ctx, discardLogger(), ft, cp, graph, loader, sink, sink, initialEng)
			close(done)
		}()

		token := waitForRotatedToken(t, home, done, 5*time.Second)
		cancel()
		<-done
		if token == "" {
			t.Fatalf("no token files rotated by serviceResumeSignals under %s", runDataDirPath(t, runID))
		}
		requireTokenFilePerms(t, token)
	})

	t.Run("resumeActiveRun", func(t *testing.T) {
		runID := "cri128-reattach-active"
		cp := &StepCheckpoint{RunID: runID, Workflow: "cri128_per_scope", WorkflowPath: wfPath}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ft := &fakeTransport{}
		resp := &pb.ReattachRunResponse{Status: "running", CurrentStep: "start", Attempt: 0, CanResume: true}
		done := make(chan struct{})
		go func() {
			resumeActiveRun(ctx, discardLogger(), ft, cp, graph, resp, nil)
			close(done)
		}()

		token := waitForRotatedToken(t, home, done, 5*time.Second)
		cancel()
		<-done
		if token == "" {
			t.Fatalf("no token files rotated by resumeActiveRun under %s", runDataDirPath(t, runID))
		}
		requireTokenFilePerms(t, token)
	})
}

// TestRunApplyServerDualWriteAdapterLifecycle is the CRI-134 dual-write
// flagship: a full server-mode run with --events-file set must mirror the
// per-scope adapter.lifecycle contract (scope_instance_id,
// shim_listen_address, token_ref) into the ND-JSON file exactly as the
// server stream carries it.
func TestRunApplyServerDualWriteAdapterLifecycle(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	fake := applytest.New(t)
	wfPath := writeScopeSessionWorkflow(t, scopeSessionWorkflowHCL)
	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")

	dialer := newPhoneHomeDialer(t)
	dialer.watch(eventsFile)

	runCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runApplyServer(runCtx, applyOptions{
			workflowPath: wfPath,
			serverURL:    fake.URL(),
			eventsPath:   eventsFile,
			name:         "cri134-dual-write",
		})
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runApplyServer: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatalf("server-mode dual-write run did not complete in time; events=%s", mustReadEventsForDiag(t, eventsFile))
	}
	dialer.stop()

	// File side: identical guarantees to the local-mode flagship run.
	assertCompletedPerScopeRun(t, eventsFile, home, dialer.runID, 1)

	// Server side: the provision_wanted AdapterEvent must survive the
	// Envelope mapping with the same per-scope contract fields and be
	// proto-equal to the mirrored file payload.
	assertServerPerScopeLifecycleParity(t, fake, eventsFile)
}

// assertServerPerScopeLifecycleParity checks that the server stream received
// the adapter.lifecycle.provision_wanted AdapterEvent with the CRI-115
// per-scope contract fields populated, and that the dual-write file payload
// for that event is proto-equal.
func assertServerPerScopeLifecycleParity(t *testing.T, fake *applytest.Fake, eventsFile string) {
	t.Helper()

	var serverProvisions []*pb.AdapterEvent
	for _, env := range fake.Events() {
		if ae := env.GetAdapterEvent(); ae != nil && ae.Kind == "adapter.lifecycle.provision_wanted" {
			serverProvisions = append(serverProvisions, ae)
		}
	}
	if len(serverProvisions) != 1 {
		t.Fatalf("server stream provision_wanted events: got %d, want 1", len(serverProvisions))
	}
	data := serverProvisions[0].Data.AsMap()
	for _, field := range []string{"scope_instance_id", "shim_listen_address", "token_ref"} {
		if v, _ := data[field].(string); v == "" {
			t.Errorf("server adapter.lifecycle event missing %s", field)
		}
	}

	var fileProvisions []*pb.AdapterEvent
	for _, line := range splitNDJSONLines(mustReadEventsForDiag(t, eventsFile)) {
		var env struct {
			PayloadType string          `json:"payload_type"`
			Payload     json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("unmarshal file envelope: %v", err)
		}
		if env.PayloadType != "AdapterEvent" {
			continue
		}
		var ae pb.AdapterEvent
		if err := protojson.Unmarshal(env.Payload, &ae); err != nil {
			t.Fatalf("unmarshal file AdapterEvent: %v", err)
		}
		if ae.Kind == "adapter.lifecycle.provision_wanted" {
			fileProvisions = append(fileProvisions, &ae)
		}
	}
	if len(fileProvisions) != 1 {
		t.Fatalf("file provision_wanted events: got %d, want 1", len(fileProvisions))
	}
	if !proto.Equal(fileProvisions[0], serverProvisions[0]) {
		want, _ := protojson.Marshal(serverProvisions[0])
		t.Errorf("file and server adapter.lifecycle payloads differ\n file: %s\n server: %s", fileProvisions[0], want)
	}
}
