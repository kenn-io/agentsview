package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// DiscoverKimiSessions finds all wire.jsonl files under the Kimi
// sessions directory. The directory structure is:
// <sessionsDir>/<project-hash>/<session-uuid>/wire.jsonl
func DiscoverKimiSessions(sessionsDir string) []DiscoveredFile {
	if sessionsDir == "" {
		return nil
	}

	projDirs, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile
	for _, projEntry := range projDirs {
		if !isDirOrSymlink(projEntry, sessionsDir) {
			continue
		}

		projDir := filepath.Join(sessionsDir, projEntry.Name())
		sessionDirs, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}

		for _, sessEntry := range sessionDirs {
			if !isDirOrSymlink(sessEntry, projDir) {
				continue
			}
			wirePath := filepath.Join(
				projDir, sessEntry.Name(), "wire.jsonl",
			)
			if _, err := os.Stat(wirePath); err != nil {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:    wirePath,
				Project: projEntry.Name(),
				Agent:   AgentKimi,
			})
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindKimiSourceFile locates a Kimi session file by its raw
// session ID (without the "kimi:" prefix). The raw ID has the
// format "<project-hash>:<session-uuid>", which maps to
// <sessionsDir>/<project-hash>/<session-uuid>/wire.jsonl.
func FindKimiSourceFile(sessionsDir, rawID string) string {
	if sessionsDir == "" {
		return ""
	}

	projHash, sessionUUID, ok := strings.Cut(rawID, ":")
	if !ok || !IsValidSessionID(projHash) ||
		!IsValidSessionID(sessionUUID) {
		return ""
	}

	candidate := filepath.Join(
		sessionsDir, projHash, sessionUUID, "wire.jsonl",
	)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

// ParseKimiSession parses a Kimi wire.jsonl file.
// Wire.jsonl contains one JSON object per line with message types:
// metadata, TurnBegin, StepBegin, ContentPart, ToolCall,
// ToolResult, StatusUpdate, TurnEnd.
func ParseKimiSession(
	path, project, machine string,
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

	// Extract session ID from path:
	// .../sessions/<project-hash>/<session-uuid>/wire.jsonl
	dir := filepath.Dir(path) // .../sessions/<project-hash>/<session-uuid>
	sessionUUID := filepath.Base(dir)
	projHash := filepath.Base(filepath.Dir(dir))

	sessionID := projHash + ":" + sessionUUID

	var (
		messages     []ParsedMessage
		firstMessage string
		ordinal      int

		startTime time.Time
		endTime   time.Time

		// Accumulate content parts and tool calls for the
		// current assistant turn.
		pendingText     []string
		pendingToolCall []ParsedToolCall
		hasThinking     bool
		hasToolUse      bool

		// Track token usage from StatusUpdate.
		totalOutputTokens int
		peakContextTokens int

		// Current record timestamp and pending assistant
		// turn timestamp (latest seen).
		currentTS time.Time
		pendingTS time.Time
	)

	flushAssistantTurn := func() {
		content := strings.Join(pendingText, "\n")
		if strings.TrimSpace(content) == "" &&
			len(pendingToolCall) == 0 {
			pendingText = nil
			pendingToolCall = nil
			pendingTS = time.Time{}
			hasThinking = false
			hasToolUse = false
			return
		}

		messages = append(messages, ParsedMessage{
			Ordinal:       ordinal,
			Role:          RoleAssistant,
			Content:       content,
			Timestamp:     pendingTS,
			HasThinking:   hasThinking,
			HasToolUse:    hasToolUse,
			ContentLength: len(content),
			ToolCalls:     pendingToolCall,
		})
		ordinal++
		pendingText = nil
		pendingToolCall = nil
		pendingTS = time.Time{}
		hasThinking = false
		hasToolUse = false
	}

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}

		root := gjson.Parse(line)

		// Top-level "type" = "metadata" line.
		if root.Get("type").Str == "metadata" {
			continue
		}

		ts := root.Get("timestamp")
		if ts.Type == gjson.Number {
			t := time.Unix(int64(ts.Float()), int64((ts.Float()-float64(int64(ts.Float())))*1e9))
			if startTime.IsZero() || t.Before(startTime) {
				startTime = t
			}
			if t.After(endTime) {
				endTime = t
			}
			currentTS = t
		}

		msgType := root.Get("message.type").Str
		payload := root.Get("message.payload")

		switch msgType {
		case "TurnBegin":
			// Flush any pending assistant content from a
			// previous turn.
			flushAssistantTurn()

			// Extract user input text.
			var userParts []string
			payload.Get("user_input").ForEach(
				func(_, block gjson.Result) bool {
					if block.Get("type").Str == "text" {
						text := block.Get("text").Str
						if text != "" {
							userParts = append(
								userParts, text,
							)
						}
					}
					return true
				},
			)

			userText := strings.Join(userParts, "\n")
			if userText == "" {
				continue
			}

			if firstMessage == "" {
				firstMessage = truncate(
					strings.ReplaceAll(userText, "\n", " "),
					300,
				)
			}

			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       userText,
				Timestamp:     currentTS,
				ContentLength: len(userText),
			})
			ordinal++

		case "ContentPart":
			contentType := payload.Get("type").Str
			switch contentType {
			case "text":
				text := payload.Get("text").Str
				if text != "" {
					if pendingTS.IsZero() {
						pendingTS = currentTS
					}
					pendingText = append(pendingText, text)
				}
			case "think":
				think := payload.Get("think").Str
				if think != "" {
					if pendingTS.IsZero() {
						pendingTS = currentTS
					}
					hasThinking = true
					pendingText = append(pendingText,
						"[Thinking]\n"+think+"\n[/Thinking]")
				}
			}

		case "ToolCall":
			if pendingTS.IsZero() {
				pendingTS = currentTS
			}
			hasToolUse = true
			fnName := payload.Get("function.name").Str
			fnArgs := payload.Get("function.arguments").Str
			toolID := payload.Get("id").Str

			tc := ParsedToolCall{
				ToolUseID: toolID,
				ToolName:  fnName,
				Category:  NormalizeToolCategory(fnName),
				InputJSON: fnArgs,
			}
			pendingToolCall = append(pendingToolCall, tc)

			// Format tool use display text.
			argsResult := gjson.Parse(fnArgs)
			pendingText = append(pendingText,
				formatKimiToolUse(fnName, argsResult))

		case "ToolResult":
			flushAssistantTurn()

			toolCallID := payload.Get("tool_call_id").Str
			returnVal := payload.Get("return_value")
			isError := returnVal.Get("is_error").Bool()

			output := extractKimiToolOutput(returnVal.Get("output"))
			if isError && output == "" {
				output = "[error]"
			}

			quoted, err := json.Marshal(output)
			if err != nil {
				continue
			}

			tr := ParsedToolResult{
				ToolUseID:     toolCallID,
				ContentRaw:    string(quoted),
				ContentLength: len(output),
			}

			messages = append(messages, ParsedMessage{
				Ordinal:     ordinal,
				Role:        RoleUser,
				Timestamp:   currentTS,
				ToolResults: []ParsedToolResult{tr},
			})
			ordinal++

		case "StatusUpdate":
			if out := payload.Get("token_usage.output"); out.Exists() {
				totalOutputTokens += int(out.Int())
			}
			if ctx := payload.Get("context_tokens"); ctx.Exists() {
				ctxTokens := int(ctx.Int())
				if ctxTokens > peakContextTokens {
					peakContextTokens = ctxTokens
				}
			}

		case "TurnEnd":
			flushAssistantTurn()

		case "StepBegin":
			// Informational; no action needed.
		}
	}

	if err := lr.Err(); err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}

	// Flush any remaining assistant content.
	flushAssistantTurn()

	if len(messages) == 0 {
		return nil, nil, nil
	}

	// Derive a cleaner project name from the hash.
	displayProject := project
	if displayProject == "" {
		displayProject = "kimi"
	}

	userCount := 0
	for _, m := range messages {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:                "kimi:" + sessionID,
		Project:           displayProject,
		Machine:           machine,
		Agent:             AgentKimi,
		FirstMessage:      firstMessage,
		StartedAt:         startTime,
		EndedAt:           endTime,
		MessageCount:      len(messages),
		UserMessageCount:  userCount,
		TotalOutputTokens: totalOutputTokens,
		PeakContextTokens: peakContextTokens,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}

	return sess, messages, nil
}

// extractKimiToolOutput extracts text from a Kimi tool result
// output field. The output can be a plain string or an array of
// objects with {"type": "text", "text": "..."} entries.
func extractKimiToolOutput(output gjson.Result) string {
	if output.Type == gjson.String {
		return output.Str
	}
	if output.IsArray() {
		var parts []string
		output.ForEach(func(_, item gjson.Result) bool {
			if t := item.Get("text").Str; t != "" {
				parts = append(parts, t)
			}
			return true
		})
		return strings.Join(parts, "\n")
	}
	if output.Raw != "" && output.Raw != "null" {
		return output.Raw
	}
	return ""
}

// formatKimiToolUse formats a Kimi tool call for display.
func formatKimiToolUse(name string, input gjson.Result) string {
	switch name {
	case "Read":
		path := input.Get("file_path").Str
		if path == "" {
			path = input.Get("path").Str
		}
		return fmt.Sprintf("[Read: %s]", path)
	case "Edit":
		return fmt.Sprintf("[Edit: %s]", input.Get("file_path").Str)
	case "Write":
		return fmt.Sprintf("[Write: %s]", input.Get("file_path").Str)
	case "Bash":
		cmd := input.Get("command").Str
		desc := input.Get("description").Str
		if desc != "" {
			return fmt.Sprintf("[Bash: %s]\n$ %s", desc, cmd)
		}
		return fmt.Sprintf("[Bash]\n$ %s", cmd)
	case "Grep":
		return fmt.Sprintf("[Grep: %s]", input.Get("pattern").Str)
	case "Glob":
		return fmt.Sprintf("[Glob: %s]", input.Get("pattern").Str)
	case "Task", "Agent":
		desc := input.Get("description").Str
		return fmt.Sprintf("[Task: %s]", desc)
	default:
		return fmt.Sprintf("[Tool: %s]", name)
	}
}


