package conformance

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// testRunOutputsTypedRoundTrip pins the RunOutputs.Output.typed_value sibling
// (field 4): the native google.protobuf.Value form of an output survives the
// wire verbatim, so typed consumers read typed_value directly instead of
// JSON-parsing the display-only render in value (field 2).
//
// Each output round-trips a distinct kind — string, number, bool, null, list,
// object, and a large integer (bit-exact consumers keep using the display
// render, since google.protobuf.Value stores numbers as doubles) — and the
// persisted Output must be proto.Equal to what was submitted.
func testRunOutputsTypedRoundTrip(t *testing.T, s Subject) { //nolint:funlen // one explicit output per cty kind keeps the pin readable
	baseURL, client, teardown := s.SetUp(t)
	defer teardown()

	const token = "token-run-outputs"
	criteriaID := s.RegisterAgent(t, "criteria-run-outputs", token)
	oClient := criteria.NewServiceClient(client, baseURL)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-run-outputs"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := runResp.Msg.RunId

	listOf := structpb.NewListValue(&structpb.ListValue{Values: []*structpb.Value{
		structpb.NewStringValue("a"), structpb.NewStringValue("b"),
	}})
	objOf := func(k, v string) *structpb.Value {
		return structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{k: structpb.NewStringValue(v)}})
	}

	want := &pb.RunOutputs{
		Outputs: []*pb.RunOutputs_Output{
			{Name: "text", Value: "hello", DeclaredType: "string", TypedValue: structpb.NewStringValue("hello")},
			{Name: "count", Value: "42", DeclaredType: "number", TypedValue: structpb.NewNumberValue(42)},
			{Name: "flag", Value: "true", TypedValue: structpb.NewBoolValue(true)},
			{Name: "empty", Value: "null", TypedValue: structpb.NewNullValue()},
			{Name: "list", Value: `["a","b"]`, TypedValue: listOf},
			{Name: "obj", Value: `{"k":"v"}`, TypedValue: objOf("k", "v")},
			// Numbers are doubles per google.protobuf.Value: this untyped
			// Go constant compiles to the IEEE-754 rounded 9007199254740992.
			// The display render (value) keeps the exact integer text; the
			// spot-check below pins the relationship.
			{Name: "big", Value: "9007199254740993", TypedValue: structpb.NewNumberValue(9007199254740993)},
		},
	}
	env := criteria.NewEnvelope(runID, want)
	env.CorrelationId = "run-outputs-typed"
	if ts := criteria.TypeString(env); ts != "run.outputs" {
		t.Errorf("TypeString(run_outputs)=%q, want run.outputs", ts)
	}
	if criteria.IsTerminal(env) {
		t.Error("run.outputs events must not be terminal run events")
	}

	submitEnvelopes(t, oClient, token, []*pb.Envelope{env})

	events := s.ListRunEvents(t, baseURL, client, token, runID, 0)
	var got *pb.RunOutputs
	for _, ev := range events {
		if ev.GetRunOutputs() != nil && ev.CorrelationId == "run-outputs-typed" {
			got = ev.GetRunOutputs()
			break
		}
	}
	if got == nil {
		t.Fatalf("run.outputs envelope not persisted; run events=%d", len(events))
	}
	if !proto.Equal(want, got) {
		t.Errorf("typed_value round-trip mismatch:\nwant:\n%v\ngot:\n%v", want, got)
	}

	// Spot-check the sibling relationship on the recovered message: every
	// output carries a non-nil typed_value next to its display render.
	for _, out := range got.GetOutputs() {
		if out.GetTypedValue() == nil {
			t.Errorf("output %q: typed_value missing after round-trip", out.GetName())
		}
	}
	// The large-integer kind pins the documented precision relationship: the
	// display render carries the exact integer text while typed_value holds
	// the double it rounds to (9007199254740993 → ...992 per IEEE-754), so
	// bit-exact consumers read value, not typed_value.
	for _, out := range got.GetOutputs() {
		if out.GetName() != "big" {
			continue
		}
		if out.GetValue() != "9007199254740993" {
			t.Errorf("big value = %q, want the exact integer text", out.GetValue())
		}
		if num := out.GetTypedValue().GetNumberValue(); num != 9007199254740992 {
			t.Errorf("big typed_value = %v, want the double-rounded 9007199254740992 (value keeps the exact text)", num)
		}
	}
}
