//go:build copilot

package copilot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/workflow"
)

// Adapter wraps the Copilot CLI SDK as an Overlord adapter.
type Adapter struct {
	mu     sync.Mutex
	client *copilot.Client
}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return Name }

func (a *Adapter) ensureClient(ctx context.Context) (*copilot.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		return a.client, nil
	}
	c := copilot.NewClient(&copilot.ClientOptions{LogLevel: "warn"})
	if err := c.Start(ctx); err != nil {
		return nil, fmt.Errorf("copilot client start: %w", err)
	}
	a.client = c
	return c, nil
}

func (a *Adapter) Execute(ctx context.Context, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	prompt := step.Config["prompt"]
	if strings.TrimSpace(prompt) == "" {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("copilot adapter: config.prompt is required")
	}
	model := step.Config["model"]
	if model == "" {
		model = "claude-sonnet-4.5"
	}
	_ = parseMaxTurns(step.Config["max_turns"]) // reserved for future use

	client, err := a.ensureClient(ctx)
	if err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}

	session, err := client.CreateSession(ctx, &copilot.SessionConfig{
		Model:               model,
		OnPermissionRequest: copilot.PermissionHandler.ApproveAll,
	})
	if err != nil {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("create session: %w", err)
	}
	defer session.Disconnect()

	var (
		finalContent string
		done         = make(chan struct{})
	)
	session.On(func(event copilot.SessionEvent) {
		switch d := event.Data.(type) {
		case *copilot.AssistantMessageData:
			finalContent = d.Content
			sink.Log("agent", []byte(d.Content))
		case *copilot.SessionIdleData:
			close(done)
		}
		sink.Adapter("session.event", map[string]any{"type": fmt.Sprintf("%T", event.Data)})
	})

	if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: prompt}); err != nil {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("send: %w", err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		return adapter.Result{Outcome: "failure"}, ctx.Err()
	}

	return adapter.Result{Outcome: parseOutcome(finalContent)}, nil
}

// parseOutcome extracts an outcome from the agent's final message.
// Convention: a line of the form `RESULT: <name>` (case-insensitive).
func parseOutcome(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= len(resultPrefix) && strings.EqualFold(line[:len(resultPrefix)], resultPrefix) {
			return strings.TrimSpace(line[len(resultPrefix):])
		}
	}
	return "needs_review"
}

func parseMaxTurns(s string) int {
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}
