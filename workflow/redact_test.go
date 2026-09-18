package workflow

import "testing"

func TestRedactSource(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "archive userinfo", source: "https://user:token@host/x.tar.gz", want: "https://redacted@host/x.tar.gz"},
		{name: "user only", source: "https://user@host/x.tar.gz", want: "https://redacted@host/x.tar.gz"},
		{name: "no userinfo", source: "https://host/x.tar.gz", want: "https://host/x.tar.gz"},
		{name: "git scheme", source: "https://ci:secret@github.com/org/repo.git?ref=v1", want: "https://redacted@github.com/org/repo.git?ref=v1"},
		{name: "at in path is not userinfo", source: "https://host/a@b/c", want: "https://host/a@b/c"},
		{name: "userinfo kept out of query", source: "https://host/a?x=u@ser", want: "https://host/a?x=u@ser"},
		{name: "local path unchanged", source: "./local/workflow", want: "./local/workflow"},
		{name: "scp-style git form unchanged", source: "git@github.com:org/repo.git", want: "git@github.com:org/repo.git"},
		{name: "malformed scheme only", source: "://", want: "://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactSource(tt.source); got != tt.want {
				t.Errorf("RedactSource(%q) = %q, want %q", tt.source, got, tt.want)
			}
		})
	}
}

func TestRedactSourceNeverPanics(t *testing.T) {
	for _, s := range []string{"", "://", "a://", "https://", "://@@", "https://@/x", "http://user:pass@", "%%zz://x@y"} {
		_ = RedactSource(s)
	}
}
