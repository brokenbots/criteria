package workflow_test

// eval_functions_hash_test.go — tests for sha256, sha1, sha512, and md5
// HCL functions.

import (
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// ── sha256 ────────────────────────────────────────────────────────────────────

func TestSha256_KnownVector(t *testing.T) {
	fn := funcFromContext(t, "sha256")
	got := callFn(t, fn, cty.StringVal("abc"))
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got.AsString() != want {
		t.Errorf("sha256(abc) = %q; want %q", got.AsString(), want)
	}
}

func TestSha256_EmptyString(t *testing.T) {
	fn := funcFromContext(t, "sha256")
	got := callFn(t, fn, cty.StringVal(""))
	const want = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got.AsString() != want {
		t.Errorf("sha256('') = %q; want %q", got.AsString(), want)
	}
}

func TestSha256_LongInput_Deterministic(t *testing.T) {
	fn := funcFromContext(t, "sha256")
	input := cty.StringVal(strings.Repeat("x", 1*1024*1024))
	v1 := callFn(t, fn, input)
	v2 := callFn(t, fn, input)
	if v1.AsString() != v2.AsString() {
		t.Error("sha256: two calls with same 1 MiB input produced different results")
	}
	if len(v1.AsString()) != 64 {
		t.Errorf("sha256: expected 64-char hex digest, got %d chars", len(v1.AsString()))
	}
}

func TestSha256_NonASCII(t *testing.T) {
	fn := funcFromContext(t, "sha256")
	// Input contains UTF-8 multibyte chars; verify well-formed hex output.
	got := callFn(t, fn, cty.StringVal("café"))
	if len(got.AsString()) != 64 {
		t.Errorf("sha256(café): expected 64-char hex, got %d chars: %s", len(got.AsString()), got.AsString())
	}
}

// ── sha1 ──────────────────────────────────────────────────────────────────────

func TestSha1_KnownVector(t *testing.T) {
	fn := funcFromContext(t, "sha1")
	got := callFn(t, fn, cty.StringVal("abc"))
	const want = "a9993e364706816aba3e25717850c26c9cd0d89d"
	if got.AsString() != want {
		t.Errorf("sha1(abc) = %q; want %q", got.AsString(), want)
	}
}

func TestSha1_EmptyString(t *testing.T) {
	fn := funcFromContext(t, "sha1")
	got := callFn(t, fn, cty.StringVal(""))
	const want = "da39a3ee5e6b4b0d3255bfef95601890afd80709"
	if got.AsString() != want {
		t.Errorf("sha1('') = %q; want %q", got.AsString(), want)
	}
}

// ── sha512 ────────────────────────────────────────────────────────────────────

func TestSha512_KnownVector(t *testing.T) {
	fn := funcFromContext(t, "sha512")
	// NIST known vector for "abc":
	const want = "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a" +
		"2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f"
	got := callFn(t, fn, cty.StringVal("abc"))
	if got.AsString() != want {
		t.Errorf("sha512(abc) = %q; want %q", got.AsString(), want)
	}
}

func TestSha512_EmptyString(t *testing.T) {
	fn := funcFromContext(t, "sha512")
	got := callFn(t, fn, cty.StringVal(""))
	if len(got.AsString()) != 128 {
		t.Errorf("sha512(''): expected 128-char hex, got %d chars", len(got.AsString()))
	}
}

// ── md5 ───────────────────────────────────────────────────────────────────────

func TestMd5_KnownVector(t *testing.T) {
	fn := funcFromContext(t, "md5")
	got := callFn(t, fn, cty.StringVal("abc"))
	const want = "900150983cd24fb0d6963f7d28e17f72"
	if got.AsString() != want {
		t.Errorf("md5(abc) = %q; want %q", got.AsString(), want)
	}
}

func TestMd5_EmptyString(t *testing.T) {
	fn := funcFromContext(t, "md5")
	got := callFn(t, fn, cty.StringVal(""))
	const want = "d41d8cd98f00b204e9800998ecf8427e"
	if got.AsString() != want {
		t.Errorf("md5('') = %q; want %q", got.AsString(), want)
	}
}
