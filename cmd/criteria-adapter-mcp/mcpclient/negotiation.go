package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Protocol versions negotiated with MCP servers.
const (
	// ProtocolVersion20260728 is the stateless-core revision: the
	// initialize/notifications/initialized handshake is gone; every request
	// carries the protocol version and client capabilities in _meta, and
	// servers advertise their contract via server/discover. The bridge
	// negotiates it probe-first (see Connect).
	ProtocolVersion20260728 = "2026-07-28"
	// ProtocolVersion20250326 is the legacy revision this client spoke
	// historically; the initialize handshake still pins it for the stdio
	// backward-compat window (see Connect).
	ProtocolVersion20250326 = "2025-03-26"
)

// _meta keys of the 2026-07-28 stateless core and result envelopes.
const (
	MetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	MetaClientInfo         = "io.modelcontextprotocol/clientInfo"
	MetaServerInfo         = "io.modelcontextprotocol/serverInfo"
)

// ResultType discriminators required on every 2026-07-28 result. Results
// that omit resultType are earlier-protocol servers and MUST be treated as
// complete.
const (
	ResultTypeComplete      = "complete"
	ResultTypeInputRequired = "input_required"
)

// Spec-reserved JSON-RPC error codes of the 2026-07-28 revision
// (-32020..-32099 are spec-reserved in this revision; -32000..-32019 remain
// implementation-defined, e.g. this client's session-close -32000).
const (
	// ErrorCodeHeaderMismatch: a session header did not match the
	// negotiated contract.
	ErrorCodeHeaderMismatch = -32020
	// ErrorCodeMissingRequiredClientCapability: the request needs a client
	// capability that the request _meta did not declare.
	ErrorCodeMissingRequiredClientCapability = -32021
	// ErrorCodeUnsupportedProtocolVersion: the requested protocol version is
	// unsupported; data carries {"supported": [...], "requested": "..."}.
	ErrorCodeUnsupportedProtocolVersion = -32022
)

// DefaultProbeTimeout bounds one server/discover probe. It is a session-open
// budget, not a call budget: healthy peers of either era answer the probe
// promptly (modern servers with a DiscoverResult, legacy ones with "unknown
// method"), so it burns in full only against silent peers. Keep it well
// under initTimeout so a probe-then-initialize open stays inside the
// pre-probe overall open budget: DefaultProbeTimeout + initTimeout.
const DefaultProbeTimeout = 500 * time.Millisecond

// Negotiation selects how a session is opened with an MCP server. The
// default implements the stdio backward-compat procedure of the 2026-07-28
// spec.
type Negotiation string

const (
	// NegotiationProbeFirst probes server/discover; a DiscoverResult adopts
	// the stateless core, any other probe failure (unknown method, transport
	// error, probe timeout) falls back to the legacy initialize handshake.
	// Empty config resolves to this.
	NegotiationProbeFirst Negotiation = "probe-first"
	// NegotiationInitialize always speaks the legacy 2025-03-26 initialize
	// handshake (compat-window escape hatch).
	NegotiationInitialize Negotiation = "initialize"
	// NegotiationDiscover speaks the stateless core only: a failed probe is
	// terminal, for deployments that know their fleet is modern.
	NegotiationDiscover Negotiation = "discover"
)

// ParseNegotiation converts an adapter config value into a Negotiation. ""
// selects the default (probe-first); matching is case-insensitive; any other
// value is an error so unknown spellings fail the session open instead of
// silently switching protocol dialect (same contract as ParseFraming).
func ParseNegotiation(s string) (Negotiation, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return NegotiationProbeFirst, nil
	case string(NegotiationProbeFirst):
		return NegotiationProbeFirst, nil
	case string(NegotiationInitialize):
		return NegotiationInitialize, nil
	case string(NegotiationDiscover):
		return NegotiationDiscover, nil
	default:
		return "", fmt.Errorf("mcpclient: unknown negotiation %q (want %q, %q, or %q)", s, NegotiationProbeFirst, NegotiationInitialize, NegotiationDiscover)
	}
}

// ServerInfo is a server's self-reported identity from a discover result
// (or a per-request result _meta). Advisory: display and logging, not
// security decisions.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// DiscoverResult is the decoded server/discover result: the server's
// supported protocol versions, capabilities, and identity.
type DiscoverResult struct {
	ResultType        string         `json:"resultType,omitempty"`
	SupportedVersions []string       `json:"supportedVersions,omitempty"`
	Capabilities      map[string]any `json:"capabilities,omitempty"`
	Instructions      string         `json:"instructions,omitempty"`
	// TTLms and CacheScope are the discover result's CacheableResult
	// freshness contract (SEP-2549): TTLms bounds how long the advertised
	// contract may be cached; CacheScope governs shared intermediaries
	// only. Both are required on modern servers but advisory to this client
	// (discovery here is per-session).
	TTLms      int64  `json:"ttlMs,omitempty"`
	CacheScope string `json:"cacheScope,omitempty"`
	// ServerInfo is _meta[MetaServerInfo] on the result.
	ServerInfo ServerInfo `json:"-"`
}

// discoverResultEnvelope decodes one server/discover result, lifting
// _meta[MetaServerInfo] into DiscoverResult.ServerInfo.
type discoverResultEnvelope struct {
	ResultType        string         `json:"resultType"`
	SupportedVersions []string       `json:"supportedVersions"`
	Capabilities      map[string]any `json:"capabilities"`
	Instructions      string         `json:"instructions"`
	TTLms             int64          `json:"ttlMs"`
	CacheScope        string         `json:"cacheScope"`
	Meta              struct {
		ServerInfo ServerInfo `json:"io.modelcontextprotocol/serverInfo"`
	} `json:"_meta"`
}

// Connect opens the session per the negotiated dialect.
//
// probe-first (the default) implements the stdio backward-compat procedure
// of the 2026-07-28 spec: issue a server/discover probe; a DiscoverResult
// marks a modern peer and the session adopts the stateless core iff the
// advertised supported set includes the version this client speaks; the
// spec-reserved UnsupportedProtocolVersion error (-32022) is NOT a legacy
// marker — the peer answered server/discover and rejected the requested
// version, so negotiation fails deterministically (do NOT fall back to the
// initialize handshake); any other probe error or probe timeout marks a
// legacy peer and the session falls back to the initialize handshake (the
// fallback is deliberately not keyed to one error code).
//
// initialize always speaks the legacy handshake. discover speaks the
// stateless core only: a failed probe is terminal.
//
// clientName/clientVersion identify this client in the legacy handshake and
// in the per-request _meta of stateless sessions.
func (c *Client) Connect(ctx context.Context, mode Negotiation, clientName, clientVersion string) error {
	c.clientName = clientName
	c.clientVersion = clientVersion
	if mode == NegotiationInitialize {
		return c.initializeLegacy(ctx)
	}

	probeCtx, cancel := context.WithTimeout(ctx, DefaultProbeTimeout)
	defer cancel()
	result, err := c.Discover(probeCtx, ProtocolVersion20260728)
	if err == nil {
		return c.adoptDiscoverResult(result)
	}
	if IsUnsupportedProtocolVersion(err) {
		supported := UnsupportedVersions(err)
		if len(supported) == 0 {
			supported = []string{"(unspecified)"}
		}
		return fmt.Errorf("mcpclient: no mutually supported protocol version: server advertises [%s] but this client speaks only %s: %w", strings.Join(supported, ", "), ProtocolVersion20260728, err)
	}
	if mode == NegotiationDiscover {
		return fmt.Errorf("mcpclient: server/discover probe failed and legacy fallback is disabled (negotiation %q): %w", mode, err)
	}
	return c.initializeLegacy(ctx)
}

// adoptDiscoverResult moves the session onto the stateless core. A modern
// peer must advertise the one protocol version this client speaks
// statelessly; anything else is a deterministic negotiation failure.
func (c *Client) adoptDiscoverResult(result *DiscoverResult) error {
	speaks := false
	for _, v := range result.SupportedVersions {
		if v == ProtocolVersion20260728 {
			speaks = true
			break
		}
	}
	if !speaks {
		versions := result.SupportedVersions
		if len(versions) == 0 {
			versions = []string{"(none advertised)"}
		}
		return fmt.Errorf("mcpclient: no mutually supported protocol version: server advertises [%s] but this client speaks only %s", strings.Join(versions, ", "), ProtocolVersion20260728)
	}
	c.protocolVersion = ProtocolVersion20260728
	c.stateless = true
	return nil
}

// Discover issues one server/discover request. It carries no session state
// and mutates none: the advertised contract is returned to the caller,
// which decides what to adopt (Connect does).
func (c *Client) Discover(ctx context.Context, preferredVersion string) (*DiscoverResult, error) {
	meta := map[string]any{
		MetaProtocolVersion:    preferredVersion,
		MetaClientCapabilities: map[string]any{},
	}
	if c.clientName != "" {
		meta[MetaClientInfo] = clientInfo{Name: c.clientName, Version: c.clientVersion}
	}
	raw, err := c.request(ctx, "server/discover", map[string]any{"_meta": meta})
	if err != nil {
		return nil, err
	}
	var env discoverResultEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("mcpclient: decode server/discover: %w", err)
	}
	if env.ResultType == ResultTypeInputRequired {
		return nil, fmt.Errorf("mcpclient: server/discover returned resultType %q; discovery is not a multi-round-trip call", env.ResultType)
	}
	return &DiscoverResult{
		ResultType:        env.ResultType,
		SupportedVersions: env.SupportedVersions,
		Capabilities:      env.Capabilities,
		Instructions:      env.Instructions,
		TTLms:             env.TTLms,
		CacheScope:        env.CacheScope,
		ServerInfo:        env.Meta.ServerInfo,
	}, nil
}

// IsUnsupportedProtocolVersion reports whether err is the spec-reserved
// UnsupportedProtocolVersion error (-32022) from a server that answered
// server/discover and rejected the requested protocol version.
func IsUnsupportedProtocolVersion(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	return rpcErr.Code == ErrorCodeUnsupportedProtocolVersion
}

// UnsupportedVersions extracts the server's advertised supported-version
// list from UnsupportedProtocolVersionError data. nil when err carries no
// list.
func UnsupportedVersions(err error) []string {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return nil
	}
	if rpcErr.Code != ErrorCodeUnsupportedProtocolVersion || len(rpcErr.Data) == 0 {
		return nil
	}
	var data struct {
		Supported []string `json:"supported"`
	}
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
		return nil
	}
	return data.Supported
}
