package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintVersion(t *testing.T) {
	var buf bytes.Buffer
	if err := printVersion(&buf); err != nil {
		t.Fatalf("printVersion error: %v", err)
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Errorf("printVersion produced empty output")
	}
}

func TestNewVersionCmd(t *testing.T) {
	cmd := NewVersionCmd()
	if cmd.Use != "version" {
		t.Errorf("Use = %q, want \"version\"", cmd.Use)
	}
	if cmd.Short == "" {
		t.Error("Short description is empty")
	}
	if cmd.Args == nil {
		t.Error("Args validator is nil")
	}
	if err := cmd.Args(cmd, []string{}); err != nil {
		t.Errorf("Args rejected zero arguments: %v", err)
	}
	if err := cmd.Args(cmd, []string{"extra"}); err == nil {
		t.Error("Args accepted unexpected argument")
	}
}
