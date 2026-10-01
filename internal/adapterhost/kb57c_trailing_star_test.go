package adapterhost

import "testing"

// TestKB57cTrailingStarPrefix: trailing-star patterns match slash-bearing
// fingerprint targets by prefix — bare '*' is the universal allow for any
// segment (allow_tools=["*"] steps), "shell:git log *" reaches deep path
// args. Interior-glob patterns keep filepath.Match semantics.
func TestKB57cTrailingStarPrefix(t *testing.T) {
	universal := NewPolicy([]string{"*"})
	_ = universal
	cases := []struct {
		patterns []string
		target   string
		want     bool
		note     string
	}{
		{[]string{"*"}, "shell", true, "bare star matches bare tool"},
		{[]string{"*"}, "shell:git config --global credential.https://github.com.helper", true, "bare star matches any segment"},
		{[]string{"shell:*"}, "shell:git push origin HEAD", true, "tool-scoped star matches deep segment"},
		{[]string{"shell:*"}, "bash:git push origin HEAD", false, "tool-scoped star must not cross tool kinds"},
		{[]string{"shell:git log *"}, "shell:git log origin/main..HEAD -- deep/path/x.go", true, "deep path after subcommand"},
		{[]string{"shell:make build"}, "shell:make ci", false, "literal still exact"},
		{[]string{"*"}, "read", true, "star covers non-shell kinds"},
		{[]string{"shell:git *st*us"}, "shell:git status", true, "interior glob falls back to filepath.Match (unchanged semantics)"},
	}
	for _, tc := range cases {
		p := NewPolicy(tc.patterns)
		req := PermissionRequest{ID: "x", Tool: toolOf(tc.target), Details: map[string]string{"full_command_text": cmdOf(tc.target)}}
		allow, _ := p.Decide(req)
		if allow != tc.want {
			t.Errorf("%s: allow=%v want=%v (pattern %v vs %q)", tc.note, allow, tc.want, tc.patterns, tc.target)
		}
	}
}

func toolOf(target string) string {
	if i := len("shell:"); i <= len(target) && target[:6] == "shell:" {
		return "shell"
	}
	if len(target) > 5 && target[:5] == "bash:" {
		return "bash"
	}
	return target
}

func cmdOf(target string) string {
	if i := indexOfColonSlash(target); i >= 0 {
		return target[i+1:]
	}
	return target
}

func indexOfColonSlash(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}
