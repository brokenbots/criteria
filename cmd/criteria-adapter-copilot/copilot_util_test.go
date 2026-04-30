// copilot_util_test.go — unit tests for event-construction helpers.

package main

import (
	"testing"
)

// TestAdapterEventEncodeErrorFallback verifies that adapterEvent does not
// silently drop an event when structpb.NewStruct fails.  Channels are not
// JSON-serialisable, so passing one triggers the encode-error path.  The
// returned event must still carry the correct kind and expose an
// _encode_error field so the caller can diagnose the problem.
func TestAdapterEventEncodeErrorFallback(t *testing.T) {
	data := map[string]any{"ch": make(chan int)}
	evt := adapterEvent("test.kind", data)

	adap := evt.GetAdapter()
	if adap == nil {
		t.Fatal("expected non-nil adapter event")
	}
	if adap.Kind != "test.kind" {
		t.Fatalf("kind=%q want test.kind", adap.Kind)
	}
	if adap.Data == nil {
		t.Fatal("expected non-nil Data on encode error")
	}
	fields := adap.Data.GetFields()
	errField, ok := fields["_encode_error"]
	if !ok {
		t.Fatalf("expected _encode_error field in fallback struct, got fields: %v", fields)
	}
	if errField.GetStringValue() == "" {
		t.Fatal("_encode_error must contain a non-empty error description")
	}
}
