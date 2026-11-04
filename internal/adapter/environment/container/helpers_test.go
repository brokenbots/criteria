package container

// helpers_test.go — covers the cty decoding and path/network helpers used by
// the container handler while building its runtime policy.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestCtyString(t *testing.T) {
	require.Equal(t, "value", ctyString(cty.StringVal("value")))
	require.Equal(t, "", ctyString(cty.NullVal(cty.String)), "null string yields empty")
	unknown := cty.UnknownVal(cty.String)
	require.Equal(t, "", ctyString(unknown), "unknown string yields empty")
	require.Equal(t, "", ctyString(cty.NumberIntVal(42)), "non-string yields empty")
}

func TestCtyStringList(t *testing.T) {
	list := cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})
	require.Equal(t, []string{"a", "b"}, ctyStringList(list))

	// Non-string, unknown and null elements are skipped rather than failing.
	tuple := cty.TupleVal([]cty.Value{
		cty.StringVal("kept"),
		cty.UnknownVal(cty.String),
		cty.NullVal(cty.String),
		cty.NumberIntVal(1),
	})
	require.Equal(t, []string{"kept"}, ctyStringList(tuple))

	require.Nil(t, ctyStringList(cty.NullVal(cty.List(cty.String))))
	require.Nil(t, ctyStringList(cty.UnknownVal(cty.List(cty.String))))
	require.Empty(t, ctyStringList(cty.ListValEmpty(cty.String)), "empty collection yields empty result")
}

func TestStringFromObject(t *testing.T) {
	obj := cty.ObjectVal(map[string]cty.Value{"image": cty.StringVal("alpine")})
	require.Equal(t, "alpine", stringFromObject(obj, "image"))
	require.Equal(t, "", stringFromObject(obj, "missing"), "missing attribute yields empty")
	require.Equal(t, "", stringFromObject(cty.NullVal(obj.Type()), "image"))
	require.Equal(t, "", stringFromObject(cty.UnknownVal(obj.Type()), "image"))
	require.Equal(t, "", stringFromObject(cty.StringVal("not-an-object"), "image"))
}

func TestStringListFromObject(t *testing.T) {
	obj := cty.ObjectVal(map[string]cty.Value{
		"mounts": cty.ListVal([]cty.Value{cty.StringVal("/data"), cty.StringVal("/logs")}),
	})
	require.Equal(t, []string{"/data", "/logs"}, stringListFromObject(obj, "mounts"))
	require.Nil(t, stringListFromObject(obj, "missing"))

	different := cty.ObjectVal(map[string]cty.Value{"other": cty.StringVal("x")})
	require.Nil(t, stringListFromObject(different, "mounts"))

	require.Nil(t, stringListFromObject(cty.NullVal(obj.Type()), "mounts"))
	require.Nil(t, stringListFromObject(cty.UnknownVal(obj.Type()), "mounts"))
	require.Nil(t, stringListFromObject(cty.StringVal("not-an-object"), "mounts"))
}

func TestValidatePath(t *testing.T) {
	require.NoError(t, validatePath("/data/inputs"))
	require.NoError(t, validatePath("/"))

	require.ErrorContains(t, validatePath(""), "empty")
	require.ErrorContains(t, validatePath("relative/path"), "not absolute")
	require.ErrorContains(t, validatePath("/data/../secret"), "traversal")
	require.ErrorContains(t, validatePath("/data/.."), "traversal")
}

func TestMakeNetworkName(t *testing.T) {
	require.Equal(t, "criteria-net-acme-demo-1", makeNetworkName("acme.demo/1"))
	require.Equal(t, "criteria-net-acme-demo", makeNetworkName("acme_demo"))

	// Network names longer than 64 characters are truncated.
	long := makeNetworkName("org/adapter/some-really-long-identifier-that-goes-on-and-on-and-on")
	require.Len(t, long, 64)
	require.True(t, strings.HasPrefix(long, "criteria-net-"))
}
