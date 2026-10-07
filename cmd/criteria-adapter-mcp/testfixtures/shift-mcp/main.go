// Command shift-mcp is a test MCP fixture whose tools/list surface changes
// over the session's life (KB-157): the first tools/list call returns
// {early, lists} and every later call returns {early, late, lists} — the
// re-discovery tests use a "late" tool appearing after OpenSession to prove
// the TTL-bounded refresh actually re-consulted the server. It also counts
// tools/list calls: the `lists` tool call returns that count as text, so
// tests can assert cache-hit/miss and single-flight behavior. Setting
// MCP_LIST_ERROR_AFTER=N makes tools/list calls after the Nth fail with a
// JSON-RPC error, exercising the refresh-failure path, and MCP_LIST_DELAY_MS
// delays each tools/list call to make concurrent misses overlap.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type request struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

type response struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

// writeMu serializes stdout frames so header+payload pairs never interleave.
var writeMu sync.Mutex

// listCalls counts tools/list requests received so far; it doubles as the
// observable the tests assert on (via the `lists` tool call).
var listCalls atomic.Int64

// listDelayMS delays each tools/list reply (MCP_LIST_DELAY_MS) to widen the
// race window for concurrent-miss single-flight tests.
var listDelayMS int64

// listErrorAfter makes tools/list calls after the Nth fail (0 = no failures).
var listErrorAfter int64

func main() {
	listDelayMS = envInt64("MCP_LIST_DELAY_MS")
	listErrorAfter = envInt64("MCP_LIST_ERROR_AFTER")
	reader := bufio.NewReader(os.Stdin)
	for {
		payload, err := readFrame(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			return
		}
		var req request
		if err := json.Unmarshal(payload, &req); err != nil {
			continue
		}

		switch req.Method {
		case "initialize":
			_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "shift-mcp", "version": "0.1.0"},
			}})
		case "tools/list":
			n := listCalls.Add(1)
			if listErrorAfter > 0 && n > listErrorAfter {
				_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Error: map[string]any{
					"code": -32001, "message": "list failure (after " + strconv.FormatInt(listErrorAfter, 10) + ")",
				}})
				continue
			}
			if listDelayMS > 0 {
				time.Sleep(time.Duration(listDelayMS) * time.Millisecond)
			}
			tools := []map[string]any{earlyTool(), listsTool()}
			if n >= 2 {
				tools = append(tools, lateTool())
			}
			_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": tools}})
		case "tools/call":
			_ = writeResponse(handleToolCall(req))
		default:
			_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Error: map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
}

func earlyTool() map[string]any {
	return map[string]any{"name": "early", "description": "Echoes args as text (present from the first listing)", "inputSchema": map[string]any{"type": "object"}}
}

func lateTool() map[string]any {
	return map[string]any{"name": "late", "description": "Echoes args as text (only present from the second listing onward)", "inputSchema": map[string]any{"type": "object"}}
}

func listsTool() map[string]any {
	return map[string]any{"name": "lists", "description": "Returns the number of tools/list calls served so far as text", "inputSchema": map[string]any{"type": "object"}}
}

// handleToolCall answers tools/call: early/late echo their argument map as
// text; lists returns the tools/list call count, the observable behind the
// cache-hit/single-flight assertions.
func handleToolCall(req request) response {
	reply := func(result map[string]any) response {
		return response{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	name, _ := req.Params["name"].(string)
	args, _ := req.Params["arguments"].(map[string]any)
	switch name {
	case "early", "late":
		return reply(map[string]any{
			"isError": false,
			"content": []map[string]any{{"type": "text", "text": encodeArgs(args)}},
		})
	case "lists":
		return reply(map[string]any{
			"isError": false,
			"content": []map[string]any{{"type": "text", "text": strconv.FormatInt(listCalls.Load(), 10)}},
		})
	default:
		return reply(map[string]any{
			"isError": true,
			"content": []map[string]any{{"type": "text", "text": "unknown tool"}},
		})
	}
}

func envInt64(name string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func encodeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func writeResponse(resp response) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return writeFrame(os.Stdout, payload)
}

func writeFrame(w io.Writer, payload []byte) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" {
				return nil, io.EOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(parts[0]), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil {
				return nil, err
			}
			length = n
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing content-length")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}
