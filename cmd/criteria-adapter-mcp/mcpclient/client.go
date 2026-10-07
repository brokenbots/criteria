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
	// the server provides one (CRI-172: passthrough into adapter outputs).
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
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
}

type callToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
	// Meta carries the MCP `_meta` request field (KB-155): the bridge passes
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

// Client is a minimal JSON-RPC stdio client for MCP servers.
type Client struct {
	reader *bufio.Reader
	writer io.WriteCloser
	// framing selects the write-side frame encoding; reads always
	// auto-detect per frame.
	framing Framing

	notify func(Notification)

	writeMu sync.Mutex
	pendMu  sync.Mutex
	pending map[string]chan rpcResponse

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
		reader:  bufio.NewReader(reader),
		writer:  writer,
		framing: framing,
		notify:  onNotification,
		pending: map[string]chan rpcResponse{},
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Close closes the write side and unblocks pending requests.
func (c *Client) Close() {
	c.closeWithError(io.EOF)
}

// Initialize performs the MCP initialize request.
func (c *Client) Initialize(ctx context.Context, clientName, clientVersion string) error {
	params := initializeParams{
		ProtocolVersion: "2025-03-26",
		Capabilities:    map[string]any{},
		ClientInfo: clientInfo{
			Name:    clientName,
			Version: clientVersion,
		},
	}
	_, err := c.request(ctx, "initialize", params)
	return err
}

// ListTools fetches tool metadata from the MCP server.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	raw, err := c.request(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out listToolsResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("mcpclient: decode tools/list: %w", err)
	}
	return out.Tools, nil
}

// CallTool executes a single MCP tool call.
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (CallToolResult, error) {
	return c.CallToolTracked(ctx, name, arguments, "")
}

// CallToolTracked executes an MCP tool call carrying `_meta.progressToken`
// (KB-155): a server that echoes progress notifications keyed by the token
// lets the bridge route them back to the issuing execute when several calls
// are multiplexed over one session. progressToken "" is untracked (plain
// CallTool shape).
func (c *Client) CallToolTracked(ctx context.Context, name string, arguments map[string]any, progressToken string) (CallToolResult, error) {
	params := callToolParams{Name: name, Arguments: arguments}
	if progressToken != "" {
		params.Meta = map[string]any{"progressToken": progressToken}
	}
	raw, err := c.request(ctx, "tools/call", params)
	if err != nil {
		return CallToolResult{}, err
	}
	var out CallToolResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return CallToolResult{}, fmt.Errorf("mcpclient: decode tools/call: %w", err)
	}
	return out, nil
}

// Notification sends a JSON-RPC notification.
func (c *Client) Notification(ctx context.Context, method string, params any) error {
	req := requestEnvelope{JSONRPC: jsonRPCVersion, Method: method, Params: params}
	return c.send(ctx, req)
}

func (c *Client) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
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
	if err := c.send(ctx, req); err != nil {
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
		// KB-155: tell the server the call was abandoned after dropping the
		// pending entry (deleted above). Without this, a multiplexed server never learns the
		// caller gave up: the request stays pending on its side until the
		// session closes, and its per-request accounting (progress tokens,
		// in-flight marks) drifts as callers rotate. The notification carries
		// the client-internal JSON-RPC id — distinct from any progress token
		// — per the MCP notifications/cancelled shape. Best-effort over a
		// detached context because the caller's context is already done.
		_ = c.Notification(context.WithoutCancel(ctx), "notifications/cancelled", map[string]any{
			"requestId": id,
			"reason":    ctx.Err().Error(),
		})
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

func (c *Client) send(ctx context.Context, v any) error {
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

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.writeOut(payload); err != nil {
		c.closeWithError(err)
		return err
	}
	return nil
}

func (c *Client) readLoop() {
	for {
		payload, err := readFrame(c.reader)
		if err != nil {
			c.closeWithError(err)
			return
		}
		var msg incomingEnvelope
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		if msg.Method != "" {
			c.handleNotification(msg.Method, msg.Params)
			continue
		}
		if len(msg.ID) > 0 {
			c.handleResponse(&msg)
		}
	}
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
	if key == "" {
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
		_ = c.writer.Close()

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
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return strings.TrimSpace(string(raw))
}

// writeOut writes one frame in the client's write framing. The default is
// Content-Length header framing (FramingLSP); FramingNDJSON writes a single
// line terminated by "\n". Reads are framing-agnostic either way.
func (c *Client) writeOut(payload []byte) error {
	if c.framing == FramingNDJSON {
		line := append(bytes.Clone(payload), '\n')
		if _, err := c.writer.Write(line); err != nil {
			return fmt.Errorf("mcpclient: write ndjson frame: %w", err)
		}
		return nil
	}
	return writeFrame(c.writer, payload)
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
			if errors.Is(err, io.EOF) {
				trimmed := strings.TrimRight(line, "\r\n")
				if strings.TrimSpace(trimmed) == "" {
					return nil, io.EOF
				}
				if payload, ok := jsonLinePayload(trimmed); ok {
					return payload, nil
				}
				return nil, fmt.Errorf("mcpclient: truncated frame at EOF: %w", io.ErrUnexpectedEOF)
			}
			return nil, fmt.Errorf("mcpclient: read frame: %w", err)
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
				return nil, io.EOF
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
