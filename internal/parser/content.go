package parser

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// ExtractTextContent extracts readable text from message content.
// content can be a string or a JSON array of blocks.
// Returns: flattened text (with inline [Thinking] markers for UI
// compatibility), concatenated thinking-block text (no markers),
// hasThinking, hasToolUse, tool calls, and tool results.
// Thinking blocks are joined with "\n\n" to give an unambiguous
// block boundary in the concatenated thinking text.
func ExtractTextContent(
	content gjson.Result,
) (string, string, bool, bool, []ParsedToolCall, []ParsedToolResult) {
	if content.Type == gjson.String {
		return content.Str, "", false, false, nil, nil
	}

	if !content.IsArray() {
		return "", "", false, false, nil, nil
	}

	var (
		parts         []string
		thinkingParts []string
		toolCalls     []ParsedToolCall
		toolResults   []ParsedToolResult
		hasThinking   bool
		hasToolUse    bool
	)
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").Str {
		case "text":
			text := block.Get("text").Str
			if text != "" {
				parts = append(parts, text)
			}
		case "thinking":
			thinking := block.Get("thinking").Str
			if thinking != "" {
				hasThinking = true
				thinkingParts = append(thinkingParts, thinking)
				parts = append(parts,
					"[Thinking]\n"+thinking+"\n[/Thinking]")
			}
		case "tool_use", "toolCall":
			// "tool_use" is the Anthropic block type; "toolCall" is
			// the camelCase variant emitted by OpenClaw. OpenClaw
			// usually carries the call arguments under "input", but
			// some tools populate only "arguments", so fall back to
			// it when "input" is missing or empty.
			hasToolUse = true
			if tc, ok := parseToolCall(block); ok {
				toolCalls = append(toolCalls, tc)
			}
			parts = append(parts, formatToolUse(block))
		case "tool_result":
			if tr, ok := parseToolResult(block); ok {
				toolResults = append(toolResults, tr)
			}
		}
		return true
	})

	return strings.Join(parts, "\n"),
		strings.Join(thinkingParts, "\n\n"),
		hasThinking, hasToolUse, toolCalls, toolResults
}

func parseToolCall(block gjson.Result) (ParsedToolCall, bool) {
	name := block.Get("name").Str
	if name == "" {
		return ParsedToolCall{}, false
	}
	input := toolCallInput(block)
	tc := ParsedToolCall{
		ToolUseID: block.Get("id").Str,
		ToolName:  name,
		Category:  NormalizeToolCategory(name),
		InputJSON: input.Raw,
	}
	switch name {
	case "Skill":
		tc.SkillName = input.Get("skill").Str
	case "skill":
		tc.SkillName = input.Get("skill").Str
		if tc.SkillName == "" {
			tc.SkillName = input.Get("name").Str
		}
	default:
		tc.SkillName = inferToolSkillName(name, tc.InputJSON)
	}
	return tc, true
}

func toolCallInput(block gjson.Result) gjson.Result {
	input := block.Get("input")
	if input.Raw == "" || input.Raw == "{}" {
		if args := block.Get("arguments"); args.Exists() {
			input = args
		}
	}
	return input
}

func parseToolResult(block gjson.Result) (ParsedToolResult, bool) {
	tuid := block.Get("tool_use_id").Str
	if tuid == "" {
		return ParsedToolResult{}, false
	}
	rc := block.Get("content")
	return ParsedToolResult{
		ToolUseID:     tuid,
		ContentLength: toolResultContentLength(rc),
		ContentRaw:    rc.Raw,
	}, true
}

func toolResultContentLength(content gjson.Result) int {
	if content.Type == gjson.String {
		return len(content.Str)
	}
	if content.IsArray() {
		total := 0
		content.ForEach(func(_, block gjson.Result) bool {
			total += len(block.Get("text").Str)
			return true
		})
		return total
	}
	// iFlow tool results use an object with nested output at
	// responseParts.functionResponse.response.output.
	if content.IsObject() {
		return len(content.Get(
			"responseParts.functionResponse.response.output",
		).Str)
	}
	return 0
}

// DecodeContent extracts the text from a raw JSON tool result content
// value (the ContentRaw field of ParsedToolResult). It handles both
// plain string and array-of-blocks formats.
func DecodeContent(raw string) string {
	return decodeContent(gjson.Parse(raw))
}

func decodeContent(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.Str
	}
	if content.IsArray() {
		var parts []string
		content.ForEach(func(_, block gjson.Result) bool {
			if t := block.Get("text").Str; t != "" {
				parts = append(parts, t)
			}
			return true
		})
		return strings.Join(parts, "")
	}
	// iFlow tool results use an object with nested output.
	if content.IsObject() {
		return content.Get(
			"responseParts.functionResponse.response.output",
		).Str
	}
	return ""
}

var todoIcons = map[string]string{
	"completed":   "✓",
	"in_progress": "→",
	"pending":     "○",
}

func formatToolUse(block gjson.Result) string {
	name := block.Get("name").Str
	input := block.Get("input")
	if input.Raw == "" || input.Raw == "{}" {
		// OpenClaw emits some tool calls with args only under
		// "arguments" rather than "input".
		if args := block.Get("arguments"); args.Exists() {
			input = args
		}
	}

	switch name {
	case "AskUserQuestion":
		return formatAskUserQuestion(name, input)
	case "TodoWrite":
		return formatTodoWrite(input)
	case "EnterPlanMode":
		return "[Entering Plan Mode]"
	case "ExitPlanMode":
		return "[Exiting Plan Mode]"
	case "Read":
		// Claude Code uses "file_path"; Amp uses "path"
		path := input.Get("file_path").Str
		if path == "" {
			path = input.Get("path").Str
		}
		return fmt.Sprintf("[Read: %s]", path)
	case "Glob":
		return formatGlob(input)
	case "Grep":
		return fmt.Sprintf("[Grep: %s]", input.Get("pattern").Str)
	case "Edit":
		return fmt.Sprintf("[Edit: %s]", input.Get("file_path").Str)
	case "Write":
		return fmt.Sprintf("[Write: %s]", input.Get("file_path").Str)
	case "Bash":
		// Claude Code uses "command"; Amp uses "cmd"
		if input.Get("command").Str == "" && input.Get("cmd").Str != "" {
			return fmt.Sprintf("[Bash]\n$ %s", input.Get("cmd").Str)
		}
		return formatBash(input)
	// Amp tools
	case "edit_file":
		return fmt.Sprintf("[Edit: %s]", input.Get("path").Str)
	case "create_file":
		return fmt.Sprintf("[Write: %s]", input.Get("path").Str)
	case "shell_command":
		return fmt.Sprintf("[Bash]\n$ %s", input.Get("command").Str)
	case "glob":
		return fmt.Sprintf("[Glob: %s]", input.Get("filePattern").Str)
	case "look_at":
		return fmt.Sprintf("[Read: %s]", input.Get("path").Str)
	case "apply_patch":
		return formatPatch(input)
	case "ApplyPatch":
		return formatPatch(input)
	case "undo_edit":
		return fmt.Sprintf("[Undo: %s]", input.Get("path").Str)
	case "finder":
		return fmt.Sprintf("[Find: %s]", input.Get("query").Str)
	case "read_web_page":
		return fmt.Sprintf("[Web: %s]", input.Get("url").Str)
	// Pi tools (lowercase variants)
	case "read":
		return fmt.Sprintf("[Read: %s]", resolveFilePath(input))
	case "read_file":
		return fmt.Sprintf("[Read: %s]", resolveFilePath(input))
	case "write":
		return fmt.Sprintf("[Write: %s]", resolveFilePath(input))
	case "edit":
		return fmt.Sprintf("[Edit: %s]", resolveFilePath(input))
	case "str_replace":
		return fmt.Sprintf("[Edit: %s]", resolveFilePath(input))
	case "bash":
		cmd := input.Get("command").Str
		if cmd == "" {
			cmd = input.Get("cmd").Str
		}
		desc := input.Get("description").Str
		if desc != "" {
			return fmt.Sprintf("[Bash: %s]\n$ %s", desc, cmd)
		}
		return fmt.Sprintf("[Bash]\n$ %s", cmd)
	case "run_command":
		return fmt.Sprintf("[Bash]\n$ %s", input.Get("command").Str)
	case "find":
		pattern := input.Get("pattern").Str
		if pattern == "" {
			pattern = input.Get("query").Str
		}
		return fmt.Sprintf("[Find: %s]", pattern)
	case "skill":
		skill := input.Get("skill").Str
		if skill == "" {
			skill = input.Get("name").Str
		}
		return fmt.Sprintf("[Skill: %s]", skill)
	case "Task", "Agent":
		return formatTask(input)
	case "Skill":
		return fmt.Sprintf("[Skill: %s]", input.Get("skill").Str)
	case "TaskCreate":
		subject := input.Get("subject").Str
		if subject != "" {
			return fmt.Sprintf("[TaskCreate: %s]", subject)
		}
		return "[TaskCreate]"
	case "TaskUpdate":
		taskID := input.Get("taskId").Str
		status := input.Get("status").Str
		if status != "" {
			return fmt.Sprintf("[TaskUpdate: #%s %s]", taskID, status)
		}
		return fmt.Sprintf("[TaskUpdate: #%s]", taskID)
	case "TaskGet":
		return fmt.Sprintf("[TaskGet: #%s]", input.Get("taskId").Str)
	case "TaskList":
		return "[TaskList]"
	case "SendMessage":
		msgType := input.Get("type").Str
		recipient := input.Get("recipient").Str
		if recipient != "" {
			return fmt.Sprintf("[SendMessage: %s to %s]", msgType, recipient)
		}
		return fmt.Sprintf("[SendMessage: %s]", msgType)
	default:
		// MCP tools may have a server prefix (e.g.
		// "Zencoder_subagent__ZencoderSubagent").
		if strings.Contains(name, "subagent") {
			return formatTask(input)
		}
		return fmt.Sprintf("[Tool: %s]", name)
	}
}

func formatAskUserQuestion(
	name string, input gjson.Result,
) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("[Question: %s]", name))
	input.Get("questions").ForEach(func(_, q gjson.Result) bool {
		lines = append(lines, "  "+q.Get("question").Str)
		q.Get("options").ForEach(func(_, opt gjson.Result) bool {
			lines = append(lines, fmt.Sprintf(
				"    - %s: %s",
				opt.Get("label").Str,
				opt.Get("description").Str,
			))
			return true
		})
		return true
	})
	return strings.Join(lines, "\n")
}

func formatTodoWrite(input gjson.Result) string {
	var lines []string
	lines = append(lines, "[Todo List]")
	input.Get("todos").ForEach(func(_, todo gjson.Result) bool {
		status := todo.Get("status").Str
		icon := todoIcons[status]
		if icon == "" {
			icon = "○"
		}
		lines = append(lines, fmt.Sprintf(
			"  %s %s", icon, todo.Get("content").Str,
		))
		return true
	})
	return strings.Join(lines, "\n")
}

func formatGlob(input gjson.Result) string {
	return fmt.Sprintf("[Glob: %s in %s]",
		input.Get("pattern").Str,
		orDefault(input.Get("path").Str, "."))
}

func formatBash(input gjson.Result) string {
	cmd := input.Get("command").Str
	desc := input.Get("description").Str
	if desc != "" {
		return fmt.Sprintf("[Bash: %s]\n$ %s", desc, cmd)
	}
	return fmt.Sprintf("[Bash]\n$ %s", cmd)
}

func formatPatch(input gjson.Result) string {
	path := resolveFilePath(input)
	if path == "" {
		return "[Patch]"
	}
	return fmt.Sprintf("[Patch: %s]", path)
}

func formatTask(input gjson.Result) string {
	desc := input.Get("description").Str
	if desc == "" {
		desc = input.Get("prompt").Str
	}
	agentType := input.Get("subagent_type").Str
	if agentType == "" {
		agentType = input.Get("agent").Str
	}
	if desc == "" && agentType == "" {
		return "[Task]"
	}
	if agentType == "" {
		return fmt.Sprintf("[Task: %s]", desc)
	}
	return fmt.Sprintf("[Task: %s (%s)]", desc, agentType)
}

// resolveFilePath extracts a file path from tool input, trying
// file_path, path, filePath, and file in order. Covers Claude Code,
// Amp, Pi, and Kiro IDE payload shapes.
func resolveFilePath(input gjson.Result) string {
	if p := input.Get("file_path").Str; p != "" {
		return p
	}
	if p := input.Get("path").Str; p != "" {
		return p
	}
	if p := input.Get("filePath").Str; p != "" {
		return p
	}
	return input.Get("file").Str
}

// ResolveFilePathFromJSON extracts a file path from a tool call's raw input
// JSON, checking file_path, path, filePath, then file. It returns "" when the
// input is not valid JSON (e.g. a raw diff string) or carries no path key.
func ResolveFilePathFromJSON(inputJSON string) string {
	if inputJSON == "" || !gjson.Valid(inputJSON) {
		return ""
	}
	return resolveFilePath(gjson.Parse(inputJSON))
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
