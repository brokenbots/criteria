package plugin

import hplugin "github.com/hashicorp/go-plugin"

const (
	// MagicCookieKey gates plugin startup to criteria-owned subprocesses.
	MagicCookieKey = "CRITERIA_PLUGIN"
	// MagicCookieValue is a fixed UUIDv4 used for criteria plugin handshakes.
	// These constants must stay in sync with sdk/pluginhost.MagicCookieValue.
	// Drift is caught at integration level by TestHandshakeInfo, which builds a
	// plugin using sdk/pluginhost and connects to it using this package's config.
	MagicCookieValue = "7a1bf31f-c805-4e75-a31c-22195c9fdd4c"
)

var HandshakeConfig = hplugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   MagicCookieKey,
	MagicCookieValue: MagicCookieValue,
}
