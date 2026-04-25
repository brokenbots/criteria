package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
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

func main() {
	reader := bufio.NewReader(os.Stdin)
	for {
		payload, err := readFrame(reader)
		if err != nil {
			if err == io.EOF {
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
				"serverInfo":      map[string]any{"name": "echo-mcp", "version": "0.1.0"},
			}})
		case "tools/list":
			_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"tools": []map[string]any{{
					"name":        "echo",
					"description": "Echoes the argument map as text",
					"inputSchema": map[string]any{"type": "object"},
				}},
			}})
		case "tools/call":
			_ = writeNotification("notifications/progress", map[string]any{"progress": 1, "total": 1})
			name, _ := req.Params["name"].(string)
			args, _ := req.Params["arguments"].(map[string]any)
			if name != "echo" {
				_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
					"isError": true,
					"content": []map[string]any{{"type": "text", "text": "unknown tool"}},
				}})
				continue
			}
			if _, leaked := args["tool"]; leaked {
				_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
					"isError": true,
					"content": []map[string]any{{"type": "text", "text": "reserved key leaked: tool"}},
				}})
				continue
			}
			if _, leaked := args["success_outcome"]; leaked {
				_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
					"isError": true,
					"content": []map[string]any{{"type": "text", "text": "reserved key leaked: success_outcome"}},
				}})
				continue
			}
			text := encodeArgs(args)
			_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"isError": false,
				"content": []map[string]any{
					{"type": "text", "text": text},
					{"type": "resource", "uri": "memory://echo"},
				},
			}})
		case "notifications/cancelled":
			// Best-effort notification from the bridge during shutdown.
		default:
			_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Error: map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
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

func writeNotification(method string, params map[string]any) error {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	return writeFrame(os.Stdout, payload)
}

func writeResponse(resp response) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return writeFrame(os.Stdout, payload)
}

func writeFrame(w io.Writer, payload []byte) error {
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
			if err == io.EOF && line == "" {
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
