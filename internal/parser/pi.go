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

// ParsePiSession parses a pi-agent JSONL session file.
// The file format uses a leading session-header entry followed by
// message, model_change, and compaction entries.
func ParsePiSession(
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

	// --- Parse session header (first non-whitespace line) ---
	// Skip whitespace-only lines to stay consistent with
	// IsPiSessionFile in discovery.go which uses TrimSpace.
	var headerLine string
	for {
		line, ok := lr.next()
		if !ok {
			return nil, nil, fmt.Errorf(
				"not a pi session: missing session header in %s", path,
			)
		}
		if strings.TrimSpace(line) != "" {
			headerLine = line
			break
		}
	}

	if !gjson.Valid(headerLine) {
		return nil, nil, fmt.Errorf(
			"not a pi session: invalid JSON header in %s", path,
		)
	}

	if gjson.Get(headerLine, "type").Str != "session" {
		return nil, nil, fmt.Errorf(
			"not a pi session: missing session header in %s", path,
		)
	}

	sessionID := gjson.Get(headerLine, "id").Str
	cwd := gjson.Get(headerLine, "cwd").Str
	headerTimestamp := parseTimestamp(gjson.Get(headerLine, "timestamp").Str)

	// If project was not passed in, derive from cwd.
	if project == "" && cwd != "" {
		project = ExtractProjectFromCwd(cwd)
	}

	// branchedFrom handling: store basename without extension.
	var parentSessionID string
	branchedFrom := gjson.Get(headerLine, "branchedFrom").Str
	if branchedFrom != "" {
		base := filepath.Base(branchedFrom)
		parentSessionID = "pi:" + strings.TrimSuffix(base, filepath.Ext(base))
	}

	// V1 detection: if header has no id, we may need to derive from filename.
	isV1 := sessionID == ""

	// --- Main message loop ---
	var (
		messages     []ParsedMessage
		firstMessage string
		ordinal      int
		userCount    int
	)

	for {
		line, ok := lr.next()
		if !ok {
			break
		}

		if !gjson.Valid(line) {
			continue
		}

		entryType := gjson.Get(line, "type").Str
		if entryType == "" {
			continue
		}

		// If any message entry has an id field, this is a V2 session.
		if isV1 && gjson.Get(line, "id").Str != "" {
			isV1 = false
		}

		switch entryType {
		case "message":
			role := gjson.Get(line, "message.role").Str
			switch role {
			case "user":
				msg := parsePiUserMessage(line, ordinal)
				if msg == nil {
					continue
				}
				if firstMessage == "" && msg.Content != "" {
					firstMessage = truncate(
						strings.ReplaceAll(msg.Content, "\n", " "),
						300,
					)
				}
				messages = append(messages, *msg)
				ordinal++
				userCount++

			case "assistant":
				msg := parsePiAssistantMessage(line, ordinal)
				if msg == nil {
					continue
				}
				messages = append(messages, *msg)
				ordinal++

			case "toolResult":
				msg := parsePiToolResultMessage(line, ordinal)
				if msg == nil {
					continue
				}
				messages = append(messages, *msg)
				ordinal++

			default:
				// skip silently
			}

		case "model_change", "compaction":
			continue

		default:
			// skip silently (e.g., thinking_level_change)
		}
	}

	if err := lr.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading pi %s: %w", path, err)
	}

	// V1 fallback: derive session ID from filename.
	if isV1 || sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}

	// Compute StartedAt and EndedAt from message timestamps.
	startedAt := headerTimestamp
	var endedAt time.Time
	for _, m := range messages {
		if m.Timestamp.IsZero() {
			continue
		}
		if startedAt.IsZero() || m.Timestamp.Before(startedAt) {
			startedAt = m.Timestamp
		}
		if endedAt.IsZero() || m.Timestamp.After(endedAt) {
			endedAt = m.Timestamp
		}
	}

	sess := &ParsedSession{
		ID:               "pi:" + sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentPi,
		ParentSessionID:  parentSessionID,
		FirstMessage:     firstMessage,
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

// parsePiUserMessage parses a message entry with role="user".
// Returns nil if the entry is malformed.
func parsePiUserMessage(line string, ordinal int) *ParsedMessage {
	content := gjson.Get(line, "message.content")

	var text string
	if content.Type == gjson.String {
		text = content.Str
	} else if content.IsArray() {
		var parts []string
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").Str == "text" {
				if t := block.Get("text").Str; t != "" {
					parts = append(parts, t)
				}
			}
			return true
		})
		text = strings.Join(parts, "\n")
	}

	ts := piTimestamp(line)

	return &ParsedMessage{
		Ordinal:       ordinal,
		Role:          RoleUser,
		Content:       text,
		Timestamp:     ts,
		ContentLength: len(text),
	}
}

// parsePiAssistantMessage parses a message entry with role="assistant".
// Returns nil if the entry is malformed.
func parsePiAssistantMessage(line string, ordinal int) *ParsedMessage {
	var (
		parts       []string
		toolCalls   []ParsedToolCall
		hasThinking bool
		hasToolUse  bool
	)

	msgContent := gjson.Get(line, "message.content")
	if msgContent.Type == gjson.String {
		// Plain string content (back-compat format variation).
		parts = append(parts, msgContent.Str)
	} else {
		msgContent.ForEach(func(_, block gjson.Result) bool {
			switch block.Get("type").Str {
			case "text":
				if t := block.Get("text").Str; t != "" {
					parts = append(parts, t)
				}
			case "thinking":
				// Set hasThinking regardless of whether the thinking
				// field is empty -- redacted thinking blocks have an
				// empty field but the block type presence is sufficient
				// to mark the message.
				hasThinking = true
				if thinking := block.Get("thinking").Str; thinking != "" {
					parts = append(parts,
						"[Thinking]\n"+thinking+"\n[/Thinking]")
				}
			case "toolCall":
				hasToolUse = true
				id := block.Get("id").Str
				name := block.Get("name").Str
				argsRaw := block.Get("arguments").Raw
				// Normalize Pi's agent__intent / _i field to
				// "description" so the frontend can use a single
				// params.description check across all agents.
				argsRaw = normalizePiIntent(argsRaw)
				toolCalls = append(toolCalls, ParsedToolCall{
					ToolUseID: id,
					ToolName:  name,
					Category:  NormalizeToolCategory(name),
					InputJSON: argsRaw,
				})
				parts = append(parts, formatPiToolUse(
					name, argsRaw,
				))
			}
			return true
		})
	}

	content := strings.Join(parts, "\n")
	ts := piTimestamp(line)

	return &ParsedMessage{
		Ordinal:       ordinal,
		Role:          RoleAssistant,
		Content:       content,
		Timestamp:     ts,
		HasThinking:   hasThinking,
		HasToolUse:    hasToolUse,
		ContentLength: len(content),
		ToolCalls:     toolCalls,
	}
}

// parsePiToolResultMessage parses a message entry with role="toolResult".
// Returns nil if the entry is malformed.
func parsePiToolResultMessage(line string, ordinal int) *ParsedMessage {
	toolUseID := gjson.Get(line, "message.toolCallId").Str
	content := gjson.Get(line, "message.content")
	contentLen := toolResultContentLength(content)

	ts := piTimestamp(line)

	return &ParsedMessage{
		Ordinal:   ordinal,
		Role:      RoleUser,
		Timestamp: ts,
		ToolResults: []ParsedToolResult{
			{
				ToolUseID:     toolUseID,
				ContentLength: contentLen,
				ContentRaw:    content.Raw,
			},
		},
	}
}

// formatPiToolUse constructs a synthetic block with "input" mapped from
// Pi's "arguments" field and delegates to formatToolUse. This avoids
// duplicating the tool-name switch logic.
func formatPiToolUse(name, argsRaw string) string {
	// Build {"name":"<name>","input":<args>} so formatToolUse can
	// read input.* paths as usual.
	var sb strings.Builder
	sb.WriteString(`{"name":`)
	nameJSON, _ := json.Marshal(name)
	sb.Write(nameJSON)
	sb.WriteString(`,"input":`)
	if argsRaw == "" {
		sb.WriteString("{}")
	} else {
		sb.WriteString(argsRaw)
	}
	sb.WriteByte('}')
	return formatToolUse(gjson.Parse(sb.String()))
}

// normalizePiIntent rewrites Pi's agent__intent or _i argument field to
// "description" so the frontend can use a uniform params.description check
// across all agents. Returns the original JSON unchanged if neither field
// is present or if "description" already exists.
func normalizePiIntent(argsRaw string) string {
	if argsRaw == "" {
		return argsRaw
	}
	// Don't overwrite an existing description field.
	if gjson.Get(argsRaw, "description").Exists() {
		return argsRaw
	}
	intent := gjson.Get(argsRaw, "agent__intent")
	if !intent.Exists() {
		intent = gjson.Get(argsRaw, "_i")
	}
	if !intent.Exists() {
		return argsRaw
	}
	// Unmarshal into a map, rename the intent key to "description",
	// and re-marshal to produce valid JSON with proper escaping.
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsRaw), &m); err != nil {
		return argsRaw
	}
	if v, ok := m["agent__intent"]; ok {
		m["description"] = v
	} else {
		m["description"] = m["_i"]
	}
	delete(m, "agent__intent")
	delete(m, "_i")
	out, err := json.Marshal(m)
	if err != nil {
		return argsRaw
	}
	return string(out)
}

// piTimestamp extracts the timestamp for a pi JSONL entry.
// Tries the top-level "timestamp" field first (ISO 8601), then
// falls back to "message.timestamp" as Unix milliseconds.
func piTimestamp(line string) time.Time {
	if ts := parseTimestamp(gjson.Get(line, "timestamp").Str); !ts.IsZero() {
		return ts
	}
	if ms := gjson.Get(line, "message.timestamp").Int(); ms != 0 {
		return time.UnixMilli(ms).UTC()
	}
	return time.Time{}
}
