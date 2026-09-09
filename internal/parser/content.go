package parser

import (
	"encoding/json"
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
			rendering := formatToolUse(block)
			if tc, ok := parseToolCall(block); ok {
				tc.Rendering = rendering
				toolCalls = append(toolCalls, tc)
			}
			parts = append(parts, rendering)
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

// ToolUseRendering returns the text formatToolUse inlines into message
// content for a tool call with the given name and raw input JSON, or "" when
// the input is not an object.
func ToolUseRendering(name, inputJSON string) string {
	block, ok := toolUseBlock(name, inputJSON)
	if !ok {
		return ""
	}
	return formatToolUse(block)
}

// RedactedToolUseRendering is ToolUseRendering with every argument except
// the call's target path removed. Storage policies that keep tool metadata
// but drop tool inputs replace the full rendering inside message text with
// it, so a transcript still shows which tool ran and on which file while
// commands, patterns, prompts, and other arguments leave the archive.
func RedactedToolUseRendering(name, inputJSON string) string {
	block, ok := toolUseBlock(name, inputJSON)
	if !ok {
		return ""
	}
	reduced, ok := toolUseBlock(
		name, reduceToolInputToPaths(block.Get("input").Raw),
	)
	if !ok {
		return ""
	}
	return collapseEmptyRenderingDetail(formatToolUse(reduced))
}

func toolUseBlock(name, inputJSON string) (gjson.Result, bool) {
	if name == "" {
		return gjson.Result{}, false
	}
	input := strings.TrimSpace(inputJSON)
	if input == "" {
		input = "{}"
	}
	if !gjson.Valid(input) {
		return gjson.Result{}, false
	}
	nameJSON, err := json.Marshal(name)
	if err != nil {
		return gjson.Result{}, false
	}
	return gjson.Parse(
		`{"name":` + string(nameJSON) + `,"input":` + input + `}`,
	), true
}

// ToolUseRenderingPair is one text a parser may inline for a tool call and
// the form that keeps only the tool label and target path.
type ToolUseRenderingPair struct {
	Full     string
	Redacted string
}

// ToolUseRenderingCandidates regenerates every rendering a parser could have
// inlined for a call from its stored name and input. Copied rows carry no
// record of which renderer produced their text, so callers try each pair.
func ToolUseRenderingCandidates(
	category, name, inputJSON string,
) []ToolUseRenderingPair {
	candidates := []ToolUseRenderingPair{
		{
			Full:     ToolUseRendering(name, inputJSON),
			Redacted: RedactedToolUseRendering(name, inputJSON),
		},
		{
			Full:     CortexToolUseRendering(category, name, inputJSON),
			Redacted: CortexRedactedToolUseRendering(category, name),
		},
		{
			Full:     geminiToolUseRendering(name, inputJSON),
			Redacted: geminiRedactedToolUseRendering(name, inputJSON),
		},
		{
			Full:     formatToolHeader(category, agyToolDetail(name, inputJSON)),
			Redacted: formatToolHeader(category, name),
		},
		{
			Full:     codexToolUseRendering(name, inputJSON),
			Redacted: collapseToolHeader(codexToolUseRendering(name, inputJSON)),
		},
		{
			Full:     grokToolUseRendering(name, inputJSON),
			Redacted: collapseToolHeader(grokToolUseRendering(name, inputJSON)),
		},
		{
			Full: formatVSCodeCopilotToolCalls([]ParsedToolCall{{
				Category: category, ToolName: name, InputJSON: inputJSON,
			}}),
			Redacted: formatToolHeader(category, name),
		},
		{
			Full: formatKimiToolUse(name, gjson.Parse(inputJSON)),
			Redacted: collapseEmptyRenderingDetail(formatKimiToolUse(
				name, gjson.Parse(reduceToolInputToPaths(inputJSON)),
			)),
		},
		{
			Full:     openHandsToolUseRendering(name, inputJSON),
			Redacted: collapseToolHeader(openHandsToolUseRendering(name, inputJSON)),
		},
	}
	// A pair whose redacted form equals its full form still matters: it
	// tells the caller the rendering carried nothing to remove.
	kept := candidates[:0]
	for _, pair := range candidates {
		if pair.Full != "" {
			kept = append(kept, pair)
		}
	}
	return kept
}

// RedactToolUseRendering returns the redacted form of full, the exact text a
// parser inlined for a call. It prefers the renderer that produced full and
// otherwise keeps only the first header line with its detail removed, so a
// rendering from any provider loses its arguments even when it cannot be
// regenerated.
func RedactToolUseRendering(full, category, name, inputJSON string) string {
	for _, pair := range ToolUseRenderingCandidates(category, name, inputJSON) {
		if pair.Full == full {
			return pair.Redacted
		}
	}
	return collapseToolHeader(full)
}

// collapseToolHeader keeps the bracketed tool label that opens a rendering
// and drops everything else: the detail after a colon inside the brackets,
// any text after the closing bracket, and every later line.
func collapseToolHeader(rendering string) string {
	first, _, _ := strings.Cut(rendering, "\n")
	first = strings.TrimSpace(first)
	if !strings.HasPrefix(first, "[") {
		return ""
	}
	inside, _, found := strings.Cut(first[1:], "]")
	if !found {
		return ""
	}
	label, _, _ := strings.Cut(inside, ":")
	label = strings.TrimSpace(label)
	if label == "" {
		return ""
	}
	return "[" + label + "]"
}

func geminiToolUseRendering(name, inputJSON string) string {
	call, ok := geminiToolCallBlock(name, inputJSON)
	if !ok {
		return ""
	}
	return formatGeminiToolCall(call)
}

func geminiRedactedToolUseRendering(name, inputJSON string) string {
	call, ok := geminiToolCallBlock(name, reduceToolInputToPaths(inputJSON))
	if !ok {
		return ""
	}
	return collapseEmptyRenderingDetail(formatGeminiToolCall(call))
}

func geminiToolCallBlock(name, inputJSON string) (gjson.Result, bool) {
	block, ok := toolUseBlock(name, inputJSON)
	if !ok {
		return gjson.Result{}, false
	}
	return gjson.Parse(
		`{"name":` + block.Get("name").Raw + `,"args":` + block.Get("input").Raw + `}`,
	), true
}

func codexToolUseRendering(name, inputJSON string) string {
	block, ok := toolUseBlock(name, inputJSON)
	if !ok {
		return ""
	}
	return formatCodexFunctionCall(
		name, gjson.Parse(`{"arguments":`+block.Get("input").Raw+`}`),
	)
}

// reduceToolInputToPaths keeps only the path-like arguments of a tool input.
func reduceToolInputToPaths(inputJSON string) string {
	input := gjson.Parse(strings.TrimSpace(inputJSON))
	kept := map[string]string{}
	for _, key := range []string{"file_path", "path", "notebook_path", "dir_path"} {
		if value := input.Get(key).Str; value != "" {
			kept[key] = value
		}
	}
	reduced, err := json.Marshal(kept)
	if err != nil {
		return "{}"
	}
	return string(reduced)
}

// collapseEmptyRenderingDetail drops the empty detail or command line a
// rendering built around a removed argument leaves behind.
func collapseEmptyRenderingDetail(rendering string) string {
	lines := strings.Split(rendering, "\n")
	collapsed := lines[:0]
	for _, line := range lines {
		if line == "$ " || line == "$" {
			continue
		}
		collapsed = append(collapsed, strings.Replace(line, ": ]", "]", 1))
	}
	return strings.Join(collapsed, "\n")
}

// grokToolUseRendering rebuilds the summary the Grok parser inlines for a
// backend tool call from the input it stored for that call.
func grokToolUseRendering(name, inputJSON string) string {
	input := strings.TrimSpace(inputJSON)
	if input == "" {
		return ""
	}
	var payload gjson.Result
	switch name {
	case "web_search":
		if !gjson.Valid(input) {
			return ""
		}
		payload = gjson.Parse(`{"action":` + input + `}`)
	case "x_search":
		quoted, err := json.Marshal(input)
		if err != nil {
			return ""
		}
		payload = gjson.Parse(`{"input":` + string(quoted) + `}`)
	case "code_interpreter":
		if !gjson.Valid(input) {
			return ""
		}
		payload = gjson.Parse(input)
	default:
		return ""
	}
	return grokBackendToolSummary(name, payload)
}

// openHandsToolUseRendering rebuilds the action text the OpenHands parser
// inlines for a call from its stored arguments. An event summary folded into
// the header cannot be rebuilt, so that case relies on the recorded
// rendering at write time.
func openHandsToolUseRendering(name, inputJSON string) string {
	input := strings.TrimSpace(inputJSON)
	if input == "" || !gjson.Valid(input) {
		return ""
	}
	return strings.TrimSpace(formatOpenHandsAction(name, gjson.Parse(input), ""))
}
