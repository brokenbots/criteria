package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSubworkflowParent writes a parent workflow file and a child
// subworkflow directory next to it, then returns the parent file path.
func writeSubworkflowParent(t *testing.T, parent, child string) string {
	t.Helper()
	dir := t.TempDir()
	parentPath := filepath.Join(dir, "parent.hcl")
	if err := os.WriteFile(parentPath, []byte(parent+"\n"), 0o600); err != nil {
		t.Fatalf("write parent: %v", err)
	}
	childDir := filepath.Join(dir, "child")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("create child dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "main.hcl"), []byte(child+"\n"), 0o644); err != nil {
		t.Fatalf("write child: %v", err)
	}
	return parentPath
}

// TestApplyCRI37_SwitchAfterSubworkflowWrite is a CLI-level regression test
// for the switch stale-data bug. A parent workflow invokes a child subworkflow,
// writes the child's output into a data block, and routes through a switch that
// must see the freshly written value. Without the fix the switch falls through
// to the failure branch; with the fix the run reaches the success terminal.
func TestApplyCRI37_SwitchAfterSubworkflowWrite(t *testing.T) {
	adapterBin := buildNoopAdapterBinary(t)
	adapterDir := t.TempDir()
	pluginPath := filepath.Join(adapterDir, "criteria-adapter-noop")
	b, err := os.ReadFile(adapterBin)
	if err != nil {
		t.Fatalf("read adapter binary: %v", err)
	}
	if err := os.WriteFile(pluginPath, b, 0o755); err != nil {
		t.Fatalf("write adapter binary: %v", err)
	}

	t.Setenv("CRITERIA_ADAPTERS", adapterDir)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	parentPath := writeSubworkflowParent(t, `
workflow {
  name = "cri37_cli_parent"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

data "internal" "verdict" {
  type  = string
  value = "initial"
}

subworkflow "child" {
  source = "./child"
}

step "run" {
  target = subworkflow.child
  outcome "success" {
    next = switch.route
    write {
      target = data.internal.verdict.value
      value  = subworkflow.verdict
    }
  }
  outcome "failure" {
    next = state.fail
  }
}

switch "route" {
  match {
    condition = data.internal.verdict.value == "reproduced"
    next      = state.done
  }
  default {
    next = state.fail
  }
}

state "done" {
  terminal = true
  success  = true
}
state "fail" {
  terminal = true
  success  = false
}
`, `
workflow {
  name = "cri37_cli_child"
  version = "0.1"
  initial_state = "done"
  target_state  = "done"
}

output "verdict" {
  value = "reproduced"
}

state "done" {
  terminal = true
  success  = true
}
`)

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	if err := runApply(context.Background(), applyOptions{workflowPath: parentPath, eventsPath: eventsFile}); err != nil {
		t.Fatalf("runApply: %v", err)
	}

	raw, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	lines := splitNDJSONLines(string(raw))

	var runCompleted *runCompletedEvent
	for _, line := range lines {
		var evt ndjsonEnvelope
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		switch evt.PayloadType {
		case "RunFailed":
			t.Fatalf("run failed: %s", string(evt.Payload))
		case "RunCompleted":
			var rc runCompletedEvent
			if err := json.Unmarshal(evt.Payload, &rc); err != nil {
				t.Fatalf("unmarshal RunCompleted: %v", err)
			}
			runCompleted = &rc
		}
	}

	if runCompleted == nil {
		t.Fatal("no RunCompleted event emitted")
	}
	if runCompleted.FinalState != "done" {
		t.Errorf("finalState = %q, want \"done\"", runCompleted.FinalState)
	}
	if !runCompleted.Success {
		t.Errorf("RunCompleted.success = false, want true; switch routed to failure branch due to stale data")
	}
}

type ndjsonEnvelope struct {
	PayloadType string          `json:"payload_type"`
	Payload     json.RawMessage `json:"payload"`
}

type runCompletedEvent struct {
	FinalState string `json:"finalState"`
	Success    bool   `json:"success"`
}

func splitNDJSONLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
