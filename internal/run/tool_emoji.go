package run

import "strings"

// toolEmoji returns a 1-rune-or-grapheme inline marker for the given tool name.
// The mapping is by case-insensitive word-boundary match against well-known tool
// name conventions, with a fallback marker for unknown tools.
//
// Categories (in priority order):
//   - Search/grep:  search, grep, find_text, query, lookup — 🔍
//   - Network/HTTP: http, fetch, request, curl, api, post, put, delete — 🌐
//   - Write/edit:   write, edit, modify, create, save, append, replace — ✏️
//   - Shell/exec:   shell, exec, bash, " sh ", cmd, command, " run " — ⚡
//   - File ops:     read, file, open, cat, ls, list, dir, find — 📄
//   - Fallback:     → (right-arrow)
//
// To support word-boundary matching for space-guarded keywords (" sh ", " run "),
// the tool name is padded with spaces before matching. This means:
//   - " sh "  matches "sh", "sh cmd", "run sh cmd", but NOT "crash" or "shebang"
//   - " run " matches "run", but NOT "return_value", "get_current_run", or "prerun"
//
// Note: "get" is intentionally absent from the network list — too many
// file-ops tools contain "get". Network identification relies on http/fetch/
// request/curl/api plus the explicit write verbs post/put/delete. A bare "get"
// tool falls through to the fallback →.
func toolEmoji(toolName string) string {
	n := " " + strings.ToLower(toolName) + " "
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
	{emoji: "⚡", keywords: []string{"shell", "exec", "bash", " sh ", "cmd", "command", " run "}},
	{emoji: "📄", keywords: []string{"read", "file", "open", "cat", "ls", "list", "dir", "find"}},
}
