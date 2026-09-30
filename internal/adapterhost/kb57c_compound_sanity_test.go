package adapterhost

import "testing"

// TestKB57cCompoundSegmentation is the regression suite for compound command
// permission support (KB-61 review death, run 747cf4ac): compound commands are
// segmented on unquoted && ; | || newlines, and a compound is granted only
// when EVERY segment matches the step's allowlist; single commands keep
// first-match-wins whole-text semantics.
func TestKB57cCompoundSegmentation(t *testing.T) {
	var pats []string
	for _, sub := range []string{"status", "diff", "log", "show", "rev-parse", "branch", "ls-remote", "remote"} {
		pats = append(pats, "shell:git "+sub, "shell:git "+sub+" *")
		for n := 2; n <= 7; n++ {
			star := "*"
			for i := 1; i < n; i++ {
				star += "/*"
			}
			pats = append(pats, "shell:git "+sub+" "+star)
		}
	}
	pats = append(pats,
		"shell:ls", "shell:ls *", "shell:ls */*", "shell:ls */*/*", "shell:ls */*/*/*",
		"shell:ls */*/*/*/*", "shell:ls */*/*/*/*/*",
		"shell:echo", "shell:echo *", "shell:cat", "shell:cat *", "shell:cat */*", "shell:cat */*/*",
		"shell:make build")
	p := NewPolicy(pats)
	cases := []struct {
		cmd  string
		want bool
		note string
	}{
		// singles: legacy semantics byte-for-byte
		{"git status", true, "plain read"},
		{"git log --oneline -15", true, "single log"},
		{"git push origin HEAD", false, "write verb single"},
		{"make ci", false, "CI gate single"},
		// compounds: every segment must match
		{"git log --oneline -15 && echo \"---S---\" && git status", true, "reviewer evidence compound"},
		{"ls -la /data/intake/KB-61/ && echo \"---\" && git diff --stat origin/main...HEAD", true, "deep-path compound"},
		{"git status && make ci", false, "CI verb must poison the compound"},
		{"git log --oneline -5 && make test", false, "test runner poisons"},
		{"git diff origin/main...HEAD -- proto/x.proto && git push origin HEAD", false, "write verb poisons"},
		{"git status && rm -rf /data", false, "destructive verb poisons"},
		// quoting: separators inside quotes are literal text
		{"echo \"a && b\"", true, "separator inside quotes is literal"},
		{"git log --oneline -5; git status", true, "semicolon compound"},
		{"git log --oneline -5 | cat", true, "pipe to allowed read"},
		{"git log --oneline -5 | sh", false, "pipe to shell denied"},
	}
	for _, tc := range cases {
		req := PermissionRequest{ID: "x", Tool: "shell", Details: map[string]string{"full_command_text": tc.cmd}}
		allow, _ := p.Decide(req)
		if allow != tc.want {
			t.Errorf("%s: allow=%v want=%v\n  cmd: %s", tc.note, allow, tc.want, tc.cmd)
		}
	}
}