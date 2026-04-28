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
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
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
	Error   *rpcError       `json:"error,omitempty"`
}

// Client is a minimal JSON-RPC stdio client for MCP servers.
type Client struct {
	reader *bufio.Reader
	writer io.WriteCloser

	notify func(Notification)

	writeMu sync.Mutex
	pendMu  sync.Mutex
	pending map[string]chan rpcResponse

	closed    chan struct{}
	closeOnce sync.Once

	nextID uint64
}

// New constructs a client and starts a read loop that dispatches responses and notifications.
func New(reader io.Reader, writer io.WriteCloser, onNotification func(Notification)) *Client {
	c := &Client{
		reader:  bufio.NewReader(reader),
		writer:  writer,
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
	raw, err := c.request(ctx, "tools/call", callToolParams{Name: name, Arguments: arguments})
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
		return nil, ctx.Err()
	case <-c.closed:
		return nil, io.EOF
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("mcpclient: rpc %s failed (%d): %s", method, resp.Error.Code, resp.Error.Message)
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
	if err := writeFrame(c.writer, payload); err != nil {
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
			c.handleResponse(msg)
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

func (c *Client) handleResponse(msg incomingEnvelope) {
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
			ch <- rpcResponse{Error: &rpcError{Code: -32000, Message: err.Error()}}
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

func readFrame(r *bufio.Reader) ([]byte, error) {
	contentLength := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("mcpclient: read header line: %w", err)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(parts[0]), "Content-Length") {
			n, parseErr := strconv.Atoi(strings.TrimSpace(parts[1]))
			if parseErr != nil {
				return nil, fmt.Errorf("mcpclient: parse content-length %q: %w", parts[1], parseErr)
			}
			contentLength = n
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
