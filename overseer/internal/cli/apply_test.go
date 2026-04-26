package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyLocal_NoopPlugin_EmitsExpectedEvents(t *testing.T) {
	pluginBin := buildNoopPluginBinary(t)
	pluginDir := t.TempDir()
	pluginPath := filepath.Join(pluginDir, "overlord-adapter-noop")
	b, err := os.ReadFile(pluginBin)
	if err != nil {
		t.Fatalf("read plugin binary: %v", err)
	}
	if err := os.WriteFile(pluginPath, b, 0o755); err != nil {
		t.Fatalf("write plugin binary: %v", err)
	}

	t.Setenv("OVERLORD_PLUGINS", pluginDir)
	t.Setenv("OVERSEER_STATE_DIR", t.TempDir())

	workflowPath := writeWorkflowFile(t, `
workflow "local_apply_noop" {
  version = "0.1"
  initial_state = "open_agent"
  target_state  = "done"

  agent "demo" {
    adapter = "noop"
    config {
      bootstrap = "true"
    }
  }

  step "open_agent" {
    agent = "demo"
    lifecycle = "open"
    outcome "success" { transition_to = "run_agent" }
    outcome "failure" { transition_to = "failed" }
  }

  step "run_agent" {
    agent = "demo"
    input {
      prompt = "hello"
    }
    outcome "success" { transition_to = "close_agent" }
    outcome "failure" { transition_to = "failed" }
  }

  step "close_agent" {
    agent = "demo"
    lifecycle = "close"
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done" {
    terminal = true
    success  = true
  }
  state "failed" {
    terminal = true
    success  = false
  }
}
`)

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	if err := runApply(context.Background(), applyOptions{workflowPath: workflowPath, eventsPath: eventsFile}); err != nil {
		t.Fatalf("runApply local: %v", err)
	}

	types, err := readPayloadTypes(eventsFile)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}

	if countPayloadType(types, "RunStarted") != 1 {
		t.Fatalf("expected exactly one RunStarted, got %d", countPayloadType(types, "RunStarted"))
	}
	if countPayloadType(types, "RunCompleted") != 1 {
		t.Fatalf("expected exactly one RunCompleted, got %d", countPayloadType(types, "RunCompleted"))
	}

	wantOrder := []string{"StepEntered", "StepOutcome", "StepTransition", "StepEntered", "StepOutcome", "StepTransition", "StepEntered", "StepOutcome", "StepTransition"}
	gotStepEvents := filterPayloadTypes(types, map[string]bool{"StepEntered": true, "StepOutcome": true, "StepTransition": true})
	if strings.Join(gotStepEvents, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("step event order mismatch\nwant: %v\ngot:  %v", wantOrder, gotStepEvents)
	}
}

type ndjsonEvent struct {
	PayloadType string `json:"payload_type"`
}

func readPayloadTypes(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var evt ndjsonEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			return nil, err
		}
		out = append(out, evt.PayloadType)
	}
	return out, nil
}

func countPayloadType(types []string, target string) int {
	n := 0
	for _, typ := range types {
		if typ == target {
			n++
		}
	}
	return n
}

func filterPayloadTypes(types []string, allowed map[string]bool) []string {
	out := make([]string, 0, len(types))
	for _, typ := range types {
		if allowed[typ] {
			out = append(out, typ)
		}
	}
	return out
}
