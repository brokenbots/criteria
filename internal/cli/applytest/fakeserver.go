// Package applytest provides a fake Connect server harness for testing
// server-mode apply functions in package cli. It stands up an in-memory
// Connect server over an httptest.Server (h2c) and exposes hooks that drive
// run lifecycle scenarios without requiring a real orchestrator.
package applytest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// ApplyExecution is the script the fake server drives:
//   - InjectPauseAt: when a WaitEntered event is received for this node name,
//     the fake waits ResumeAfter and then sends a ResumeRun control message.
//   - NeverResume: when true, InjectPauseAt triggers the pause watch but never
//     sends a ResumeRun — use this to test timeout paths in drainResumeCycles.
//   - DropStreamAt: when a StepEntered event is received for this step name,
//     the fake closes the SubmitEvents stream once (forcing a client reconnect).
//   - CancelAt: when a StepEntered event is received for this step name,
//     the fake sends a RunCancel control message.
type ApplyExecution struct {
	InjectPauseAt string        // wait node name; empty = no pause injection
	NeverResume   bool          // when true, InjectPauseAt fires the hook but never schedules a resume
	ResumeAfter   time.Duration // delay before ResumeRun; defaults to 10ms when zero
	DropStreamAt  string        // step name; empty = no stream drop
	CancelAt      string        // step name; empty = no cancellation
}

// Fake stands up an in-memory server endpoint over loopback and exposes
// hooks tests use to drive the run lifecycle.
type Fake struct {
	// Execution prescribes the scripted lifecycle the fake drives.
	Execution ApplyExecution

	mu      sync.Mutex
	allEvts []*pb.Envelope

	handler *fakeHandler
	srv     *httptest.Server

	// caCertPEM holds the PEM-encoded CA certificate used to start a TLS
	// server (set by NewTLS / NewMTLS; nil for plain h2c servers).
	caCertPEM []byte
	// caKeyPEM holds the PEM-encoded CA private key (set by NewMTLS; nil
	// otherwise). Exposed so tests can prove the CA cert is rejected when
	// used as a client credential.
	caKeyPEM []byte
	// clientCertPEM / clientKeyPEM hold the PEM-encoded client certificate
	// and private key that tests should present when connecting to an mTLS
	// fake server (set by NewMTLS; nil otherwise).
	clientCertPEM []byte
	clientKeyPEM  []byte

	// goroutine lifecycle: cancel stops InjectPauseAt goroutines; wg blocks
	// until they exit so t.Cleanup can call srv.Close without racing.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// CACertPEM returns the PEM-encoded CA certificate for TLS fake servers,
// or nil for plain h2c fakes.
func (f *Fake) CACertPEM() []byte { return f.caCertPEM }

// CAKeyPEM returns the PEM-encoded CA private key for mTLS fake servers,
// or nil for plain h2c / TLS-only fakes. Intended for negative tests that
// must verify the CA cert is rejected when presented as a client credential.
func (f *Fake) CAKeyPEM() []byte { return f.caKeyPEM }

// ClientCertPEM returns the PEM-encoded client certificate for mTLS fake
// servers, or nil for plain h2c / TLS-only fakes.
func (f *Fake) ClientCertPEM() []byte { return f.clientCertPEM }

// ClientKeyPEM returns the PEM-encoded client private key for mTLS fake
// servers, or nil for plain h2c / TLS-only fakes.
func (f *Fake) ClientKeyPEM() []byte { return f.clientKeyPEM }

// newFakeHandler allocates and initialises the internal handler for f.
func newFakeHandler(f *Fake) *fakeHandler {
	return &fakeHandler{
		parent:      f,
		criteriaID:  "test-criteria-id",
		token:       "test-token",
		events:      make(map[string][]*pb.Envelope),
		controls:    make(chan *pb.ControlMessage, 32),
		ctlAttached: make(chan struct{}, 1),
	}
}

// New starts a fake server on a random loopback port and registers t.Cleanup
// to cancel pending goroutines, wait for them to exit, then close the server.
//
// h2c connections are hijacked by the h2c library, so httptest.Server.Close()
// cannot close them (hijacked connections are removed from httptest's tracking).
// We intercept ConnState before Start() to collect the hijacked net.Conn values
// and close them explicitly in cleanup, which unblocks serverConn.readFrames and
// serverConn.serve goroutines so goleak.VerifyNone(t) in per-test cleanup passes.
func New(t testing.TB) *Fake {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &Fake{ctx: ctx, cancel: cancel}
	f.handler = newFakeHandler(f)

	mux := http.NewServeMux()
	path, h := criteriav1connect.NewCriteriaServiceHandler(f.handler)
	mux.Handle(path, h)

	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))

	// Intercept the ConnState hook before srv.Start() so httptest.Server.wrap()
	// captures this as oldHook and calls it after its own tracking logic. We
	// record hijacked connections so the cleanup can close them explicitly.
	var hijackedMu sync.Mutex
	var hijackedConns []net.Conn
	srv.Config.ConnState = func(c net.Conn, cs http.ConnState) {
		if cs == http.StateHijacked {
			hijackedMu.Lock()
			hijackedConns = append(hijackedConns, c)
			hijackedMu.Unlock()
		}
	}

	srv.Start()
	f.srv = srv

	t.Cleanup(func() {
		cancel()
		f.wg.Wait()
		// Close hijacked h2c connections: not tracked by httptest after hijack,
		// so srv.Config.Close() and srv.Close() cannot reach them.
		hijackedMu.Lock()
		for _, c := range hijackedConns {
			_ = c.Close()
		}
		hijackedMu.Unlock()
		// Close any non-hijacked connections in StateActive (belt-and-suspenders).
		_ = srv.Config.Close()
		srv.Close()
	})
	return f
}

// generateSelfSignedCert generates a self-signed RSA CA certificate valid for
// 127.0.0.1 that can be used as a server certificate in tests.
// Returns (certPEM, keyPEM) in PEM encoding.
func generateSelfSignedCert(t testing.TB) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("applytest: generate RSA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"criteria-test"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		// ExtKeyUsageClientAuth is included here solely so Go's EKU chain-
		// validation accepts the leaf client cert (issued by this CA) for client
		// authentication. The CA cert itself is prevented from being used as a
		// client cert by the VerifyPeerCertificate hook in NewMTLS (IsCA=true check).
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("applytest: create certificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("applytest: marshal private key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// generateClientLeafCert generates a leaf client certificate signed by the
// provided CA key/cert. The leaf has IsCA=false and only ExtKeyUsageClientAuth,
// making it distinct from any CA certificate. Returns (certPEM, keyPEM).
func generateClientLeafCert(t testing.TB, caPriv *rsa.PrivateKey, caCert *x509.Certificate) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("applytest: generate leaf RSA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{Organization: []string{"criteria-test-client"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &priv.PublicKey, caPriv)
	if err != nil {
		t.Fatalf("applytest: create leaf certificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("applytest: marshal leaf private key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// NewTLS starts an HTTPS/h2 fake server using a freshly generated self-signed
// certificate. Call CACertPEM() to retrieve the PEM-encoded CA certificate
// that clients must trust.
func NewTLS(t testing.TB) *Fake {
	t.Helper()
	certPEM, keyPEM := generateSelfSignedCert(t)

	ctx, cancel := context.WithCancel(context.Background())
	f := &Fake{ctx: ctx, cancel: cancel, caCertPEM: certPEM}
	f.handler = newFakeHandler(f)

	mux := http.NewServeMux()
	path, h := criteriav1connect.NewCriteriaServiceHandler(f.handler)
	mux.Handle(path, h)

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("applytest: load TLS key pair: %v", err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	f.srv = srv

	t.Cleanup(func() {
		cancel()
		f.wg.Wait()
		// srv.Config.Close() closes TLS h2 connections in StateActive, which
		// httptest.Server.CloseClientConnections() would skip (it only closes idle).
		_ = srv.Config.Close()
		srv.Close()
	})
	return f
}

// parseCACert decodes a PEM-encoded cert+key pair produced by
// generateSelfSignedCert and returns the parsed private key and certificate,
// which are needed to sign leaf certificates in NewMTLS.
func parseCACert(t testing.TB, certPEM, keyPEM []byte) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	keyDER, _ := pem.Decode(keyPEM)
	privAny, err := x509.ParsePKCS8PrivateKey(keyDER.Bytes)
	if err != nil {
		t.Fatalf("applytest: parse CA private key: %v", err)
	}
	certDER, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(certDER.Bytes)
	if err != nil {
		t.Fatalf("applytest: parse CA certificate: %v", err)
	}
	return privAny.(*rsa.PrivateKey), cert
}

// rejectCACertClient is a tls.Config.VerifyPeerCertificate hook for mTLS servers
// that rejects any client certificate with IsCA=true. This prevents the CA
// certificate from being accidentally accepted as a client credential.
func rejectCACertClient(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return err
	}
	if leaf.IsCA {
		return fmt.Errorf("applytest: client presented a CA certificate; use the leaf client cert instead")
	}
	return nil
}

// NewMTLS starts an HTTPS/h2 fake server that requires mutual TLS
// authentication. A self-signed CA certificate is generated and used directly
// as the server certificate (no separate server leaf cert). The client
// certificate is a distinct leaf certificate signed by the same CA, with
// IsCA=false and ExtKeyUsageClientAuth only. Call CACertPEM(), CAKeyPEM(),
// ClientCertPEM(), and ClientKeyPEM() to retrieve the credential material.
// Using the CA certificate as a client certificate will fail mTLS verification
// (enforced by the rejectCACertClient VerifyPeerCertificate hook).
func NewMTLS(t testing.TB) *Fake {
	t.Helper()
	caCertPEM, caKeyPEM := generateSelfSignedCert(t)
	caPriv, caCert := parseCACert(t, caCertPEM, caKeyPEM)
	clientCertPEM, clientKeyPEM := generateClientLeafCert(t, caPriv, caCert)

	ctx, cancel := context.WithCancel(context.Background())
	f := &Fake{
		ctx:           ctx,
		cancel:        cancel,
		caCertPEM:     caCertPEM,
		caKeyPEM:      caKeyPEM,
		clientCertPEM: clientCertPEM,
		clientKeyPEM:  clientKeyPEM,
	}
	f.handler = newFakeHandler(f)

	mux := http.NewServeMux()
	path, h := criteriav1connect.NewCriteriaServiceHandler(f.handler)
	mux.Handle(path, h)

	cert, err := tls.X509KeyPair(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("applytest: load TLS key pair: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caCertPEM)

	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{
		Certificates:          []tls.Certificate{cert},
		ClientAuth:            tls.RequireAndVerifyClientCert,
		ClientCAs:             pool,
		VerifyPeerCertificate: rejectCACertClient,
	}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	f.srv = srv

	t.Cleanup(func() {
		cancel()
		f.wg.Wait()
		// srv.Config.Close() closes TLS h2 connections in StateActive, which
		// httptest.Server.CloseClientConnections() would skip (it only closes idle).
		_ = srv.Config.Close()
		srv.Close()
	})
	return f
}

// URL returns the base URL of the fake server. For plain h2c servers this is
// an http:// address; for TLS and mTLS servers it is an https:// address.
func (f *Fake) URL() string { return f.srv.URL }

// Events returns a point-in-time snapshot of all envelopes the fake received.
func (f *Fake) Events() []*pb.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*pb.Envelope, len(f.allEvts))
	copy(out, f.allEvts)
	return out
}

// HasStepEntered reports whether the fake received a StepEntered event for
// the named step.
func (f *Fake) HasStepEntered(step string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, env := range f.allEvts {
		if se := env.GetStepEntered(); se != nil && se.Step == step {
			return true
		}
	}
	return false
}

// HasEventOfType reports whether the fake received at least one event with
// the given payload type name (e.g. "WaitEntered", "RunCompleted").
func (f *Fake) HasEventOfType(typeName string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, env := range f.allEvts {
		if envelopeTypeName(env) == typeName {
			return true
		}
	}
	return false
}

// SinceSeqHeaders returns a snapshot of the since_seq header values received
// across all SubmitEvents connections. An empty string means no since_seq was
// sent (first connection); a numeric string comes from a reconnect.
func (f *Fake) SinceSeqHeaders() []string {
	f.handler.mu.Lock()
	defer f.handler.mu.Unlock()
	out := make([]string, len(f.handler.sinceSeqHdr))
	copy(out, f.handler.sinceSeqHdr)
	return out
}

// WaitForCond polls pred at 5ms intervals until it returns true or d elapses,
// then fails the test.
func (f *Fake) WaitForCond(t testing.TB, d time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !pred() {
		t.Fatalf("WaitForCond: condition not met within %s", d)
	}
}

// envelopeTypeName returns a human-readable payload type name for an envelope.
func envelopeTypeName(env *pb.Envelope) string {
	switch {
	case env.GetStepEntered() != nil:
		return "StepEntered"
	case env.GetStepOutcome() != nil:
		return "StepOutcome"
	case env.GetStepTransition() != nil:
		return "StepTransition"
	case env.GetRunStarted() != nil:
		return "RunStarted"
	case env.GetRunCompleted() != nil:
		return "RunCompleted"
	case env.GetRunFailed() != nil:
		return "RunFailed"
	case env.GetWaitEntered() != nil:
		return "WaitEntered"
	case env.GetWaitResumed() != nil:
		return "WaitResumed"
	default:
		return "Unknown"
	}
}

// --- internal handler -------------------------------------------------------

type fakeHandler struct {
	criteriav1connect.UnimplementedCriteriaServiceHandler

	parent     *Fake
	criteriaID string
	token      string

	mu          sync.Mutex
	events      map[string][]*pb.Envelope // run_id → ordered, persisted envelopes
	sinceSeqHdr []string                  // since_seq header values per connection
	dropDone    bool                      // true after DropStreamAt has fired once

	controls    chan *pb.ControlMessage
	ctlAttached chan struct{}
}

func (h *fakeHandler) Register(_ context.Context, _ *connect.Request[pb.RegisterRequest]) (*connect.Response[pb.RegisterResponse], error) {
	return connect.NewResponse(&pb.RegisterResponse{
		CriteriaId: h.criteriaID,
		Token:      h.token,
	}), nil
}

func (h *fakeHandler) Heartbeat(_ context.Context, _ *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error) {
	return connect.NewResponse(&pb.HeartbeatResponse{}), nil
}

func (h *fakeHandler) CreateRun(_ context.Context, req *connect.Request[pb.CreateRunRequest]) (*connect.Response[pb.Run], error) {
	id := uuid.NewString()
	h.mu.Lock()
	h.events[id] = nil
	h.mu.Unlock()
	return connect.NewResponse(&pb.Run{
		RunId:        id,
		CriteriaId:   req.Msg.CriteriaId,
		WorkflowName: req.Msg.WorkflowName,
		Status:       "pending",
	}), nil
}

func (h *fakeHandler) SubmitEvents(_ context.Context, stream *connect.BidiStream[pb.Envelope, pb.Ack]) error {
	sinceRaw := stream.RequestHeader().Get("since_seq")
	h.mu.Lock()
	h.sinceSeqHdr = append(h.sinceSeqHdr, sinceRaw)
	h.mu.Unlock()

	var sinceSeq uint64
	replayRequested := sinceRaw != ""
	if replayRequested {
		if v, err := strconv.ParseUint(sinceRaw, 10, 64); err == nil {
			sinceSeq = v
		}
	}

	replayed := map[string]bool{}

	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		// On first message for a run, replay any persisted events above sinceSeq.
		if replayRequested && !replayed[msg.RunId] {
			if err := h.replayAcks(stream, msg.RunId, sinceSeq); err != nil {
				return err
			}
			replayed[msg.RunId] = true
		}

		seq, cid, shouldDrop, isDuplicate := h.persistMsg(msg)

		if shouldDrop {
			return connect.NewError(connect.CodeUnavailable, errors.New("applytest: stream drop injected"))
		}

		if err := stream.Send(&pb.Ack{RunId: msg.RunId, Seq: seq, CorrelationId: cid}); err != nil {
			return err
		}

		if !isDuplicate {
			h.triggerActions(msg)
		}
	}
}

// replayAcks sends ack messages for all persisted events above sinceSeq.
func (h *fakeHandler) replayAcks(stream *connect.BidiStream[pb.Envelope, pb.Ack], runID string, sinceSeq uint64) error {
	h.mu.Lock()
	prior := append([]*pb.Envelope(nil), h.events[runID]...)
	h.mu.Unlock()
	for _, p := range prior {
		if p.Seq <= sinceSeq {
			continue
		}
		if err := stream.Send(&pb.Ack{RunId: p.RunId, Seq: p.Seq, CorrelationId: p.CorrelationId}); err != nil {
			return err
		}
	}
	return nil
}

// persistMsg deduplicates the envelope, applies DropStreamAt logic, persists
// the event if it should be stored, and returns (seq, correlationID, shouldDrop, isDuplicate).
func (h *fakeHandler) persistMsg(msg *pb.Envelope) (seq uint64, cid string, shouldDrop, isDuplicate bool) {
	h.mu.Lock()
	list := h.events[msg.RunId]

	// Dedup on (run_id, correlation_id) mirrors the real server's behaviour.
	if msg.CorrelationId != "" {
		for _, e := range list {
			if e.CorrelationId == msg.CorrelationId {
				seq = e.Seq
				isDuplicate = true
				break
			}
		}
	}

	// DropStreamAt fires once, before the event is persisted.
	ex := h.parent.Execution
	shouldDrop = ex.DropStreamAt != "" && !h.dropDone
	if shouldDrop {
		if se := msg.GetStepEntered(); se == nil || se.Step != ex.DropStreamAt {
			shouldDrop = false
		}
	}
	if shouldDrop {
		h.dropDone = true
	}

	if !isDuplicate && !shouldDrop {
		msg.Seq = uint64(len(list) + 1)
		seq = msg.Seq
		h.events[msg.RunId] = append(list, msg)
		h.parent.mu.Lock()
		h.parent.allEvts = append(h.parent.allEvts, msg)
		h.parent.mu.Unlock()
	}
	cid = msg.CorrelationId
	h.mu.Unlock()
	return seq, cid, shouldDrop, isDuplicate
}

// triggerActions fires scripted control messages in response to a received event.
func (h *fakeHandler) triggerActions(env *pb.Envelope) {
	ex := h.parent.Execution

	if ex.CancelAt != "" {
		if se := env.GetStepEntered(); se != nil && se.Step == ex.CancelAt {
			h.sendControl(&pb.ControlMessage{
				Command: &pb.ControlMessage_RunCancel{
					RunCancel: &pb.RunCancel{RunId: env.RunId, Reason: "applytest: cancel injected"},
				},
			})
		}
	}

	if ex.InjectPauseAt != "" {
		if we := env.GetWaitEntered(); we != nil && we.Node == ex.InjectPauseAt {
			h.schedulePauseResume(env.RunId)
		}
	}
}

// sendControl sends a ControlMessage non-blocking on a best-effort basis.
func (h *fakeHandler) sendControl(msg *pb.ControlMessage) {
	select {
	case h.controls <- msg:
	default:
	}
}

// schedulePauseResume starts a goroutine that sends a ResumeRun message after
// the configured delay. It is a no-op when NeverResume is true, allowing tests
// to verify timeout paths in drainResumeCycles without an actual resume signal.
func (h *fakeHandler) schedulePauseResume(runID string) {
	ex := h.parent.Execution
	if ex.NeverResume {
		return
	}
	delay := ex.ResumeAfter
	if delay <= 0 {
		delay = 10 * time.Millisecond
	}
	h.parent.wg.Add(1)
	go func() {
		defer h.parent.wg.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-h.parent.ctx.Done():
			return
		case <-timer.C:
		}
		h.sendControl(&pb.ControlMessage{
			Command: &pb.ControlMessage_ResumeRun{
				ResumeRun: &pb.ResumeRun{
					RunId:   runID,
					Signal:  "resume",
					Payload: map[string]string{"outcome": "received"},
				},
			},
		})
	}()
}

func (h *fakeHandler) Control(ctx context.Context, _ *connect.Request[pb.ControlSubscribeRequest], stream *connect.ServerStream[pb.ControlMessage]) error {
	if err := stream.Send(&pb.ControlMessage{
		Command: &pb.ControlMessage_ControlReady{ControlReady: &pb.ControlReady{}},
	}); err != nil {
		return err
	}
	select {
	case h.ctlAttached <- struct{}{}:
	default:
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-h.controls:
			if !ok {
				return nil
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}
