package remote

// peer_parity_test.go — T-07 requirement 3: every v2 AdapterService RPC a
// peerHandle serves behaves identically to the production local rpcHandle
// over the same fake adapter implementation. The local variant drives the
// exact same fakePeer over an in-process bufconn with NewRPCHandle, so any
// divergence between the two transports is a peer-path bug, not a fixture
// difference. ADR-0006 prompt failure modes (accepted for capable adapters,
// typed errors for shell) are asserted at the SessionManager boundary, where
// the capability gate lives.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// parityHandle is the full v2 surface both handles must present: the core
// Handle plus the three capability narrow interfaces (log stream, bidi
// permission stream, prompt).
type parityHandle interface {
	adapterhost.Handle
	adapterhost.LogStreamStarter
	StartPermissionStream(ctx context.Context, sessionID string, requests <-chan *v2.PermissionEvent) (func(), error)
	Prompt(ctx context.Context, req *adapterhost.PromptRequest) (*adapterhost.PromptResponse, error)
}

// localBufconnHandle serves a fakePeer over an in-process bufconn and
// returns the production local rpcHandle for it. The nil go-plugin client
// is safe: Kill nil-guards the client and only the process-group signal
// path touches it.
func localBufconnHandle(t *testing.T, fp *fakePeer) adapterhost.Handle {
	t.Helper()
	lis := bufconn.Listen(1024)
	srv := grpc.NewServer()
	srv.RegisterService(adapterhost.AdapterServiceDescWithPrompt(), fp)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
		if err := <-errCh; err != nil && !errors.Is(err, net.ErrClosed) {
			t.Logf("bufconn server returned: %v", err)
		}
	})

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return adapterhost.NewRPCHandle("noop", nil, adapterhost.NewClientForConn(conn))
}

// parityVariant is one transport under test with its fakePeer fixture.
type parityVariant struct {
	name   string
	handle adapterhost.Handle
	fp     *fakePeer
}

// buildParityVariants returns the two transports under test — the local
// rpcHandle and the peerHandle — each backed by an identically configured
// fakePeer: prompt-capable, carrying a snapshot blob, with the schema
// version pre-seeded so Snapshot/Restore observations are meaningful.
func buildParityVariants(t *testing.T) []parityVariant {
	t.Helper()
	caps := []string{"pause", "snapshot", adapterhost.PromptCapability}

	mkFP := func() *fakePeer {
		fp := newFakePeer("")
		fp.caps = caps
		fp.promptAccept = true
		fp.snapshot = []byte("snap-1")
		fp.schemaVersion = 7
		return fp
	}

	localFP := mkFP()
	local := localBufconnHandle(t, localFP)

	peerFP := mkFP()
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	peerFP.connect(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	peerHandle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}

	return []parityVariant{
		{name: "local", handle: local, fp: localFP},
		{name: "peer", handle: peerHandle, fp: peerFP},
	}
}

// TestPeerLifecycleParity drives all 12 v2 AdapterService RPCs through both
// transports with an identical scripted session and asserts each observation
// matches the shared expectation. Kill is intentionally absent: it is a
// host-side teardown primitive, not a v2 RPC, and the peer variant tears the
// child connection down.
func TestPeerLifecycleParity(t *testing.T) {
	rows := []struct {
		name string
		run  func(t *testing.T, h parityHandle, fp *fakePeer) string
		want string
	}{
		{
			name: "Info",
			run: func(t *testing.T, h parityHandle, fp *fakePeer) string {
				info, err := h.Info(context.Background())
				if err != nil {
					return "err=" + err.Error()
				}
				return fmt.Sprintf("caps=%s err=<nil>", strings.Join(info.Capabilities, ","))
			},
			want: "caps=pause,snapshot,supports_prompt err=<nil>",
		},
		{
			name: "OpenSession",
			run: func(t *testing.T, h parityHandle, fp *fakePeer) string {
				if err := h.OpenSession(context.Background(), "s1", map[string]string{"k": "v"}, nil); err != nil {
					return "err=" + err.Error()
				}
				return "ok"
			},
			want: "ok",
		},
		{
			name: "StartLogStream",
			run: func(t *testing.T, h parityHandle, fp *fakePeer) string {
				sink := &logEventCollector{}
				cancelLog, _, err := h.StartLogStream(context.Background(), "s1", sink)
				if err != nil {
					return "err=" + err.Error()
				}
				defer cancelLog()
				waitFor(t, "log line over transport", func() bool { return sink.count("hello from peer") > 0 })
				return "log=hello from peer"
			},
			want: "log=hello from peer",
		},
		{
			name: "StartPermissionStream",
			run: func(t *testing.T, h parityHandle, fp *fakePeer) string {
				requests := make(chan *v2.PermissionEvent, 1)
				cancel, err := h.StartPermissionStream(context.Background(), "s1", requests)
				if err != nil {
					return "err=" + err.Error()
				}
				defer cancel()
				requests <- &v2.PermissionEvent{Event: &v2.PermissionEvent_Request{Request: &v2.PermissionRequest{RequestId: "r1"}}}
				delivered := false
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					fp.mu.Lock()
					n := len(fp.permRecv)
					fp.mu.Unlock()
					if n >= 1 {
						delivered = true
						break
					}
					time.Sleep(25 * time.Millisecond)
				}
				if !delivered {
					return "permission NOT delivered"
				}
				return "permission delivered"
			},
			want: "permission delivered",
		},
		{
			name: "Execute",
			run: func(t *testing.T, h parityHandle, fp *fakePeer) string {
				res, err := h.Execute(context.Background(), "s1", &workflow.StepNode{Name: "develop"}, &peerEventCollector{})
				if err != nil {
					return "err=" + err.Error()
				}
				return fmt.Sprintf("outcome=%s err=<nil>", res.Outcome)
			},
			want: "outcome=success err=<nil>",
		},
		{
			name: "Pause",
			run: func(t *testing.T, h parityHandle, fp *fakePeer) string {
				if err := h.Pause(context.Background(), "s1"); err != nil {
					return "err=" + err.Error()
				}
				return "ok"
			},
			want: "ok",
		},
		{
			name: "Inspect",
			run: func(t *testing.T, h parityHandle, fp *fakePeer) string {
				resp, err := h.Inspect(context.Background(), "s1")
				if err != nil {
					return "err=" + err.Error()
				}
				if len(resp.Fields) != 1 || resp.Fields[0].Key != "paused" {
					return "unexpected fields"
				}
				return fmt.Sprintf("paused=%t", resp.Fields[0].Value.GetBoolValue())
			},
			want: "paused=true",
		},
		{
			name: "Snapshot",
			run: func(t *testing.T, h parityHandle, fp *fakePeer) string {
				resp, err := h.Snapshot(context.Background(), "s1")
				if err != nil {
					return "err=" + err.Error()
				}
				return fmt.Sprintf("state=%s schema=%d", resp.State, resp.SchemaVersion)
			},
			want: "state=snap-1 schema=7",
		},
		{
			name: "Restore",
			run: func(t *testing.T, h parityHandle, fp *fakePeer) string {
				if err := h.Restore(context.Background(), "s1", []byte("restored-9"), 9); err != nil {
					return "err=" + err.Error()
				}
				fp.mu.Lock()
				defer fp.mu.Unlock()
				if string(fp.restored) != "restored-9" || fp.schemaVersion != 9 {
					return "adapter did not receive restored state"
				}
				return "ok"
			},
			want: "ok",
		},
		{
			name: "Resume",
			run: func(t *testing.T, h parityHandle, fp *fakePeer) string {
				if err := h.Resume(context.Background(), "s1"); err != nil {
					return "err=" + err.Error()
				}
				return "ok"
			},
			want: "ok",
		},
		{
			name: "Prompt",
			run: func(t *testing.T, h parityHandle, fp *fakePeer) string {
				resp, err := h.Prompt(context.Background(), &adapterhost.PromptRequest{SessionID: "s1", Prompt: "mid-turn check"})
				if err != nil {
					return "err=" + err.Error()
				}
				return fmt.Sprintf("accepted=%v detail=%q", resp.Accepted, resp.Detail)
			},
			want: "accepted=true detail=\"\"",
		},
		{
			name: "CloseSession",
			run: func(t *testing.T, h parityHandle, fp *fakePeer) string {
				if err := h.CloseSession(context.Background(), "s1"); err != nil {
					return "err=" + err.Error()
				}
				return "ok"
			},
			want: "ok",
		},
	}

	variants := buildParityVariants(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			for _, v := range variants {
				got := row.run(t, v.handle.(parityHandle), v.fp)
				if got != row.want {
					t.Errorf("%s transport: got %q, want %q", v.name, got, row.want)
				}
			}
		})
	}
}

// TestPeerPromptParity_ADR0006Modes drives the SessionManager prompt
// boundary on both transports (ADR-0006 D2/D3/D9): the deterministic failure
// taxonomy — capability gate, adapter acceptance, adapter rejection with the
// adapter's own detail — must hold identically over the peer transport.
func TestPeerPromptParity_ADR0006Modes(t *testing.T) {
	cases := []struct {
		name         string
		caps         []string
		promptAccept bool
		promptDetail string
		check        func(t *testing.T, transport string, sessionID string, err error)
	}{
		{
			name: "shell adapter routes to typed unsupported error",
			caps: []string{"pause", "snapshot"},
			check: func(t *testing.T, transport string, _ string, err error) {
				if !errors.Is(err, adapterhost.ErrPromptUnsupportedAdapter) {
					t.Errorf("%s transport: err = %v, want ErrPromptUnsupportedAdapter", transport, err)
				}
			},
		},
		{
			name:         "capable adapter accepts the prompt",
			caps:         []string{"pause", "snapshot", adapterhost.PromptCapability},
			promptAccept: true,
			check: func(t *testing.T, transport string, sessionID string, err error) {
				if err != nil {
					t.Errorf("%s transport: err = %v, want accepted", transport, err)
					return
				}
				if sessionID != "noop.develop" {
					t.Errorf("%s transport: sessionID = %q, want the opened session", transport, sessionID)
				}
			},
		},
		{
			name:         "capable adapter rejection carries adapter detail",
			caps:         []string{"pause", "snapshot", adapterhost.PromptCapability},
			promptDetail: "agent busy",
			check: func(t *testing.T, transport string, _ string, err error) {
				var rejected *adapterhost.PromptRejectedError
				if !errors.As(err, &rejected) {
					t.Errorf("%s transport: err = %v, want PromptRejectedError", transport, err)
					return
				}
				if rejected.Detail != "agent busy" {
					t.Errorf("%s transport: Detail = %q, want the adapter's own detail", transport, rejected.Detail)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, transport := range []string{"local", "peer"} {
				sm, handle := buildPromptParitySession(t, transport, tc.caps, tc.promptAccept, tc.promptDetail)
				sessionID, err := sm.Prompt(context.Background(), "noop.develop", &workflow.StepNode{Name: "develop"}, "", "mid-turn")
				tc.check(t, transport, sessionID, err)
				if t.Failed() {
					continue
				}
				_ = handle
			}
		})
	}
}

// buildPromptParitySession opens a SessionManager session over the named
// transport with the given adapter capabilities and prompt behavior, so the
// ADR-0006 prompt modes can be asserted on identical fixtures.
func buildPromptParitySession(t *testing.T, transport string, caps []string, promptAccept bool, promptDetail string) (*adapterhost.SessionManager, adapterhost.Handle) {
	t.Helper()
	fp := newFakePeer("")
	fp.caps = caps
	fp.promptAccept = promptAccept
	fp.promptDetail = promptDetail

	var handle adapterhost.Handle
	switch transport {
	case "local":
		handle = localBufconnHandle(t, fp)
	case "peer":
		provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
		fp.connect(t, addr)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h, err := provider.WaitForHandle(ctx, "noop", "")
		if err != nil {
			t.Fatalf("WaitForHandle: %v", err)
		}
		handle = h
	default:
		t.Fatalf("unknown transport %q", transport)
	}

	sm := adapterhost.NewSessionManager(&peerLoader{handle: handle})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sm.Open(ctx, "noop.develop", "noop", "", nil, nil); err != nil {
		t.Fatalf("%s transport: Open: %v", transport, err)
	}
	return sm, handle
}
