package audit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/audit"
)

func TestCanonicalJSON_KeyOrdering(t *testing.T) {
	// Keys sorted in different orders must produce identical output.
	a := map[string]any{"b": 2.0, "a": 1.0}
	b := map[string]any{"a": 1.0, "b": 2.0}

	ca, err := audit.CanonicalJSON(a)
	require.NoError(t, err)
	cb, err := audit.CanonicalJSON(b)
	require.NoError(t, err)

	assert.Equal(t, string(ca), string(cb), "canonical JSON must be the same regardless of Go map order")
	assert.Equal(t, `{"a":1,"b":2}`, string(ca))
}

func TestCanonicalJSON_NestedOrdering(t *testing.T) {
	v := map[string]any{
		"z": map[string]any{"y": "val2", "x": "val1"},
		"a": "top",
	}
	got, err := audit.CanonicalJSON(v)
	require.NoError(t, err)
	assert.Equal(t, `{"a":"top","z":{"x":"val1","y":"val2"}}`, string(got))
}

func TestCanonicalJSON_Array(t *testing.T) {
	v := []any{3.0, 1.0, 2.0}
	got, err := audit.CanonicalJSON(v)
	require.NoError(t, err)
	// Arrays preserve insertion order.
	assert.Equal(t, `[3,1,2]`, string(got))
}

func TestCanonicalJSON_Null(t *testing.T) {
	got, err := audit.CanonicalJSON(nil)
	require.NoError(t, err)
	assert.Equal(t, "null", string(got))
}

func TestCanonicalJSON_Bool(t *testing.T) {
	tr, err := audit.CanonicalJSON(true)
	require.NoError(t, err)
	assert.Equal(t, "true", string(tr))

	fa, err := audit.CanonicalJSON(false)
	require.NoError(t, err)
	assert.Equal(t, "false", string(fa))
}

func TestCanonicalJSON_String(t *testing.T) {
	got, err := audit.CanonicalJSON("hello world")
	require.NoError(t, err)
	assert.Equal(t, `"hello world"`, string(got))
}

func TestArgsDigest_Deterministic(t *testing.T) {
	// Same logical args with different Go map traversal order must produce the
	// same digest — this is the core invariant for PermissionRequest.args_digest.
	a := map[string]any{"b": 2.0, "a": 1.0}
	b := map[string]any{"a": 1.0, "b": 2.0}

	da, err := audit.ArgsDigest(a)
	require.NoError(t, err)
	db, err := audit.ArgsDigest(b)
	require.NoError(t, err)
	assert.Equal(t, da, db)
}

func TestArgsDigest_Format(t *testing.T) {
	digest, err := audit.ArgsDigest(map[string]any{"cmd": "echo hi"})
	require.NoError(t, err)
	assert.Len(t, digest, 64, "SHA-256 hex digest must be 64 hex characters")
}

func TestArgsDigest_DifferentArgs_DifferentDigests(t *testing.T) {
	d1, err := audit.ArgsDigest(map[string]any{"cmd": "echo hello"})
	require.NoError(t, err)
	d2, err := audit.ArgsDigest(map[string]any{"cmd": "echo world"})
	require.NoError(t, err)
	assert.NotEqual(t, d1, d2)
}

func TestCanonicalJSON_SpecExample(t *testing.T) {
	// Workstream spec: canonical_json({"b":2,"a":1}) == canonical_json({"a":1,"b":2})
	a := map[string]any{"b": 2.0, "a": 1.0}
	b := map[string]any{"a": 1.0, "b": 2.0}

	da, err := audit.ArgsDigest(a)
	require.NoError(t, err)
	db, err := audit.ArgsDigest(b)
	require.NoError(t, err)

	assert.Equal(t, da, db, "spec: same content, different order → same digest")
}
