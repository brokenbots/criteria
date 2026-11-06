package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Transport selects the wire dialect between this client and an MCP server.
type Transport string

const (
	// TransportStdio is the framed-pipe dialect for process servers
	// (command/args/env/cwd/framing).
	TransportStdio Transport = "stdio"
	// TransportHTTP is the Streamable HTTP dialect of the 2026-07-28
	// revision (url/headers/timeout).
	TransportHTTP Transport = "http"
)

// ParseTransport converts an adapter config value into a Transport. ""
// selects the default (stdio); matching is case-insensitive; any other
// value is an error so unknown spellings fail the session open instead of
// silently switching dialect (same contract as ParseFraming and
// ParseNegotiation).
func ParseTransport(s string) (Transport, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return TransportStdio, nil
	case string(TransportStdio):
		return TransportStdio, nil
	case string(TransportHTTP):
		return TransportHTTP, nil
	default:
		return "", fmt.Errorf("mcpclient: unknown transport %q (want %q or %q)", s, TransportStdio, TransportHTTP)
	}
}

// HeaderEntry is one custom HTTP header applied to Streamable HTTP requests
// (the adapter's headers config and per-request x-mcp-header tool
// parameters).
type HeaderEntry struct {
	Name  string
	Value string
}

// NewHeaderEntry validates and returns one custom header: the name must be
// a non-empty HTTP token (RFC 9110 tchar) and the value must carry no line
// breaks or control characters. Names under transport control
// (Content-Type, Content-Length, Host, Accept, Mcp-Method, Mcp-Name,
// Mcp-Session-Id, Mcp-Protocol-Version, Last-Event-ID) are rejected — the
// transport owns them, and accepting them would let a caller smuggle
// session, routing, or framing state.
func NewHeaderEntry(name, value string) (HeaderEntry, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return HeaderEntry{}, errors.New("mcpclient: header name is empty")
	}
	for _, r := range name {
		if !isTokenRune(r) {
			return HeaderEntry{}, fmt.Errorf("mcpclient: header name %q is not a valid HTTP token", name)
		}
	}
	if err := validateHeaderNameControl(name); err != nil {
		return HeaderEntry{}, err
	}
	if strings.ContainsFunc(value, isForbiddenHeaderByte) {
		return HeaderEntry{}, fmt.Errorf("mcpclient: header %q value contains line breaks or control characters", name)
	}
	return HeaderEntry{Name: name, Value: value}, nil
}

// validateHeaderNameControl rejects names whose reserved status depends on a
// controlled set of transport/protocol header names, matched
// case-insensitively.
func validateHeaderNameControl(name string) error {
	if isReservedHeaderName(name) {
		return fmt.Errorf("mcpclient: header %q is under transport control", name)
	}
	return nil
}

// headerTokenNameChars is the RFC 9110 tchar set minus tchar-agnostic ones
// below; token chars are ALPHA / DIGIT / !#$%&'*+-.^_`|~.
func isTokenRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", r)
}

// isForbiddenHeaderByte reports whether b is a byte that may not appear in
// an HTTP field value: any control byte except tab, plus DEL. Obs-text
// (0x80-0xFF) is allowed.
func isForbiddenHeaderByte(r rune) bool {
	return (r < 0x20 && r != '\t') || r == 0x7F
}

// reservedHTTPHeaderNames are header names the transport sets itself,
// matched case-insensitively: transport-protocol headers (Mcp-*) and the
// framing/routing headers a custom value would corrupt or smuggle state
// through.
var reservedHTTPHeaderNames = map[string]struct{}{
	"accept":               {},
	"content-type":         {},
	"content-length":       {},
	"host":                 {},
	"last-event-id":        {},
	"mcp-method":           {},
	"mcp-name":             {},
	"mcp-protocol-version": {},
	"mcp-session-id":       {},
}

func isReservedHeaderName(name string) bool {
	_, ok := reservedHTTPHeaderNames[strings.ToLower(name)]
	return ok
}

// HTTPConfig configures the Streamable HTTP transport dialect.
type HTTPConfig struct {
	// Endpoint is the MCP endpoint URL every request POSTs to.
	Endpoint string
	// Headers are custom headers applied to every request after the
	// transport-controlled ones; per-request x-mcp-header entries win on
	// collision.
	Headers []HeaderEntry
	// RequestTimeout bounds each request and notification POST; the
	// subscriptions/listen stream is never bounded by it. Zero means
	// unbounded (the engine's own call deadline still applies to requests).
	RequestTimeout time.Duration
	// Listen opts the session into the single long-lived
	// subscriptions/listen stream for server-to-client notifications.
	Listen bool
}

// NewHTTPClient constructs a client speaking the Streamable HTTP dialect.
// The returned client still negotiates (Connect): on http, negotiation is a
// single server/discover probe in the current revision and there is no
// legacy initialize fallback (that handshake is removed on this transport).
func NewHTTPClient(cfg HTTPConfig, onNotification func(Notification)) *Client {
	c := &Client{
		notify:  onNotification,
		pending: map[string]chan rpcResponse{},
		closed:  make(chan struct{}),
	}
	c.tr = &httpTransport{
		owner:          c,
		endpoint:       cfg.Endpoint,
		headers:        cfg.Headers,
		requestTimeout: cfg.RequestTimeout,
		listen:         cfg.Listen,
		client: &http.Client{
			// A redirect response is an MCP error surface, not a follow
			// instruction: ErrUseLastResponse keeps the 3xx as the POST
			// response instead of replaying the request (custom and
			// x-mcp-header values included) against another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	return c
}

// httpTransport is the Streamable HTTP dialect: every request is a POST to
// the configured endpoint carrying transport-controlled headers plus any
// custom headers; responses are plain JSON envelopes or SSE streams of
// them. There is no session id and no GET long-poll — the 2026-07-28
// revision removed both, server-minted handles ride tool arguments instead,
// and subscriptions/listen is the single long-lived stream for
// server-to-client notifications when the session opts in.
type httpTransport struct {
	owner          *Client
	endpoint       string
	headers        []HeaderEntry
	requestTimeout time.Duration
	listen         bool
	client         *http.Client

	listenMu     sync.Mutex
	listenCancel context.CancelFunc
	listenIDs    atomic.Uint64
}

const (
	// maxHTTPResponseBytes bounds one Streamable HTTP response read; a
	// healthy MCP response (one envelope or an SSE event run) is far
	// smaller.
	maxHTTPResponseBytes = 16 << 20
	// maxDrainBytes bounds how much of an ignored response body is read
	// before it is discarded.
	maxDrainBytes = 64 << 10
	// ListenMethod is the MCP method of the single long-lived
	// server-to-client notification stream.
	ListenMethod = "subscriptions/listen"
	// DefaultListenReopenDelay is the pause between subscriptions/listen
	// attempts; it reuses the probe budget constant as its scale.
	DefaultListenReopenDelay = DefaultProbeTimeout
	// listenAttempts bounds subscriptions/listen opens per session. The
	// 2026-07-28 revision has no stream resumability (no Last-Event-ID):
	// once the attempts run out, notifications stop arriving while
	// requests keep working.
	listenAttempts = 2
)

// errResponseSettled is an internal stop signal: a response stream is
// response-scoped and stops being read once its own response arrived.
var errResponseSettled = errors.New("mcpclient: response settled")

func (t *httpTransport) send(ctx context.Context, req *wireRequest) error {
	// Defense in depth: the bridge validates per-request entries at the
	// config/param boundary; the transport re-runs the same checks so a
	// smuggled reserved or malformed name fails the call, never the HTTP
	// exchange.
	for _, h := range req.headers {
		if _, err := NewHeaderEntry(h.Name, h.Value); err != nil {
			return fmt.Errorf("mcpclient: per-request header: %w", err)
		}
	}
	if req.id == "" {
		return t.postNotification(ctx, req)
	}
	return t.postRequest(ctx, req)
}

// postRequest performs one request POST and consumes its response stream
// synchronously: the response settles this call, and progress
// notifications or interleaved server-to-client requests on the same SSE
// stream dispatch as they arrive. A stream that breaks before settling the
// call loses the in-flight request (the 2026-07-28 revision removed
// server-side resumability): the caller re-issues it as a new request with
// a new request id, and the failure is per-call — the session keeps
// working.
func (t *httpTransport) postRequest(ctx context.Context, req *wireRequest) error {
	reqCtx, cancel := t.bounded(ctx)
	if cancel != nil {
		defer cancel()
	}
	httpReq, err := t.newRequest(reqCtx, req, []string{"application/json", "text/event-stream"})
	if err != nil {
		return err
	}
	httpResp, err := t.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("mcpclient: POST %s: %w", req.method, err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		drainResponse(httpResp.Body)
		return fmt.Errorf("mcpclient: POST %s: unexpected status %d", req.method, httpResp.StatusCode)
	}
	pumpErr := t.pumpResponse(req, httpResp.Header.Get("Content-Type"), io.LimitReader(httpResp.Body, maxHTTPResponseBytes))
	// The SSE pump may stop reading early (the call's own response ends
	// the response-scoped stream): drain the remainder so the pooled
	// connection returns to the pool promptly.
	drainResponse(httpResp.Body)
	if pumpErr != nil {
		return pumpErr
	}
	if t.owner.hasPending(req.id) {
		// The stream ended without settling this call (or a broken stream
		// mid-read). No resumability: the request is lost; re-issue as a
		// new request with a new id.
		return fmt.Errorf("mcpclient: %s: response stream ended without a response (no server-side resumability; re-issue as a new request)", req.method)
	}
	return nil
}

// pumpResponse dispatches one successful response body: JSON is a single
// envelope; SSE events are envelopes streamed as they arrive; anything else
// is a per-call content-type failure.
func (t *httpTransport) pumpResponse(req *wireRequest, contentType string, body io.Reader) error {
	mediaType := contentType
	if i := strings.Index(contentType, ";"); i >= 0 {
		mediaType = contentType[:i]
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	switch mediaType {
	case "application/json":
		payload, err := io.ReadAll(body)
		if err != nil {
			return fmt.Errorf("mcpclient: %s: read response: %w", req.method, err)
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			return nil
		}
		return t.owner.dispatchInbound(payload)
	case "text/event-stream":
		return t.pumpSSE(body, req.id)
	default:
		return fmt.Errorf("mcpclient: %s: unexpected response content type %q (want application/json or text/event-stream)", req.method, mediaType)
	}
}

// pumpSSE streams the SSE events of one response. Envelopes dispatch as
// they arrive (progress first), and reading stops early once the call's
// own response settles — the stream is response-scoped.
func (t *httpTransport) pumpSSE(body io.Reader, callID string) error {
	err := parseSSEEvents(body, func(data []byte) error {
		if err := t.owner.dispatchInbound(data); err != nil {
			return err
		}
		if !t.owner.hasPending(callID) {
			return errResponseSettled
		}
		return nil
	})
	if errors.Is(err, errResponseSettled) {
		return nil
	}
	return err
}

// postNotification delivers a notification POST; any 2xx settles it and
// the body is ignored (notifications have no response).
func (t *httpTransport) postNotification(ctx context.Context, req *wireRequest) error {
	reqCtx, cancel := t.bounded(ctx)
	if cancel != nil {
		defer cancel()
	}
	httpReq, err := t.newRequest(reqCtx, req, []string{"application/json"})
	if err != nil {
		return err
	}
	httpResp, err := t.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("mcpclient: POST %s: %w", req.method, err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return fmt.Errorf("mcpclient: POST %s: unexpected status %d", req.method, httpResp.StatusCode)
	}
	drainResponse(httpResp.Body)
	return nil
}

// bounded applies the configured request timeout to a request or
// notification context; the listen stream is never bounded by it (its
// lifetime is the session's, not one call's).
func (t *httpTransport) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	if t.requestTimeout <= 0 {
		return ctx, nil
	}
	return context.WithTimeout(ctx, t.requestTimeout)
}

// newRequest builds the POST one JSON-RPC envelope needs per the
// 2026-07-28 Streamable HTTP rules: transport-controlled headers first
// (Content-Type, Accept, Mcp-Method, Mcp-Name, Mcp-Protocol-Version), then
// the config's custom headers, then per-request headers (x-mcp-header
// parameters win on collision). Header map assignment preserves caller
// case; Header.Set would canonicalize it away.
func (t *httpTransport) newRequest(ctx context.Context, req *wireRequest, accept []string) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(req.envelope))
	if err != nil {
		return nil, fmt.Errorf("mcpclient: build %s request: %w", req.method, err)
	}
	httpReq.Header["Content-Type"] = []string{"application/json"}
	httpReq.Header["Accept"] = []string{strings.Join(accept, ", ")}
	httpReq.Header["Mcp-Method"] = []string{req.method}
	if name := t.owner.clientName; name != "" {
		httpReq.Header["Mcp-Name"] = []string{name}
	} else {
		return nil, errors.New("mcpclient: request before Connect carries no client identity (Mcp-Name)")
	}
	if t.owner.protocolVersion != "" {
		httpReq.Header["Mcp-Protocol-Version"] = []string{t.owner.protocolVersion}
	}
	for _, h := range t.headers {
		httpReq.Header[h.Name] = []string{h.Value}
	}
	for _, h := range req.headers {
		httpReq.Header[h.Name] = []string{h.Value}
	}
	return httpReq, nil
}

// startListenStream opens the single long-lived subscriptions/listen stream
// for server-to-client notifications, best effort in the background: a
// failed or broken listen stream defers to the session (requests keep
// working; notifications stop after listenAttempts).
func (t *httpTransport) startListenStream(ctx context.Context) {
	if !t.listen {
		return
	}
	listenCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t.listenMu.Lock()
	t.listenCancel = cancel
	t.listenMu.Unlock()
	go t.listenLoop(listenCtx)
}

// listenLoop holds the subscriptions/listen stream open for the session's
// lifetime, re-opening it bounded times on stream breaks; there is no
// resumability (no Last-Event-ID), so notifications that arrive after the
// final attempt are lost — losing a toolsListChanged notification to a
// broken stream is benign: nothing in the session depends on catching
// every one (discovery refreshes re-list tools regardless).
func (t *httpTransport) listenLoop(ctx context.Context) {
	for attempt := 1; attempt <= listenAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		_ = t.listenOnce(ctx)
		if attempt < listenAttempts {
			select {
			case <-ctx.Done():
				return
			case <-time.After(DefaultListenReopenDelay):
			}
		}
	}
}

// listenOnce opens one subscriptions/listen stream and feeds its envelopes
// to the shared inbound dispatcher. The subscribe POST itself may answer
// with a result envelope (unknown id: dropped) or hold the stream open as
// SSE for the session's lifetime.
func (t *httpTransport) listenOnce(ctx context.Context) error {
	c := t.owner
	id := fmt.Sprintf("listen-%d", t.listenIDs.Add(1))
	payload, err := json.Marshal(requestEnvelope{JSONRPC: jsonRPCVersion, ID: id, Method: ListenMethod})
	if err != nil {
		return err
	}
	httpReq, err := t.newRequest(ctx, &wireRequest{envelope: payload, method: ListenMethod, id: id}, []string{"application/json", "text/event-stream"})
	if err != nil {
		return err
	}
	httpResp, err := t.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("mcpclient: subscriptions/listen: %w", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return fmt.Errorf("mcpclient: subscriptions/listen: unexpected status %d", httpResp.StatusCode)
	}
	return parseSSEEvents(io.LimitReader(httpResp.Body, maxHTTPResponseBytes), func(data []byte) error {
		return c.dispatchInbound(data)
	})
}

func (t *httpTransport) close() error {
	t.listenMu.Lock()
	cancel := t.listenCancel
	t.listenCancel = nil
	t.listenMu.Unlock()
	if cancel != nil {
		cancel()
	}
	t.client.CloseIdleConnections()
	return nil
}

// drainResponse reads a response body up to maxDrainBytes so keep-alive
// connections return to the pool promptly; the caller owns closing it.
func drainResponse(body io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrainBytes))
}

// parseSSEEvents decodes an SSE stream, invoking fn once per event with
// non-empty data; framing rules live in sseDecoder.
func parseSSEEvents(r io.Reader, fn func(data []byte) error) error {
	return (&sseDecoder{}).read(bufio.NewReader(r), fn)
}

// sseDecoder composes one SSE stream's events: it accumulates each event's
// data field values and hands the composed payload to fn.
type sseDecoder struct {
	data []string
}

// read runs the byte-level line scan: \r\n, \n, or \r terminated lines.
// EOF flushes a trailing unterminated event, mirroring servers that close
// a stream right after composing its final event. fn errors abort the
// stream.
func (d *sseDecoder) read(reader *bufio.Reader, fn func(data []byte) error) error {
	var line []byte
	advance := func() error {
		err := d.processLine(line, fn)
		line = line[:0]
		return err
	}
	for {
		b, brk := reader.ReadByte()
		switch {
		case brk != nil:
			return d.finish(line, fn)
		case b == '\n' || b == '\r':
			if b == '\r' {
				// CRLF: consume the LF that completes the break.
				if next, peekErr := reader.Peek(1); peekErr == nil && next[0] == '\n' {
					_, _ = reader.Discard(1)
				}
			}
			if processErr := advance(); processErr != nil {
				return processErr
			}
		default:
			line = append(line, b)
		}
	}
}

// finish flushes a trailing unterminated line (if one is pending) and then
// the composed event, on stream end.
func (d *sseDecoder) finish(line []byte, fn func(data []byte) error) error {
	if len(line) > 0 {
		if err := d.processLine(line, fn); err != nil {
			return err
		}
	}
	return d.flush(fn)
}

// processLine applies one SSE line to the decoder: a blank line flushes the
// composed event (multi-line data joined with "\n"), ":" starts a comment,
// a field value strips exactly one leading space, and event/id/retry fields
// are parsed and dropped — the 2026-07-28 revision removed server-side
// stream resumability, so no event ids are tracked. Unknown fields are
// ignored.
func (d *sseDecoder) processLine(line []byte, fn func(data []byte) error) error {
	lineStr := string(line)
	if lineStr == "" {
		return d.flush(fn)
	}
	if strings.HasPrefix(lineStr, ":") {
		return nil
	}
	name, value, _ := strings.Cut(lineStr, ":")
	value = strings.TrimPrefix(value, " ")
	switch name {
	case "data":
		d.data = append(d.data, value)
	case "event", "id", "retry":
		// no resumability, event kinds unused
	default:
		// unknown fields ignored
	}
	return nil
}

// flush hands fn the composed event, if one is pending.
func (d *sseDecoder) flush(fn func(data []byte) error) error {
	if len(d.data) == 0 {
		return nil
	}
	payload := []byte(strings.Join(d.data, "\n"))
	d.data = nil
	return fn(payload)
}
