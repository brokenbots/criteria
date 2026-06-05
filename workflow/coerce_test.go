package workflow

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestDecodeTypedJSON(t *testing.T) {
	t.Run("undeclared infers native number", func(t *testing.T) {
		v, err := DecodeTypedJSON([]byte(`42`), cty.NilType)
		if err != nil || !v.RawEquals(cty.NumberIntVal(42)) {
			t.Fatalf("got %#v err=%v, want number 42", v, err)
		}
	})

	t.Run("undeclared infers native object", func(t *testing.T) {
		v, err := DecodeTypedJSON([]byte(`{"id":7,"name":"x"}`), cty.NilType)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !v.Type().IsObjectType() || !v.GetAttr("id").RawEquals(cty.NumberIntVal(7)) {
			t.Errorf("got %#v, want object with id=7", v)
		}
	})

	t.Run("dynamic declared infers native array", func(t *testing.T) {
		v, err := DecodeTypedJSON([]byte(`[1,2,3]`), cty.DynamicPseudoType)
		if err != nil || v.LengthInt() != 3 {
			t.Fatalf("got %#v err=%v, want 3-tuple", v, err)
		}
	})

	t.Run("concrete declared type decodes strictly", func(t *testing.T) {
		v, err := DecodeTypedJSON([]byte(`"hello"`), cty.String)
		if err != nil || !v.RawEquals(cty.StringVal("hello")) {
			t.Fatalf("got %#v err=%v, want string hello", v, err)
		}
	})

	t.Run("concrete type mismatch errors", func(t *testing.T) {
		if _, err := DecodeTypedJSON([]byte(`"notnum"`), cty.Number); err == nil {
			t.Error("expected error decoding JSON string against number type")
		}
	})
}

// ctyStrs wraps a string-keyed map as the typed cty map that the step-output
// storage helpers now accept (all values cty.String).
func ctyStrs(m map[string]string) map[string]cty.Value {
	out := make(map[string]cty.Value, len(m))
	for k, v := range m {
		out[k] = cty.StringVal(v)
	}
	return out
}

func TestCoerceStringToCty_Scalars(t *testing.T) {
	t.Run("nil type preserves string", func(t *testing.T) {
		v, err := CoerceStringToCty("hello", cty.NilType)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !v.RawEquals(cty.StringVal("hello")) {
			t.Errorf("got %#v, want string \"hello\"", v)
		}
	})

	t.Run("string", func(t *testing.T) {
		v, err := CoerceStringToCty("hi", cty.String)
		if err != nil || !v.RawEquals(cty.StringVal("hi")) {
			t.Fatalf("got %#v err=%v", v, err)
		}
	})

	t.Run("number", func(t *testing.T) {
		v, err := CoerceStringToCty("42", cty.Number)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !v.RawEquals(cty.NumberIntVal(42)) {
			t.Errorf("got %#v, want 42", v)
		}
	})

	t.Run("number invalid", func(t *testing.T) {
		if _, err := CoerceStringToCty("notnum", cty.Number); err == nil {
			t.Error("expected error coercing non-numeric string to number")
		}
	})

	t.Run("bool", func(t *testing.T) {
		for _, s := range []string{"true", "1"} {
			v, err := CoerceStringToCty(s, cty.Bool)
			if err != nil || !v.RawEquals(cty.True) {
				t.Errorf("%q: got %#v err=%v, want true", s, v, err)
			}
		}
		for _, s := range []string{"false", "0"} {
			v, err := CoerceStringToCty(s, cty.Bool)
			if err != nil || !v.RawEquals(cty.False) {
				t.Errorf("%q: got %#v err=%v, want false", s, v, err)
			}
		}
		if _, err := CoerceStringToCty("maybe", cty.Bool); err == nil {
			t.Error("expected error coercing 'maybe' to bool")
		}
	})
}

func TestCoerceStringToCty_Dynamic(t *testing.T) {
	t.Run("json object", func(t *testing.T) {
		v, err := CoerceStringToCty(`{"k":1,"nested":{"id":7}}`, cty.DynamicPseudoType)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !v.Type().IsObjectType() {
			t.Fatalf("expected object type, got %s", v.Type().FriendlyName())
		}
		got := v.GetAttr("nested").GetAttr("id")
		if !got.RawEquals(cty.NumberIntVal(7)) {
			t.Errorf("nested.id = %#v, want 7", got)
		}
	})

	t.Run("json array", func(t *testing.T) {
		v, err := CoerceStringToCty(`[1,2,3]`, cty.DynamicPseudoType)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !v.Type().IsTupleType() {
			t.Fatalf("expected tuple type, got %s", v.Type().FriendlyName())
		}
		if v.LengthInt() != 3 {
			t.Errorf("length = %d, want 3", v.LengthInt())
		}
	})

	t.Run("non-json falls back to string", func(t *testing.T) {
		v, err := CoerceStringToCty("just a string", cty.DynamicPseudoType)
		if err != nil {
			t.Fatalf("expected lenient fallback, got error: %v", err)
		}
		if !v.RawEquals(cty.StringVal("just a string")) {
			t.Errorf("got %#v, want raw string fallback", v)
		}
	})
}

func TestCoerceStringToCty_ConcreteStructured(t *testing.T) {
	t.Run("declared object decodes strictly", func(t *testing.T) {
		ty := cty.Object(map[string]cty.Type{"id": cty.Number, "name": cty.String})
		v, err := CoerceStringToCty(`{"id":3,"name":"x"}`, ty)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !v.GetAttr("id").RawEquals(cty.NumberIntVal(3)) {
			t.Errorf("id = %#v, want 3", v.GetAttr("id"))
		}
	})

	t.Run("declared object rejects malformed json", func(t *testing.T) {
		ty := cty.Object(map[string]cty.Type{"id": cty.Number})
		if _, err := CoerceStringToCty("not json", ty); err == nil {
			t.Error("expected hard error decoding malformed JSON against a concrete type")
		}
	})
}
