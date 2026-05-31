// Package langserver implements a minimal LSP server for Criteria workflow files.
package langserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/hcl/v2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// Serve starts the language server reading JSON-RPC from stdin and writing to stdout.
func Serve() error {
	s := newServer()
	return s.run()
}

// jsonRPCMessage is a raw JSON-RPC message.
type jsonRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type server struct {
	mu       sync.Mutex
	conn     *stdioConn
	docs     map[uri.URI]document // open documents
	ctx      context.Context
	cancel   context.CancelFunc
	shutdown bool
}

type document struct {
	uri     uri.URI
	version int32
}

type stdioConn struct {
	stdin  *bufio.Reader
	stdout io.Writer
	mu     sync.Mutex
}

func newServer() *server {
	ctx, cancel := context.WithCancel(context.Background())
	return &server{
		docs:   make(map[uri.URI]document),
		ctx:    ctx,
		cancel: cancel,
	}
}

func (s *server) run() error {
	if s.conn == nil {
		s.conn = &stdioConn{
			stdin:  bufio.NewReader(os.Stdin),
			stdout: os.Stdout,
		}
	}

	for {
		select {
		case <-s.ctx.Done():
			return nil
		default:
		}

		msg, err := s.conn.readMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			slog.Error("read message", "error", err)
			continue
		}

		if msg.Method != "" {
			// Process messages serially to avoid races on s.docs and preserve ordering.
			s.handleMessage(msg)
		}
	}
}

func (c *stdioConn) readMessage() (*jsonRPCMessage, error) {
	var contentLength int
	for {
		line, err := c.stdin.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length: ") {
			v, err := strconv.Atoi(strings.TrimPrefix(line, "Content-Length: "))
			if err != nil {
				return nil, err
			}
			contentLength = v
		}
	}
	if contentLength == 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}

	body := make([]byte, contentLength)
	_, err := io.ReadFull(c.stdin, body)
	if err != nil {
		return nil, err
	}

	var msg jsonRPCMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func (c *stdioConn) writeMessage(msg *jsonRPCMessage) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.stdout, "Content-Length: %d\r\n\r\n", len(b))
	_, err = c.stdout.Write(b)
	return err
}

func (c *stdioConn) notify(method string, params any) error {
	paramsBytes, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.writeMessage(&jsonRPCMessage{
		JSONRPC: "2.0",
		Method:  method,
		Params:  paramsBytes,
	})
}

func (c *stdioConn) reply(id json.RawMessage, result any, rpcErr *jsonRPCError) error {
	msg := &jsonRPCMessage{
		JSONRPC: "2.0",
		ID:      id,
		Error:   rpcErr,
	}
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		msg.Result = b
	}
	return c.writeMessage(msg)
}

func (s *server) handleMessage(msg *jsonRPCMessage) {
	var id json.RawMessage
	if len(msg.ID) > 0 && string(msg.ID) != "null" {
		id = msg.ID
	}

	switch msg.Method {
	case "initialize":
		s.handleInitializeRequest(id, msg.Params)
	case "initialized":
		// No-op.
	case "shutdown":
		s.handleShutdownRequest(id)
	case "exit":
		s.cancel()
	case "textDocument/didOpen", "textDocument/didChange", "textDocument/didSave":
		s.handleDocumentNotification(msg.Method, msg.Params)
	case "textDocument/documentSymbol":
		s.handleDocumentSymbolRequest(id, msg.Params)
	case "textDocument/definition":
		s.handleDefinitionRequest(id, msg.Params)
	default:
		if id != nil {
			s.replyError(id, -32601, fmt.Sprintf("method not found: %s", msg.Method))
		}
	}
}

func (s *server) handleInitializeRequest(id, params json.RawMessage) {
	var p protocol.InitializeParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.replyError(id, -32602, "Invalid params")
		return
	}
	result := s.handleInitialize(&p)
	if err := s.conn.reply(id, result, nil); err != nil {
		slog.Error("reply initialize", "error", err)
	}
}

func (s *server) handleShutdownRequest(id json.RawMessage) {
	s.mu.Lock()
	s.shutdown = true
	s.mu.Unlock()
	if err := s.conn.reply(id, nil, nil); err != nil {
		slog.Error("reply shutdown", "error", err)
	}
}

func (s *server) handleDocumentNotification(method string, params json.RawMessage) {
	switch method {
	case "textDocument/didOpen":
		var p protocol.DidOpenTextDocumentParams
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		s.handleDidOpen(&p)
	case "textDocument/didChange":
		var p protocol.DidChangeTextDocumentParams
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		s.handleDidChange(&p)
	case "textDocument/didSave":
		var p protocol.DidSaveTextDocumentParams
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		s.handleDidSave(&p)
	case "textDocument/didClose":
		var p protocol.DidCloseTextDocumentParams
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		s.handleDidClose(&p)
	}
}

func (s *server) handleDocumentSymbolRequest(id, params json.RawMessage) {
	var p protocol.DocumentSymbolParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.replyError(id, -32602, "Invalid params")
		return
	}
	result := s.handleDocumentSymbol(&p)
	if err := s.conn.reply(id, result, nil); err != nil {
		slog.Error("reply documentSymbol", "error", err)
	}
}

func (s *server) handleDefinitionRequest(id, params json.RawMessage) {
	var p protocol.DefinitionParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.replyError(id, -32602, "Invalid params")
		return
	}
	result := s.handleDefinition(&p)
	if err := s.conn.reply(id, result, nil); err != nil {
		slog.Error("reply definition", "error", err)
	}
}

func (s *server) replyError(id json.RawMessage, code int, message string) {
	_ = s.conn.reply(id, nil, &jsonRPCError{Code: code, Message: message})
}

func (s *server) handleInitialize(_ *protocol.InitializeParams) protocol.InitializeResult {
	return protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{
			TextDocumentSync: &protocol.TextDocumentSyncOptions{
				OpenClose: true,
				Change:    protocol.TextDocumentSyncKindNone,
				Save:      &protocol.SaveOptions{IncludeText: false},
			},
			DocumentSymbolProvider: true,
			DefinitionProvider:     true,
		},
		ServerInfo: &protocol.ServerInfo{
			Name:    "criteria-langserver",
			Version: "0.1.0",
		},
	}
}

func (s *server) handleDidOpen(params *protocol.DidOpenTextDocumentParams) {
	s.mu.Lock()
	s.docs[params.TextDocument.URI] = document{
		uri:     params.TextDocument.URI,
		version: params.TextDocument.Version,
	}
	s.mu.Unlock()

	s.publishDiagnostics(params.TextDocument.URI)
}

func (s *server) handleDidChange(params *protocol.DidChangeTextDocumentParams) {
	s.mu.Lock()
	if doc, ok := s.docs[params.TextDocument.URI]; ok {
		doc.version = params.TextDocument.Version
		s.docs[params.TextDocument.URI] = doc
	}
	s.mu.Unlock()
}

func (s *server) handleDidSave(params *protocol.DidSaveTextDocumentParams) {
	s.publishDiagnostics(params.TextDocument.URI)
}

func (s *server) handleDidClose(params *protocol.DidCloseTextDocumentParams) {
	s.mu.Lock()
	delete(s.docs, params.TextDocument.URI)
	s.mu.Unlock()
	// Publish empty diagnostics so the client clears any stale ones.
	_ = s.conn.notify("textDocument/publishDiagnostics", protocol.PublishDiagnosticsParams{
		URI:         params.TextDocument.URI,
		Diagnostics: nil,
	})
}

func (s *server) publishDiagnostics(docURI uri.URI) {
	dir := filepath.Dir(uriToPath(docURI))
	diags := s.compileDiagnostics(dir)

	// Group diagnostics by file path.
	byFile := make(map[string][]protocol.Diagnostic)
	for _, d := range diags {
		byFile[d.file] = append(byFile[d.file], protocol.Diagnostic{
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(d.line - 1), Character: uint32(d.col - 1)},
				End:   protocol.Position{Line: uint32(d.endLine - 1), Character: uint32(d.endCol - 1)},
			},
			Severity: severityToLSP(d.severity),
			Message:  d.message,
			Source:   "criteria",
		})
	}

	// Publish for each file with diagnostics.
	for file, fileDiags := range byFile {
		_ = s.conn.notify("textDocument/publishDiagnostics", protocol.PublishDiagnosticsParams{
			URI:         pathToURI(file),
			Diagnostics: fileDiags,
		})
	}

	// Also publish empty diagnostics for open files that had none this round.
	s.mu.Lock()
	for u := range s.docs {
		path := uriToPath(u)
		if _, ok := byFile[path]; !ok {
			_ = s.conn.notify("textDocument/publishDiagnostics", protocol.PublishDiagnosticsParams{
				URI:         u,
				Diagnostics: nil,
			})
		}
	}
	s.mu.Unlock()
}

func severityToLSP(sev hcl.DiagnosticSeverity) protocol.DiagnosticSeverity {
	switch sev {
	case hcl.DiagError:
		return protocol.DiagnosticSeverityError
	case hcl.DiagWarning:
		return protocol.DiagnosticSeverityWarning
	default:
		return protocol.DiagnosticSeverityInformation
	}
}

func uriToPath(u uri.URI) string {
	return u.Filename()
}

func pathToURI(path string) uri.URI {
	return uri.File(path)
}
