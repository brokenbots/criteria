package plugin

import hplugin "github.com/hashicorp/go-plugin"

const (
	// MagicCookieKey gates plugin startup to overseer-owned subprocesses.
	MagicCookieKey = "OVERLORD_PLUGIN"
	// MagicCookieValue is a fixed UUIDv4 generated for Overlord plugin handshakes.
	MagicCookieValue = "7a1bf31f-c805-4e75-a31c-22195c9fdd4c"
)

var HandshakeConfig = hplugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   MagicCookieKey,
	MagicCookieValue: MagicCookieValue,
}
