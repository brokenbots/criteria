package cli

import (
	"os"
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
