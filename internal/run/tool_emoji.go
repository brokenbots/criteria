package run

import "strings"

// toolEmoji returns a 1-rune-or-grapheme inline marker for the given tool name.
// The mapping is by case-insensitive substring match against well-known tool
// name conventions, with a fallback marker for unknown tools.
//
// Categories (in priority order):
//   - Search/grep:  search, grep, find_text, query, lookup — 🔍
//   - Network/HTTP: http, fetch, request, curl, api, post, put, delete — 🌐
//   - Write/edit:   write, edit, modify, create, save, append, replace — ✏️
//   - Shell/exec:   shell, exec, bash, " sh ", cmd, command, run — ⚡
//   - File ops:     read, file, open, cat, ls, list, dir, find — 📄
//   - Fallback:     → (right-arrow)
//
// The match is case-insensitive substring matching on the tool name. Earlier
// categories win for ambiguous names (e.g. "grep_files" matches search before
// file, "http_post" matches network before write/edit).
//
// Note: "get" is intentionally absent from the network list — too many
// file-ops tools contain "get". Network identification relies on http/fetch/
// request/curl/api plus the explicit write verbs post/put/delete. A bare "get"
// tool falls through to the fallback →.
//
// The " sh " keyword (with surrounding spaces) is a deliberate guard against
// false positives such as "crash" or "shebang" matching "sh".
func toolEmoji(toolName string) string {
	n := strings.ToLower(toolName)
	for _, cat := range emojiCategories {
		for _, kw := range cat.keywords {
			if strings.Contains(n, kw) {
				return cat.emoji
			}
		}
	}
	return "→"
}

type emojiCategory struct {
	emoji    string
	keywords []string
}

// emojiCategories is ordered by priority: first match wins.
var emojiCategories = []emojiCategory{
	{emoji: "🔍", keywords: []string{"search", "grep", "find_text", "query", "lookup"}},
	{emoji: "🌐", keywords: []string{"http", "fetch", "request", "curl", "api", "post", "put", "delete"}},
	{emoji: "✏️", keywords: []string{"write", "edit", "modify", "create", "save", "append", "replace"}},
	{emoji: "⚡", keywords: []string{"shell", "exec", "bash", " sh ", "cmd", "command", "run"}},
	{emoji: "📄", keywords: []string{"read", "file", "open", "cat", "ls", "list", "dir", "find"}},
}
