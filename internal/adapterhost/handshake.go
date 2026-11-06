package adapterhost

import (
	hplugin "github.com/hashicorp/go-plugin"

	"github.com/brokenbots/criteria/internal/envreg"
)

var (
	// MagicCookieKey gates adapter startup to criteria-owned subprocesses;
	// the name comes from the registry row CRITERIAPlugin (CRITERIA_PLUGIN),
	// so the handshake, the child-environment scrubbing, and the docs table
	// all share one literal.
	MagicCookieKey = envreg.CRITERIAPlugin.Env()
	// MagicCookieValue is a fixed UUIDv4 used for criteria adapter handshakes.
	// These constants must stay in sync with sdk/adapterhost.MagicCookieValue.
	// Drift is caught at integration level by TestHandshakeInfo, which builds a
	// adapter using sdk/adapterhost and connects to it using this package's config.
	MagicCookieValue = "7a1bf31f-c805-4e75-a31c-22195c9fdd4c"
)

var HandshakeConfig = hplugin.HandshakeConfig{
	ProtocolVersion:  2,
	MagicCookieKey:   MagicCookieKey,
	MagicCookieValue: MagicCookieValue,
}
