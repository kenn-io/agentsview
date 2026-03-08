package parser

import "testing"

func TestNormalizeToolCategory(t *testing.T) {
	tests := []struct {
		toolName string
		want     string
	}{
		// Claude Code tools
		{"Read", "Read"},
		{"Edit", "Edit"},
		{"Write", "Write"},
		{"NotebookEdit", "Write"},
		{"Bash", "Bash"},
		{"Grep", "Grep"},
		{"Glob", "Glob"},
		{"Task", "Task"},
		{"Agent", "Task"},
		{"Skill", "Tool"},

		// Codex tools
		{"shell_command", "Bash"},
		{"exec_command", "Bash"},
		{"apply_patch", "Edit"},
		{"write_stdin", "Bash"},
		{"shell", "Bash"},

		// Gemini tools
		{"read_file", "Read"},
		{"write_file", "Write"},
		{"edit_file", "Write"},
		{"run_command", "Bash"},
		{"execute_command", "Bash"},
		{"search_files", "Grep"},
		{"grep", "Grep"},

		// OpenCode tools (lowercase)
		// "grep" already tested above in Gemini section.
		{"read", "Read"},
		{"edit", "Edit"},
		{"write", "Write"},
		{"bash", "Bash"},
		{"glob", "Glob"},
		{"task", "Task"},

		// Amp tools
		{"create_file", "Write"},
		{"look_at", "Read"},
		{"undo_edit", "Edit"},
		{"finder", "Grep"},
		{"read_web_page", "Read"},
		{"skill", "Tool"},

		// Pi tools
		{"str_replace", "Edit"},
		{"find", "Read"},

		// Copilot tools
		{"view", "Read"},
		{"report_intent", "Tool"},

		// Unknown
		{"view_image", "Other"},
		{"update_plan", "Other"},
		{"list_mcp_resources", "Other"},
		{"AskUserQuestion", "Other"},
		{"EnterPlanMode", "Other"},
		{"ExitPlanMode", "Other"},
		{"", "Other"},
		{"some_random_tool", "Other"},
	}

	for _, tt := range tests {
		testName := tt.toolName
		if testName == "" {
			testName = "empty_string"
		}
		t.Run(testName, func(t *testing.T) {
			got := NormalizeToolCategory(tt.toolName)
			if got != tt.want {
				t.Errorf(
					"NormalizeToolCategory(%q) = %q, want %q",
					tt.toolName, got, tt.want,
				)
			}
		})
	}
}
