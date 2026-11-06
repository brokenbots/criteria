package mcpclient

import (
	"strings"
	"testing"
)

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
			if back := DecodeHeaderValue(got); back != tc.in {
				t.Fatalf("DecodeHeaderValue(encode(%q)) = %q want the original", tc.in, back)
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
			if got := DecodeHeaderValue(tc.in); got != tc.want {
				t.Fatalf("DecodeHeaderValue(%q) = %q want %q", tc.in, got, tc.want)
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

// StandardHeaders pins the transport-controlled header set: Content-Type
// and the dual-media Accept on every POST, Mcp-Method only when the body
// has a method (it must equal the body method), Mcp-Name only for a
// name-bearing method (Base64 sentinel over the value), and the announced
// protocol version.
func TestStandardHeaders(t *testing.T) {
	names := func(hdrs []HeaderEntry) string {
		out := make([]string, 0, len(hdrs))
		for _, h := range hdrs {
			out = append(out, h.Name)
		}
		return strings.Join(out, ",")
	}
	values := func(hdrs []HeaderEntry, name string) string {
		for _, h := range hdrs {
			if h.Name == name {
				return h.Value
			}
		}
		return "<absent>"
	}

	// A method-bearing POST carries the full set, method header first.
	hdrs := StandardHeaders("tools/call", "echo", ProtocolVersion20260728)
	if got, want := names(hdrs), "Content-Type,Accept,Mcp-Method,Mcp-Name,Mcp-Protocol-Version"; got != want {
		t.Fatalf("header names = %s, want %s", got, want)
	}
	for _, tc := range [][2]string{
		{"Content-Type", "application/json"},
		{"Accept", "application/json, text/event-stream"},
		{"Mcp-Method", "tools/call"},
		{"Mcp-Name", "echo"},
		{"Mcp-Protocol-Version", ProtocolVersion20260728},
	} {
		if got := values(hdrs, tc[0]); got != tc[1] {
			t.Fatalf("%s = %q, want %q", tc[0], got, tc[1])
		}
	}

	// A method-free POST (a wire probe with no single body method) carries
	// no Mcp-Method; a name-free, version-free POST carries neither
	// Mcp-Name nor Mcp-Protocol-Version.
	hdrs = StandardHeaders("", "", "")
	if got, want := names(hdrs), "Content-Type,Accept"; got != want {
		t.Fatalf("method-free header names = %s, want %s", got, want)
	}

	// The name-bearing rule keys on the name argument, not the method.
	hdrs = StandardHeaders("resources/read", "memory://a b", "")
	if got := values(hdrs, "Mcp-Name"); got != "memory://a b" {
		t.Fatalf("Mcp-Name = %q, want the verbatim safe value", got)
	}
	hdrs = StandardHeaders("tools/list", "", "")
	if got := values(hdrs, "Mcp-Name"); got != "<absent>" {
		t.Fatalf("name-free method still carries Mcp-Name: %q", got)
	}
}
