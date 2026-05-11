package run

import "testing"

func TestToolEmoji_FileOps(t *testing.T) {
	cases := []string{"read_file", "open_path", "list_dir", "cat", "ls"}
	for _, name := range cases {
		if got := toolEmoji(name); got != "📄" {
			t.Errorf("toolEmoji(%q) = %q, want 📄", name, got)
		}
	}
	// find_text is a search keyword — must not match file ops.
	if got := toolEmoji("find_text"); got != "🔍" {
		t.Errorf("toolEmoji(find_text) = %q, want 🔍 (search wins over file)", got)
	}
	// find_files matches "find" in file ops (not a search keyword).
	if got := toolEmoji("find_files"); got != "📄" {
		t.Errorf("toolEmoji(find_files) = %q, want 📄", got)
	}
}

func TestToolEmoji_WriteEdit(t *testing.T) {
	cases := []string{"write_file", "edit", "modify_doc", "create", "save", "append", "replace"}
	for _, name := range cases {
		if got := toolEmoji(name); got != "✏️" {
			t.Errorf("toolEmoji(%q) = %q, want ✏️", name, got)
		}
	}
}

func TestToolEmoji_ShellExec(t *testing.T) {
	cases := []string{"shell", "exec_command", "bash_run"}
	for _, name := range cases {
		if got := toolEmoji(name); got != "⚡" {
			t.Errorf("toolEmoji(%q) = %q, want ⚡", name, got)
		}
	}
	// " sh " with surrounding spaces — the keyword has embedded spaces.
	if got := toolEmoji("run sh cmd"); got != "⚡" {
		t.Errorf("toolEmoji(\"run sh cmd\") = %q, want ⚡", got)
	}
}

func TestToolEmoji_Network(t *testing.T) {
	cases := []string{"http_post", "fetch_url", "request_get", "curl", "api_call", "delete_resource"}
	for _, name := range cases {
		if got := toolEmoji(name); got != "🌐" {
			t.Errorf("toolEmoji(%q) = %q, want 🌐", name, got)
		}
	}
}

func TestToolEmoji_Search(t *testing.T) {
	cases := []string{"search", "grep", "find_text", "query", "lookup"}
	for _, name := range cases {
		if got := toolEmoji(name); got != "🔍" {
			t.Errorf("toolEmoji(%q) = %q, want 🔍", name, got)
		}
	}
}

func TestToolEmoji_Fallback(t *testing.T) {
	cases := []string{"weird_thing", "xyz", ""}
	for _, name := range cases {
		if got := toolEmoji(name); got != "→" {
			t.Errorf("toolEmoji(%q) = %q, want →", name, got)
		}
	}
}

// TestToolEmoji_PriorityOrder_GrepFiles confirms search wins over file ops.
func TestToolEmoji_PriorityOrder_GrepFiles(t *testing.T) {
	if got := toolEmoji("grep_files"); got != "🔍" {
		t.Errorf("toolEmoji(grep_files) = %q, want 🔍 (search wins over file)", got)
	}
}

// TestToolEmoji_PriorityOrder_HttpRead confirms network wins over file ops.
func TestToolEmoji_PriorityOrder_HttpRead(t *testing.T) {
	if got := toolEmoji("http_read"); got != "🌐" {
		t.Errorf("toolEmoji(http_read) = %q, want 🌐 (network wins over file)", got)
	}
}

// TestToolEmoji_PriorityOrder_EditCommand confirms write/edit wins over shell.
func TestToolEmoji_PriorityOrder_EditCommand(t *testing.T) {
	if got := toolEmoji("edit_command"); got != "✏️" {
		t.Errorf("toolEmoji(edit_command) = %q, want ✏️ (write wins over shell)", got)
	}
}

func TestToolEmoji_CaseInsensitive(t *testing.T) {
	if got := toolEmoji("READ_FILE"); got != "📄" {
		t.Errorf("toolEmoji(READ_FILE) = %q, want 📄 (case-insensitive)", got)
	}
}

// TestToolEmoji_FalsePositive_CrashIsNotShell confirms that "crash" does not
// match the " sh " keyword (which has surrounding spaces as a deliberate guard).
func TestToolEmoji_FalsePositive_CrashIsNotShell(t *testing.T) {
	if got := toolEmoji("crash_handler"); got != "→" {
		t.Errorf("toolEmoji(crash_handler) = %q, want → (crash must not match sh keyword)", got)
	}
}
