package plugin

import (
	"path/filepath"
	"strings"
)

// PermissionRequest is the host-side view of a plugin's permission request.
type PermissionRequest struct {
	// ID is the opaque request identifier assigned by the plugin. It must be
	// echoed back in the Permit RPC so the plugin can correlate responses.
	ID string
	// Tool is the tool or permission name being requested (e.g. "read_file",
	// "shell:git status"). This is matched against the AllowTools patterns.
	Tool string
	// Details is an optional map of extra context from the plugin.
	Details map[string]string
}

// PermissionPolicy decides whether to allow or deny a permission request.
type PermissionPolicy interface {
	// Decide returns (allow, reason). reason is a human-readable string
	// explaining the decision (e.g. "matched: read_file" or
	// "no matching allow_tools entry").
	Decide(req PermissionRequest) (allow bool, reason string)
}

// NewPolicy returns a PermissionPolicy that evaluates requests against the
// given glob patterns. Patterns are matched against req.Tool using
// path/filepath.Match semantics ('*' matches any sequence within a segment,
// '?' matches any single character; colons in patterns such as "shell:git *"
// are treated as literals). First-match wins; an empty pattern list produces
// a deny-all policy.
//
// Examples:
//
//	NewPolicy([]string{"read_file"})          // allows any read_file call
//	NewPolicy([]string{"shell:git status"})   // allows exactly "shell:git status"
//	NewPolicy([]string{"shell:git *"})        // allows any git sub-command
//	NewPolicy([]string{"shell:*"})            // allows any shell command
//	NewPolicy(nil)                            // denies everything (default)
func NewPolicy(patterns []string) PermissionPolicy {
	if len(patterns) == 0 {
		return denyAllPolicy{}
	}
	return &allowlistPolicy{patterns: append([]string(nil), patterns...)}
}

// denyAllPolicy is the default when no allow_tools are configured.
type denyAllPolicy struct{}

func (denyAllPolicy) Decide(_ PermissionRequest) (bool, string) {
	return false, "no matching allow_tools entry"
}

// allowlistPolicy evaluates requests against a list of glob patterns.
type allowlistPolicy struct {
	patterns []string
}

func (p *allowlistPolicy) Decide(req PermissionRequest) (bool, string) {
	targets := permissionMatchTargets(req)
	for _, pat := range p.patterns {
		for _, target := range targets {
			matched, err := filepath.Match(pat, target)
			if err != nil {
				// Invalid pattern: skip rather than panic or deny all.
				continue
			}
			if matched {
				return true, "matched: " + pat
			}
		}
	}
	return false, "no matching allow_tools entry"
}

// permissionMatchTargets returns ordered candidates for matching allow_tools:
//  1. raw tool kind (e.g. "shell")
//  2. tool + detail-derived fingerprint (e.g. "shell:git status")
//
// The first matching pattern wins. Duplicate candidates are removed while
// preserving order.
func permissionMatchTargets(req PermissionRequest) []string {
	tool := strings.TrimSpace(req.Tool)
	if tool == "" {
		return nil
	}
	targets := []string{tool}
	for _, fp := range requestFingerprints(req.Details) {
		fp = strings.TrimSpace(fp)
		if fp == "" {
			continue
		}
		targets = append(targets, tool+":"+fp)
	}
	return dedupeStrings(targets)
}

// requestFingerprints extracts optional arg/command fingerprints from plugin
// request details so callers can allow specific subcommands like
// "shell:git status" while denying broad "shell:*".
func requestFingerprints(details map[string]string) []string {
	if len(details) == 0 {
		return nil
	}
	var out []string
	if v := strings.TrimSpace(details["command"]); v != "" {
		out = append(out, v)
	}
	if v := strings.TrimSpace(details["commands"]); v != "" {
		for _, cmd := range strings.Split(v, ",") {
			cmd = strings.TrimSpace(cmd)
			if cmd != "" {
				out = append(out, cmd)
			}
		}
	}
	if v := strings.TrimSpace(details["full_command_text"]); v != "" {
		out = append(out, v)
	}
	return dedupeStrings(out)
}

func dedupeStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
