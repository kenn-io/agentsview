package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// kimiSessionIDFromPath extracts the raw Kimi session ID from its
// wire.jsonl path. Legacy paths yield "<project>:<session>"; .kimi-code
// paths yield "<workdir>:<agent>:<session>".
func kimiSessionIDFromPath(path string) string {
	dir := filepath.Dir(path)
	base := filepath.Base(dir)
	parent := filepath.Dir(dir)
	if filepath.Base(parent) == "agents" {
		// New layout: .../<workdir>_<hash>/session_<uuid>/agents/<agent>/wire.jsonl
		agentID := base
		sessionDir := filepath.Dir(parent)
		sessionUUID := filepath.Base(sessionDir)
		workdirDir := filepath.Dir(sessionDir)
		projHash := filepath.Base(workdirDir)
		return projHash + ":" + agentID + ":" + sessionUUID
	}

	// Legacy layout: .../<project-hash>/<session-uuid>/wire.jsonl
	sessionUUID := base
	projHash := filepath.Base(parent)
	return projHash + ":" + sessionUUID
}

// DecodeKimiProjectDir extracts a human-readable project name from a
// .kimi-code session directory. The directory is encoded as
// "wd_<workdir>_<12-hex-hash>" (e.g. "wd_kimi-code_057f5c09ee3f"),
// where the workdir name may itself contain underscores or hyphens.
// Legacy .kimi sessions use opaque project hashes, which do not
// carry the "wd_" prefix and are returned unchanged.
func DecodeKimiProjectDir(dirName string) string {
	if dirName == "" || !strings.HasPrefix(dirName, "wd_") {
		return dirName
	}
	name := strings.TrimPrefix(dirName, "wd_")
	if i := strings.LastIndex(name, "_"); i > 0 && isKimiHash(name[i+1:]) {
		name = name[:i]
	}
	return name
}

// isKimiHash reports whether s is a 12-character hexadecimal hash of
// the kind .kimi-code appends to its workdir directory names.
func isKimiHash(s string) bool {
	if len(s) != 12 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHex := (c >= '0' && c <= '9') ||
			(c >= 'a' && c <= 'f') ||
			(c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// kimiIDComponentsValid reports whether the given path-derived
// components can form a session ID that provider raw-ID lookup can
// round-trip back to the source file. Each component must itself be a
// valid session ID (alphanumeric, '-', '_'); a ':' or any other
// character outside that set would break the ':'-delimited ID split
// and validation. Sessions with such names are skipped at discovery
// time rather than imported in a state that cannot be resynced.
func kimiIDComponentsValid(components ...string) bool {
	for _, c := range components {
		if !IsValidSessionID(c) {
			return false
		}
	}
	return true
}

// parseSession parses a Kimi wire.jsonl file. Legacy Kimi CLI
// sessions store nested message.type records (TurnBegin, ContentPart,
// ToolCall, ToolResult, StatusUpdate, TurnEnd). Kimi Code sessions store
// top-level records (turn.prompt, context.append_loop_event, usage.record).
func parseKimiSession(
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

	// Extract session ID from path. Both legacy and .kimi-code
	// layouts are supported.
	sessionID := kimiSessionIDFromPath(path)

	var (
		messages     []ParsedMessage
		firstMessage string
		ordinal      int

		startTime time.Time
		endTime   time.Time

		// Accumulate content parts and tool calls for the
		// current assistant turn.
		pendingText             []string
		pendingThinkingText     []string
		pendingToolCall         []ParsedToolCall
		pendingModel            string
		pendingTokenUsage       json.RawMessage
		pendingContextTokens    int
		pendingOutputTokens     int
		pendingHasContextTokens bool
		pendingHasOutputTokens  bool
		pendingStopReason       string
		hasThinking             bool
		hasToolUse              bool

		// Track token usage from StatusUpdate.
		totalOutputTokens    int
		peakContextTokens    int
		hasTotalOutputTokens bool
		hasPeakContextTokens bool

		// Current record timestamp and pending assistant
		// turn timestamp (latest seen).
		currentTS time.Time
		pendingTS time.Time

		currentModel string
	)

	resetAssistantTurn := func() {
		pendingText = nil
		pendingThinkingText = nil
		pendingToolCall = nil
		pendingModel = ""
		pendingTokenUsage = nil
		pendingContextTokens = 0
		pendingOutputTokens = 0
		pendingHasContextTokens = false
		pendingHasOutputTokens = false
		pendingStopReason = ""
		pendingTS = time.Time{}
		hasThinking = false
		hasToolUse = false
	}

	flushAssistantTurn := func() {
		content := strings.Join(pendingText, "\n")
		if strings.TrimSpace(content) == "" &&
			len(pendingToolCall) == 0 {
			resetAssistantTurn()
			return
		}

		messages = append(messages, ParsedMessage{
			Ordinal:          ordinal,
			Role:             RoleAssistant,
			Content:          content,
			ThinkingText:     strings.Join(pendingThinkingText, "\n"),
			Timestamp:        pendingTS,
			HasThinking:      hasThinking,
			HasToolUse:       hasToolUse,
			ContentLength:    len(content),
			ToolCalls:        pendingToolCall,
			Model:            pendingModel,
			TokenUsage:       pendingTokenUsage,
			ContextTokens:    pendingContextTokens,
			OutputTokens:     pendingOutputTokens,
			HasContextTokens: pendingHasContextTokens,
			HasOutputTokens:  pendingHasOutputTokens,
			StopReason:       pendingStopReason,
		})
		ordinal++
		resetAssistantTurn()
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
		recordType := root.Get("type").Str

		if t, ok := kimiRecordTimestamp(root); ok {
			if startTime.IsZero() || t.Before(startTime) {
				startTime = t
			}
			if t.After(endTime) {
				endTime = t
			}
			currentTS = t
		}

		// Top-level "type" = "metadata" line.
		if recordType == "metadata" {
			continue
		}

		msgType := root.Get("message.type").Str
		payload := root.Get("message.payload")

		if msgType == "" && recordType != "" {
			switch recordType {
			case "config.update":
				if model := root.Get("modelAlias").Str; model != "" {
					currentModel = model
				}

			case "turn.prompt", "turn.steer":
				flushAssistantTurn()

				userText := kimiContentPartsText(root.Get("input"))
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

			case "context.append_loop_event":
				event := root.Get("event")
				switch event.Get("type").Str {
				case "content.part":
					part := event.Get("part")
					contentType := part.Get("type").Str
					switch contentType {
					case "text":
						text := part.Get("text").Str
						if text != "" {
							if pendingTS.IsZero() {
								pendingTS = currentTS
							}
							pendingText = append(pendingText, text)
						}
					case "think":
						think := part.Get("think").Str
						if think == "" {
							think = part.Get("text").Str
						}
						if think != "" {
							if pendingTS.IsZero() {
								pendingTS = currentTS
							}
							hasThinking = true
							pendingThinkingText = append(
								pendingThinkingText, think,
							)
							pendingText = append(pendingText,
								"[Thinking]\n"+think+"\n[/Thinking]")
						}
					}

				case "tool.call":
					if pendingTS.IsZero() {
						pendingTS = currentTS
					}
					hasToolUse = true
					fnName := event.Get("name").Str
					fnArgs := kimiRawJSON(event.Get("args"))
					toolID := event.Get("toolCallId").Str

					tc := ParsedToolCall{
						ToolUseID: toolID,
						ToolName:  fnName,
						Category:  NormalizeToolCategory(fnName),
						InputJSON: fnArgs,
						SkillName: inferToolSkillName(
							fnName, fnArgs,
						),
					}
					pendingToolCall = append(pendingToolCall, tc)

					argsResult := kimiJSONResult(event.Get("args"))
					pendingText = append(pendingText,
						formatKimiToolUse(fnName, argsResult))

				case "tool.result":
					flushAssistantTurn()

					toolCallID := event.Get("toolCallId").Str
					result := event.Get("result")
					isError := result.Get("isError").Bool()
					output := extractKimiToolOutput(result.Get("output"))
					if output == "" {
						output = result.Get("message").Str
					}
					if isError && output == "" {
						output = "[error]"
					}

					quoted, err := json.Marshal(output)
					if err != nil {
						continue
					}

					messages = append(messages, ParsedMessage{
						Ordinal:   ordinal,
						Role:      RoleUser,
						Timestamp: currentTS,
						ToolResults: []ParsedToolResult{{
							ToolUseID:     toolCallID,
							ContentRaw:    string(quoted),
							ContentLength: len(output),
						}},
					})
					ordinal++

				case "step.end":
					if pendingModel == "" {
						pendingModel = currentModel
					}
					pendingStopReason = event.Get("finishReason").Str
					if usage := event.Get("usage"); usage.Exists() {
						tokenUsage, outputTokens, contextTokens,
							hasOutput, hasContext :=
							kimiNativeTokenUsage(usage)
						pendingTokenUsage = tokenUsage
						pendingOutputTokens = outputTokens
						pendingContextTokens = contextTokens
						pendingHasOutputTokens = hasOutput
						pendingHasContextTokens = hasContext
						if hasOutput {
							hasTotalOutputTokens = true
							totalOutputTokens += outputTokens
						}
						if hasContext {
							hasPeakContextTokens = true
							if contextTokens > peakContextTokens {
								peakContextTokens = contextTokens
							}
						}
					}
					flushAssistantTurn()

				case "step.begin":
					// Informational; no action needed.
				}

			case "usage.record":
				if model := root.Get("model").Str; model != "" {
					currentModel = model
				}
				if usage := root.Get("usage"); usage.Exists() &&
					len(messages) > 0 {
					last := &messages[len(messages)-1]
					if last.Role == RoleAssistant &&
						len(last.TokenUsage) == 0 {
						tokenUsage, outputTokens, contextTokens,
							hasOutput, hasContext :=
							kimiNativeTokenUsage(usage)
						last.Model = currentModel
						last.TokenUsage = tokenUsage
						last.OutputTokens = outputTokens
						last.ContextTokens = contextTokens
						last.HasOutputTokens = hasOutput
						last.HasContextTokens = hasContext
						if hasOutput {
							hasTotalOutputTokens = true
							totalOutputTokens += outputTokens
						}
						if hasContext {
							hasPeakContextTokens = true
							if contextTokens > peakContextTokens {
								peakContextTokens = contextTokens
							}
						}
					}
				}
			}
			continue
		}

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
					pendingThinkingText = append(
						pendingThinkingText, think,
					)
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
				SkillName: inferToolSkillName(
					fnName, fnArgs,
				),
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
				hasTotalOutputTokens = true
				totalOutputTokens += int(out.Int())
			}
			if ctx := payload.Get("context_tokens"); ctx.Exists() {
				hasPeakContextTokens = true
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
		ID:                          "kimi:" + sessionID,
		Project:                     displayProject,
		Machine:                     machine,
		Agent:                       AgentKimi,
		FirstMessage:                firstMessage,
		StartedAt:                   startTime,
		EndedAt:                     endTime,
		MessageCount:                len(messages),
		UserMessageCount:            userCount,
		TotalOutputTokens:           totalOutputTokens,
		PeakContextTokens:           peakContextTokens,
		HasTotalOutputTokens:        hasTotalOutputTokens,
		HasPeakContextTokens:        hasPeakContextTokens,
		aggregateTokenPresenceKnown: true,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}

	return sess, messages, nil
}

func kimiRecordTimestamp(root gjson.Result) (time.Time, bool) {
	if ts := root.Get("timestamp"); ts.Type == gjson.Number {
		sec := int64(ts.Float())
		nsec := int64((ts.Float() - float64(sec)) * 1e9)
		return time.Unix(sec, nsec), true
	}
	if ts := root.Get("time"); ts.Type == gjson.Number {
		return time.UnixMilli(ts.Int()), true
	}
	if ts := root.Get("created_at"); ts.Type == gjson.Number {
		return time.UnixMilli(ts.Int()), true
	}
	return time.Time{}, false
}

func kimiContentPartsText(parts gjson.Result) string {
	if !parts.IsArray() {
		return ""
	}
	var out []string
	parts.ForEach(func(_, part gjson.Result) bool {
		if part.Get("type").Str == "text" {
			if text := part.Get("text").Str; text != "" {
				out = append(out, text)
			}
		}
		return true
	})
	return strings.Join(out, "\n")
}

func kimiRawJSON(value gjson.Result) string {
	if !value.Exists() {
		return ""
	}
	if value.Type == gjson.String && gjson.Valid(value.Str) {
		return value.Str
	}
	return value.Raw
}

func kimiJSONResult(value gjson.Result) gjson.Result {
	raw := kimiRawJSON(value)
	if gjson.Valid(raw) {
		return gjson.Parse(raw)
	}
	return value
}

func kimiNativeTokenUsage(
	usage gjson.Result,
) (json.RawMessage, int, int, bool, bool) {
	var (
		inputOther          int
		output              int
		inputCacheRead      int
		inputCacheCreate    int
		hasInputOther       bool
		hasOutput           bool
		hasInputCacheRead   bool
		hasInputCacheCreate bool
	)

	if f := usage.Get("inputOther"); f.Exists() {
		inputOther = int(f.Int())
		hasInputOther = true
	}
	if f := usage.Get("output"); f.Exists() {
		output = int(f.Int())
		hasOutput = true
	}
	if f := usage.Get("inputCacheRead"); f.Exists() {
		inputCacheRead = int(f.Int())
		hasInputCacheRead = true
	}
	if f := usage.Get("inputCacheCreation"); f.Exists() {
		inputCacheCreate = int(f.Int())
		hasInputCacheCreate = true
	}

	if !hasInputOther && !hasOutput && !hasInputCacheRead &&
		!hasInputCacheCreate {
		return nil, 0, 0, false, false
	}

	normalized := map[string]int{
		"input_tokens":                inputOther,
		"output_tokens":               output,
		"cache_read_input_tokens":     inputCacheRead,
		"cache_creation_input_tokens": inputCacheCreate,
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, 0, 0, false, false
	}

	contextTokens := inputOther + inputCacheRead + inputCacheCreate
	hasContext := hasInputOther || hasInputCacheRead || hasInputCacheCreate
	return raw, output, contextTokens, hasOutput, hasContext
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
