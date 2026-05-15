package conformance

// fixtures.go — adapter targets, event sinks, and step/config factories used
// by the conformance test suite.

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// adapterTarget wraps a plain adapter.Adapter for use as an executeTarget.
type adapterTarget struct {
	impl adapter.Adapter
}

func (a adapterTarget) Name() string {
	return a.impl.Name()
}

func (a adapterTarget) Execute(ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	return a.impl.Execute(ctx, step, sink)
}

// pluginSessionTarget wraps a adapterhost.Handle + session ID for use as an executeTarget.
type pluginSessionTarget struct {
	handle    adapterhost.Handle
	sessionID string
	name      string
}

func (p pluginSessionTarget) Name() string {
	return p.name
}

func (p pluginSessionTarget) Execute(ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	return p.handle.Execute(ctx, p.sessionID, step, sink)
}

// baseStep returns a minimal StepNode for use in conformance tests.
func baseStep(name, adapterName string, config map[string]string) *workflow.StepNode {
	cfg := make(map[string]string, len(config))
	for k, v := range config {
		cfg[k] = v
	}
	return &workflow.StepNode{
		Name:       name,
		TargetKind: workflow.StepTargetAdapter,
		AdapterRef: adapterName,
		Input:      cfg,
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Name: "success", Next: "done"},
			"failure": {Name: "failure", Next: "done"},
		},
	}
}

// cloneConfig returns a shallow copy of a string map.
func cloneConfig(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// longRunningConfig returns a config that triggers a long-running execution,
// or (nil, false) if the base config has no recognisable long-running key.
func longRunningConfig(base map[string]string) (map[string]string, bool) {
	cfg := cloneConfig(base)
	if _, ok := cfg["command"]; ok {
		cfg["command"] = longRunningCommand()
		return cfg, true
	}
	if _, ok := cfg["delay_ms"]; ok {
		cfg["delay_ms"] = "5000"
		return cfg, true
	}
	return nil, false
}

func longRunningCommand() string {
	if runtime.GOOS == "windows" {
		return "ping 127.0.0.1 -n 6 >NUL"
	}
	return "sleep 5"
}

// noopSink discards all events.
type noopSink struct{}

func (noopSink) Log(string, []byte)  {}
func (noopSink) Adapter(string, any) {}

// recordingSink records all events for assertion by tests.
type recordingSink struct {
	mu            sync.Mutex
	logEvents     int
	adapterEvts   int
	chunks        [][]byte
	adapterData   []string
	adapterEvents []recordedAdapterEvent
}

func (s *recordingSink) Log(_ string, chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logEvents++
	s.chunks = append(s.chunks, append([]byte(nil), chunk...))
}

func (s *recordingSink) Adapter(kind string, data any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adapterEvts++
	s.adapterData = append(s.adapterData, fmt.Sprint(data))
	if eventMap, ok := data.(map[string]any); ok {
		copied := make(map[string]any, len(eventMap))
		for k, v := range eventMap {
			copied[k] = v
		}
		s.adapterEvents = append(s.adapterEvents, recordedAdapterEvent{kind: kind, data: copied})
	}
}

func (s *recordingSink) totalEvents() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logEvents + s.adapterEvts
}

func (s *recordingSink) logChunks() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.chunks))
	for i := range s.chunks {
		out[i] = append([]byte(nil), s.chunks[i]...)
	}
	return out
}

func (s *recordingSink) containsText(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, chunk := range s.chunks {
		if bytes.Contains(chunk, []byte(text)) {
			return true
		}
	}
	for _, payload := range s.adapterData {
		if strings.Contains(payload, text) {
			return true
		}
	}
	return false
}

func (s *recordingSink) firstAdapterEvent(kind string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evt := range s.adapterEvents {
		if evt.kind == kind {
			copied := make(map[string]any, len(evt.data))
			for k, v := range evt.data {
				copied[k] = v
			}
			return copied, true
		}
	}
	return nil, false
}

type recordedAdapterEvent struct {
	kind string
	data map[string]any
}
