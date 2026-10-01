package adapterhost

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/brokenbots/criteria/workflow"
)

// PermissionRequest is the host-side view of an adapter's permission request.
type PermissionRequest struct {
	// ID is the opaque request identifier assigned by the adapter. It must be
	// echoed back in the Permit RPC so the adapter can correlate responses.
	ID string
	// Tool is the tool or permission name being requested (e.g. "read_file",
	// "shell:git status"). This is matched against the AllowTools patterns.
	Tool string
	// Details is an optional map of extra context from the adapter.
	Details map[string]string
}

// PermissionPolicy decides whether to allow or deny a permission request.
type PermissionPolicy interface {
	// Decide returns (allow, reason). reason is a human-readable string
	// explaining the decision (e.g. "matched: read_file" or
	// "no matching allow_tools entry").
	Decide(req PermissionRequest) (allow bool, reason string)
}

// CombinedPolicy wraps the legacy allow_tools matcher and optionally an
// environment-level policy (network, filesystem, etc.) from WS09.
// It implements PermissionPolicy so it can be used with the existing
// permissionState.Evaluate path.
type CombinedPolicy struct {
	Tools   PermissionPolicy // allow_tools matcher (nil → deny-all)
	Env     *workflow.ResolvedPolicy
	Adapter string // adapter name for alias resolution
}

// NewCombinedPolicy builds a CombinedPolicy from raw allow_tools patterns.
// If env is non-nil, the returned policy also checks env-level constraints.
func NewCombinedPolicy(adapterName string, patterns []string, env *workflow.ResolvedPolicy) *CombinedPolicy {
	var aliases map[string]string
	if adapterName != "" {
		aliases = adapterPermissionAliases[adapterName]
	}
	return &CombinedPolicy{
		Tools:   NewPolicyWithAliases(patterns, aliases),
		Env:     env,
		Adapter: adapterName,
	}
}

// Decide evaluates the request against allow_tools first, then env policy.
func (p *CombinedPolicy) Decide(req PermissionRequest) (allow bool, reason string) {
	if p == nil {
		return false, "no matching allow_tools entry"
	}
	// 1. allow_tools check
	if p.Tools != nil {
		allow, reason = p.Tools.Decide(req)
	} else {
		allow, reason = false, "no matching allow_tools entry"
	}
	if !allow {
		return false, reason
	}
	// 2. env-policy checks. Environment policy is a second layer applied only
	// after allow_tools grants the request. Filesystem write operations are
	// denied under a read-only filesystem policy, while read operations remain
	// allowed. Network egress operations are denied when AllowEgress is false,
	// while purely local operations remain allowed.
	if p.Env != nil {
		if p.Env.Filesystem != nil && p.Env.Filesystem.ReadOnly && isFilesystemWrite(req) {
			return false, "denied by environment policy: filesystem read-only"
		}
		if p.Env.Network != nil && !p.Env.Network.AllowEgress && isNetworkEgress(req) {
			return false, "denied by environment policy: network egress disabled"
		}
	}
	return true, reason
}

// isFilesystemWrite reports whether req is a filesystem write operation.
//
// Classification scheme:
//   - Known write tool kinds (e.g. "write_file") are always treated as writes.
//   - For "shell" requests, the command text is inspected for common write
//     verbs (touch, rm, cp, mv, mkdir, ...) or shell output redirection
//     operators (>, >>). Read-only commands such as "cat" and "pwd" are not matched.
func isFilesystemWrite(req PermissionRequest) bool {
	if filesystemWriteToolKinds[req.Tool] {
		return true
	}
	if req.Tool != "shell" {
		return false
	}
	cmd := commandText(req)
	if cmd == "" {
		return false
	}
	lower := strings.ToLower(cmd)
	for _, verb := range filesystemWriteVerbs {
		if wordBoundaryContains(lower, verb) {
			return true
		}
	}
	// Shell output redirection creates or overwrites a file.
	return strings.Contains(lower, ">")
}

// isNetworkEgress reports whether req performs network egress.
//
// Classification scheme:
//   - Known network tool kinds (currently "fetch") are always treated as egress.
//   - For "shell" requests, the command text is inspected for network client
//     verbs (curl, wget, ssh, scp, ...) or URL-like markers (://). Local
//     commands such as "cat" and "pwd" are not matched.
func isNetworkEgress(req PermissionRequest) bool {
	if networkToolKinds[req.Tool] {
		return true
	}
	if req.Tool != "shell" {
		return false
	}
	cmd := commandText(req)
	if cmd == "" {
		return false
	}
	lower := strings.ToLower(cmd)
	for _, verb := range networkEgressVerbs {
		if wordBoundaryContains(lower, verb) {
			return true
		}
	}
	return strings.Contains(lower, "://")
}

// commandText returns the concatenated command hints from the request details.
func commandText(req PermissionRequest) string {
	if len(req.Details) == 0 {
		return ""
	}
	keys := []string{"full_command_text", "command", "commands"}
	var parts []string
	for _, k := range keys {
		if v := strings.TrimSpace(req.Details[k]); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " ")
}

// wordBoundaryContains reports whether s contains word as a whole word.
func wordBoundaryContains(s, word string) bool {
	// Fast path for common delimiters.
	for _, sep := range []string{" ", "\t", ";", "&", "|", "(", ")"} {
		if strings.Contains(s, sep+word+sep) || strings.HasPrefix(s, word+sep) || strings.HasSuffix(s, sep+word) {
			return true
		}
	}
	return s == word
}

var filesystemWriteToolKinds = map[string]bool{
	"write_file":  true,
	"edit_file":   true,
	"delete_file": true,
	"move_file":   true,
	"create_file": true,
}

var filesystemWriteVerbs = []string{
	"touch", "rm", "cp", "mv", "mkdir", "rmdir", "chmod", "chown", "tee",
	"shred", "mkfs", "dd",
}

var networkToolKinds = map[string]bool{
	"fetch": true,
}

var networkEgressVerbs = []string{
	"curl", "wget", "ssh", "scp", "sftp", "ping", "dig", "nslookup", "nc",
	"telnet", "ftp",
}

// adapterPermissionAliases maps adapter name → (user-facing allow_tools name → canonical SDK kind).
//
// Background (UF#02): some adapters (e.g. Copilot) report short permission kinds
// at runtime ("read", "write") while users naturally write tool names like
// "read_file" or "write_file" in their allow_tools lists. This map lets the host
// policy engine resolve those aliases so `allow_tools = ["read_file"]` grants the
// "read" permission correctly.
//
// The compiler obtains aliases from the adapter's InfoResponse via
// internal/adapterhost/loader.go (AdapterInfoFromProto), so compile-time
// diagnostics stay in sync with the live adapter vocabulary. This map is used
// only by the runtime policy engine; keep it aligned with the aliases the
// adapter publishes in its InfoResponse.
var adapterPermissionAliases = map[string]map[string]string{
	"copilot": {
		"read_file":  "read",
		"write_file": "write",
	},
}

// NewPolicy returns a PermissionPolicy that evaluates requests against the
// given glob patterns. Patterns are matched against req.Tool using
// path/filepath.Match semantics: '*' matches any sequence of non-slash
// characters and does not cross '/', '?' matches any single non-slash character,
// and '[a-z]' matches a character class. There is no '**' recursive syntax.
// Colons in patterns such as "shell:git *" are treated as literals.
// First-match wins; an empty pattern list produces a deny-all policy.
//
// Examples:
//
//	NewPolicy([]string{"read_file"})            // allows the read_file tool
//	NewPolicy([]string{"shell:git status"})     // allows exactly "shell:git status"
//	NewPolicy([]string{"shell:git *"})          // allows "shell:git status" but not "shell:git add src/main.go"
//	NewPolicy([]string{"shell:*/*/*"})         // allows "shell:cat /etc/hosts"
//	NewPolicy([]string{"shell:cat /etc/hosts"}) // allows exactly "shell:cat /etc/hosts"
//	NewPolicy([]string{"*"})                    // allows any tool
//	NewPolicy(nil)                              // denies everything (default)
func NewPolicy(patterns []string) PermissionPolicy {
	return NewPolicyWithAliases(patterns, nil)
}

// NewPolicyWithAliases is like NewPolicy but also accepts an alias map (alias → canonical)
// so user-facing names like "read_file" resolve to the canonical SDK kind "read" at
// match time. Pass nil when the adapter reports no aliased kinds.
func NewPolicyWithAliases(patterns []string, aliases map[string]string) PermissionPolicy {
	if len(patterns) == 0 {
		return denyAllPolicy{}
	}
	return &allowlistPolicy{
		patterns: append([]string(nil), patterns...),
		aliases:  aliases,
	}
}

// PermissionDenialSuggestion returns a hint string for the permission.denied event,
// suggesting what the operator should add to allow_tools. It includes known aliases
// when the adapter reports any for the requested tool.
// Returns an empty string when no suggestion is available.
func PermissionDenialSuggestion(adapterName, tool string) string {
	var aliases []string
	for alias, canonical := range adapterPermissionAliases[adapterName] {
		if canonical == tool {
			aliases = append(aliases, alias)
		}
	}
	if len(aliases) == 0 {
		return ""
	}
	sort.Strings(aliases)
	return "add '" + tool + "' to allow_tools (aliases: " + strings.Join(aliases, ", ") + ")"
}

// denyAllPolicy is the default when no allow_tools are configured.
type denyAllPolicy struct{}

func (denyAllPolicy) Decide(_ PermissionRequest) (allow bool, reason string) {
	return false, "no matching allow_tools entry"
}

// allowlistPolicy evaluates requests against a list of glob patterns.
type allowlistPolicy struct {
	patterns []string
	aliases  map[string]string // user-facing name → canonical SDK kind
}

func (p *allowlistPolicy) Decide(req PermissionRequest) (allow bool, reason string) {
	targets := permissionMatchTargets(req)
	bad := &badPatternTracker{}
	// KB-57c: a compound request (2+ per-segment targets) is granted only when
	// EVERY segment matches some allow entry — a single matching segment must
	// never carry a compound whose other segments would be denied standalone
	// ("git status && make ci" stays denied even though "shell:git status *"
	// matches its first segment). Single-target requests keep legacy
	// first-match-wins semantics byte-for-byte.
	if !reqIsCompound(req.Details) {
		return p.decideSingle(targets, bad)
	}
	return p.decideCompound(req, targets, bad)
}

// decideSingle is the legacy first-match-wins evaluation over all targets
// (bare tool kind first, then fingerprints).
func (p *allowlistPolicy) decideSingle(targets []string, bad *badPatternTracker) (allowed bool, reason string) {
	for _, pat := range p.patterns {
		for _, target := range targets {
			if matched, reason := p.matchPattern(pat, target, bad); matched {
				return true, reason
			}
		}
	}
	return false, bad.denialReason()
}

// decideCompound grants only when every command-segment target matches some
// allow entry (the bare tool-kind target is not a command segment and is
// skipped); the reported reason is the first matched segment's for the event
// stream.
func (p *allowlistPolicy) decideCompound(req PermissionRequest, targets []string, bad *badPatternTracker) (allowed bool, reason string) {
	tool := strings.TrimSpace(req.Tool)
	var firstReason string
	for _, target := range targets {
		if target == tool {
			continue
		}
		matched, reason := p.firstMatch(target, bad)
		if !matched {
			return false, "no matching allow_tools entry for every segment of the compound command"
		}
		if firstReason == "" {
			firstReason = reason
		}
	}
	if firstReason == "" {
		return false, bad.denialReason()
	}
	return true, firstReason
}

// firstMatch returns the first pattern that matches target (including via a
// canonical alias).
func (p *allowlistPolicy) firstMatch(target string, bad *badPatternTracker) (matched bool, reason string) {
	for _, pat := range p.patterns {
		if matched, reason := p.matchPattern(pat, target, bad); matched {
			return true, reason
		}
	}
	return false, ""
}

// reqIsCompound reports whether the request's own fingerprints contained a
// compound command (the same segmentation rule requestFingerprints used).
func reqIsCompound(details map[string]string) bool {
	for _, key := range []string{"command", "commands", "full_command_text"} {
		if v := strings.TrimSpace(details[key]); v != "" {
			if _, ok := segmentCompoundCommand(v); ok {
				return true
			}
		}
	}
	return false
}

// matchPattern evaluates a single pattern against a target. It returns (true, reason)
// on a match, otherwise (false, ""). Malformed patterns are recorded in bad.
//
// KB-57c (trailing-star prefix match): a trailing '*' is matched as a
// slash-PERMISSIVE prefix — filepath.Match's '*' cannot cross '/', which
// silently breaks intent for command fingerprints: "shell:git log *" must
// reach "git log origin/main..HEAD -- deep/path/x.go", and bare '*' (the
// universal allow) must reach any segment. Pattern prefixes remain literal +
// segment-correct: the match is a pure prefix check on the pattern's
// glob-free part. Non-trailing globs keep filepath.Match semantics
// ("shell:git */*"-style entries continue to behave as before; the workflow
// lists never use them).
func (p *allowlistPolicy) matchPattern(pat, target string, bad *badPatternTracker) (matched bool, reason string) {
	if strings.HasSuffix(pat, "*") {
		prefix := pat[:len(pat)-1]
		if idx := strings.Index(prefix, "*"); idx >= 0 {
			// An interior glob plus a trailing star ('a*b*') is not present
			// in any shipped list; fall back to filepath.Match semantics
			// (and record the pattern as intentionally unmatched here) so
			// behavior for unexpected lists never silently widens.
			if ok, err := filepath.Match(pat, target); err == nil && ok {
				return true, "matched: " + pat + " (interior glob)"
			}
			return false, ""
		}
		if strings.HasPrefix(target, prefix) {
			return true, "matched: " + pat
		}
		return false, ""
	}
	if ok, err := filepath.Match(pat, target); err != nil {
		bad.record(pat)
		return false, ""
	} else if ok {
		return true, "matched: " + pat
	}

	// If pat is an alias (e.g. "read_file" → "read"), also try matching the
	// canonical form against the target so allow_tools entries using the friendly
	// alias work transparently.
	if canonical, ok := p.aliases[pat]; ok {
		if ok, err := filepath.Match(canonical, target); err != nil {
			// A canonical alias pattern should never be malformed; if it is,
			// surface it too.
			bad.record(canonical)
			return false, ""
		} else if ok {
			return true, "matched: " + pat + " (alias for " + canonical + ")"
		}
	}
	return false, ""
}

// badPatternTracker records malformed glob patterns and builds a denial reason.
type badPatternTracker struct {
	patterns []string
}

func (b *badPatternTracker) record(pat string) {
	if b == nil {
		return
	}
	for _, v := range b.patterns {
		if v == pat {
			return
		}
	}
	b.patterns = append(b.patterns, pat)
}

func (b *badPatternTracker) denialReason() string {
	if b == nil || len(b.patterns) == 0 {
		return "no matching allow_tools entry"
	}
	return "no matching allow_tools entry; invalid pattern(s): " + strings.Join(b.patterns, ", ")
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

// requestFingerprints extracts optional arg/command fingerprints from adapter
// request details so callers can allow specific subcommands like
// "shell:git status" while denying broad "shell:*".
//
// KB-57c (compound permission support): compound commands (&& ; | || newlines)
// are ADDITIONALLY segmented, and every segment is emitted as its own
// fingerprint target. The allowlist policy requires only ONE target to match
// (first-match-wins), so whole-text matching alone would let
// "git status && make ci" through on the strength of its first segment alone.
// With per-segment targets, an allowlist entry matched by any segment grants
// the request ONLY if every other segment also matches some entry — enforced
// by allowlistPolicy.Decide: a segmented request is granted when EVERY target
// matches, never when only one does. Single (non-compound) commands keep the
// legacy whole-text semantics byte-for-byte.
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
	out = dedupeStrings(out)

	// KB-57c: emit per-segment targets for compound commands. A segmented
	// result replaces the whole-text target so a compound can never ride in
	// on one matching segment.
	if len(out) == 0 {
		return out
	}
	segmented := make([]string, 0, len(out))
	compound := false
	for _, v := range out {
		segs, isCompound := segmentCompoundCommand(v)
		if isCompound {
			compound = true
			segmented = append(segmented, segs...)
			continue
		}
		segmented = append(segmented, v)
	}
	if compound {
		out = dedupeStrings(segmented)
	}
	return out
}

// compoundSeparators are the shell control operators that separate independent
// commands in one line. Quotes are respected by the scanner: a separator
// inside single or double quotes (or escaped with a backslash) is literal
// text, not a split point. Redirection operators alone (>, >>, <, 2>&1) do
// NOT split — they are part of the command's own text and the whole-text
// fingerprint still matches patterns written for them.
var compoundSeparators = []string{"&&", "||", ";", "\n", "|"}

// segmentCompoundCommand splits a compound command line into its segments.
// Returns (nil, false) when the text contains no unquoted compound separator —
// callers keep the legacy whole-text target. Empty segments (e.g. from ";;")
// are dropped. Quote-aware: separators inside '...' or "..." never split.
func segmentCompoundCommand(text string) ([]string, bool) {
	segs := make([]string, 0, 4)
	for _, line := range splitRespectingQuotes(text) {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			segs = append(segs, trimmed)
		}
	}
	if len(segs) <= 1 {
		return nil, false
	}
	return segs, true
}

// splitRespectingQuotes splits text on unquoted compound separators.
// Handles single quotes, double quotes, and backslash escapes. A separator
// inside quotes is literal; a partial separator at a quote boundary does not
// split. Trailing separators yield no empty segment (trimmed by the caller).
func splitRespectingQuotes(text string) []string {
	var segs []string
	var cur strings.Builder
	var quote rune
	escaped := false
	for i := 0; i < len(text); {
		if n := findSeparator(text, i, quote, escaped); n > 0 {
			segs = append(segs, strings.TrimSpace(cur.String()))
			cur.Reset()
			i += n
			continue
		}
		quote, escaped = advance(text, i, quote, escaped, &cur)
		i++
	}
	if tail := strings.TrimSpace(cur.String()); tail != "" {
		segs = append(segs, tail)
	}
	return segs
}

// findSeparator returns the length of the compound separator starting at pos,
// or 0 when pos sits inside a quote, an escape, or no separator matches.
func findSeparator(text string, pos int, quote rune, escaped bool) int {
	if quote != 0 || escaped {
		return 0
	}
	for _, sep := range compoundSeparators {
		if pos+len(sep) <= len(text) && text[pos:pos+len(sep)] == sep {
			return len(sep)
		}
	}
	return 0
}

// advance consumes one non-separator character at i, updating quote/escape
// state and writing the consumed character to cur.
func advance(text string, i int, quote rune, escaped bool, cur *strings.Builder) (newQuote rune, newEscaped bool) {
	c := rune(text[i])
	switch {
	case escaped:
		cur.WriteRune(c)
		return quote, false
	case quote == 0 && c == '\\':
		cur.WriteRune(c)
		return quote, true
	case quote == 0 && (c == '\'' || c == '"'):
		cur.WriteRune(c)
		return c, false
	case quote != 0 && c == quote:
		cur.WriteRune(c)
		return 0, false
	default:
		cur.WriteRune(c)
		return quote, escaped
	}
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
