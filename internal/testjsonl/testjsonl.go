// Package testjsonl provides shared JSONL fixture builders for
// Claude and Codex session test data. Used by both parser and
// sync test packages.
package testjsonl

import (
	"encoding/json"
	"strings"
)

// ClaudeUserJSON returns a Claude user message as a JSON string.
func ClaudeUserJSON(
	content, timestamp string, cwd ...string,
) string {
	m := map[string]any{
		"type":      "user",
		"timestamp": timestamp,
		"message": map[string]any{
			"content": content,
		},
	}
	if len(cwd) > 0 {
		m["cwd"] = cwd[0]
	}
	return mustMarshal(m)
}

// ClaudeUserWithSessionIDJSON returns a Claude user message
// with a sessionId field as a JSON string.
func ClaudeUserWithSessionIDJSON(
	content, timestamp, sessionID string, cwd ...string,
) string {
	m := map[string]any{
		"type":      "user",
		"timestamp": timestamp,
		"sessionId": sessionID,
		"message": map[string]any{
			"content": content,
		},
	}
	if len(cwd) > 0 {
		m["cwd"] = cwd[0]
	}
	return mustMarshal(m)
}

// ClaudeMetaUserJSON returns a Claude user message with
// optional isMeta and isCompactSummary flags as a JSON string.
func ClaudeMetaUserJSON(
	content, timestamp string, meta, compact bool,
) string {
	m := map[string]any{
		"type":      "user",
		"timestamp": timestamp,
		"message": map[string]any{
			"content": content,
		},
	}
	if meta {
		m["isMeta"] = true
	}
	if compact {
		m["isCompactSummary"] = true
	}
	return mustMarshal(m)
}

// ClaudeToolResultUserJSON returns a Claude user message
// containing only a tool_result block (no text content).
// These messages are filtered out by pairAndFilter.
func ClaudeToolResultUserJSON(
	toolUseID, resultContent, timestamp string,
) string {
	m := map[string]any{
		"type":      "user",
		"timestamp": timestamp,
		"message": map[string]any{
			"content": []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": toolUseID,
				"content":     resultContent,
			}},
		},
	}
	return mustMarshal(m)
}

// ClaudeAssistantJSON returns a Claude assistant message as a
// JSON string.
func ClaudeAssistantJSON(content any, timestamp string) string {
	m := map[string]any{
		"type":      "assistant",
		"timestamp": timestamp,
		"message": map[string]any{
			"content": content,
		},
	}
	return mustMarshal(m)
}

// ClaudeSnapshotJSON returns a Claude snapshot message as a
// JSON string.
func ClaudeSnapshotJSON(timestamp string) string {
	m := map[string]any{
		"type": "user",
		"snapshot": map[string]any{
			"timestamp": timestamp,
		},
		"message": map[string]any{
			"content": "hello",
		},
	}
	return mustMarshal(m)
}

// CodexSessionMetaJSON returns a Codex session_meta message as
// a JSON string.
func CodexSessionMetaJSON(
	id, cwd, originator, timestamp string,
) string {
	m := map[string]any{
		"type":      "session_meta",
		"timestamp": timestamp,
		"payload": map[string]any{
			"id":         id,
			"cwd":        cwd,
			"originator": originator,
		},
	}
	return mustMarshal(m)
}

// CodexMsgJSON returns a Codex response_item message as a JSON
// string.
func CodexMsgJSON(role, text, timestamp string) string {
	contentType := "output_text"
	if role == "user" {
		contentType = "input_text"
	}
	m := map[string]any{
		"type":      "response_item",
		"timestamp": timestamp,
		"payload": map[string]any{
			"role": role,
			"content": []map[string]string{
				{
					"type": contentType,
					"text": text,
				},
			},
		},
	}
	return mustMarshal(m)
}

// CodexFunctionCallJSON returns a Codex function_call
// response_item as a JSON string.
func CodexFunctionCallJSON(
	name, summary, timestamp string,
) string {
	payload := map[string]any{
		"type":    "function_call",
		"name":    name,
		"call_id": "call_test",
	}
	if summary != "" {
		payload["summary"] = summary
	}
	m := map[string]any{
		"type":      "response_item",
		"timestamp": timestamp,
		"payload":   payload,
	}
	return mustMarshal(m)
}

// CodexFunctionCallArgsJSON returns a Codex function_call
// response_item with arguments payload.
func CodexFunctionCallArgsJSON(
	name string, arguments any, timestamp string,
) string {
	payload := map[string]any{
		"type":      "function_call",
		"name":      name,
		"call_id":   "call_test",
		"arguments": arguments,
	}
	m := map[string]any{
		"type":      "response_item",
		"timestamp": timestamp,
		"payload":   payload,
	}
	return mustMarshal(m)
}

// CodexFunctionCallFieldsJSON returns a Codex function_call
// response_item with explicit arguments and input fields.
func CodexFunctionCallFieldsJSON(
	name string, arguments, input any, timestamp string,
) string {
	payload := map[string]any{
		"type":    "function_call",
		"name":    name,
		"call_id": "call_test",
	}
	if arguments != nil {
		payload["arguments"] = arguments
	}
	if input != nil {
		payload["input"] = input
	}
	m := map[string]any{
		"type":      "response_item",
		"timestamp": timestamp,
		"payload":   payload,
	}
	return mustMarshal(m)
}

// CodexTurnContextJSON returns a Codex turn_context entry as a
// JSON string with the given model.
func CodexTurnContextJSON(model, timestamp string) string {
	m := map[string]any{
		"type":      "turn_context",
		"timestamp": timestamp,
		"payload": map[string]any{
			"model": model,
			"cwd":   "/tmp",
		},
	}
	return mustMarshal(m)
}

// ClaudeEntryJSON returns a Claude JSONL entry with uuid and
// parentUuid fields.
func ClaudeEntryJSON(
	entryType, content, timestamp, uuid, parentUuid string,
	cwd ...string,
) string {
	m := map[string]any{
		"type":      entryType,
		"timestamp": timestamp,
		"uuid":      uuid,
		"message": map[string]any{
			"content": content,
		},
	}
	if parentUuid != "" {
		m["parentUuid"] = parentUuid
	}
	if len(cwd) > 0 {
		m["cwd"] = cwd[0]
	}
	return mustMarshal(m)
}

// JoinJSONL joins JSON lines with newlines and appends a
// trailing newline.
func JoinJSONL(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

// SessionBuilder constructs JSONL session content using a
// fluent API.
type SessionBuilder struct {
	lines []string
}

// NewSessionBuilder returns a new empty SessionBuilder.
func NewSessionBuilder() *SessionBuilder {
	return &SessionBuilder{}
}

// AddClaudeUser appends a Claude user message line.
func (b *SessionBuilder) AddClaudeUser(
	timestamp, content string, cwd ...string,
) *SessionBuilder {
	b.lines = append(b.lines, ClaudeUserJSON(content, timestamp, cwd...))
	return b
}

// AddClaudeUserWithSessionID appends a Claude user message
// line with a sessionId field.
func (b *SessionBuilder) AddClaudeUserWithSessionID(
	timestamp, content, sessionID string, cwd ...string,
) *SessionBuilder {
	b.lines = append(
		b.lines,
		ClaudeUserWithSessionIDJSON(
			content, timestamp, sessionID, cwd...,
		),
	)
	return b
}

// AddClaudeUserWithUUID appends a Claude user message with
// uuid and parentUuid fields.
func (b *SessionBuilder) AddClaudeUserWithUUID(
	timestamp, content, uuid, parentUuid string,
	cwd ...string,
) *SessionBuilder {
	b.lines = append(b.lines, ClaudeEntryJSON(
		"user", content, timestamp, uuid, parentUuid, cwd...,
	))
	return b
}

// AddClaudeAssistantWithUUID appends a Claude assistant message
// with uuid and parentUuid fields.
func (b *SessionBuilder) AddClaudeAssistantWithUUID(
	timestamp, text, uuid, parentUuid string,
) *SessionBuilder {
	m := map[string]any{
		"type":      "assistant",
		"timestamp": timestamp,
		"uuid":      uuid,
		"message": map[string]any{
			"content": []map[string]string{
				{"type": "text", "text": text},
			},
		},
	}
	if parentUuid != "" {
		m["parentUuid"] = parentUuid
	}
	b.lines = append(b.lines, mustMarshal(m))
	return b
}

// AddClaudeMetaUser appends a Claude user message line with
// isMeta and/or isCompactSummary flags.
func (b *SessionBuilder) AddClaudeMetaUser(
	timestamp, content string, meta, compact bool,
) *SessionBuilder {
	b.lines = append(
		b.lines,
		ClaudeMetaUserJSON(content, timestamp, meta, compact),
	)
	return b
}

// AddClaudeAssistant appends a Claude assistant message line.
func (b *SessionBuilder) AddClaudeAssistant(
	timestamp, text string,
) *SessionBuilder {
	b.lines = append(b.lines, ClaudeAssistantJSON(
		[]map[string]string{{"type": "text", "text": text}},
		timestamp,
	))
	return b
}

// AddCodexMeta appends a Codex session_meta line.
func (b *SessionBuilder) AddCodexMeta(
	timestamp, id, cwd, originator string,
) *SessionBuilder {
	b.lines = append(
		b.lines,
		CodexSessionMetaJSON(id, cwd, originator, timestamp),
	)
	return b
}

// AddCodexMessage appends a Codex response_item line.
func (b *SessionBuilder) AddCodexMessage(
	timestamp, role, text string,
) *SessionBuilder {
	b.lines = append(b.lines, CodexMsgJSON(role, text, timestamp))
	return b
}

// AddCodexFunctionCall appends a Codex function_call line.
func (b *SessionBuilder) AddCodexFunctionCall(
	timestamp, name, summary string,
) *SessionBuilder {
	b.lines = append(
		b.lines,
		CodexFunctionCallJSON(name, summary, timestamp),
	)
	return b
}

// AddRaw appends an arbitrary raw line.
func (b *SessionBuilder) AddRaw(line string) *SessionBuilder {
	b.lines = append(b.lines, line)
	return b
}

// String returns the JSONL content with a trailing newline.
func (b *SessionBuilder) String() string {
	return strings.Join(b.lines, "\n") + "\n"
}

// StringNoTrailingNewline returns the JSONL content without a
// trailing newline.
func (b *SessionBuilder) StringNoTrailingNewline() string {
	return strings.Join(b.lines, "\n")
}

// GeminiToolCall defines a tool call for Gemini test fixtures.
type GeminiToolCall struct {
	ID           string
	Name         string
	DisplayName  string
	Args         map[string]string
	ResultOutput string // if set, generates inline functionResponse result
}

// GeminiThought defines a thought for Gemini test fixtures.
type GeminiThought struct {
	Subject     string
	Description string
	Timestamp   string
}

// GeminiMsgOpts holds optional fields for a Gemini assistant
// message.
type GeminiMsgOpts struct {
	Thoughts  []GeminiThought
	ToolCalls []GeminiToolCall
	Model     string
}

// GeminiUserMsg builds a Gemini user message object.
func GeminiUserMsg(
	id, timestamp, content string,
) map[string]any {
	return map[string]any{
		"id":        id,
		"timestamp": timestamp,
		"type":      "user",
		"content":   content,
	}
}

// GeminiAssistantMsg builds a Gemini assistant message object.
func GeminiAssistantMsg(
	id, timestamp, content string, opts *GeminiMsgOpts,
) map[string]any {
	m := map[string]any{
		"id":        id,
		"timestamp": timestamp,
		"type":      "gemini",
		"content":   content,
	}
	if opts == nil {
		return m
	}
	if opts.Model != "" {
		m["model"] = opts.Model
	}
	if len(opts.Thoughts) > 0 {
		var thoughts []map[string]string
		for _, th := range opts.Thoughts {
			thoughts = append(thoughts, map[string]string{
				"subject":     th.Subject,
				"description": th.Description,
				"timestamp":   th.Timestamp,
			})
		}
		m["thoughts"] = thoughts
	}
	if len(opts.ToolCalls) > 0 {
		var tcs []map[string]any
		for _, tc := range opts.ToolCalls {
			entry := map[string]any{
				"name":        tc.Name,
				"displayName": tc.DisplayName,
				"status":      "success",
			}
			if tc.ID != "" {
				entry["id"] = tc.ID
			}
			if tc.Args != nil {
				entry["args"] = tc.Args
			}
			if tc.ResultOutput != "" {
				entry["result"] = []map[string]any{
					{
						"functionResponse": map[string]any{
							"id":   tc.ID,
							"name": tc.Name,
							"response": map[string]any{
								"output": tc.ResultOutput,
							},
						},
					},
				}
			}
			tcs = append(tcs, entry)
		}
		m["toolCalls"] = tcs
	}
	return m
}

// GeminiInfoMsg builds a Gemini info/system message object.
func GeminiInfoMsg(
	id, timestamp, content, msgType string,
) map[string]any {
	return map[string]any{
		"id":        id,
		"timestamp": timestamp,
		"type":      msgType,
		"content":   content,
	}
}

// GeminiSessionJSON builds a complete Gemini session JSON
// string from the given parameters.
func GeminiSessionJSON(
	sessionID, projectHash string,
	startTime, lastUpdated string,
	messages []map[string]any,
) string {
	session := map[string]any{
		"sessionId":   sessionID,
		"projectHash": projectHash,
		"startTime":   startTime,
		"lastUpdated": lastUpdated,
		"messages":    messages,
	}
	b, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(b)
}

func mustMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
