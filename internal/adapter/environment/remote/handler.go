// Package remote implements the "remote" environment handler and host-side
// phone-home shim for WS20.
package remote

import (
	"context"
	"fmt"
	"regexp"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

// Config is the runtime configuration for a remote environment block.
type Config struct {
	ListenAddress         string
	AcceptToken           string
	PolicyMode            string
	AcceptDigestFrom      string
	ServerCertPath        string
	ServerKeyPath         string
	ClientCAPath          string
	ClientIdentityPattern string
}

// RemoteHandler implements the environment handler interface for the
// "remote" environment type.
type RemoteHandler struct{}

// Type returns "remote".
func (h *RemoteHandler) Type() string { return "remote" }

// SupportedOSes returns nil (all OSes supported for remote).
func (h *RemoteHandler) SupportedOSes() []string { return nil }

// ValidateFields checks accepted attributes at compile time.
// Blocks (mtls, network, filesystem, resources) are not visible to
// JustAttributes and are validated at runtime in Prepare.
func (h *RemoteHandler) ValidateFields(body hcl.Body) hcl.Diagnostics {
	attrs, diags := body.JustAttributes()
	for name := range attrs {
		switch name {
		case "variables", "policy_mode", "os",
			"listen_address", "accept_token", "accept_digest_from":
			// accepted
		default:
			rng := attrs[name].Range
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("remote environment: unknown attribute %q", name),
				Detail:   "remote environments accept variables, policy_mode, os, listen_address, accept_token, and accept_digest_from.",
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

	// Extract attributes, ignoring blocks (mtls, network, etc.).
	var getAttr func(name string) (*hcl.Attribute, bool)
	if raw, ok := rawBody.(*hclsyntax.Body); ok {
		getAttr = func(name string) (*hcl.Attribute, bool) {
			if a, ok := raw.Attributes[name]; ok {
				return &hcl.Attribute{
					Name:      a.Name,
					Expr:      a.Expr,
					Range:     a.SrcRange,
					NameRange: a.NameRange,
				}, true
			}
			return nil, false
		}
	} else {
		attrs, diags := rawBody.JustAttributes()
		if diags.HasErrors() {
			return nil, fmt.Errorf("remote environment: parse attributes: %w", diags)
		}
		getAttr = func(name string) (*hcl.Attribute, bool) {
			a, ok := attrs[name]
			return a, ok
		}
	}

	if v, ok := getAttr("listen_address"); ok {
		val, err := attrAsString(v)
		if err != nil {
			return nil, fmt.Errorf("remote environment: listen_address: %w", err)
		}
		cfg.ListenAddress = val
	}
	if v, ok := getAttr("accept_token"); ok {
		val, err := attrAsString(v)
		if err != nil {
			return nil, fmt.Errorf("remote environment: accept_token: %w", err)
		}
		cfg.AcceptToken = val
	}
	if v, ok := getAttr("policy_mode"); ok {
		val, err := attrAsString(v)
		if err != nil {
			return nil, fmt.Errorf("remote environment: policy_mode: %w", err)
		}
		cfg.PolicyMode = val
	}
	if v, ok := getAttr("accept_digest_from"); ok {
		val, err := attrAsString(v)
		if err != nil {
			return nil, fmt.Errorf("remote environment: accept_digest_from: %w", err)
		}
		cfg.AcceptDigestFrom = val
	}

	// Parse the mtls { ... } block
	var mtlsBlock hcl.Body
	if raw, ok := rawBody.(*hclsyntax.Body); ok {
		for _, block := range raw.Blocks {
			if block.Type == "mtls" && len(block.Labels) == 0 {
				mtlsBlock = block.Body
				break
			}
		}
	}
	if mtlsBlock != nil {
		mtlsAttrs, mtlsDiags := mtlsBlock.JustAttributes()
		if mtlsDiags.HasErrors() {
			return nil, fmt.Errorf("remote environment: mtls block: %w", mtlsDiags)
		}
		if v, ok := mtlsAttrs["server_cert"]; ok {
			val, err := attrAsString(v)
			if err != nil {
				return nil, fmt.Errorf("remote environment: mtls.server_cert: %w", err)
			}
			cfg.ServerCertPath = val
		}
		if v, ok := mtlsAttrs["server_key"]; ok {
			val, err := attrAsString(v)
			if err != nil {
				return nil, fmt.Errorf("remote environment: mtls.server_key: %w", err)
			}
			cfg.ServerKeyPath = val
		}
		if v, ok := mtlsAttrs["client_ca"]; ok {
			val, err := attrAsString(v)
			if err != nil {
				return nil, fmt.Errorf("remote environment: mtls.client_ca: %w", err)
			}
			cfg.ClientCAPath = val
		}
		if v, ok := mtlsAttrs["client_identity_pattern"]; ok {
			val, err := attrAsString(v)
			if err != nil {
				return nil, fmt.Errorf("remote environment: mtls.client_identity_pattern: %w", err)
			}
			cfg.ClientIdentityPattern = val
		}
	}

	return cfg, nil
}

// ValidateClientIdentity checks whether the extracted certificate subject
// matches the configured regex pattern.
func ValidateClientIdentity(subject string, pattern string) error {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("invalid client_identity_pattern: %w", err)
	}
	if !re.MatchString(subject) {
		return fmt.Errorf("certificate subject %q does not match pattern %q", subject, pattern)
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
