package oci

import "testing"

// TestIsLocalhost pins the plain-HTTP registry allow-list. The loopback
// forms — DNS name, IPv4, IPv6 literal (bare and bracketed, with or without a
// port) — must match so local registries default to PlainHTTP; anything
// non-loopback must stay on TLS. Regression case: bare "::1" was previously
// truncated by a last-colon cut that treated the leading colon as a port
// separator, so IPv6 loopback never matched.
func TestIsLocalhost(t *testing.T) {
	cases := []struct {
		name string
		host string
		want bool
	}{
		{name: "dns name", host: "localhost", want: true},
		{name: "dns name with port", host: "localhost:5000", want: true},
		{name: "dns name uppercase", host: "LOCALHOST:5000", want: true},
		{name: "ipv4 loopback", host: "127.0.0.1", want: true},
		{name: "ipv4 loopback with port", host: "127.0.0.1:5000", want: true},
		{name: "ipv4 loopback other octet", host: "127.0.0.2", want: true},
		{name: "ipv6 loopback bare", host: "::1", want: true},
		{name: "ipv6 loopback bracketed", host: "[::1]", want: true},
		{name: "ipv6 loopback bracketed with port", host: "[::1]:5000", want: true},
		{name: "ipv6 loopback expanded", host: "0:0:0:0:0:0:0:1", want: true},
		{name: "public dns", host: "example.com", want: false},
		{name: "public dns with port", host: "example.com:5000", want: false},
		{name: "public dns uppercase", host: "EXAMPLE.COM", want: false},
		{name: "ipv4 non loopback", host: "10.0.0.1", want: false},
		{name: "ipv4 non loopback with port", host: "10.0.0.1:5000", want: false},
		{name: "ipv4 non loopback adjacent octet", host: "128.0.0.1", want: false},
		{name: "ipv6 non loopback bare", host: "::2", want: false},
		{name: "ipv6 non loopback bracketed", host: "[::2]", want: false},
		{name: "ipv6 non loopback bracketed with port", host: "[::2]:5000", want: false},
		{name: "empty", host: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsLocalhost(tc.host); got != tc.want {
				t.Errorf("IsLocalhost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}
