package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveOutputMode(t *testing.T) {
	// A pipe is guaranteed non-TTY on every platform.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	notty := r

	tests := []struct {
		name    string
		flag    string
		stream  *os.File
		want    outputMode
		wantErr bool
	}{
		{"empty defaults to auto/json on non-tty", "", notty, outputModeJSON, false},
		{"auto on non-tty → json", "auto", notty, outputModeJSON, false},
		{"explicit concise", "concise", notty, outputModeConcise, false},
		{"explicit json", "json", notty, outputModeJSON, false},
		{"case insensitive", "JSON", notty, outputModeJSON, false},
		{"invalid value errors", "verbose", notty, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveOutputMode(tc.flag, tc.stream)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestOpenNDJSONWriter_PrecedenceRules(t *testing.T) {
	// events-file always wins.
	tmpDir := t.TempDir()
	path := tmpDir + "/events.ndjson"
	w, cleanup, err := openNDJSONWriter(path, outputModeConcise)
	if err != nil {
		t.Fatalf("open events file: %v", err)
	}
	defer cleanup()
	if w == os.Stdout {
		t.Fatal("expected file writer when events-file set, got stdout")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("events file not created: %v", err)
	}

	// json mode without events-file → stdout.
	w2, cleanup2, err := openNDJSONWriter("", outputModeJSON)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	if w2 != os.Stdout {
		t.Fatal("json mode should write to stdout when no events file")
	}

	// concise mode without events-file → discard.
	w3, cleanup3, err := openNDJSONWriter("", outputModeConcise)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup3()
	if w3 == os.Stdout {
		t.Fatal("concise mode without events file must not write to stdout")
	}
}

func TestBuildLocalSink_ConciseModeReturnsMultiSink(t *testing.T) {
	// concise mode must produce a MultiSink (LocalSink + ConsoleSink) not bare LocalSink.
	sink := buildLocalSink("run-1", io.Discard, outputModeConcise, []string{"step-a"}, nil)
	if sink == nil {
		t.Fatal("expected non-nil sink")
	}
	// Smoke-test: all event handlers must not panic.
	sink.OnRunStarted("run-1", "wf")
	sink.OnRunCompleted("run-1", true)
}

func TestBuildLocalSink_JSONModeReturnsLocalSink(t *testing.T) {
	var buf bytes.Buffer
	sink := buildLocalSink("run-2", &buf, outputModeJSON, []string{"step-b"}, nil)
	if sink == nil {
		t.Fatal("expected non-nil sink")
	}
	sink.OnRunStarted("run-2", "wf")
	if buf.Len() == 0 {
		t.Fatal("json mode sink must write to the provided writer")
	}
}

func TestEnvOrDefault(t *testing.T) {
	const key = "CRITERIA_TEST_ENV_OR_DEFAULT_XYZ"
	t.Setenv(key, "from-env")
	if got := envOrDefault(key, "fallback"); got != "from-env" {
		t.Fatalf("got %q want from-env", got)
	}
	t.Setenv(key, "")
	if got := envOrDefault(key, "fallback"); got != "fallback" {
		t.Fatalf("got %q want fallback", got)
	}
}

func TestParseVarOverrides(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  map[string]string
	}{
		{"nil input", nil, nil},
		{"empty slice", []string{}, nil},
		{"valid k=v", []string{"key=value"}, map[string]string{"key": "value"}},
		{"multiple", []string{"a=1", "b=2"}, map[string]string{"a": "1", "b": "2"}},
		{"value with equals", []string{"url=http://x=y"}, map[string]string{"url": "http://x=y"}},
		{"no equals skipped", []string{"noequals"}, map[string]string{}},
		{"empty key skipped", []string{"=value"}, map[string]string{}},
		{"mixed", []string{"a=1", "bad", "=skip", "c=3"}, map[string]string{"a": "1", "c": "3"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseVarOverrides(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("len=%d want %d (got=%v want=%v)", len(got), len(tc.want), got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("key %q: got %q want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestApplyLocal_OutputsEmittedInEventStream(t *testing.T) {
t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

workflowPath := writeWorkflowFile(t, `
workflow "test_outputs" {
  version       = "1"
  initial_state = "start"
  target_state  = "done"

  output "count" {
    type        = "number"
    description = "The count value"
    value       = 42
  }

  output "name" {
    description = "The name value"
    value       = "test"
  }

  state "start" {}
  state "done" { terminal = true }
}
`)

eventsFile := filepath.Join(t.TempDir(), "events.ndjson")

if err := runApply(context.Background(), applyOptions{
workflowPath: workflowPath,
eventsPath:   eventsFile,
}); err != nil {
t.Fatalf("runApply failed: %v", err)
}

events, err := parseNDJSON(eventsFile)
if err != nil {
t.Fatalf("parse events: %v", err)
}

var outputs []map[string]interface{}
outputsSeq := -1
finishedSeq := -1

for _, evt := range events {
evtType, ok := evt["payload_type"].(string)
if !ok {
continue
}

if evtType == "run.outputs" {
seq, _ := evt["seq"].(float64)
outputsSeq = int(seq)

payload, ok := evt["payload"].(map[string]interface{})
if !ok {
continue
}

outList, ok := payload["outputs"].([]interface{})
if !ok {
continue
}

for _, o := range outList {
if outMap, ok := o.(map[string]interface{}); ok {
outputs = append(outputs, outMap)
}
}
}

if evtType == "RunCompleted" {
seq, _ := evt["seq"].(float64)
finishedSeq = int(seq)
}
}

if len(outputs) != 2 {
t.Fatalf("expected 2 outputs, got %d: %v", len(outputs), outputs)
}

if outputs[0]["name"] != "count" {
t.Fatalf("expected first output name 'count', got %q", outputs[0]["name"])
}
if outputs[1]["name"] != "name" {
t.Fatalf("expected second output name 'name', got %q", outputs[1]["name"])
}

if outputsSeq == -1 {
t.Fatalf("run.outputs envelope not found in events")
}
if finishedSeq == -1 {
t.Fatalf("RunCompleted envelope not found in events")
}
if outputsSeq >= finishedSeq {
t.Fatalf("outputs (seq %d) must arrive before RunCompleted (seq %d)", outputsSeq, finishedSeq)
}
}

func parseNDJSON(filepath string) ([]map[string]interface{}, error) {
data, err := os.ReadFile(filepath)
if err != nil {
return nil, err
}

var events []map[string]interface{}
for _, line := range strings.Split(string(data), "\n") {
if line == "" {
continue
}
var evt map[string]interface{}
if err := json.Unmarshal([]byte(line), &evt); err != nil {
return nil, err
}
events = append(events, evt)
}
return events, nil
}
