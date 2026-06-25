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

func parseWorkBuddySession(path, project, machine string) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	if project == "" {
		project = filepath.Base(filepath.Dir(path))
		if filepath.Base(filepath.Dir(path)) == "subagents" {
			project = filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
		}
	}

	sessionID, parentID, isSubagent := workBuddyIDs(path, project)

	var (
		messages      []ParsedMessage
		ordinal       int
		startedAt     time.Time
		endedAt       time.Time
		firstMsg      string
		cwd           string
		realUserCount int
		malformed     int
	)

	lr := newLineReader(f, maxLineSize)
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			malformed++
			continue
		}

		root := gjson.Parse(line)
		if c := root.Get("cwd").Str; cwd == "" && c != "" {
			cwd = c
			if extracted := ExtractProjectFromCwd(c); extracted != "" {
				project = extracted
			}
			if !isSubagent {
				sessionID, parentID, _ = workBuddyIDs(path, project)
			}
		}
		ts := workBuddyTimestamp(root.Get("timestamp"))
		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(endedAt) {
				endedAt = ts
			}
		}

		switch root.Get("type").Str {
		case "message":
			role, ok := workBuddyRole(root.Get("role").Str)
			if !ok {
				continue
			}
			content := workBuddyContentText(root.Get("content"))
			if strings.TrimSpace(content) == "" {
				continue
			}
			if firstMsg == "" && role == RoleUser {
				firstMsg = truncate(strings.ReplaceAll(content, "\n", " "), 300)
			}
			msg := ParsedMessage{
				Ordinal:       ordinal,
				Role:          role,
				Content:       content,
				Timestamp:     ts,
				ContentLength: len(content),
			}
			if role == RoleAssistant {
				applyWorkBuddyUsage(&msg, root)
			}
			messages = append(messages, msg)
			ordinal++
			if role == RoleUser {
				realUserCount++
			}
		case "function_call":
			name := root.Get("name").Str
			callID := root.Get("callId").Str
			if name == "" || callID == "" {
				continue
			}
			msg := ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Timestamp:     ts,
				HasToolUse:    true,
				ContentLength: len(name),
				ToolCalls: []ParsedToolCall{{
					ToolUseID: callID,
					ToolName:  name,
					Category:  NormalizeToolCategory(name),
					InputJSON: workBuddyInputJSON(root.Get("arguments")),
				}},
			}
			applyWorkBuddyUsage(&msg, root)
			messages = append(messages, msg)
			ordinal++
		case "function_call_result":
			callID := root.Get("callId").Str
			if callID == "" {
				continue
			}
			output := root.Get("output")
			contentLen := len(output.String())
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Timestamp:     ts,
				ContentLength: contentLen,
				ToolResults: []ParsedToolResult{{
					ToolUseID:     callID,
					ContentLength: contentLen,
					ContentRaw:    workBuddyResultRaw(output),
				}},
			})
			ordinal++
		}
	}

	if err := lr.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}

	if len(messages) == 0 {
		return nil, nil, nil
	}

	sess := &ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentWorkBuddy,
		ParentSessionID:  parentID,
		RelationshipType: sourceTypeIfRel(isSubagent),
		Cwd:              cwd,
		MalformedLines:   malformed,
		FirstMessage:     firstMsg,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: realUserCount,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}
	accumulateMessageTokenUsage(sess, messages)
	return sess, messages, nil
}

func workBuddyIDs(path, project string) (sessionID, parentID string, isSubagent bool) {
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if filepath.Base(filepath.Dir(path)) != "subagents" {
		return "workbuddy:" + stem, "", false
	}
	parent := filepath.Base(filepath.Dir(filepath.Dir(path)))
	if !IsValidSessionID(parent) || filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path)))) != project {
		return "workbuddy:" + stem, "", false
	}
	parentID = "workbuddy:" + parent
	return parentID + ":subagent:" + stem, parentID, true
}

func workBuddyTimestamp(v gjson.Result) time.Time {
	if !v.Exists() || v.Type != gjson.Number {
		return time.Time{}
	}
	return time.UnixMilli(v.Int())
}

func workBuddyRole(role string) (RoleType, bool) {
	switch role {
	case "user":
		return RoleUser, true
	case "assistant":
		return RoleAssistant, true
	default:
		return "", false
	}
}

func workBuddyContentText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.Str
	}
	var parts []string
	if content.IsArray() {
		content.ForEach(func(_, part gjson.Result) bool {
			for _, key := range []string{"text", "input_text", "output_text"} {
				if text := part.Get(key).Str; text != "" {
					parts = append(parts, text)
					break
				}
			}
			return true
		})
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func workBuddyInputJSON(arguments gjson.Result) string {
	if arguments.Type == gjson.String {
		return arguments.Str
	}
	return arguments.Raw
}

func workBuddyResultRaw(output gjson.Result) string {
	// Object content items such as {"type":"text","text":"..."} are not
	// recognized by DecodeContent's object branch (which handles only the
	// iFlow shape), so extract their text and store it as a plain string.
	if output.IsObject() {
		if text := output.Get("text").Str; text != "" {
			quoted, _ := json.Marshal(text)
			return string(quoted)
		}
	}
	if output.Exists() && output.Raw != "" && output.Type != gjson.String {
		return output.Raw
	}
	quoted, _ := json.Marshal(output.String())
	return string(quoted)
}

func applyWorkBuddyUsage(msg *ParsedMessage, root gjson.Result) {
	if model := root.Get("providerData.model").Str; model != "" {
		msg.Model = model
	}
	usage := root.Get("providerData.usage")
	if !usage.Exists() {
		usage = root.Get("providerData.rawUsage")
	}
	if !usage.Exists() {
		return
	}

	inputField := firstExisting(usage, "inputTokens", "input_tokens", "prompt_tokens")
	outputField := firstExisting(usage, "outputTokens", "output_tokens", "completion_tokens")
	cacheReadField := firstExisting(usage, "cacheReadInputTokens", "cache_read_input_tokens", "prompt_tokens_details.cached_tokens")
	cacheCreateField := firstExisting(usage, "cacheCreationInputTokens", "cache_creation_input_tokens")
	reasoningField := firstExisting(usage, "reasoningTokens", "reasoning_tokens", "completion_tokens_details.reasoning_tokens")

	if !inputField.Exists() && !outputField.Exists() && !cacheReadField.Exists() && !cacheCreateField.Exists() && !reasoningField.Exists() {
		return
	}

	input := int(inputField.Int())
	output := int(outputField.Int())
	cacheRead := int(cacheReadField.Int())
	cacheCreate := int(cacheCreateField.Int())
	reasoning := int(reasoningField.Int())
	if usage.Get("prompt_tokens").Exists() {
		input = max(input-cacheRead, 0)
	}
	normalized := map[string]int{}
	if inputField.Exists() {
		normalized["input_tokens"] = input
	}
	if outputField.Exists() {
		normalized["output_tokens"] = output
	}
	if cacheReadField.Exists() {
		normalized["cache_read_input_tokens"] = cacheRead
	}
	if cacheCreateField.Exists() {
		normalized["cache_creation_input_tokens"] = cacheCreate
	}
	if reasoningField.Exists() {
		normalized["reasoning_tokens"] = reasoning
	}
	j, err := json.Marshal(normalized)
	if err != nil {
		return
	}
	msg.TokenUsage = j
	msg.OutputTokens = output
	msg.HasOutputTokens = outputField.Exists()
	msg.ContextTokens = input + cacheRead + cacheCreate
	msg.HasContextTokens = inputField.Exists() || cacheReadField.Exists() || cacheCreateField.Exists()
}

func firstExisting(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		value := root.Get(path)
		if value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}

func sourceTypeIfRel(isSubagent bool) RelationshipType {
	if isSubagent {
		return RelSubagent
	}
	return RelNone
}
