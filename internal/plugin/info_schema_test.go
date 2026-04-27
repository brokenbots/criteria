package plugin_test

import (
	"testing"

	pluginpkg "github.com/brokenbots/overlord/overseer/internal/plugin"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
	"github.com/brokenbots/overlord/workflow"
)

// TestInfoResponseSchemaRoundTrip exercises the production AdapterInfoFromProto
// translation used by the loader when a plugin responds to an Info() call.
func TestInfoResponseSchemaRoundTrip(t *testing.T) {
	resp := &pb.InfoResponse{
		Name:    "test-adapter",
		Version: "1.0.0",
		ConfigSchema: &pb.AdapterSchemaProto{Fields: map[string]*pb.ConfigFieldProto{
			"max_turns":     {Type: "number", Doc: "max turns"},
			"system_prompt": {Required: false, Type: "string"},
		}},
		InputSchema: &pb.AdapterSchemaProto{Fields: map[string]*pb.ConfigFieldProto{
			"prompt": {Required: true, Type: "string", Doc: "user prompt"},
		}},
	}

	info := pluginpkg.AdapterInfoFromProto(resp)

	// Verify config schema round-trip.
	maxTurns, ok := info.ConfigSchema["max_turns"]
	if !ok {
		t.Fatal("ConfigSchema missing max_turns")
	}
	if maxTurns.Type != workflow.ConfigFieldNumber {
		t.Errorf("max_turns type: got %v, want ConfigFieldNumber", maxTurns.Type)
	}
	if maxTurns.Doc != "max turns" {
		t.Errorf("max_turns doc: got %q, want %q", maxTurns.Doc, "max turns")
	}
	if maxTurns.Required {
		t.Error("max_turns should not be Required")
	}

	sysPrompt, ok := info.ConfigSchema["system_prompt"]
	if !ok {
		t.Fatal("ConfigSchema missing system_prompt")
	}
	if sysPrompt.Type != workflow.ConfigFieldString {
		t.Errorf("system_prompt type: got %v, want ConfigFieldString", sysPrompt.Type)
	}

	// Verify input schema round-trip.
	prompt, ok := info.InputSchema["prompt"]
	if !ok {
		t.Fatal("InputSchema missing prompt")
	}
	if !prompt.Required {
		t.Error("prompt should be Required=true")
	}
	if prompt.Type != workflow.ConfigFieldString {
		t.Errorf("prompt type: got %v, want ConfigFieldString", prompt.Type)
	}
	if prompt.Doc != "user prompt" {
		t.Errorf("prompt doc: got %q, want %q", prompt.Doc, "user prompt")
	}
}

func TestInfoResponseBoolAndListTypes(t *testing.T) {
	resp := &pb.InfoResponse{
		InputSchema: &pb.AdapterSchemaProto{Fields: map[string]*pb.ConfigFieldProto{
			"flag":  {Type: "bool"},
			"items": {Type: "list_string"},
		}},
	}
	info := pluginpkg.AdapterInfoFromProto(resp)

	flag, ok := info.InputSchema["flag"]
	if !ok {
		t.Fatal("InputSchema missing flag")
	}
	if flag.Type != workflow.ConfigFieldBool {
		t.Errorf("flag type: got %v, want ConfigFieldBool", flag.Type)
	}

	items, ok := info.InputSchema["items"]
	if !ok {
		t.Fatal("InputSchema missing items")
	}
	if items.Type != workflow.ConfigFieldListString {
		t.Errorf("items type: got %v, want ConfigFieldListString", items.Type)
	}
}

func TestLegacyInfoResponseWithoutSchema(t *testing.T) {
	// A legacy plugin that does not populate schema fields should yield a
	// permissive (nil-schema) AdapterInfo so the compiler does not block it.
	resp := &pb.InfoResponse{
		Name:    "legacy",
		Version: "0.0.1",
	}

	info := pluginpkg.AdapterInfoFromProto(resp)

	if info.ConfigSchema != nil {
		t.Errorf("expected nil ConfigSchema for legacy plugin, got %v", info.ConfigSchema)
	}
	if info.InputSchema != nil {
		t.Errorf("expected nil InputSchema for legacy plugin, got %v", info.InputSchema)
	}
}

func TestUnknownFieldTypeDefaultsToString(t *testing.T) {
	resp := &pb.InfoResponse{
		InputSchema: &pb.AdapterSchemaProto{Fields: map[string]*pb.ConfigFieldProto{
			"future_type": {Type: "some_future_type"},
		}},
	}
	info := pluginpkg.AdapterInfoFromProto(resp)
	ft, ok := info.InputSchema["future_type"]
	if !ok {
		t.Fatal("expected field future_type")
	}
	if ft.Type != workflow.ConfigFieldString {
		t.Errorf("unknown type should default to ConfigFieldString, got %v", ft.Type)
	}
}
