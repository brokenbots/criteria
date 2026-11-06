package mcpclient

import "testing"

// Header-value encoding: the header-safe set is printable US-ASCII with no
// edge whitespace; everything else travels in the Base64 sentinel, and
// decode inverts encode.
func TestEncodeHeaderValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"tool name", "echo", "echo"},
		{"internal space", "a b", "a b"},
		{"empty", "", ""},
		{"leading space", " echo", "=?base64?IGVjaG8=?="},
		{"trailing space", "echo ", "=?base64?ZWNobyA=?="},
		{"tab", "a\tb", "=?base64?YQli?="},
		{"del", "a\x7fb", "=?base64?YX9i?="},
		{"control byte", "a\x01b", "=?base64?YQFi?="},
		{"obs text", "caf\xc3\xa9", "=?base64?Y2Fmw6k=?="},
		{"newline", "a\nb", "=?base64?YQpi?="},
		{"sentinel-shaped safe value is wrapped", "=?base64?ZWNobw==?=", "=?base64?PT9iYXNlNjQ/WldOb2J3PT0/PQ==?="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeHeaderValue(tc.in)
			if got != tc.want {
				t.Fatalf("encodeHeaderValue(%q) = %q want %q", tc.in, got, tc.want)
			}
			if back := decodeHeaderValue(got); back != tc.in {
				t.Fatalf("decodeHeaderValue(encode(%q)) = %q want the original", tc.in, back)
			}
		})
	}
}

func TestDecodeHeaderValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"verbatim passes through", "echo", "echo"},
		{"sentinel decodes", "=?base64?ZWNobw==?=", "echo"},
		{"malformed sentinel passes through", "=?base64?not!base64?=", "=?base64?not!base64?="},
		{"prefix only passes through", "=?base64?", "=?base64?"},
		{"bare sentinel markers pass through", "?=", "?="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeHeaderValue(tc.in); got != tc.want {
				t.Fatalf("decodeHeaderValue(%q) = %q want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsHeaderValueSafe(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"echo", true},
		{"a b", true},
		{"\x7f", false},
		{"\x1f", false},
		{"caf\xc3\xa9", false},
		{" lead", false},
		{"trail ", false},
		{"tab\t", false},
	}
	for _, tc := range cases {
		if got := isHeaderValueSafe(tc.in); got != tc.want {
			t.Fatalf("isHeaderValueSafe(%q) = %v want %v", tc.in, got, tc.want)
		}
	}
}
