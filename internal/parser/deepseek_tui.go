package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
)

const deepSeekTUIPrefix = "deepseek-tui:"

func parseDeepSeekTUISession(
	path, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if !gjson.ValidBytes(data) {
		return nil, nil, fmt.Errorf("invalid JSON in %s", path)
	}

	root := gjson.ParseBytes(data)
	rawID := root.Get("metadata.id").Str
	if !IsValidSessionID(rawID) {
		rawID = deepSeekTUISessionIDFromPath(path)
	}
	if rawID == "" {
		return nil, nil, fmt.Errorf("missing or invalid id in %s", path)
	}

	metadata := root.Get("metadata")
	workspace := metadata.Get("workspace").Str
	project := ExtractProjectFromCwd(workspace)
	if project == "" {
		project = "deepseek_tui"
	}
	model := metadata.Get("model").Str

	startedAt := parseTimestamp(metadata.Get("created_at").Str)
	endedAt := parseTimestamp(metadata.Get("updated_at").Str)

	var (
		messages     []ParsedMessage
		firstMessage string
		ordinal      int
	)
	root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
		roleStr := msg.Get("role").Str
		role, ok := deepSeekTUIRole(roleStr)
		if !ok {
			return true
		}

		ts := parseTimestamp(msg.Get("timestamp").Str)
		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(endedAt) {
				endedAt = ts
			}
		}

		content, thinking, hasThinking, hasToolUse, calls, results :=
			extractDeepSeekTUIContent(msg.Get("content"))
		if strings.TrimSpace(content) == "" && len(calls) == 0 &&
			len(results) == 0 {
			return true
		}
		if role == RoleUser && firstMessage == "" &&
			strings.TrimSpace(content) != "" {
			firstMessage = truncate(
				strings.ReplaceAll(content, "\n", " "),
				300,
			)
		}

		msgModel := msg.Get("model").Str
		if msgModel == "" {
			msgModel = model
		}
		messages = append(messages, ParsedMessage{
			Ordinal:       ordinal,
			Role:          role,
			Content:       content,
			ThinkingText:  thinking,
			Timestamp:     ts,
			HasThinking:   hasThinking,
			HasToolUse:    hasToolUse,
			ContentLength: len(content),
			ToolCalls:     calls,
			ToolResults:   results,
			Model:         msgModel,
		})
		ordinal++
		return true
	})

	if len(messages) == 0 {
		return nil, nil, nil
	}

	sessionName := metadata.Get("title").Str
	if firstMessage == "" {
		firstMessage = sessionName
	}
	userCount := 0
	for _, msg := range messages {
		if msg.Role == RoleUser && strings.TrimSpace(msg.Content) != "" {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:               deepSeekTUIPrefix + rawID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentDeepSeekTUI,
		Cwd:              workspace,
		FirstMessage:     firstMessage,
		SessionName:      sessionName,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}
	return sess, messages, nil
}

func isDeepSeekTUISessionFile(name string) bool {
	if name == "latest.json" || name == "offline_queue.json" {
		return false
	}
	stem, ok := strings.CutSuffix(name, ".json")
	return ok && IsValidSessionID(stem)
}

func deepSeekTUISessionIDFromPath(path string) string {
	name := filepath.Base(path)
	stem, ok := strings.CutSuffix(name, ".json")
	if !ok || !IsValidSessionID(stem) {
		return ""
	}
	return stem
}

func deepSeekTUIRole(role string) (RoleType, bool) {
	switch role {
	case "user":
		return RoleUser, true
	case "assistant":
		return RoleAssistant, true
	default:
		return "", false
	}
}

func extractDeepSeekTUIContent(
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
		calls         []ParsedToolCall
		results       []ParsedToolResult
		hasThinking   bool
		hasToolUse    bool
	)
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").Str {
		case "text":
			if text := block.Get("text").Str; text != "" {
				parts = append(parts, text)
			}
		case "thinking":
			thinking := block.Get("thinking").Str
			if thinking == "" {
				thinking = block.Get("text").Str
			}
			if thinking != "" {
				hasThinking = true
				thinkingParts = append(thinkingParts, thinking)
				parts = append(parts,
					"[Thinking]\n"+thinking+"\n[/Thinking]")
			}
		case "tool_use", "server_tool_use":
			if call, ok := deepSeekTUIToolCall(block); ok {
				hasToolUse = true
				calls = append(calls, call)
				parts = append(parts, formatToolUse(block))
			}
		case "tool_result", "tool_search_tool_result",
			"code_execution_tool_result":
			if result, ok := deepSeekTUIToolResult(block); ok {
				results = append(results, result)
			}
		}
		return true
	})

	return strings.Join(parts, "\n"),
		strings.Join(thinkingParts, "\n\n"),
		hasThinking, hasToolUse, calls, results
}

func deepSeekTUIToolCall(block gjson.Result) (ParsedToolCall, bool) {
	name := block.Get("name").Str
	if name == "" {
		name = block.Get("tool_name").Str
	}
	if name == "" {
		return ParsedToolCall{}, false
	}
	input := block.Get("input")
	if !input.Exists() {
		input = block.Get("parameters")
	}
	return ParsedToolCall{
		ToolUseID: block.Get("id").Str,
		ToolName:  name,
		Category:  NormalizeToolCategory(name),
		InputJSON: input.Raw,
	}, true
}

func deepSeekTUIToolResult(block gjson.Result) (ParsedToolResult, bool) {
	toolUseID := block.Get("tool_use_id").Str
	if toolUseID == "" {
		toolUseID = block.Get("toolUseID").Str
	}
	if toolUseID == "" {
		toolUseID = block.Get("id").Str
	}
	if toolUseID == "" {
		return ParsedToolResult{}, false
	}

	content := block.Get("content")
	if !content.Exists() {
		content = block.Get("result")
	}
	if !content.Exists() {
		content = block.Get("output")
	}
	return ParsedToolResult{
		ToolUseID:     toolUseID,
		ContentLength: deepSeekTUIContentLength(content),
		ContentRaw:    deepSeekTUIResultRaw(content),
	}, true
}

// deepSeekTUIObjectText extracts the string content from an object-shaped
// tool result such as {"output":"..."}. It reports whether a known field
// holds a string value, treating an empty string as present so a valid
// no-output result is not mistaken for a missing field.
func deepSeekTUIObjectText(content gjson.Result) (string, bool) {
	for _, key := range []string{"output", "text", "content"} {
		if field := content.Get(key); field.Exists() &&
			field.Type == gjson.String {
			return field.Str, true
		}
	}
	return "", false
}

func deepSeekTUIResultRaw(content gjson.Result) string {
	// Object results such as {"output":"..."} are not recognized by
	// DecodeContent's object branch (which handles only the iFlow shape),
	// so extract the known string field and store it as a plain string.
	if content.IsObject() {
		if text, ok := deepSeekTUIObjectText(content); ok {
			quoted, _ := json.Marshal(text)
			return string(quoted)
		}
	}
	if content.Raw == "" {
		return `""`
	}
	return content.Raw
}

func deepSeekTUIContentLength(content gjson.Result) int {
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
	if content.IsObject() {
		if text, ok := deepSeekTUIObjectText(content); ok {
			return len(text)
		}
	}
	if content.Raw == "" {
		return 0
	}
	var decoded any
	if err := json.Unmarshal([]byte(content.Raw), &decoded); err == nil {
		if text, ok := decoded.(string); ok {
			return len(text)
		}
	}
	return len(content.Raw)
}
