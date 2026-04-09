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
	case "spawn_agent":
		return "Task"

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

	// Cursor tools
	case "Shell":
		return "Bash"
	case "StrReplace":
		return "Edit"
	case "LS":
		return "Read"

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

	// Hermes Agent tools (excluding names already handled above:
	// read_file→Read, write_file→Write, search_files→Grep,
	// edit_file→Edit, run_command/execute_command→Bash)
	case "patch":
		return "Edit"
	case "terminal":
		return "Bash"
	case "browser_navigate", "browser_snapshot", "browser_click",
		"browser_type", "browser_scroll", "browser_press",
		"browser_back", "browser_close", "browser_vision",
		"browser_console", "browser_get_images":
		return "Tool"
	case "vision_analyze":
		return "Read"
	case "delegate_task":
		return "Task"
	case "execute_code":
		return "Bash"
	case "todo", "memory", "session_search", "skill_view",
		"skills_list", "skill_manage", "clarify",
		"text_to_speech", "cronjob":
		return "Tool"

	// Zencoder tools (not already covered above)
	case "WebFetch":
		return "Read"
	case "TodoWrite":
		return "Tool"
	case "subagent__ZencoderSubagent":
		return "Task"
	case "zencoder-rag-mcp__web_search":
		return "Read"

	// ChatGPT tools
	case "code_interpreter":
		return "Bash"

	// Warp tools
	case "read_files":
		return "Read"
	case "apply_file_diff":
		return "Edit"
	case "search_codebase":
		return "Grep"
	case "call_mcp_tool", "read_mcp_resource":
		return "Tool"
	case "suggest_plan", "suggest_create_plan":
		return "Tool"
	case "write_to_long_running_shell_command":
		return "Bash"
	case "read_shell_command_output":
		return "Read"
	case "use_computer":
		return "Tool"

	default:
		// MCP tools may carry a server prefix (e.g.
		// "Zencoder_subagent__ZencoderSubagent") or use
		// spawn_subagent naming ("mcp__zen_subagents__spawn_subagent").
		if strings.Contains(rawName, "subagent") {
			return "Task"
		}
		return "Other"
	}
}
