package pluginhost

import hplugin "github.com/hashicorp/go-plugin"

const (
	// MagicCookieKey is the environment variable Criteria sets before launching
	// a plugin subprocess. Plugins that see any other value should exit
	// immediately.
	MagicCookieKey = "CRITERIA_PLUGIN"
	// MagicCookieValue is the fixed token that gates plugin startup to
	// Criteria-owned subprocesses.
	MagicCookieValue = "7a1bf31f-c805-4e75-a31c-22195c9fdd4c"
)

// HandshakeConfig is the go-plugin handshake shared between every Criteria host
// and every adapter plugin process. A mismatch causes the plugin to exit with a
// clear error instead of attempting a broken RPC session.
var HandshakeConfig = hplugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   MagicCookieKey,
	MagicCookieValue: MagicCookieValue,
}
