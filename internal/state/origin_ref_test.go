package state

import (
	"encoding/json"
	"testing"
)

func TestOriginRef_JSONRoundTrip(t *testing.T) {
	orig := OriginRef{Kind: "variable", Name: "api_key"}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != `"variable:api_key"` {
		t.Errorf("expected JSON \"variable:api_key\", got %s", string(data))
	}

	var parsed OriginRef
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Kind != "variable" || parsed.Name != "api_key" {
		t.Errorf("round-trip mismatch: got %+v", parsed)
	}
}

func TestOriginRef_TextRoundTrip(t *testing.T) {
	orig := OriginRef{Kind: "adapter_secret", Name: "shell.default.token"}
	text, err := orig.MarshalText()
	if err != nil {
		t.Fatalf("marshal text: %v", err)
	}
	if string(text) != "adapter_secret:shell.default.token" {
		t.Errorf("expected text adapter_secret:shell.default.token, got %s", string(text))
	}

	var parsed OriginRef
	if err := parsed.UnmarshalText(text); err != nil {
		t.Fatalf("unmarshal text: %v", err)
	}
	if parsed.Kind != "adapter_secret" || parsed.Name != "shell.default.token" {
		t.Errorf("round-trip mismatch: got %+v", parsed)
	}
}

func TestOriginRef_InvalidForm(t *testing.T) {
	var parsed OriginRef
	if err := json.Unmarshal([]byte(`"nocolon"`), &parsed); err == nil {
		t.Error("expected error for missing colon, got nil")
	}
}
