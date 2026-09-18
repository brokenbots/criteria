package workflow

import "strings"

// RedactSource masks userinfo credentials in a URL-shaped source
// ("https://user:token@host/x.tar.gz" → "https://redacted@host/x.tar.gz") so
// secrets never reach diagnostics, structured logs, or recorded provenance.
// Workflow sources in memory and in lockfiles keep the raw form; everything
// rendered for humans — CLI resolver errors, run metadata, compile-time
// diagnostics — must pass through this helper first (CRI-225/CRI-226).
// Sources without a "://" separator (local paths, scp-style git forms) are
// returned unchanged.
func RedactSource(source string) string {
	idx := strings.Index(source, "://")
	if idx == -1 {
		return source
	}
	rest := source[idx+3:]
	// The userinfo delimiter can only appear in the authority component,
	// which ends at the first "/", "?" or "#"; an "@" later in the path must
	// not be mistaken for one. Within the authority, the first "@" is the
	// delimiter (userinfo cannot contain a literal "@").
	authority := rest
	if end := strings.IndexAny(rest, "/?#"); end != -1 {
		authority = rest[:end]
	}
	at := strings.Index(authority, "@")
	if at == -1 {
		return source
	}
	return source[:idx+3] + "redacted@" + rest[at+1:]
}
