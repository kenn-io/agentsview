package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

type commandCodeMeta struct {
	Title       string `json:"title"`
	UserRenamed bool   `json:"userRenamed"`
	ProjectPath string `json:"projectPath"`
	Cwd         string `json:"cwd"`
}

// parseSession parses a Command Code JSONL transcript.
func (p *commandCodeProvider) parseSession(
	path, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	lr := newLineReader(f, maxLineSize)
	var (
		sessionID      string
		cwd            string
		gitBranch      string
		firstMessage   string
		startedAt      time.Time
		endedAt        time.Time
		ordinal        int
		userCount      int
		malformedLines int
		messages       []ParsedMessage
	)

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			malformedLines++
			continue
		}

		root := gjson.Parse(line)
		if sessionID == "" {
			sessionID = root.Get("sessionId").Str
		}
		if cwd == "" {
			cwd = commandCodeCwd(root)
		}
		if gitBranch == "" {
			gitBranch = root.Get("gitBranch").Str
		}

		ts := parseTimestamp(root.Get("timestamp").Str)
		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if endedAt.IsZero() || ts.After(endedAt) {
				endedAt = ts
			}
		}

		role := root.Get("role").Str
		content := root.Get("content")
		text, thinking, hasThinking, hasToolUse, toolCalls, toolResults :=
			extractCommandCodeContent(content)
		text = strings.TrimSpace(text)

		switch role {
		case "user":
			if text == "" && len(toolResults) == 0 {
				continue
			}
			if firstMessage == "" && text != "" {
				firstMessage = truncate(
					strings.ReplaceAll(text, "\n", " "),
					300,
				)
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       text,
				ThinkingText:  thinking,
				Timestamp:     ts,
				HasThinking:   hasThinking,
				HasToolUse:    hasToolUse,
				ContentLength: len(text),
				ToolCalls:     toolCalls,
				ToolResults:   toolResults,
			})
			ordinal++
			if text != "" {
				userCount++
			}

		case "assistant":
			if text == "" && !hasThinking &&
				len(toolCalls) == 0 && len(toolResults) == 0 {
				continue
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       text,
				ThinkingText:  thinking,
				Timestamp:     ts,
				HasThinking:   hasThinking,
				HasToolUse:    hasToolUse,
				ContentLength: len(text),
				ToolCalls:     toolCalls,
				ToolResults:   toolResults,
			})
			ordinal++

		case "tool":
			if len(toolResults) == 0 {
				continue
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Timestamp:     ts,
				ContentLength: 0,
				ToolResults:   toolResults,
			})
			ordinal++
		}
	}

	if err := lr.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading command-code %s: %w", path, err)
	}
	if len(messages) == 0 {
		return nil, nil, nil
	}

	meta := loadCommandCodeMeta(path)
	if meta != nil {
		if cwd == "" {
			cwd = commandCodeFirstNonEmpty(meta.Cwd, meta.ProjectPath)
		}
		if firstMessage == "" {
			firstMessage = meta.Title
		}
	}
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}

	project := ExtractProjectFromCwd(cwd)
	if project == "" {
		project = NormalizeName(filepath.Base(filepath.Dir(path)))
	}

	sessionName := ""
	if meta != nil {
		sessionName = meta.Title
	}

	sess := &ParsedSession{
		ID:               "commandcode:" + sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentCommandCode,
		Cwd:              cwd,
		GitBranch:        gitBranch,
		SourceVersion:    "2",
		MalformedLines:   malformedLines,
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

func loadCommandCodeMeta(path string) *commandCodeMeta {
	metaPath := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil
	}
	var meta commandCodeMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil
	}
	return &meta
}

func commandCodeCwd(root gjson.Result) string {
	for _, key := range []string{
		"metadata.cwd",
		"metadata.projectPath",
		"metadata.context.cwd",
		"cwd",
	} {
		if v := root.Get(key).Str; v != "" {
			return v
		}
	}
	return ""
}

func extractCommandCodeContent(
	content gjson.Result,
) (string, string, bool, bool, []ParsedToolCall, []ParsedToolResult) {
	if content.Type == gjson.String {
		return content.Str, "", false, false, nil, nil
	}
	if !content.IsArray() {
		return "", "", false, false, nil, nil
	}

	var (
		textParts     []string
		thinkingParts []string
		toolCalls     []ParsedToolCall
		toolResults   []ParsedToolResult
		hasThinking   bool
		hasToolUse    bool
	)

	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").Str {
		case "text":
			if text := block.Get("text").Str; text != "" {
				textParts = append(textParts, text)
			}
		case "reasoning", "thinking":
			thinking := commandCodeFirstNonEmpty(
				block.Get("text").Str,
				block.Get("thinking").Str,
			)
			if thinking != "" {
				hasThinking = true
				thinkingParts = append(thinkingParts, thinking)
			}
		case "tool-call", "tool_use":
			toolName := commandCodeFirstNonEmpty(
				block.Get("toolName").Str,
				block.Get("name").Str,
			)
			if toolName == "" {
				return true
			}
			hasToolUse = true
			input := block.Get("input")
			inputJSON := input.Raw
			if inputJSON == "" {
				inputJSON = "{}"
			}
			toolCalls = append(toolCalls, ParsedToolCall{
				ToolUseID: blockID(block, "toolCallId", "id"),
				ToolName:  toolName,
				Category:  NormalizeToolCategory(toolName),
				InputJSON: inputJSON,
			})
		case "tool-result", "tool_result":
			toolUseID := blockID(block, "toolCallId", "tool_use_id")
			contentRaw, contentLen := commandCodeToolResultContent(block)
			if toolUseID == "" || contentRaw == "" {
				return true
			}
			toolResults = append(toolResults, ParsedToolResult{
				ToolUseID:     toolUseID,
				ContentLength: contentLen,
				ContentRaw:    contentRaw,
			})
		}
		return true
	})

	return strings.Join(textParts, "\n"),
		strings.Join(thinkingParts, "\n\n"),
		hasThinking, hasToolUse, toolCalls, toolResults
}

func commandCodeToolResultContent(block gjson.Result) (string, int) {
	output := block.Get("output")
	if !output.Exists() {
		output = block.Get("content")
	}
	if output.Exists() {
		if output.IsObject() {
			if value := output.Get("value"); value.Exists() &&
				value.Type == gjson.String {
				return strconv.Quote(value.Str), len(value.Str)
			}
		}
		return output.Raw, toolResultContentLength(output)
	}

	for _, key := range []string{"text", "error", "value"} {
		if text := block.Get(key).Str; text != "" {
			return strconv.Quote(text), len(text)
		}
	}
	return "", 0
}

func blockID(block gjson.Result, keys ...string) string {
	for _, key := range keys {
		if value := block.Get(key).Str; value != "" {
			return value
		}
	}
	return ""
}

func commandCodeFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
