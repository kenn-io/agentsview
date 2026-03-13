package parser

import "strings"

// NormalizeToolCategory maps a raw tool name to a normalized
// category. Categories: Read, Edit, Write, Bash, Grep, Glob,
// Task, Tool, Other.
func NormalizeToolCategory(rawName string) string {
	switch rawName {
	// Claude Code tools
	case "Read":
		return "Read"
	case "Edit":
		return "Edit"
	case "Write", "NotebookEdit":
		return "Write"
	case "Bash":
		return "Bash"
	case "Grep":
		return "Grep"
	case "Glob":
		return "Glob"
	case "Task", "Agent":
		return "Task"
	case "Skill":
		return "Tool"

	// Codex tools
	case "shell_command", "exec_command",
		"write_stdin", "shell":
		return "Bash"
	case "apply_patch":
		return "Edit"

	// Gemini tools
	case "read_file", "list_directory":
		return "Read"
	case "write_file":
		return "Write"
	case "edit_file", "replace":
		return "Edit"
	case "run_command", "execute_command", "run_shell_command":
		return "Bash"
	case "search_files", "grep", "grep_search":
		return "Grep"

	// OpenCode tools (lowercase variants)
	// Note: "grep" is handled above in the Gemini section.
	case "read":
		return "Read"
	case "edit":
		return "Edit"
	case "write":
		return "Write"
	case "bash":
		return "Bash"
	case "glob":
		return "Glob"
	case "task":
		return "Task"

	// Copilot tools
	// Note: "edit_file" (Edit), "shell" (Bash), "grep" (Grep),
	// and "glob" (Glob) are handled in earlier sections.
	case "view":
		return "Read"
	case "report_intent":
		return "Tool"

	// Cursor (file-based JSONL) tools
	case "Shell":
		return "Bash"
	case "StrReplace":
		return "Edit"
	case "LS":
		return "Read"

	// Cursor vscdb tool names
	// Note: "apply_patch" is handled above (Codex section).
	// Note: "web_search", "web_fetch" handled below (OpenClaw).
	case "run_terminal_command_v2", "run_terminal_cmd":
		return "Bash"
	case "read_file_v2":
		return "Read"
	case "edit_file_v2", "search_replace":
		return "Edit"
	case "ripgrep_raw_search", "rg":
		return "Grep"
	case "glob_file_search", "file_search":
		return "Glob"
	case "task_v2":
		return "Task"
	case "delete_file":
		return "Write"
	case "list_dir_v2", "list_dir":
		return "Read"
	case "read_lints":
		return "Read"
	case "todo_write", "create_plan", "ask_question",
		"switch_mode", "codebase_search",
		"semantic_search_full":
		return "Tool"

	// Amp tools (not already covered above)
	// Note: "create_file" is also used by Pi.
	case "create_file":
		return "Write"
	case "look_at":
		return "Read"
	case "undo_edit":
		return "Edit"
	case "finder":
		return "Grep"
	case "read_web_page":
		return "Read"
	case "skill":
		return "Tool"

	// Pi tools (not already covered above)
	// Note: "grep", "run_command", "read_file", "create_file" are handled above.
	case "find":
		return "Read"
	case "str_replace":
		return "Edit"

	// OpenClaw tools
	case "exec":
		return "Bash"
	case "process":
		return "Bash"
	case "browser", "web_search", "web_fetch":
		return "Tool"
	case "image", "canvas", "tts":
		return "Tool"
	case "message", "nodes":
		return "Tool"
	case "sessions_list", "sessions_history",
		"sessions_send", "sessions_spawn":
		return "Task"
	case "subagents", "agents_list", "session_status":
		return "Task"

	default:
		// MCP tool invocations are prefixed with "mcp-".
		if strings.HasPrefix(rawName, "mcp-") {
			return "Tool"
		}
		return "Other"
	}
}
