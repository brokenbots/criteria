package workflow_test

// eval_functions_stdlib_test.go — smoke tests for cty/function/stdlib
// functions registered in the workflow evaluation context.

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// TestStdlib_Substr verifies substr(string, offset, length).
func TestStdlib_Substr(t *testing.T) {
	fn := funcFromContext(t, "substr")
	got := callFn(t, fn, cty.StringVal("hello world"), cty.NumberIntVal(0), cty.NumberIntVal(5))
	if got.AsString() != "hello" {
		t.Errorf("substr(hello world, 0, 5) = %q; want hello", got.AsString())
	}
}

// TestStdlib_Replace verifies replace(string, old, new).
func TestStdlib_Replace(t *testing.T) {
	fn := funcFromContext(t, "replace")
	got := callFn(t, fn, cty.StringVal("foo bar"), cty.StringVal("foo"), cty.StringVal("baz"))
	if got.AsString() != "baz bar" {
		t.Errorf("replace(foo bar, foo, baz) = %q; want baz bar", got.AsString())
	}
}

// TestStdlib_Format verifies format(fmt, ...args).
func TestStdlib_Format(t *testing.T) {
	fn := funcFromContext(t, "format")
	got := callFn(t, fn, cty.StringVal("hello %s"), cty.StringVal("world"))
	if got.AsString() != "hello world" {
		t.Errorf("format(hello %%s, world) = %q; want hello world", got.AsString())
	}
}

// TestStdlib_Join verifies join(sep, lists...).
func TestStdlib_Join(t *testing.T) {
	fn := funcFromContext(t, "join")
	list := cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b"), cty.StringVal("c")})
	got := callFn(t, fn, cty.StringVal("-"), list)
	if got.AsString() != "a-b-c" {
		t.Errorf("join(-, [a,b,c]) = %q; want a-b-c", got.AsString())
	}
}

// TestStdlib_Length verifies length(list) and strlen(string).
func TestStdlib_Length(t *testing.T) {
	fn := funcFromContext(t, "length")
	strlenFn := funcFromContext(t, "strlen")

	gotStr := callFn(t, strlenFn, cty.StringVal("hello"))
	n, _ := gotStr.AsBigFloat().Int64()
	if n != 5 {
		t.Errorf("strlen(hello) = %d; want 5", n)
	}

	list := cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})
	gotList := callFn(t, fn, list)
	n, _ = gotList.AsBigFloat().Int64()
	if n != 2 {
		t.Errorf("length([a,b]) = %d; want 2", n)
	}
}

// TestStdlib_LowerUpper verifies lower(string) and upper(string).
func TestStdlib_LowerUpper(t *testing.T) {
	lowerFn := funcFromContext(t, "lower")
	upperFn := funcFromContext(t, "upper")

	got := callFn(t, lowerFn, cty.StringVal("HeLLo"))
	if got.AsString() != "hello" {
		t.Errorf("lower(HeLLo) = %q; want hello", got.AsString())
	}

	got = callFn(t, upperFn, cty.StringVal("HeLLo"))
	if got.AsString() != "HELLO" {
		t.Errorf("upper(HeLLo) = %q; want HELLO", got.AsString())
	}
}

// TestStdlib_Split verifies split(sep, string).
func TestStdlib_Split(t *testing.T) {
	fn := funcFromContext(t, "split")
	got := callFn(t, fn, cty.StringVal(","), cty.StringVal("a,b,c"))
	if got.Type().IsTupleType() {
		if got.LengthInt() != 3 {
			t.Errorf("split(, a,b,c) length = %d; want 3", got.LengthInt())
		}
	} else if got.Type().IsListType() {
		if got.LengthInt() != 3 {
			t.Errorf("split(, a,b,c) length = %d; want 3", got.LengthInt())
		}
	} else {
		t.Errorf("split(, a,b,c) type = %s; want list or tuple", got.Type().FriendlyName())
	}
}

// TestStdlib_Contains verifies contains(list, value).
func TestStdlib_Contains(t *testing.T) {
	fn := funcFromContext(t, "contains")
	list := cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b"), cty.StringVal("c")})
	got := callFn(t, fn, list, cty.StringVal("b"))
	if !got.True() {
		t.Errorf("contains([a,b,c], b) = false; want true")
	}
	got = callFn(t, fn, list, cty.StringVal("z"))
	if got.True() {
		t.Errorf("contains([a,b,c], z) = true; want false")
	}
}

// TestStdlib_Lookup verifies lookup(map, key, default).
func TestStdlib_Lookup(t *testing.T) {
	fn := funcFromContext(t, "lookup")
	m := cty.MapVal(map[string]cty.Value{"a": cty.StringVal("alpha"), "b": cty.StringVal("beta")})
	got := callFn(t, fn, m, cty.StringVal("a"), cty.StringVal("fallback"))
	if got.AsString() != "alpha" {
		t.Errorf("lookup(map, a) = %q; want alpha", got.AsString())
	}
	got = callFn(t, fn, m, cty.StringVal("z"), cty.StringVal("fallback"))
	if got.AsString() != "fallback" {
		t.Errorf("lookup(map, z) = %q; want fallback", got.AsString())
	}
}

// TestStdlib_Merge verifies merge(maps...).
func TestStdlib_Merge(t *testing.T) {
	fn := funcFromContext(t, "merge")
	m1 := cty.MapVal(map[string]cty.Value{"a": cty.StringVal("1")})
	m2 := cty.MapVal(map[string]cty.Value{"b": cty.StringVal("2")})
	got := callFn(t, fn, m1, m2)
	if !got.Type().IsMapType() {
		t.Fatalf("merge type = %s; want map", got.Type().FriendlyName())
	}
	if got.Index(cty.StringVal("a")).AsString() != "1" {
		t.Errorf("merge.a = %q; want 1", got.Index(cty.StringVal("a")).AsString())
	}
	if got.Index(cty.StringVal("b")).AsString() != "2" {
		t.Errorf("merge.b = %q; want 2", got.Index(cty.StringVal("b")).AsString())
	}
}

// TestStdlib_Coalesce verifies coalesce(vals...) — returns first non-null.
func TestStdlib_Coalesce(t *testing.T) {
	fn := funcFromContext(t, "coalesce")
	got := callFn(t, fn, cty.NullVal(cty.String), cty.StringVal("second"), cty.StringVal("third"))
	if got.AsString() != "second" {
		t.Errorf("coalesce(null, second, third) = %q; want second", got.AsString())
	}
}

// TestStdlib_KeysValues verifies keys(map) and values(map).
func TestStdlib_KeysValues(t *testing.T) {
	keysFn := funcFromContext(t, "keys")
	valuesFn := funcFromContext(t, "values")
	m := cty.MapVal(map[string]cty.Value{"a": cty.StringVal("1"), "b": cty.StringVal("2")})

	gotKeys := callFn(t, keysFn, m)
	if gotKeys.LengthInt() != 2 {
		t.Errorf("keys length = %d; want 2", gotKeys.LengthInt())
	}

	gotValues := callFn(t, valuesFn, m)
	if gotValues.LengthInt() != 2 {
		t.Errorf("values length = %d; want 2", gotValues.LengthInt())
	}
}

// TestStdlib_AbsCeilFloor verifies abs, ceil, and floor.
func TestStdlib_AbsCeilFloor(t *testing.T) {
	absFn := funcFromContext(t, "abs")
	ceilFn := funcFromContext(t, "ceil")
	floorFn := funcFromContext(t, "floor")

	got := callFn(t, absFn, cty.NumberIntVal(-5))
	n, _ := got.AsBigFloat().Int64()
	if n != 5 {
		t.Errorf("abs(-5) = %d; want 5", n)
	}

	got = callFn(t, ceilFn, cty.NumberFloatVal(2.1))
	n, _ = got.AsBigFloat().Int64()
	if n != 3 {
		t.Errorf("ceil(2.1) = %d; want 3", n)
	}

	got = callFn(t, floorFn, cty.NumberFloatVal(2.9))
	n, _ = got.AsBigFloat().Int64()
	if n != 2 {
		t.Errorf("floor(2.9) = %d; want 2", n)
	}
}

// TestStdlib_MaxMin verifies max and min.
func TestStdlib_MaxMin(t *testing.T) {
	maxFn := funcFromContext(t, "max")
	minFn := funcFromContext(t, "min")

	got := callFn(t, maxFn, cty.NumberIntVal(3), cty.NumberIntVal(7), cty.NumberIntVal(1))
	n, _ := got.AsBigFloat().Int64()
	if n != 7 {
		t.Errorf("max(3,7,1) = %d; want 7", n)
	}

	got = callFn(t, minFn, cty.NumberIntVal(3), cty.NumberIntVal(7), cty.NumberIntVal(1))
	n, _ = got.AsBigFloat().Int64()
	if n != 1 {
		t.Errorf("min(3,7,1) = %d; want 1", n)
	}
}

// TestStdlib_Reverse verifies reverselist(list).
func TestStdlib_Reverse(t *testing.T) {
	fn := funcFromContext(t, "reverselist")
	list := cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b"), cty.StringVal("c")})
	got := callFn(t, fn, list)
	if got.LengthInt() != 3 {
		t.Fatalf("reverselist length = %d; want 3", got.LengthInt())
	}
	if got.Index(cty.NumberIntVal(0)).AsString() != "c" {
		t.Errorf("reverselist[0] = %q; want c", got.Index(cty.NumberIntVal(0)).AsString())
	}
	if got.Index(cty.NumberIntVal(2)).AsString() != "a" {
		t.Errorf("reverselist[2] = %q; want a", got.Index(cty.NumberIntVal(2)).AsString())
	}
}

// TestStdlib_Sort verifies sort(list).
func TestStdlib_Sort(t *testing.T) {
	fn := funcFromContext(t, "sort")
	list := cty.ListVal([]cty.Value{cty.StringVal("c"), cty.StringVal("a"), cty.StringVal("b")})
	got := callFn(t, fn, list)
	if got.LengthInt() != 3 {
		t.Fatalf("sort length = %d; want 3", got.LengthInt())
	}
	if got.Index(cty.NumberIntVal(0)).AsString() != "a" {
		t.Errorf("sort[0] = %q; want a", got.Index(cty.NumberIntVal(0)).AsString())
	}
	if got.Index(cty.NumberIntVal(1)).AsString() != "b" {
		t.Errorf("sort[1] = %q; want b", got.Index(cty.NumberIntVal(1)).AsString())
	}
	if got.Index(cty.NumberIntVal(2)).AsString() != "c" {
		t.Errorf("sort[2] = %q; want c", got.Index(cty.NumberIntVal(2)).AsString())
	}
}

// TestStdlib_Regex verifies regex(pattern, string).
func TestStdlib_Regex(t *testing.T) {
	fn := funcFromContext(t, "regex")
	got := callFn(t, fn, cty.StringVal(`^[a-z]+`), cty.StringVal("hello123"))
	if got.AsString() != "hello" {
		t.Errorf("regex(^[a-z]+, hello123) = %q; want hello", got.AsString())
	}
}

// TestStdlib_Range verifies range(end) and range(start, end).
func TestStdlib_Range(t *testing.T) {
	fn := funcFromContext(t, "range")
	got := callFn(t, fn, cty.NumberIntVal(3))
	if got.LengthInt() != 3 {
		t.Errorf("range(3) length = %d; want 3", got.LengthInt())
	}
	for i := int64(0); i < 3; i++ {
		n, _ := got.Index(cty.NumberIntVal(i)).AsBigFloat().Int64()
		if n != i {
			t.Errorf("range(3)[%d] = %d; want %d", i, n, i)
		}
	}
}

// TestStdlib_Trim verifies trim, trimspace, trimprefix, trimsuffix.
func TestStdlib_Trim(t *testing.T) {
	trimFn := funcFromContext(t, "trim")
	trimspaceFn := funcFromContext(t, "trimspace")
	trimprefixFn := funcFromContext(t, "trimprefix")
	trimsuffixFn := funcFromContext(t, "trimsuffix")

	got := callFn(t, trimFn, cty.StringVal("  hello  "), cty.StringVal(" "))
	if got.AsString() != "hello" {
		t.Errorf("trim(  hello  , \" \") = %q; want hello", got.AsString())
	}

	got = callFn(t, trimspaceFn, cty.StringVal("\t hello \n"))
	if got.AsString() != "hello" {
		t.Errorf("trimspace(\\t hello \\n) = %q; want hello", got.AsString())
	}

	got = callFn(t, trimprefixFn, cty.StringVal("prefix-text"), cty.StringVal("prefix-"))
	if got.AsString() != "text" {
		t.Errorf("trimprefix(prefix-text, prefix-) = %q; want text", got.AsString())
	}

	got = callFn(t, trimsuffixFn, cty.StringVal("text-suffix"), cty.StringVal("-suffix"))
	if got.AsString() != "text" {
		t.Errorf("trimsuffix(text-suffix, -suffix) = %q; want text", got.AsString())
	}
}

// TestStdlib_Chomp verifies chomp(string) — strips trailing \n and \r\n.
func TestStdlib_Chomp(t *testing.T) {
	fn := funcFromContext(t, "chomp")
	got := callFn(t, fn, cty.StringVal("hello\n\n"))
	if got.AsString() != "hello" {
		t.Errorf("chomp(hello\\n\\n) = %q; want hello", got.AsString())
	}
	got = callFn(t, fn, cty.StringVal("hello\n"))
	if got.AsString() != "hello" {
		t.Errorf("chomp(hello\\n) = %q; want hello", got.AsString())
	}
}

// TestStdlib_Indent verifies indent(spaces, string).
func TestStdlib_Indent(t *testing.T) {
	fn := funcFromContext(t, "indent")
	got := callFn(t, fn, cty.NumberIntVal(2), cty.StringVal("hello\nworld"))
	// stdlib indent does NOT indent the first line.
	want := "hello\n  world"
	if got.AsString() != want {
		t.Errorf("indent(2, hello\\nworld) = %q; want %q", got.AsString(), want)
	}
}

// TestStdlib_Parseint verifies parseint(string, base).
func TestStdlib_Parseint(t *testing.T) {
	fn := funcFromContext(t, "parseint")
	got := callFn(t, fn, cty.StringVal("FF"), cty.NumberIntVal(16))
	n, _ := got.AsBigFloat().Int64()
	if n != 255 {
		t.Errorf("parseint(FF, 16) = %d; want 255", n)
	}
}

// TestStdlib_Pow verifies pow(base, exponent).
func TestStdlib_Pow(t *testing.T) {
	fn := funcFromContext(t, "pow")
	got := callFn(t, fn, cty.NumberIntVal(2), cty.NumberIntVal(8))
	n, _ := got.AsBigFloat().Int64()
	if n != 256 {
		t.Errorf("pow(2, 8) = %d; want 256", n)
	}
}

// TestStdlib_Signum verifies signum(number).
func TestStdlib_Signum(t *testing.T) {
	fn := funcFromContext(t, "signum")
	got := callFn(t, fn, cty.NumberIntVal(-42))
	n, _ := got.AsBigFloat().Int64()
	if n != -1 {
		t.Errorf("signum(-42) = %d; want -1", n)
	}
	got = callFn(t, fn, cty.NumberIntVal(99))
	n, _ = got.AsBigFloat().Int64()
	if n != 1 {
		t.Errorf("signum(99) = %d; want 1", n)
	}
}

// TestStdlib_Flatten verifies flatten(nested_list).
func TestStdlib_Flatten(t *testing.T) {
	fn := funcFromContext(t, "flatten")
	nested := cty.ListVal([]cty.Value{
		cty.ListVal([]cty.Value{cty.NumberIntVal(1), cty.NumberIntVal(2)}),
		cty.ListVal([]cty.Value{cty.NumberIntVal(3)}),
	})
	got := callFn(t, fn, nested)
	if got.LengthInt() != 3 {
		t.Fatalf("flatten length = %d; want 3", got.LengthInt())
	}
	for i := int64(0); i < 3; i++ {
		n, _ := got.Index(cty.NumberIntVal(i)).AsBigFloat().Int64()
		if n != i+1 {
			t.Errorf("flatten[%d] = %d; want %d", i, n, i+1)
		}
	}
}

// TestStdlib_Distinct verifies distinct(list).
func TestStdlib_Distinct(t *testing.T) {
	fn := funcFromContext(t, "distinct")
	list := cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b"), cty.StringVal("a"), cty.StringVal("c")})
	got := callFn(t, fn, list)
	if got.LengthInt() != 3 {
		t.Errorf("distinct length = %d; want 3", got.LengthInt())
	}
}

// TestStdlib_Compact verifies compact(list) — removes empty strings.
func TestStdlib_Compact(t *testing.T) {
	fn := funcFromContext(t, "compact")
	list := cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal(""), cty.StringVal("b")})
	got := callFn(t, fn, list)
	if got.LengthInt() != 2 {
		t.Errorf("compact length = %d; want 2", got.LengthInt())
	}
}

// TestStdlib_Concat verifies concat(lists...).
func TestStdlib_Concat(t *testing.T) {
	fn := funcFromContext(t, "concat")
	l1 := cty.ListVal([]cty.Value{cty.StringVal("a")})
	l2 := cty.ListVal([]cty.Value{cty.StringVal("b")})
	got := callFn(t, fn, l1, l2)
	if got.LengthInt() != 2 {
		t.Errorf("concat length = %d; want 2", got.LengthInt())
	}
}

// TestStdlib_Slice verifies slice(list, start, end).
func TestStdlib_Slice(t *testing.T) {
	fn := funcFromContext(t, "slice")
	list := cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b"), cty.StringVal("c"), cty.StringVal("d")})
	got := callFn(t, fn, list, cty.NumberIntVal(1), cty.NumberIntVal(3))
	if got.LengthInt() != 2 {
		t.Fatalf("slice length = %d; want 2", got.LengthInt())
	}
	if got.Index(cty.NumberIntVal(0)).AsString() != "b" {
		t.Errorf("slice[0] = %q; want b", got.Index(cty.NumberIntVal(0)).AsString())
	}
	if got.Index(cty.NumberIntVal(1)).AsString() != "c" {
		t.Errorf("slice[1] = %q; want c", got.Index(cty.NumberIntVal(1)).AsString())
	}
}

// TestStdlib_Chunklist verifies chunklist(list, size).
func TestStdlib_Chunklist(t *testing.T) {
	fn := funcFromContext(t, "chunklist")
	list := cty.ListVal([]cty.Value{cty.NumberIntVal(1), cty.NumberIntVal(2), cty.NumberIntVal(3), cty.NumberIntVal(4)})
	got := callFn(t, fn, list, cty.NumberIntVal(2))
	if got.LengthInt() != 2 {
		t.Fatalf("chunklist outer length = %d; want 2", got.LengthInt())
	}
	inner := got.Index(cty.NumberIntVal(0))
	if inner.LengthInt() != 2 {
		t.Errorf("chunklist[0] length = %d; want 2", inner.LengthInt())
	}
}

// TestStdlib_RegexReplace verifies regexreplace(string, pattern, replacement).
func TestStdlib_RegexReplace(t *testing.T) {
	fn := funcFromContext(t, "regexreplace")
	got := callFn(t, fn, cty.StringVal("hello planet"), cty.StringVal(`planet`), cty.StringVal("universe"))
	if got.AsString() != "hello universe" {
		t.Errorf("regexreplace(hello planet, planet arg, universe) = %q; want hello universe", got.AsString())
	}
}

// TestStdlib_Startswith verifies startswith(string, prefix).
func TestStdlib_Startswith(t *testing.T) {
	fn := funcFromContext(t, "startswith")
	got := callFn(t, fn, cty.StringVal("hello world"), cty.StringVal("hello"))
	if !got.True() {
		t.Errorf("startswith(hello world, hello) = false; want true")
	}
	got = callFn(t, fn, cty.StringVal("hello world"), cty.StringVal("world"))
	if got.True() {
		t.Errorf("startswith(hello world, world) = true; want false")
	}
}

// TestStdlib_Endswith verifies endswith(string, suffix).
func TestStdlib_Endswith(t *testing.T) {
	fn := funcFromContext(t, "endswith")
	got := callFn(t, fn, cty.StringVal("hello world"), cty.StringVal("world"))
	if !got.True() {
		t.Errorf("endswith(hello world, world) = false; want true")
	}
	got = callFn(t, fn, cty.StringVal("hello world"), cty.StringVal("hello"))
	if got.True() {
		t.Errorf("endswith(hello world, hello) = true; want false")
	}
}

// TestStdlib_Strrev verifies strrev(string) reverses by rune.
func TestStdlib_Strrev(t *testing.T) {
	fn := funcFromContext(t, "strrev")
	got := callFn(t, fn, cty.StringVal("hello"))
	if got.AsString() != "olleh" {
		t.Errorf("strrev(hello) = %q; want olleh", got.AsString())
	}
	got = callFn(t, fn, cty.StringVal("café"))
	if got.AsString() != "éfac" {
		t.Errorf("strrev(café) = %q; want éfac", got.AsString())
	}
}
