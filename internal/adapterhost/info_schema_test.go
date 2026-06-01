package adapterhost_test

import (
	"testing"

	adapterhostpkg "github.com/brokenbots/criteria/internal/adapterhost"
	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
)

// TestInfoResponseSchemaRoundTrip exercises the production AdapterInfoFromProto
// translation used by the loader when an adapter responds to an Info() call.
func TestInfoResponseSchemaRoundTrip(t *testing.T) {
	resp := &v2.InfoResponse{
		Name:    "test-adapter",
		Version: "1.0.0",
		ConfigSchema: &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{
			"max_turns":     {Type: "number", Description: "max turns"},
			"system_prompt": {Required: false, Type: "string"},
		}},
		InputSchema: &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{
			"prompt": {Required: true, Type: "string", Description: "user prompt"},
		}},
	}

	info := adapterhostpkg.AdapterInfoFromProto(resp)

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

// TestAdapterInfoFromProto_PropagatesCapabilities verifies that capabilities in
// the InfoResponse are copied into AdapterInfo.Capabilities by AdapterInfoFromProto.
// This covers the production proto→schema translation path used by collectSchemas.
func TestAdapterInfoFromProto_PropagatesCapabilities(t *testing.T) {
	resp := &v2.InfoResponse{
		Name:         "test-adapter",
		Version:      "1.0.0",
		Capabilities: []string{"parallel_safe", "some_other_cap"},
	}

	info := adapterhostpkg.AdapterInfoFromProto(resp)

	if len(info.Capabilities) != 2 {
		t.Fatalf("Capabilities len = %d; want 2", len(info.Capabilities))
	}
	found := map[string]bool{}
	for _, c := range info.Capabilities {
		found[c] = true
	}
	if !found["parallel_safe"] {
		t.Errorf("Capabilities does not contain 'parallel_safe'; got %v", info.Capabilities)
	}
	if !found["some_other_cap"] {
		t.Errorf("Capabilities does not contain 'some_other_cap'; got %v", info.Capabilities)
	}
}

// TestAdapterInfoFromProto_EmptyCapabilities verifies that when InfoResponse has
// no capabilities, AdapterInfo.Capabilities is nil (not an empty non-nil slice)
// so the compiler treats the adapter as having no declared capabilities.
func TestAdapterInfoFromProto_EmptyCapabilities(t *testing.T) {
	resp := &v2.InfoResponse{Name: "bare", Version: "0.1"}
	info := adapterhostpkg.AdapterInfoFromProto(resp)
	if len(info.Capabilities) != 0 {
		t.Errorf("expected empty Capabilities for bare InfoResponse; got %v", info.Capabilities)
	}
}

// TestAdapterInfoFromProto_PropagatesSupportedFeatures verifies that
// supported_features in the InfoResponse are copied into
// AdapterInfo.SupportedFeatures by AdapterInfoFromProto.
func TestAdapterInfoFromProto_PropagatesSupportedFeatures(t *testing.T) {
	resp := &v2.InfoResponse{
		Name:              "test-adapter",
		Version:           "1.0.0",
		SupportedFeatures: []string{"pause", "resume", "inspect"},
	}

	info := adapterhostpkg.AdapterInfoFromProto(resp)

	if len(info.SupportedFeatures) != 3 {
		t.Fatalf("SupportedFeatures len = %d; want 3", len(info.SupportedFeatures))
	}
	found := map[string]bool{}
	for _, f := range info.SupportedFeatures {
		found[f] = true
	}
	for _, want := range []string{"pause", "resume", "inspect"} {
		if !found[want] {
			t.Errorf("SupportedFeatures does not contain %q; got %v", want, info.SupportedFeatures)
		}
	}
}

// TestAdapterInfoFromProto_EmptySupportedFeatures verifies that when
// InfoResponse has no supported_features, AdapterInfo.SupportedFeatures is nil.
func TestAdapterInfoFromProto_EmptySupportedFeatures(t *testing.T) {
	resp := &v2.InfoResponse{Name: "bare", Version: "0.1"}
	info := adapterhostpkg.AdapterInfoFromProto(resp)
	if len(info.SupportedFeatures) != 0 {
		t.Errorf("expected empty SupportedFeatures for bare InfoResponse; got %v", info.SupportedFeatures)
	}
}

func TestInfoResponseBoolAndListTypes(t *testing.T) {
	resp := &v2.InfoResponse{
		InputSchema: &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{
			"flag":  {Type: "bool"},
			"items": {Type: "list_string"},
		}},
	}
	info := adapterhostpkg.AdapterInfoFromProto(resp)

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
	// A legacy adapter that does not populate schema fields should yield a
	// permissive (nil-schema) AdapterInfo so the compiler does not block it.
	resp := &v2.InfoResponse{
		Name:    "legacy",
		Version: "0.0.1",
	}

	info := adapterhostpkg.AdapterInfoFromProto(resp)

	if info.ConfigSchema != nil {
		t.Errorf("expected nil ConfigSchema for legacy adapter, got %v", info.ConfigSchema)
	}
	if info.InputSchema != nil {
		t.Errorf("expected nil InputSchema for legacy adapter, got %v", info.InputSchema)
	}
}

func TestUnknownFieldTypeDefaultsToString(t *testing.T) {
	resp := &v2.InfoResponse{
		InputSchema: &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{
			"future_type": {Type: "some_future_type"},
		}},
	}
	info := adapterhostpkg.AdapterInfoFromProto(resp)
	ft, ok := info.InputSchema["future_type"]
	if !ok {
		t.Fatal("expected field future_type")
	}
	if ft.Type != workflow.ConfigFieldString {
		t.Errorf("unknown type should default to ConfigFieldString, got %v", ft.Type)
	}
}
