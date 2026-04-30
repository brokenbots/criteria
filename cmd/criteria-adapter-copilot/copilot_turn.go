// copilot_turn.go — per-Execute turn execution: state machine, event handling, and request config.

package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	copilot "github.com/github/copilot-sdk/go"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	pluginhost "github.com/brokenbots/criteria/sdk/pluginhost"
)

// turnState tracks per-Execute state: final content, turn count, and channels
// for coordinating the event handler goroutine with the wait loop.
type turnState struct {
	finalContent   string
	assistantTurns int
	turnDone       chan struct{}
	errCh          chan error
	maxTurns       int
}

func newTurnState(maxTurns int) *turnState {
	return &turnState{
		turnDone: make(chan struct{}, 1),
		errCh:    make(chan error, 1),
		maxTurns: maxTurns,
	}
}

// sendErr non-blockingly forwards a non-nil error to the error channel.
func (ts *turnState) sendErr(err error) {
	if err == nil {
		return
	}
	select {
	case ts.errCh <- err:
	default:
	}
}

// handleEvent returns a SessionEventHandler that dispatches SDK events to the
// appropriate per-event-type methods on ts.
func (ts *turnState) handleEvent(sink pluginhost.ExecuteEventSender) func(copilot.SessionEvent) {
	return func(event copilot.SessionEvent) {
		switch d := event.Data.(type) {
		case *copilot.AssistantMessageDeltaData:
			ts.handleAssistantDelta(sink, event.Type, d)
		case *copilot.AssistantMessageData:
			ts.handleAssistantMessage(sink, event.Type, d)
		case *copilot.ExternalToolRequestedData:
			ts.sendErr(sink.Send(adapterEvent("tool.invocation", map[string]any{
				"request_id":   d.RequestID,
				"tool_call_id": d.ToolCallID,
				"name":         d.ToolName,
				"arguments":    stringifyAny(d.Arguments),
				"event_type":   string(event.Type),
			})))
		case *copilot.ExternalToolCompletedData:
			ts.sendErr(sink.Send(adapterEvent("tool.result", map[string]any{
				"request_id": d.RequestID,
				"event_type": string(event.Type),
			})))
		case *copilot.SessionIdleData:
			select {
			case ts.turnDone <- struct{}{}:
			default:
			}
		}
	}
}

// handleAssistantDelta forwards a streaming delta event.
func (ts *turnState) handleAssistantDelta(sink pluginhost.ExecuteEventSender, eventType copilot.SessionEventType, d *copilot.AssistantMessageDeltaData) {
	if d.DeltaContent == "" {
		return
	}
	ts.sendErr(sink.Send(logEvent("agent", d.DeltaContent)))
	ts.sendErr(sink.Send(adapterEvent("agent.message", map[string]any{
		"message_id": d.MessageID,
		"delta":      d.DeltaContent,
		"event_type": string(eventType),
	})))
}

// handleAssistantMessage processes a complete assistant turn, forwarding
// content and tool invocations, then enforcing the max_turns limit.
func (ts *turnState) handleAssistantMessage(sink pluginhost.ExecuteEventSender, eventType copilot.SessionEventType, d *copilot.AssistantMessageData) {
	ts.finalContent = d.Content
	ts.sendErr(sink.Send(logEvent("agent", d.Content)))
	ts.sendErr(sink.Send(adapterEvent("agent.message", map[string]any{
		"message_id": d.MessageID,
		"content":    d.Content,
		"event_type": string(eventType),
	})))
	for _, tr := range d.ToolRequests {
		ts.sendErr(sink.Send(adapterEvent("tool.invocation", map[string]any{
			"tool_call_id": tr.ToolCallID,
			"name":         tr.Name,
			"arguments":    stringifyAny(tr.Arguments),
			"event_type":   string(eventType),
		})))
	}
	ts.assistantTurns++
	if ts.maxTurns > 0 && ts.assistantTurns >= ts.maxTurns {
		ts.sendErr(sink.Send(adapterEvent("limit.reached", map[string]any{
			"max_turns": strconv.Itoa(ts.maxTurns),
		})))
		ts.sendErr(errMaxTurnsReached)
	}
}

// awaitOutcome blocks until the session becomes idle, an error occurs, or ctx
// is cancelled. It emits the result event and returns.
func (ts *turnState) awaitOutcome(ctx context.Context, s *sessionState, sink pluginhost.ExecuteEventSender) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-ts.errCh:
			if errors.Is(err, errMaxTurnsReached) {
				return sink.Send(resultEvent("needs_review"))
			}
			return err
		case <-ts.turnDone:
			s.mu.Lock()
			denied := s.permissionDeny
			s.mu.Unlock()
			if denied {
				return sink.Send(resultEvent("needs_review"))
			}
			return sink.Send(resultEvent(parseOutcome(ts.finalContent)))
		}
	}
}

func (p *copilotPlugin) Execute(ctx context.Context, req *pb.ExecuteRequest, sink pluginhost.ExecuteEventSender) error {
	s, prompt, maxTurns, err := p.prepareExecute(req)
	if err != nil {
		return err
	}

	s.execMu.Lock()
	defer s.execMu.Unlock()

	cleanup := s.beginExecution(sink)
	defer cleanup()

	state := newTurnState(maxTurns)
	unsubscribe := s.session.On(state.handleEvent(sink))
	defer unsubscribe()

	restoreEffort, err := applyRequestEffort(ctx, s, s.session, req.GetConfig())
	if err != nil {
		return err
	}
	defer restoreEffort()

	if err := applyRequestModel(ctx, s.session, req.GetConfig()); err != nil {
		return err
	}

	if _, err := s.session.Send(ctx, copilot.MessageOptions{Prompt: prompt}); err != nil {
		return fmt.Errorf("copilot: send prompt: %w", err)
	}

	return state.awaitOutcome(ctx, s, sink)
}

// prepareExecute validates the request and returns the session state, prompt,
// and max_turns limit. Returns an error when any required field is missing or
// the session is unknown.
func (p *copilotPlugin) prepareExecute(req *pb.ExecuteRequest) (s *sessionState, prompt string, maxTurns int, err error) {
	s = p.getSession(req.GetSessionId())
	if s == nil {
		return nil, "", 0, fmt.Errorf("copilot: unknown session %q", req.GetSessionId())
	}

	prompt = strings.TrimSpace(req.GetConfig()["prompt"])
	if prompt == "" {
		return nil, "", 0, fmt.Errorf("copilot: config.prompt is required")
	}

	if raw := strings.TrimSpace(req.GetConfig()["max_turns"]); raw != "" {
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n < 0 {
			return nil, "", 0, fmt.Errorf("copilot: invalid max_turns %q", raw)
		}
		maxTurns = n
	}
	return s, prompt, maxTurns, nil
}

// beginExecution marks the session active and wires up the event sink.
// The returned cleanup function must be deferred by the caller.
func (s *sessionState) beginExecution(sink pluginhost.ExecuteEventSender) func() {
	execDone := make(chan struct{})
	s.mu.Lock()
	s.active = true
	s.activeCh = execDone
	s.sink = sink
	s.permissionDeny = false
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.active = false
		s.sink = nil
		if s.activeCh != nil {
			close(s.activeCh)
			s.activeCh = nil
		}
		s.mu.Unlock()
	}
}

// parseOutcome extracts the RESULT: <outcome> line from the final assistant
// message, lower-casing the value. Returns "needs_review" when absent or empty.
func parseOutcome(content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, resultPrefix) {
			outcome := strings.TrimSpace(trimmed[len(resultPrefix):])
			if outcome == "" {
				return "needs_review"
			}
			return strings.ToLower(outcome)
		}
	}
	return "needs_review"
}
