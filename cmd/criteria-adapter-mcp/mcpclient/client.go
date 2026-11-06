package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const jsonRPCVersion = "2.0"

// wireRequest describes one outgoing JSON-RPC envelope as a transport
// needs it: the encoded envelope, its method and id (id "" marks a
// notification, which gets no response), per-request HTTP headers (http
// transport only; a stdio send with headers is invalid), protocol-mandated
// Mcp-Param-* mirror headers from an annotated tool (http transport only;
// applied last so they win over caller-supplied headers), the request's
// name — the tool name / prompt name / resource uri of a name-bearing
// method, the Mcp-Name header source — and the protocol version the POST
// announces (the Mcp-Protocol-Version header source; empty is invalid on
// http).
type wireRequest struct {
	envelope        []byte
	method          string
	id              string
	headers         []HeaderEntry
	paramHeaders    []HeaderEntry
	name            string
	protocolVersion string
}

// wireRequestName derives the request name a method carries in its
// Mcp-Name header per the 2026-07-28 revision: the tool name for
// tools/call (read off the typed params struct), the prompt name for
// prompts/get, the resource uri for resources/read. Every other method —
// server/discover, tools/list, notifications, subscriptions/listen —
// carries no request name, and the header is omitted. The client's own
// identity never gates a request and never rides this header; it is the
// _meta[clientInfo] of each exchange.
func wireRequestName(method string, params any) string {
	switch method {
	case "tools/call":
		if p, ok := params.(callToolParams); ok {
			return p.Name
		}
	case "prompts/get":
		if p, ok := params.(map[string]any); ok {
			name, _ := p["name"].(string)
			return name
		}
	case "resources/read":
		if p, ok := params.(map[string]any); ok {
			uri, _ := p["uri"].(string)
			return uri
		}
	}
	return ""
}

// transport abstracts the wire between this client and an MCP server. The
// stdio dialect (framed pipes) and the Streamable HTTP dialect (POST +
// SSE response streams, the 2026-07-28 revision) both implement it; the
// JSON-RPC dispatch machinery is shared (request/pending/dispatch).
type transport interface {
	// send delivers one encoded JSON-RPC envelope. For a request (id != "")
	// send returns once the response has been routed into the client's
	// pending dispatcher or the send has failed: a stdio transport reads
	// the response on the background read loop, an http transport drains
	// the POST response stream synchronously before returning.
	send(ctx context.Context, req *wireRequest) error
	// close tears the transport down (pipe writer, response streams,
	// background listeners). Failing pending calls lives in
	// closeWithError, which calls this exactly once.
	close() error
}

// errBatchedTraffic reports batched JSON-RPC traffic from a peer: MCP
// transports never batch (one envelope per message in both directions), so
// a batch on the receive side is a protocol violation. The session closes
// on it, failing every pending call deterministically with this message
// under the implementation-defined session-close code (-32000).
var errBatchedTraffic = errors.New("mcpclient: batched JSON-RPC traffic received (MCP peers never batch)")

// Notification is a server-initiated JSON-RPC notification.
type Notification struct {
	Method string
	Params map[string]any
}

// Tool is the subset of MCP tool metadata used by the bridge.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// CallToolResult is the subset of tools/call output used by the bridge.
type CallToolResult struct {
	Content []map[string]any `json:"content"`
	IsError bool             `json:"isError,omitempty"`
	// StructuredContent carries the MCP structuredContent field verbatim when
	// the server provides one (passthrough into adapter outputs).
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	// ResultType is the required 2026-07-28 discriminator on every result
	// ("complete" | "input_required"). Legacy results omit it and MUST be
	// treated as complete (see IsInputRequired).
	ResultType string `json:"resultType,omitempty"`
	// InputRequests carries the interim multi-round-trip contract
	// (SEP-2322) when ResultType is input_required: a map of caller-facing
	// input request keys to full request envelopes. The retry contract is
	// not supported yet (first pass maps input_required to a typed call
	// error); RequestState is the server's opaque round-trip handle.
	InputRequests map[string]json.RawMessage `json:"inputRequests,omitempty"`
	RequestState  string                     `json:"requestState,omitempty"`
}

// IsInputRequired reports an interim multi-round-trip result (SEP-2322):
// the server needs additional input before the call can complete. A result
// with no resultType is an earlier-protocol server and is complete by
// definition.
func (r *CallToolResult) IsInputRequired() bool {
	return r.ResultType == ResultTypeInputRequired
}

// Framing selects the write-side frame encoding for the JSON-RPC stdio
// transport. The read side always auto-detects framing per frame (see
// readFrame), so a client writing either shape can read a peer speaking
// either shape.
type Framing string

const (
	// FramingLSP writes Content-Length header framing: the historical
	// dialect of the in-tree MCP adapter and fixtures, and the default.
	FramingLSP Framing = "lsp"
	// FramingNDJSON writes newline-delimited JSON: the MCP stdio wire
	// shape. One JSON object per line, terminated by "\n".
	FramingNDJSON Framing = "ndjson"
)

// ParseFraming converts an adapter config value into a Framing. "" selects
// the default (FramingLSP); matching is case-insensitive; any other value
// is an error so unknown spellings fail the session open instead of
// silently downgrading the transport.
func ParseFraming(s string) (Framing, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return FramingLSP, nil
	case string(FramingLSP):
		return FramingLSP, nil
	case string(FramingNDJSON):
		return FramingNDJSON, nil
	default:
		return "", fmt.Errorf("mcpclient: unknown framing %q (want %q or %q)", s, FramingLSP, FramingNDJSON)
	}
}

// RPCError is a typed JSON-RPC error, exactly as the server delivered it.
// Server error responses, and the transport error synthesized for pending
// calls when the session closes, both surface as *RPCError so callers can
// assert code boundaries with errors.As. The Error string keeps the
// historical "mcpclient: rpc error %d: %s" shape for log readability.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("mcpclient: rpc error %d: %s", e.Code, e.Message)
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      clientInfo     `json:"clientInfo"`
}

type listToolsResult struct {
	Tools []Tool `json:"tools"`
	// 2026-07-28: every result carries a required resultType and is a
	// SEP-2549 CacheableResult (freshness contract for client-side caching).
	ResultType string `json:"resultType,omitempty"`
	TTLms      int64  `json:"ttlMs,omitempty"`
	CacheScope string `json:"cacheScope,omitempty"`
}

type callToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
	// Meta carries the MCP `_meta` request field: the bridge passes
	// a progressToken so the server's notifications/progress traffic can be
	// routed back to the issuing execute when several calls are multiplexed
	// over one session.
	Meta map[string]any `json:"_meta,omitempty"`
}

type requestEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type incomingEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Client is a minimal JSON-RPC client for MCP servers, speaking the stdio
// dialect (framed pipes, via New/NewWithFraming) or the Streamable HTTP
// dialect (via NewHTTPClient).
type Client struct {
	tr transport

	// framing selects the write-side frame encoding of the stdio
	// transport; reads always auto-detect per frame. Only the stdio
	// framing probe (Connect, framingNegotiable) re-points it while the
	// session is still probe-only.
	framing Framing
	// framingNegotiable records that no framing was pinned in config: the
	// framing probe in Connect may try both stdio framings to find which
	// one the peer parses (see negotiation.go). An explicitly pinned
	// framing never flips.
	framingNegotiable bool

	notify func(Notification)

	// protocolVersion and stateless carry the negotiated session dialect.
	// Stateless sessions (2026-07-28) stamp _meta onto every request; legacy
	// sessions keep the historical bare request shape.
	protocolVersion string
	stateless       bool
	// clientName/clientVersion identify this client in the legacy initialize
	// handshake and in the per-request _meta of stateless sessions.
	clientName    string
	clientVersion string

	pendMu  sync.Mutex
	pending map[string]chan rpcResponse

	// toolParamMu guards toolParams: the Mcp-Param-* mirror bindings
	// resolved from listed tools' x-mcp-header schema annotations, keyed
	// by tool name. ListToolsDetailed rebuilds them; CallToolTracked
	// consults them per call (bindings are http-transport-only).
	toolParamMu sync.Mutex
	toolParams  map[string][]toolParamHeader

	closed    chan struct{}
	closeOnce sync.Once

	nextID uint64
}

// New constructs a client and starts a read loop that dispatches responses
// and notifications. Writes use the default Content-Length framing; use
// NewWithFraming to opt into newline-delimited JSON writes.
func New(reader io.Reader, writer io.WriteCloser, onNotification func(Notification)) *Client {
	return NewWithFraming(reader, writer, FramingLSP, onNotification)
}

// NewWithFraming constructs a client like New with an explicit write-side
// framing (FramingLSP or FramingNDJSON).
func NewWithFraming(reader io.Reader, writer io.WriteCloser, framing Framing, onNotification func(Notification)) *Client {
	if framing == "" {
		framing = FramingLSP
	}
	c := &Client{
		framing: framing,
		notify:  onNotification,
		pending: map[string]chan rpcResponse{},
		closed:  make(chan struct{}),
	}
	t := &stdioTransport{owner: c, reader: bufio.NewReader(reader), writer: writer}
	c.tr = t
	go t.readLoop(c)
	return c
}

// Transport reports which dialect the client speaks: TransportStdio for a
// framed process pipe, TransportHTTP for a Streamable HTTP endpoint.
func (c *Client) Transport() Transport {
	if c.isHTTP() {
		return TransportHTTP
	}
	return TransportStdio
}

// EnableFramingNegotiation marks the stdio session's write framing as
// unpinned: Connect may probe both stdio framings to find the one the peer
// parses. Must be called before Connect; an explicitly pinned framing never
// flips.
func (c *Client) EnableFramingNegotiation() {
	c.framingNegotiable = true
}

// Close closes the write side and unblocks pending requests.
func (c *Client) Close() {
	c.closeWithError(io.EOF)
}

// Initialize performs the legacy MCP initialize handshake and pins the
// session to the pre-2026-07-28 dialect: requests carry no _meta. This is
// the 2025-03-26 contract; new callers should use Connect, which negotiates
// per the stdio backward-compat procedure (probe-first server/discover with
// an initialize fallback).
func (c *Client) Initialize(ctx context.Context, clientName, clientVersion string) error {
	c.clientName = clientName
	c.clientVersion = clientVersion
	return c.initializeLegacy(ctx)
}

// initializeLegacy performs the 2025-03-26 initialize handshake and marks
// the session legacy (no per-request _meta).
func (c *Client) initializeLegacy(ctx context.Context) error {
	params := initializeParams{
		ProtocolVersion: ProtocolVersion20250326,
		Capabilities:    map[string]any{},
		ClientInfo: clientInfo{
			Name:    c.clientName,
			Version: c.clientVersion,
		},
	}
	if _, err := c.request(ctx, "initialize", params, nil, nil, ""); err != nil {
		return err
	}
	c.protocolVersion = ProtocolVersion20250326
	return nil
}

// requestMeta builds the per-request `_meta` of the 2026-07-28 stateless
// core: the negotiated protocol version and this client's capabilities ride
// on every request because the initialize handshake no longer exists. Legacy
// sessions send no _meta — the historical wire shape is preserved byte for
// byte.
func (c *Client) requestMeta() map[string]any {
	if !c.stateless {
		return nil
	}
	meta := map[string]any{
		MetaProtocolVersion:    c.protocolVersion,
		MetaClientCapabilities: map[string]any{},
	}
	if c.clientName != "" {
		meta[MetaClientInfo] = clientInfo{Name: c.clientName, Version: c.clientVersion}
	}
	return meta
}

// ToolsList is one decoded tools/list page. On 2026-07-28 servers the whole
// result is a SEP-2549 CacheableResult: TTLms bounds how long the advertised
// tools surface may be cached and CacheScope governs shared intermediaries
// only (per-connection tool snapshots stay per-connection). Legacy servers
// omit all of it; TTLms 0 then means "no hint".
type ToolsList struct {
	Tools      []Tool
	TTLms      int64
	CacheScope string
}

// ListToolsDetailed fetches tool metadata, exposing the CacheableResult
// freshness contract (SEP-2549) alongside the tools. Stateless sessions
// carry _meta on the request; legacy sessions keep the historical
// empty-params shape.
func (c *Client) ListToolsDetailed(ctx context.Context) (ToolsList, error) {
	params := map[string]any{}
	if meta := c.requestMeta(); meta != nil {
		params = map[string]any{"_meta": meta}
	}
	raw, err := c.request(ctx, "tools/list", params, nil, nil, "")
	if err != nil {
		return ToolsList{}, err
	}
	var out listToolsResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return ToolsList{}, fmt.Errorf("mcpclient: decode tools/list: %w", err)
	}
	if out.ResultType == ResultTypeInputRequired {
		return ToolsList{}, fmt.Errorf("mcpclient: tools/list returned resultType %q (SEP-2322); discovery is not a multi-round-trip call", out.ResultType)
	}
	kept, bindings := c.retainConformantTools(out.Tools)
	c.setToolParams(bindings)
	return ToolsList{Tools: kept, TTLms: out.TTLms, CacheScope: out.CacheScope}, nil
}

// retainConformantTools validates listed tools against the x-mcp-header
// annotation rules and returns the conformant surface with the resolved
// Mcp-Param-* bindings per tool name. A tool whose annotation cannot be
// resolved statically — an annotation outside a plain properties chain, a
// non-token or colliding header name, a non-primitive value type — has no
// mirrorable path, and a mirror the transport cannot honor is dropped from
// the surface rather than partially honored: calls to it would carry a
// required-header set the server cannot rely on.
func (c *Client) retainConformantTools(tools []Tool) ([]Tool, map[string][]toolParamHeader) {
	kept := make([]Tool, 0, len(tools))
	bindings := map[string][]toolParamHeader{}
	for _, tool := range tools {
		headers, ok := resolveToolParamHeaders(tool.InputSchema)
		if !ok {
			continue
		}
		kept = append(kept, tool)
		if len(headers) > 0 {
			bindings[tool.Name] = headers
		}
	}
	return kept, bindings
}

// setToolParams installs the mirror bindings resolved from the latest
// tools/list.
func (c *Client) setToolParams(bindings map[string][]toolParamHeader) {
	c.toolParamMu.Lock()
	defer c.toolParamMu.Unlock()
	c.toolParams = bindings
}

// toolParamsFor returns the mirror bindings recorded for a tool name.
func (c *Client) toolParamsFor(name string) []toolParamHeader {
	c.toolParamMu.Lock()
	defer c.toolParamMu.Unlock()
	return c.toolParams[name]
}

// ListTools fetches tool metadata from the MCP server.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	page, err := c.ListToolsDetailed(ctx)
	if err != nil {
		return nil, err
	}
	return page.Tools, nil
}

// CallTool executes a single MCP tool call.
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (CallToolResult, error) {
	return c.CallToolTracked(ctx, name, arguments, "", nil)
}

// CallToolTracked executes an MCP tool call carrying `_meta.progressToken`
// a server that echoes progress notifications keyed by the token
// lets the bridge route them back to the issuing execute when several calls
// are multiplexed over one session. progressToken "" is untracked (plain
// CallTool shape). extraHeaders carries per-request HTTP headers on the
// http transport (the bridge's x-mcp-header tool parameters); the stdio
// transport rejects any.
func (c *Client) CallToolTracked(ctx context.Context, name string, arguments map[string]any, progressToken string, extraHeaders []HeaderEntry) (CallToolResult, error) {
	params := callToolParams{Name: name, Arguments: arguments}
	// The progress token rides the same _meta as the 2026-07-28
	// stateless-core keys; the stateless meta is merged last so the
	// protocolVersion/capabilities keys are always present.
	if progressToken != "" {
		params.Meta = map[string]any{"progressToken": progressToken}
	}
	if meta := c.requestMeta(); meta != nil {
		if params.Meta == nil {
			params.Meta = meta
		} else {
			for k, v := range meta {
				params.Meta[k] = v
			}
		}
	}
	// Protocol-mandated mirrors (x-mcp-header schema annotations) are
	// http-only: the stdio dialect carries no custom headers, so an
	// annotated tool called over stdio would drop its required header set
	// server-side — the call is refused client-side instead.
	var paramHeaders []HeaderEntry
	if bindings := c.toolParamsFor(name); len(bindings) > 0 {
		if c.Transport() != TransportHTTP {
			return CallToolResult{}, fmt.Errorf("mcpclient: tool %q mirrors Mcp-Param-* headers from its x-mcp-header schema annotation; mirror headers are http-transport-only (stdio carries no custom headers)", name)
		}
		paramHeaders = mirrorToolParamHeaders(bindings, arguments)
	}
	raw, err := c.request(ctx, "tools/call", params, extraHeaders, paramHeaders, "")
	if err != nil {
		return CallToolResult{}, err
	}
	var out CallToolResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return CallToolResult{}, fmt.Errorf("mcpclient: decode tools/call: %w", err)
	}
	return out, nil
}

// Notification sends a JSON-RPC notification. On a stateless session the
// notification params carry the same per-message _meta as requests: the
// stateless core has no handshake that would have carried the keys, so
// they ride every client-to-server message.
func (c *Client) Notification(ctx context.Context, method string, params any) error {
	if c.stateless {
		params = withStatelessMeta(params, c.requestMeta())
	}
	req := requestEnvelope{JSONRPC: jsonRPCVersion, Method: method, Params: params}
	return c.send(ctx, method, "", nil, nil, req, "")
}

// withStatelessMeta merges the stateless-core _meta into a notification's
// params, which may be any JSON value; non-object params (and nil) degrade
// to an object carrying only the _meta keys.
func withStatelessMeta(params any, meta map[string]any) any {
	if meta == nil {
		return params
	}
	if obj, ok := params.(map[string]any); ok {
		merged := make(map[string]any, len(obj)+len(meta))
		for k, v := range obj {
			merged[k] = v
		}
		for k, v := range meta {
			merged[k] = v
		}
		return merged
	}
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	return out
}

func (c *Client) request(ctx context.Context, method string, params any, headers, paramHeaders []HeaderEntry, announceVersion string) (json.RawMessage, error) {
	id := strconv.FormatUint(atomic.AddUint64(&c.nextID, 1), 10)
	ch := make(chan rpcResponse, 1)

	c.pendMu.Lock()
	select {
	case <-c.closed:
		c.pendMu.Unlock()
		return nil, io.EOF
	default:
	}
	c.pending[id] = ch
	c.pendMu.Unlock()

	req := requestEnvelope{JSONRPC: jsonRPCVersion, ID: id, Method: method, Params: params}
	if err := c.send(ctx, method, id, headers, paramHeaders, req, announceVersion); err != nil {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		// Tell the server the call was abandoned after dropping the
		// pending entry (deleted above). Without this, a multiplexed server never learns the
		// caller gave up: the request stays pending on its side until the
		// session closes, and its per-request accounting (progress tokens,
		// in-flight marks) drifts as callers rotate. The notification carries
		// the client-internal JSON-RPC id — distinct from any progress token
		// — per the MCP notifications/cancelled shape. Best-effort over a
		// detached context because the caller's context is already done.
		// On Streamable HTTP the notification POST is omitted: there the
		// abandoned call's response stream close is the cancellation signal
		// (no server-side resumability), and a notifications/cancelled POST
		// after the stream is gone cancels nothing — the revision does not
		// define one for this transport. stdio keeps sending it. Best-effort
		// over a detached context because the caller's context is already done.
		if c.Transport() != TransportHTTP {
			_ = c.Notification(context.WithoutCancel(ctx), "notifications/cancelled", map[string]any{
				"requestId": id,
				"reason":    ctx.Err().Error(),
			})
		}
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// send encodes one JSON-RPC envelope and hands it to the transport. A send
// failure is fatal to the session on stdio (the shared pipe state is
// unreadable from here on) and per-request on http (each POST stands
// alone); a batch violation reported by the transport's inbound dispatch
// closes the session itself via closeWithError. announceVersion carries an
// explicit protocol-version announcement (only the server/discover probe
// announces pre-adoption); without one the negotiated version is announced.
// The request name (Mcp-Name) and the announced version are stamped here so
// the header set and the request body share one source.
func (c *Client) send(ctx context.Context, method, id string, headers, paramHeaders []HeaderEntry, v any, announceVersion string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return io.EOF
	default:
	}

	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mcpclient: marshal request: %w", err)
	}

	var name string
	if env, ok := v.(requestEnvelope); ok {
		name = wireRequestName(method, env.Params)
	}
	if announceVersion == "" {
		announceVersion = c.protocolVersion
	}

	if err := c.tr.send(ctx, &wireRequest{envelope: payload, method: method, id: id, headers: headers, paramHeaders: paramHeaders, name: name, protocolVersion: announceVersion}); err != nil {
		if _, isStdio := c.tr.(*stdioTransport); isStdio {
			c.closeWithError(err)
			return err
		}
		return err
	}
	return nil
}

// stdioTransport is the framed-pipe transport dialect: one background read
// loop auto-detects framing per frame; writes go out in the owner client's
// write framing. Send failures are fatal to the session (the shared pipe
// state is unreadable from here on). The read loop is the only reader of
// the pipe; closing the writer is what unblocks it downstream — close()
// must return without waiting on the loop.
type stdioTransport struct {
	owner   *Client
	reader  *bufio.Reader
	writeMu sync.Mutex
	writer  io.WriteCloser
}

// replaceFraming swaps the write framing (the framing probe re-points it
// between probes) under the write lock so writeOut never reads mid-flip.
func (t *stdioTransport) replaceFraming(f Framing) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.owner.framing = f
}

// currentFraming returns the write framing under the write lock.
func (t *stdioTransport) currentFraming() Framing {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.owner.framing
}

func (t *stdioTransport) send(_ context.Context, req *wireRequest) error {
	if len(req.headers) > 0 {
		return errors.New("mcpclient: per-request HTTP headers are not valid on the stdio transport")
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if err := t.writeOut(req.envelope); err != nil {
		return err
	}
	return nil
}

func (t *stdioTransport) close() error {
	return t.writer.Close()
}

// readLoop decodes inbound frames until the stream breaks, dispatching
// them through the shared envelope pipeline. Frame read failures (EOF,
// truncation) close the session. The loop exits after close() because the
// writer close is the downstream unblock of the pending read.
func (t *stdioTransport) readLoop(c *Client) {
	for {
		payload, err := readFrame(t.reader)
		if err != nil {
			c.closeWithError(err)
			return
		}
		// Inbound dispatch folds every actionable outcome into session
		// state itself (batch traffic closes the session; unparseable
		// noise is dropped), so the read loop has nothing to do with the
		// return value.
		_ = c.dispatchInbound(payload)
		if c.isClosed() {
			return
		}
	}
}

// dispatchInbound parses one inbound JSON-RPC envelope and routes it:
// server-to-client frames go to the notification callback, responses to the
// pending-call dispatcher. Envelopes that do not parse are dropped (the
// historical stdio posture for stray peer noise). A batch payload is a
// protocol violation on MCP transports and closes the session (see
// errBatchedTraffic). Shared by the stdio read loop and the http
// transport's response and subscription readers.
func (c *Client) dispatchInbound(payload []byte) error {
	if len(payload) > 0 && payload[0] == '[' {
		c.closeWithError(errBatchedTraffic)
		return errBatchedTraffic
	}
	var msg incomingEnvelope
	if err := json.Unmarshal(payload, &msg); err == nil {
		return c.routeInbound(&msg)
	}
	// Unparseable inbound noise: dropped without touching pending calls or
	// the session (the historical stdio posture for stray peer bytes).
	return nil
}

// routeInbound sends one parsed inbound envelope to its handler.
func (c *Client) routeInbound(msg *incomingEnvelope) error {
	if msg.Method != "" {
		c.handleNotification(msg.Method, msg.Params)
		return nil
	}
	if len(msg.ID) > 0 {
		c.handleResponse(msg)
	}
	return nil
}

// hasPending reports whether an id still has an unsettled pending call;
// the http transport uses it to stop reading a response stream once the
// call's own response arrived.
func (c *Client) hasPending(id string) bool {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	_, ok := c.pending[id]
	return ok
}

// isClosed reports whether the session already closed.
func (c *Client) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// writeResyncNewline terminates an unterminated partial line that a
// header-framed probe leaves behind on newline-based peers: newline-based
// wire frames must end in "\n", so a Content-Length frame's payload stays
// unparsed there until the next write glues with it. Writing a bare newline
// (not a framed request) lets a line reader parse the abandoned probe's
// payload as its own line and puts the following write on a clean line
// boundary; its late reply binds to the abandoned request id and drops.
func (t *stdioTransport) writeResyncNewline() {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, _ = t.writer.Write([]byte("\n"))
}

// stdio returns the stdio transport of this client, or nil when the client
// speaks the http dialect.
func (c *Client) stdio() *stdioTransport {
	t, _ := c.tr.(*stdioTransport)
	return t
}

// isHTTP reports whether this client speaks the Streamable HTTP dialect.
func (c *Client) isHTTP() bool {
	_, ok := c.tr.(*httpTransport)
	return ok
}

func (c *Client) handleNotification(method string, raw json.RawMessage) {
	if c.notify == nil {
		return
	}
	params := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &params)
	}
	c.notify(Notification{Method: method, Params: params})
}

func (c *Client) handleResponse(msg *incomingEnvelope) {
	key := normalizeID(msg.ID)
	// A JSON-RPC parse error (or similar global failure) replies with id
	// null: the peer could not parse the request id either. It binds to no
	// pending call by id, but when exactly one request is outstanding it is
	// unambiguously the answer to it (or a global peer failure for that
	// frame) — resolve it right away instead of leaving the caller to wedge
	// until its timeout. With several outstanding requests the attribution
	// is ambiguous (never guess across concurrent calls) and the
	// response stays dropped, as does any id-less reply with nothing
	// pending.
	if key == "" {
		c.pendMu.Lock()
		if len(c.pending) == 1 {
			for id, ch := range c.pending {
				delete(c.pending, id)
				c.pendMu.Unlock()
				if msg.Error == nil {
					msg.Error = &RPCError{Code: -32000, Message: "mcpclient: response carries no request id"}
				}
				ch <- rpcResponse{ID: msg.ID, Error: msg.Error}
				return
			}
		}
		c.pendMu.Unlock()
		return
	}
	c.pendMu.Lock()
	ch, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.pendMu.Unlock()
	if ok {
		ch <- rpcResponse{ID: msg.ID, Result: msg.Result, Error: msg.Error}
	}
}

func (c *Client) closeWithError(err error) {
	if err == nil {
		err = io.EOF
	}
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.tr.close()

		c.pendMu.Lock()
		deferred := make([]chan rpcResponse, 0, len(c.pending))
		for key, ch := range c.pending {
			delete(c.pending, key)
			deferred = append(deferred, ch)
		}
		c.pendMu.Unlock()

		for _, ch := range deferred {
			ch <- rpcResponse{Error: &RPCError{Code: -32000, Message: err.Error()}}
		}
	})
}

func normalizeID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(raw))
	// JSON null is the spec's "no id" marker (parse-error replies): it binds
	// to no pending call, same as an absent id.
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(trimmed), &s); err == nil {
		return s
	}
	var n float64
	if err := json.Unmarshal([]byte(trimmed), &n); err == nil {
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return trimmed
}

// writeOut writes one frame in the stdio transport's write framing. The
// default is Content-Length header framing (FramingLSP); FramingNDJSON
// writes a single line terminated by "\n". Reads are framing-agnostic
// either way.
func (t *stdioTransport) writeOut(payload []byte) error {
	if t.owner.framing == FramingNDJSON {
		line := append(bytes.Clone(payload), '\n')
		if _, err := t.writer.Write(line); err != nil {
			return fmt.Errorf("mcpclient: write ndjson frame: %w", err)
		}
		return nil
	}
	return writeFrame(t.writer, payload)
}

// writeFrame writes one Content-Length header frame (the historical dialect
// of the in-tree adapter and fixtures).
func writeFrame(w io.Writer, payload []byte) error {
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))
	if _, err := io.WriteString(w, header); err != nil {
		return fmt.Errorf("mcpclient: write header: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("mcpclient: write payload: %w", err)
	}
	return nil
}

// readFrame reads one JSON-RPC frame, auto-detecting the framing per frame:
// a Content-Length header line opens header framing (the remaining header
// block is read to the blank separator, then the payload by length); a line
// that parses as JSON is taken as an NDJSON frame; anything else is garbage
// injected into the stream by the peer and is skipped. A truncated final
// frame surfaces deterministically as io.ErrUnexpectedEOF so pending calls
// fail on session close instead of hanging.
func readFrame(r *bufio.Reader) ([]byte, error) {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("mcpclient: read frame: %w", err)
			}
			return finalLineFrame(line)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		if isContentLengthHeader(trimmed) {
			payload, err := readHeaderFramedBody(r, trimmed)
			if err != nil {
				return nil, err
			}
			return payload, nil
		}
		if payload, ok := jsonLinePayload(trimmed); ok {
			return payload, nil
		}
		// Garbage line: skip it and keep listening.
	}
}

// finalLineFrame classifies a final line that arrived without a newline
// before EOF: a complete JSON line is a valid frame; a header, garbage, or a
// partial payload is a truncated frame so pending calls fail instead of hang.
func finalLineFrame(line string) ([]byte, error) {
	trimmed := strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(trimmed) == "" {
		return nil, io.EOF
	}
	if payload, ok := jsonLinePayload(trimmed); ok {
		return payload, nil
	}
	return nil, fmt.Errorf("mcpclient: truncated frame at EOF: %w", io.ErrUnexpectedEOF)
}

// isContentLengthHeader reports whether the line opens header framing.
func isContentLengthHeader(line string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "content-length:")
}

// readHeaderFramedBody consumes the remaining header lines of a Content-
// Length framed frame (first line already seen: contentLengthHeader), then
// reads the payload by length.
func readHeaderFramedBody(r *bufio.Reader, firstHeaderLine string) ([]byte, error) {
	contentLength := -1
	line := firstHeaderLine
	for {
		name, value, found := strings.Cut(line, ":")
		if found && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr != nil {
				return nil, fmt.Errorf("mcpclient: parse content-length %q: %w", value, parseErr)
			}
			contentLength = n
		}
		next, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				// A Content-Length header was seen, so the peer started a
				// frame; EOF before the header block completed is a
				// truncated frame.
				return nil, fmt.Errorf("mcpclient: truncated header block at EOF: %w", io.ErrUnexpectedEOF)
			}
			return nil, fmt.Errorf("mcpclient: read header line: %w", err)
		}
		line = strings.TrimRight(next, "\r\n")
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	if contentLength < 0 {
		return nil, errors.New("mcpclient: missing content-length header")
	}
	if contentLength == 0 {
		return []byte{}, nil
	}
	payload := make([]byte, contentLength)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("mcpclient: read payload: %w", err)
	}
	return bytes.Clone(payload), nil
}

// jsonLinePayload reports whether the trimmed line is a complete JSON value
// and returns it (only objects and arrays count as frames; bare scalars and
// "null" are treated as garbage so stray lines never resolve as requests).
func jsonLinePayload(line string) ([]byte, bool) {
	t := strings.TrimSpace(line)
	if t == "" {
		return nil, false
	}
	switch t[0] {
	case '{', '[':
	default:
		return nil, false
	}
	if !json.Valid([]byte(t)) {
		return nil, false
	}
	return []byte(t), true
}
