// Package remote implements the "remote" environment handler and host-side
// phone-home shim for WS20.
package remote

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

// Handshake deadline bounds. Defaults match the previously hardcoded values
// so existing configurations keep the same behavior.
const (
	DefaultTLSHandshakeDeadline      = 10 * time.Second
	DefaultIdentityHandshakeDeadline = 5 * time.Second
	MinHandshakeDeadline             = 100 * time.Millisecond
	MaxHandshakeDeadline             = 5 * time.Minute
)

// Config is the runtime configuration for a remote environment block.
type Config struct {
	ListenAddress             string
	AcceptToken               string
	PolicyMode                string
	AcceptDigestFrom          string
	ServerCertPath            string
	ServerKeyPath             string
	ClientCAPath              string
	ClientIdentityPattern     string
	Insecure                  bool
	TLSHandshakeDeadline      time.Duration
	IdentityHandshakeDeadline time.Duration
}

// RemoteHandler implements the environment handler interface for the
// "remote" environment type.
type RemoteHandler struct{}

// Type returns "remote".
func (h *RemoteHandler) Type() string { return "remote" }

// SupportedOSes returns nil (all OSes supported for remote).
func (h *RemoteHandler) SupportedOSes() []string { return nil }

// ValidateFields checks accepted attributes at compile time.
// Blocks (mtls, network, filesystem, resources) are tolerated here because
// they are handled separately by ParseConfig at runtime. mtls may also appear
// as a boolean attribute (mtls = true).
func (h *RemoteHandler) ValidateFields(body hcl.Body) hcl.Diagnostics {
	attrs, diags := workflow.BodyJustAttributesToleratingBlocks(body, workflow.HandlerAllowedBlocks(h.Type()))
	for name := range attrs {
		switch name {
		case "variables", "policy_mode", "os", "working_directory",
			"listen_address", "mtls", "accept_token", "accept_digest_from", "insecure",
			"process",
			"tls_handshake_deadline", "identity_handshake_deadline":
			// accepted; process.exec is validated at compile time and enforced at
			// runtime: exact allow-lists are rejected because remote isolation
			// cannot enforce per-path exec restrictions, while the reserved "*"
			// wildcard preserves the default-open behavior explicitly.
		default:
			rng := attrs[name].Range
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("remote environment: unknown attribute %q", name),
				Detail:   "remote environments accept variables, policy_mode, os, working_directory, listen_address, mtls, accept_token, accept_digest_from, insecure, process, tls_handshake_deadline, and identity_handshake_deadline.",
				Subject:  &rng,
			})
		}
	}
	return diags
}

// IsolationKind returns workflow.EnvIsolationRemote.
func (h *RemoteHandler) IsolationKind() workflow.EnvIsolationKind { return workflow.EnvIsolationRemote }

// Prepare parses the HCL body into a typed Config.
func (h *RemoteHandler) Prepare(_ context.Context, body hcl.Body) error {
	// This is a skeleton that the engine calls; real parsing happens
	// when the engine builds the Shim from the EnvironmentNode.
	return nil
}

// ParseConfig extracts a Config from an EnvironmentNode's RawBody.
func ParseConfig(rawBody hcl.Body) (*Config, error) {
	if rawBody == nil {
		return nil, fmt.Errorf("remote environment: no HCL body available")
	}

	cfg := &Config{
		PolicyMode:       "permissive",
		AcceptDigestFrom: "lockfile",
	}

	getAttr, err := buildAttrGetter(rawBody)
	if err != nil {
		return nil, err
	}

	if err := parseTopLevelAttrs(cfg, getAttr); err != nil {
		return nil, err
	}
	if cfg.AcceptDigestFrom != "" && cfg.AcceptDigestFrom != "lockfile" {
		return nil, fmt.Errorf("remote environment: accept_digest_from must be \"lockfile\" (got %q)", cfg.AcceptDigestFrom)
	}
	if err := parseMTLSBlock(cfg, rawBody); err != nil {
		return nil, err
	}
	if err := validateNetworkBlock(rawBody); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validateNetworkBlock checks that a remote environment's advisory network
// block uses only supported forms. Exact allow-lists are rejected because the
// host-side remote handler cannot enforce per-endpoint egress restrictions;
// the reserved wildcard "*" is the explicit opt-in to unrestricted egress.
func validateNetworkBlock(rawBody hcl.Body) error {
	var netBlock hcl.Body
	if raw, ok := rawBody.(*hclsyntax.Body); ok {
		for _, block := range raw.Blocks {
			if block.Type == "network" && len(block.Labels) == 0 {
				netBlock = block.Body
				break
			}
		}
	}
	if netBlock == nil {
		return nil
	}
	attrs, diags := netBlock.JustAttributes()
	if diags.HasErrors() {
		return fmt.Errorf("remote environment: network block: %w", diags)
	}
	attr, ok := attrs["allow"]
	if !ok {
		return nil
	}
	val, diags := attr.Expr.Value(nil)
	if diags.HasErrors() {
		return fmt.Errorf("remote environment: network.allow: %w", diags)
	}
	allow, hasAllow, err := workflow.NetworkAllowFromObject(cty.ObjectVal(map[string]cty.Value{
		"allow": val,
	}))
	if !hasAllow {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remote environment: network.allow: %w", err)
	}
	class, err := workflow.ClassifyNetworkAllow(allow)
	if err != nil {
		return fmt.Errorf("remote environment: network.allow: %w", err)
	}
	if class == workflow.NetworkAllowExact {
		return fmt.Errorf(
			"remote environment cannot enforce an exact network.allow allow-list at the host; " +
				"use network.allow = [\"*\"] to explicitly declare unrestricted egress intent, or omit the block")
	}
	return nil
}

func buildAttrGetter(rawBody hcl.Body) (func(string) (*hcl.Attribute, bool), error) {
	if raw, ok := rawBody.(*hclsyntax.Body); ok {
		return func(name string) (*hcl.Attribute, bool) {
			if a, ok := raw.Attributes[name]; ok {
				return &hcl.Attribute{
					Name:      a.Name,
					Expr:      a.Expr,
					Range:     a.SrcRange,
					NameRange: a.NameRange,
				}, true
			}
			return nil, false
		}, nil
	}
	attrs, diags := rawBody.JustAttributes()
	if diags.HasErrors() {
		return nil, fmt.Errorf("remote environment: parse attributes: %w", diags)
	}
	return func(name string) (*hcl.Attribute, bool) {
		a, ok := attrs[name]
		return a, ok
	}, nil
}

func parseTopLevelAttrs(cfg *Config, getAttr func(string) (*hcl.Attribute, bool)) error {
	for _, mapping := range []struct {
		name   string
		target *string
	}{
		{"listen_address", &cfg.ListenAddress},
		{"accept_token", &cfg.AcceptToken},
		{"policy_mode", &cfg.PolicyMode},
		{"accept_digest_from", &cfg.AcceptDigestFrom},
	} {
		if v, ok := getAttr(mapping.name); ok {
			val, err := attrAsString(v)
			if err != nil {
				return fmt.Errorf("remote environment: %s: %w", mapping.name, err)
			}
			*mapping.target = val
		}
	}

	if v, ok := getAttr("insecure"); ok {
		val, err := attrAsBool(v)
		if err != nil {
			return fmt.Errorf("remote environment: insecure: %w", err)
		}
		cfg.Insecure = val
	}

	for _, mapping := range []struct {
		name   string
		target *time.Duration
		def    time.Duration
	}{
		{"tls_handshake_deadline", &cfg.TLSHandshakeDeadline, DefaultTLSHandshakeDeadline},
		{"identity_handshake_deadline", &cfg.IdentityHandshakeDeadline, DefaultIdentityHandshakeDeadline},
	} {
		if v, ok := getAttr(mapping.name); ok {
			d, err := attrAsDuration(v)
			if err != nil {
				return fmt.Errorf("remote environment: %s: %w", mapping.name, err)
			}
			if err := validateHandshakeDeadline(mapping.name, d); err != nil {
				return err
			}
			*mapping.target = d
		} else {
			*mapping.target = mapping.def
		}
	}

	return nil
}

// validateHandshakeDeadline enforces the documented bounds so that handshake
// timeouts cannot be disabled (zero) or weakened by an excessive wait.
func validateHandshakeDeadline(name string, d time.Duration) error {
	if d < MinHandshakeDeadline {
		return fmt.Errorf("remote environment: %s must be at least %s (got %s)", name, MinHandshakeDeadline, d)
	}
	if d > MaxHandshakeDeadline {
		return fmt.Errorf("remote environment: %s must be at most %s (got %s)", name, MaxHandshakeDeadline, d)
	}
	return nil
}

func parseMTLSBlock(cfg *Config, rawBody hcl.Body) error {
	var mtlsBlock hcl.Body
	if raw, ok := rawBody.(*hclsyntax.Body); ok {
		for _, block := range raw.Blocks {
			if block.Type == "mtls" && len(block.Labels) == 0 {
				mtlsBlock = block.Body
				break
			}
		}
	}
	if mtlsBlock == nil {
		return nil
	}
	mtlsAttrs, mtlsDiags := mtlsBlock.JustAttributes()
	if mtlsDiags.HasErrors() {
		return fmt.Errorf("remote environment: mtls block: %w", mtlsDiags)
	}
	for _, mapping := range []struct {
		name   string
		target *string
	}{
		{"server_cert", &cfg.ServerCertPath},
		{"server_key", &cfg.ServerKeyPath},
		{"client_ca", &cfg.ClientCAPath},
		{"client_identity_pattern", &cfg.ClientIdentityPattern},
	} {
		if v, ok := mtlsAttrs[mapping.name]; ok {
			val, err := attrAsString(v)
			if err != nil {
				return fmt.Errorf("remote environment: mtls.%s: %w", mapping.name, err)
			}
			*mapping.target = val
		}
	}
	return nil
}

// ValidateClientIdentity checks whether the extracted certificate subject
// matches the compiled regex. A nil regexp always matches.
func ValidateClientIdentity(subject string, re *regexp.Regexp) error {
	if re == nil {
		return nil
	}
	if !re.MatchString(subject) {
		return fmt.Errorf("certificate subject %q does not match pattern", subject)
	}
	return nil
}

// attrAsString evaluates a cty attribute as a plain string.
func attrAsString(attr *hcl.Attribute) (string, error) {
	val, diags := attr.Expr.Value(nil)
	if diags.HasErrors() {
		return "", diags
	}
	if val.Type() != cty.String {
		return "", fmt.Errorf("expected string, got %s", val.Type().FriendlyName())
	}
	return val.AsString(), nil
}

// attrAsBool evaluates a cty attribute as a plain bool.
func attrAsBool(attr *hcl.Attribute) (bool, error) {
	val, diags := attr.Expr.Value(nil)
	if diags.HasErrors() {
		return false, diags
	}
	if val.Type() != cty.Bool {
		return false, fmt.Errorf("expected bool, got %s", val.Type().FriendlyName())
	}
	return val.True(), nil
}

// attrAsDuration evaluates a cty attribute as a Go time.Duration string.
func attrAsDuration(attr *hcl.Attribute) (time.Duration, error) {
	val, diags := attr.Expr.Value(nil)
	if diags.HasErrors() {
		return 0, diags
	}
	if val.Type() != cty.String {
		return 0, fmt.Errorf("expected duration string, got %s", val.Type().FriendlyName())
	}
	d, err := time.ParseDuration(val.AsString())
	if err != nil {
		return 0, fmt.Errorf("invalid duration: %w", err)
	}
	return d, nil
}
