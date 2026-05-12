package workflow_test

// eval_functions_encoding_test.go — tests for base64encode, base64decode,
// jsonencode, jsondecode, urlencode, yamlencode, and yamldecode HCL functions.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// ── base64encode ──────────────────────────────────────────────────────────────

func TestBase64Encode_HappyPath(t *testing.T) {
	fn := funcFromContext(t, "base64encode")
	got := callFn(t, fn, cty.StringVal("hello"))
	const want = "aGVsbG8="
	if got.AsString() != want {
		t.Errorf("base64encode(hello) = %q; want %q", got.AsString(), want)
	}
}

func TestBase64Encode_Empty(t *testing.T) {
	fn := funcFromContext(t, "base64encode")
	got := callFn(t, fn, cty.StringVal(""))
	if got.AsString() != "" {
		t.Errorf("base64encode('') = %q; want %q", got.AsString(), "")
	}
}

// ── base64decode ──────────────────────────────────────────────────────────────

func TestBase64Decode_HappyPath(t *testing.T) {
	fn := funcFromContext(t, "base64decode")
	got := callFn(t, fn, cty.StringVal("aGVsbG8="))
	const want = "hello"
	if got.AsString() != want {
		t.Errorf("base64decode(aGVsbG8=) = %q; want %q", got.AsString(), want)
	}
}

func TestBase64Decode_InvalidInput_Error(t *testing.T) {
	fn := funcFromContext(t, "base64decode")
	err := callFnError(t, fn, cty.StringVal("not base64!!"))
	if !strings.Contains(err.Error(), "base64decode()") {
		t.Errorf("error %q should mention base64decode()", err.Error())
	}
}

func TestBase64Encode_RoundTrip_Binary(t *testing.T) {
	encFn := funcFromContext(t, "base64encode")
	decFn := funcFromContext(t, "base64decode")
	// Use binary bytes {0x00, 0xFF, 0x7F} encoded as a Go string.
	original := string([]byte{0x00, 0xFF, 0x7F})
	encoded := callFn(t, encFn, cty.StringVal(original))
	decoded := callFn(t, decFn, encoded)
	if decoded.AsString() != original {
		t.Errorf("base64 round-trip: got %q; want %q", decoded.AsString(), original)
	}
}

// ── jsonencode ────────────────────────────────────────────────────────────────

func TestJsonEncode_String(t *testing.T) {
	fn := funcFromContext(t, "jsonencode")
	got := callFn(t, fn, cty.StringVal("hi"))
	const want = `"hi"`
	if got.AsString() != want {
		t.Errorf("jsonencode(hi) = %q; want %q", got.AsString(), want)
	}
}

func TestJsonEncode_Number(t *testing.T) {
	fn := funcFromContext(t, "jsonencode")
	got := callFn(t, fn, cty.NumberIntVal(42))
	const want = "42"
	if got.AsString() != want {
		t.Errorf("jsonencode(42) = %q; want %q", got.AsString(), want)
	}
}

func TestJsonEncode_Object(t *testing.T) {
	fn := funcFromContext(t, "jsonencode")
	obj := cty.ObjectVal(map[string]cty.Value{
		"a": cty.NumberIntVal(1),
		"b": cty.StringVal("x"),
	})
	got := callFn(t, fn, obj)
	// cty objects don't guarantee key order; compare via unmarshal.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got.AsString()), &decoded); err != nil {
		t.Fatalf("jsonencode result is not valid JSON: %v (got %q)", err, got.AsString())
	}
	if v, ok := decoded["a"]; !ok || v != float64(1) {
		t.Errorf("jsonencode(obj).a = %v; want 1", v)
	}
	if v, ok := decoded["b"]; !ok || v != "x" {
		t.Errorf("jsonencode(obj).b = %v; want x", v)
	}
}

func TestJsonEncode_NullValue(t *testing.T) {
	fn := funcFromContext(t, "jsonencode")
	got := callFn(t, fn, cty.NullVal(cty.String))
	const want = "null"
	if got.AsString() != want {
		t.Errorf("jsonencode(null) = %q; want %q", got.AsString(), want)
	}
}

func TestJsonEncode_List(t *testing.T) {
	fn := funcFromContext(t, "jsonencode")
	list := cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})
	got := callFn(t, fn, list)
	const want = `["a","b"]`
	if got.AsString() != want {
		t.Errorf("jsonencode([a,b]) = %q; want %q", got.AsString(), want)
	}
}

// ── jsondecode ────────────────────────────────────────────────────────────────

func TestJsonDecode_String(t *testing.T) {
	fn := funcFromContext(t, "jsondecode")
	got := callFn(t, fn, cty.StringVal(`"hi"`))
	if got.Type() != cty.String {
		t.Errorf("jsondecode(\"hi\") type = %s; want string", got.Type().FriendlyName())
	}
	if got.AsString() != "hi" {
		t.Errorf("jsondecode(\"hi\") = %q; want %q", got.AsString(), "hi")
	}
}

func TestJsonDecode_Number(t *testing.T) {
	fn := funcFromContext(t, "jsondecode")
	got := callFn(t, fn, cty.StringVal("42"))
	if got.Type() != cty.Number {
		t.Fatalf("jsondecode(42) type = %s; want number", got.Type().FriendlyName())
	}
	n, _ := got.AsBigFloat().Int64()
	if n != 42 {
		t.Errorf("jsondecode(42) value = %d; want 42", n)
	}
}

func TestJsonDecode_Object(t *testing.T) {
	fn := funcFromContext(t, "jsondecode")
	got := callFn(t, fn, cty.StringVal(`{"a":1}`))
	if !got.Type().IsObjectType() {
		t.Fatalf("jsondecode({a:1}) type = %s; want object", got.Type().FriendlyName())
	}
	aVal := got.GetAttr("a")
	if aVal.Type() != cty.Number {
		t.Errorf("jsondecode({a:1}).a type = %s; want number", aVal.Type().FriendlyName())
	}
	n, _ := aVal.AsBigFloat().Int64()
	if n != 1 {
		t.Errorf("jsondecode({a:1}).a value = %d; want 1", n)
	}
}

func TestJsonDecode_InvalidJSON_Error(t *testing.T) {
	fn := funcFromContext(t, "jsondecode")
	err := callFnError(t, fn, cty.StringVal("{not json"))
	if !strings.Contains(err.Error(), "jsondecode()") {
		t.Errorf("error %q should mention jsondecode()", err.Error())
	}
}

func TestJsonRoundTrip_Object_BitExact(t *testing.T) {
	encFn := funcFromContext(t, "jsonencode")
	decFn := funcFromContext(t, "jsondecode")
	original := cty.ObjectVal(map[string]cty.Value{
		"key": cty.StringVal("value"),
		"num": cty.NumberIntVal(99),
	})
	encoded := callFn(t, encFn, original)
	decoded := callFn(t, decFn, encoded)
	if decoded.GetAttr("key").AsString() != "value" {
		t.Errorf("round-trip key = %q; want value", decoded.GetAttr("key").AsString())
	}
	n, _ := decoded.GetAttr("num").AsBigFloat().Int64()
	if n != 99 {
		t.Errorf("round-trip num = %d; want 99", n)
	}
}

// ── urlencode ─────────────────────────────────────────────────────────────────

func TestUrlEncode_Spaces(t *testing.T) {
	fn := funcFromContext(t, "urlencode")
	got := callFn(t, fn, cty.StringVal("a b"))
	const want = "a+b"
	if got.AsString() != want {
		t.Errorf("urlencode(a b) = %q; want %q", got.AsString(), want)
	}
}

func TestUrlEncode_Special(t *testing.T) {
	fn := funcFromContext(t, "urlencode")
	got := callFn(t, fn, cty.StringVal("?&=#"))
	const want = "%3F%26%3D%23"
	if got.AsString() != want {
		t.Errorf("urlencode(?&=#) = %q; want %q", got.AsString(), want)
	}
}

func TestUrlEncode_UTF8(t *testing.T) {
	fn := funcFromContext(t, "urlencode")
	got := callFn(t, fn, cty.StringVal("café"))
	const want = "caf%C3%A9"
	if got.AsString() != want {
		t.Errorf("urlencode(café) = %q; want %q", got.AsString(), want)
	}
}

// ── yamlencode ────────────────────────────────────────────────────────────────

func TestYamlEncode_Object(t *testing.T) {
	fn := funcFromContext(t, "yamlencode")
	obj := cty.ObjectVal(map[string]cty.Value{
		"a": cty.NumberIntVal(1),
		"b": cty.StringVal("x"),
	})
	got := callFn(t, fn, obj)
	// yaml.v3 serialises map keys alphabetically; assert exact field substrings.
	if !strings.Contains(got.AsString(), "a: 1") {
		t.Errorf("yamlencode: expected 'a: 1' in output, got: %q", got.AsString())
	}
	if !strings.Contains(got.AsString(), "b: x") {
		t.Errorf("yamlencode: expected 'b: x' in output, got: %q", got.AsString())
	}
}

// ── yamldecode ────────────────────────────────────────────────────────────────

func TestYamlDecode_Object(t *testing.T) {
	fn := funcFromContext(t, "yamldecode")
	got := callFn(t, fn, cty.StringVal("a: 1\nb: x\n"))
	if !got.Type().IsObjectType() {
		t.Fatalf("yamldecode type = %s; want object", got.Type().FriendlyName())
	}
	if got.GetAttr("b").AsString() != "x" {
		t.Errorf("yamldecode.b = %q; want x", got.GetAttr("b").AsString())
	}
	aVal := got.GetAttr("a")
	if aVal.Type() != cty.Number {
		t.Fatalf("yamldecode.a type = %s; want number", aVal.Type().FriendlyName())
	}
	n, _ := aVal.AsBigFloat().Int64()
	if n != 1 {
		t.Errorf("yamldecode.a = %d; want 1", n)
	}
}

func TestYamlDecode_InvalidYAML_Error(t *testing.T) {
	fn := funcFromContext(t, "yamldecode")
	// This YAML is invalid (mapping value where sequence is expected).
	err := callFnError(t, fn, cty.StringVal("key: [bad\n  - item"))
	if !strings.Contains(err.Error(), "yamldecode()") {
		t.Errorf("error %q should mention yamldecode()", err.Error())
	}
}

func TestYamlRoundTrip_NestedObject(t *testing.T) {
	encFn := funcFromContext(t, "yamlencode")
	decFn := funcFromContext(t, "yamldecode")
	original := cty.ObjectVal(map[string]cty.Value{
		"name":  cty.StringVal("test"),
		"count": cty.NumberIntVal(3),
	})
	encoded := callFn(t, encFn, original)
	decoded := callFn(t, decFn, encoded)
	if !decoded.Type().IsObjectType() {
		t.Fatalf("yaml round-trip: decoded type = %s; want object", decoded.Type().FriendlyName())
	}
	if decoded.GetAttr("name").AsString() != "test" {
		t.Errorf("yaml round-trip name = %q; want test", decoded.GetAttr("name").AsString())
	}
	n, _ := decoded.GetAttr("count").AsBigFloat().Int64()
	if n != 3 {
		t.Errorf("yaml round-trip count = %d; want 3", n)
	}
}
