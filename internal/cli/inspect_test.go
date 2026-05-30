package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func TestRenderInspect_EmptyAdapter(t *testing.T) {
	msg := &pb.InspectRunResponse{
		SessionId:   "abc123",
		Adapter:     "",
		CurrentStep: "generate_outline",
	}
	var buf bytes.Buffer
	renderInspect(&buf, msg)
	out := buf.String()
	if strings.Contains(out, "()") {
		t.Fatalf("output should not contain empty parenthetical: %s", out)
	}
	if !strings.Contains(out, "session abc123\n") {
		t.Fatalf("expected session line without adapter: %s", out)
	}
}

func TestRenderInspect_PopulatedFields(t *testing.T) {
	msg := &pb.InspectRunResponse{
		SessionId:          "abc123",
		Adapter:            "claude.assistant",
		CurrentStep:        "generate_outline",
		PendingPermissions: 2,
		LastActivityAt:     timestamppb.New(time.Now().Add(-3 * time.Second)),
		StateJson:          `{"turns_taken":4,"tools_invoked":["read_file","edit_file"],"last_user_message":"hello"}`,
	}
	var buf bytes.Buffer
	renderInspect(&buf, msg)
	out := buf.String()
	want := []string{
		"session abc123 (claude.assistant)",
		"current_step:           generate_outline",
		"pending_permissions:    2",
		"last_activity:",
		"state summary:",
		"turns_taken:",
		"tools_invoked:",
		"last_user_message:",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("expected output to contain %q, got:\n%s", w, out)
		}
	}
}

func TestRenderInspect_MissingLastActivity(t *testing.T) {
	msg := &pb.InspectRunResponse{
		SessionId:   "abc123",
		Adapter:     "test",
		CurrentStep: "step1",
	}
	var buf bytes.Buffer
	renderInspect(&buf, msg)
	out := buf.String()
	if strings.Contains(out, "last_activity") {
		t.Fatalf("output should not contain last_activity when missing: %s", out)
	}
}

func TestRenderStateJSON_Valid(t *testing.T) {
	var buf bytes.Buffer
	renderStateJSON(&buf, `{"key1":"value1","key2":42,"key3":["a","b"]}`)
	out := buf.String()
	if !strings.Contains(out, "key1:") || !strings.Contains(out, "key2:") || !strings.Contains(out, "key3:") {
		t.Fatalf("expected all keys in output: %s", out)
	}
}

func TestRenderStateJSON_Invalid(t *testing.T) {
	var buf bytes.Buffer
	renderStateJSON(&buf, "not-json")
	out := buf.String()
	if !strings.Contains(out, "(raw):") {
		t.Fatalf("expected raw fallback for invalid JSON: %s", out)
	}
}

func TestRenderStateJSON_Empty(t *testing.T) {
	var buf bytes.Buffer
	renderStateJSON(&buf, "")
	out := buf.String()
	if out != "" {
		t.Fatalf("expected empty output for empty state_json: %q", out)
	}
}

func TestRenderStateJSON_NestedObjects(t *testing.T) {
	var buf bytes.Buffer
	renderStateJSON(&buf, `{"outer":{"inner":true}}`)
	out := buf.String()
	if !strings.Contains(out, "outer:") {
		t.Fatalf("expected outer key in output: %s", out)
	}
}

func TestRoundDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{3599 * time.Second, "59m"},
		{3600 * time.Second, "1h"},
		{7200 * time.Second, "2h"},
	}
	for _, tc := range tests {
		got := roundDuration(tc.d)
		if got != tc.want {
			t.Fatalf("roundDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
