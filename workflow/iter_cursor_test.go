package workflow

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// TestSerializeIterCursor_NilOrEmpty verifies that a nil cursor and a
// zero-value (empty StepName) cursor both serialize to an empty string.
func TestSerializeIterCursor_NilOrEmpty(t *testing.T) {
	s, err := SerializeIterCursor(nil)
	if err != nil {
		t.Fatalf("nil cursor: %v", err)
	}
	if s != "" {
		t.Errorf("nil cursor: want empty string, got %q", s)
	}

	s, err = SerializeIterCursor(&IterCursor{})
	if err != nil {
		t.Fatalf("empty cursor: %v", err)
	}
	if s != "" {
		t.Errorf("empty cursor: want empty string, got %q", s)
	}
}

// TestDeserializeIterCursor_Empty verifies that an empty string returns a
// zero-value IterCursor without error.
func TestDeserializeIterCursor_Empty(t *testing.T) {
	cur, err := DeserializeIterCursor("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cur == nil {
		t.Fatal("expected non-nil cursor")
	}
	if cur.StepName != "" {
		t.Errorf("expected empty StepName, got %q", cur.StepName)
	}
}

// TestSerializeIterCursor_RoundTrip verifies that a cursor serialized via
// SerializeIterCursor is faithfully restored by DeserializeIterCursor.
func TestSerializeIterCursor_RoundTrip(t *testing.T) {
	orig := &IterCursor{
		StepName:   "process",
		Index:      2,
		Total:      5,
		AnyFailed:  true,
		InProgress: false,
		OnFailure:  "continue",
		Key:        cty.StringVal("mykey"),
		Keys:       []cty.Value{cty.StringVal("a"), cty.StringVal("b")},
	}

	data, err := SerializeIterCursor(orig)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if data == "" {
		t.Fatal("expected non-empty serialized cursor")
	}

	restored, err := DeserializeIterCursor(data)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if restored.StepName != orig.StepName {
		t.Errorf("StepName: want %q, got %q", orig.StepName, restored.StepName)
	}
	if restored.Index != orig.Index {
		t.Errorf("Index: want %d, got %d", orig.Index, restored.Index)
	}
	if restored.Total != orig.Total {
		t.Errorf("Total: want %d, got %d", orig.Total, restored.Total)
	}
	if restored.AnyFailed != orig.AnyFailed {
		t.Errorf("AnyFailed: want %v, got %v", orig.AnyFailed, restored.AnyFailed)
	}
	if restored.OnFailure != orig.OnFailure {
		t.Errorf("OnFailure: want %q, got %q", orig.OnFailure, restored.OnFailure)
	}
	if restored.Key == cty.NilVal || restored.Key.AsString() != "mykey" {
		t.Errorf("Key: want 'mykey', got %v", restored.Key)
	}
	if len(restored.Keys) != 2 {
		t.Errorf("Keys: want 2, got %d", len(restored.Keys))
	}
}

// TestSerializeIterCursor_WithPrev verifies that the Prev cty value is
// preserved through a serialize/deserialize round-trip.
func TestSerializeIterCursor_WithPrev(t *testing.T) {
	prevVal := cty.ObjectVal(map[string]cty.Value{
		"result": cty.StringVal("done"),
	})
	cur := &IterCursor{
		StepName: "loop",
		Index:    0,
		Total:    1,
		Prev:     prevVal,
	}
	data, err := SerializeIterCursor(cur)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	restored, err := DeserializeIterCursor(data)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if restored.Prev == cty.NilVal {
		t.Fatal("Prev should be restored but is NilVal")
	}
	if !restored.Prev.Type().HasAttribute("result") {
		t.Fatal("Prev.result attribute missing after round-trip")
	}
	if v := restored.Prev.GetAttr("result").AsString(); v != "done" {
		t.Errorf("Prev.result: want 'done', got %q", v)
	}
}
