package mcpclient

import (
	"encoding/base64"
	"strings"
)

// Header-value encoding for the 2026-07-28 request headers. Mcp-Name and
// Mcp-Param-* carry values that arrive from tool names, resource uris and
// argument values; a value outside the header-safe set is carried in the
// revision's Base64 sentinel `=?base64?<base64>?=` so arbitrary strings
// stay expressible in a single header value.

// headerValueBase64SentinelPrefix opens the encoded form of a header value
// that is not safe to emit verbatim.
const headerValueBase64SentinelPrefix = "=?base64?"

// headerValueBase64SentinelSuffix closes the encoded form.
const headerValueBase64SentinelSuffix = "?="

// isHeaderValueSafe reports whether a string can be emitted verbatim as a
// request-header value: printable US-ASCII (0x20–0x7E) with no leading or
// trailing whitespace. Tab, DEL, other control bytes and obs-text (bytes
// >= 0x80) are not safe and travel in the Base64 sentinel instead.
func isHeaderValueSafe(s string) bool {
	if s == "" {
		return true
	}
	if s[0] == ' ' || s[0] == '\t' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}

// encodeHeaderValue returns the header-safe wire form of a request-name or
// parameter value: verbatim when the value is header-safe, the Base64
// sentinel otherwise. A header-safe value that would otherwise be
// indistinguishable from an encoded form (it opens with the sentinel
// prefix) is wrapped too, so decode inverts encode without ambiguity.
func encodeHeaderValue(s string) string {
	if isHeaderValueSafe(s) && !strings.HasPrefix(s, headerValueBase64SentinelPrefix) {
		return s
	}
	return headerValueBase64SentinelPrefix + base64.StdEncoding.EncodeToString([]byte(s)) + headerValueBase64SentinelSuffix
}

// decodeHeaderValue inverts encodeHeaderValue, restoring the original
// value from the Base64 sentinel. A verbatim value and a malformed
// sentinel round-trip to themselves so a peer that does not understand the
// sentinel still sees what was sent. Decoding is used by servers (and
// tests) that mirror a header value back against the request body.
func decodeHeaderValue(s string) string {
	if len(s) < len(headerValueBase64SentinelPrefix)+len(headerValueBase64SentinelSuffix) ||
		s[:len(headerValueBase64SentinelPrefix)] != headerValueBase64SentinelPrefix ||
		s[len(s)-len(headerValueBase64SentinelSuffix):] != headerValueBase64SentinelSuffix {
		return s
	}
	raw := s[len(headerValueBase64SentinelPrefix) : len(s)-len(headerValueBase64SentinelSuffix)]
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return s
	}
	return string(decoded)
}
