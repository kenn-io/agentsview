// ABOUTME: Parses Hermes Agent JSONL session files into structured session data.
// ABOUTME: Handles Hermes's OpenAI-style message format with session_meta header,
// ABOUTME: user/assistant/tool roles, and function-call tool invocations.
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

// ParseHermesSession parses a Hermes Agent JSONL session file.
//
// Hermes stores sessions as flat JSONL files in ~/.hermes/sessions/
// with filenames like 20260403_153620_5a3e2ff1.jsonl.
//
// Line format:
//   - First line: {"role":"session_meta", "tools":[...], "model":"...", "platform":"...", "timestamp":"..."}
//   - User messages: {"role":"user", "content":"...", "timestamp":"..."}
//   - Assistant messages: {"role":"assistant", "content":"...", "reasoning":"...",
//     "finish_reason":"tool_calls|stop", "tool_calls":[...], "timestamp":"..."}
//   - Tool results: {"role":"tool", "content":"...", "tool_call_id":"...", "timestamp":"..."}
func ParseHermesSession(path, project, machine string) (*ParsedSession, []ParsedMessage, error) {
	if strings.HasSuffix(path, ".json") {
		return parseHermesJSONSession(path, project, machine)
	}
	return parseHermesJSONLSession(path, project, machine)
}

// parseHermesJSONLSession parses a Hermes Agent JSONL session file.
func parseHermesJSONLSession(path, project, machine string) (*ParsedSession, []ParsedMessage, error) {
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
		messages        []ParsedMessage
		startedAt       time.Time
		endedAt         time.Time
		ordinal         int
		realUserCount   int
		firstMsg        string
		sessionPlatform string
	)

	// Extract session ID from filename: 20260403_153620_5a3e2ff1.jsonl -> 20260403_153620_5a3e2ff1
	sessionID := HermesSessionID(filepath.Base(path))

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}

		role := gjson.Get(line, "role").Str
		ts := parseHermesTimestamp(gjson.Get(line, "timestamp").Str)

		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(endedAt) {
				endedAt = ts
			}
		}

		switch role {
		case "session_meta":
			// Extract model and platform from session header.
			sessionPlatform = gjson.Get(line, "platform").Str
			continue

		case "user":
			content := gjson.Get(line, "content").Str
			content = strings.TrimSpace(content)
			if content == "" {
				continue
			}

			// Strip skill injection prefixes for cleaner display.
			displayContent := stripHermesSkillPrefix(content)

			if firstMsg == "" && displayContent != "" {
				firstMsg = truncate(
					strings.ReplaceAll(displayContent, "\n", " "),
					300,
				)
			}

			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       displayContent,
				Timestamp:     ts,
				ContentLength: len(content),
			})
			ordinal++
			realUserCount++

		case "assistant":
			content := gjson.Get(line, "content").Str
			content = strings.TrimSpace(content)
			reasoning := gjson.Get(line, "reasoning").Str
			hasThinking := reasoning != ""

			// Extract tool calls from the assistant message.
			var toolCalls []ParsedToolCall
			tcArray := gjson.Get(line, "tool_calls")
			if tcArray.IsArray() {
				tcArray.ForEach(func(_, tc gjson.Result) bool {
					name := tc.Get("function.name").Str
					if name != "" {
						toolCalls = append(toolCalls, ParsedToolCall{
							ToolUseID: tc.Get("id").Str,
							ToolName:  name,
							Category:  NormalizeToolCategory(name),
							InputJSON: tc.Get("function.arguments").Str,
						})
					}
					return true
				})
			}
			hasToolUse := len(toolCalls) > 0

			// Build display content: include reasoning if present.
			displayContent := content
			if hasThinking && content == "" {
				// Assistant message with only reasoning and tool calls.
				displayContent = ""
			}
			if hasThinking {
				displayContent = "[Thinking]\n" + reasoning + "\n[/Thinking]\n" + displayContent
			}

			if displayContent == "" && len(toolCalls) == 0 {
				continue
			}

			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       displayContent,
				Timestamp:     ts,
				HasThinking:   hasThinking,
				HasToolUse:    hasToolUse,
				ContentLength: len(content) + len(reasoning),
				ToolCalls:     toolCalls,
			})
			ordinal++

		case "tool":
			// Tool results in Hermes are separate messages with
			// tool_call_id linking back to the assistant's tool call.
			toolCallID := gjson.Get(line, "tool_call_id").Str
			if toolCallID == "" {
				continue
			}
			content := gjson.Get(line, "content").Str
			contentLen := len(content)

			// Preserve tool output as JSON-quoted string so
			// pairToolResults / DecodeContent can surface it in the UI.
			quoted, _ := json.Marshal(content)

			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       "",
				Timestamp:     ts,
				ContentLength: contentLen,
				ToolResults: []ParsedToolResult{{
					ToolUseID:     toolCallID,
					ContentRaw:    string(quoted),
					ContentLength: contentLen,
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

	fullID := "hermes:" + sessionID

	// Derive project from the session platform or default.
	if project == "" {
		if sessionPlatform != "" {
			project = "hermes-" + sessionPlatform
		} else {
			project = "hermes"
		}
	}

	sess := &ParsedSession{
		ID:               fullID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentHermes,
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

	return sess, messages, nil
}

// parseHermesJSONSession parses a Hermes CLI-format JSON session file.
func parseHermesJSONSession(path, project, machine string) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}

	root := gjson.ParseBytes(data)
	if !root.IsObject() {
		return nil, nil, fmt.Errorf("invalid JSON in %s", path)
	}

	sessionID := HermesSessionID(filepath.Base(path))
	sessionPlatform := root.Get("platform").Str
	startedAt := parseHermesTimestamp(root.Get("session_start").Str)
	endedAt := parseHermesTimestamp(root.Get("last_updated").Str)

	var (
		messages      []ParsedMessage
		ordinal       int
		realUserCount int
		firstMsg      string
	)

	root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").Str
		// Extract per-message timestamp when available.
		msgTS := parseHermesTimestamp(msg.Get("timestamp").Str)

		// Reconcile per-message timestamps with session bounds so
		// StartedAt/EndedAt stay correct even if envelope fields
		// are missing or stale.
		if !msgTS.IsZero() {
			if startedAt.IsZero() || msgTS.Before(startedAt) {
				startedAt = msgTS
			}
			if msgTS.After(endedAt) {
				endedAt = msgTS
			}
		}

		switch role {
		case "user":
			content := strings.TrimSpace(msg.Get("content").Str)
			if content == "" {
				return true
			}

			displayContent := stripHermesSkillPrefix(content)

			if firstMsg == "" && displayContent != "" {
				firstMsg = truncate(
					strings.ReplaceAll(displayContent, "\n", " "),
					300,
				)
			}

			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       displayContent,
				Timestamp:     msgTS,
				ContentLength: len(content),
			})
			ordinal++
			realUserCount++

		case "assistant":
			content := strings.TrimSpace(msg.Get("content").Str)
			reasoning := msg.Get("reasoning").Str
			if reasoning == "" {
				reasoning = msg.Get("reasoning_details").Str
			}
			hasThinking := reasoning != ""

			var toolCalls []ParsedToolCall
			tcArray := msg.Get("tool_calls")
			if tcArray.IsArray() {
				tcArray.ForEach(func(_, tc gjson.Result) bool {
					name := tc.Get("function.name").Str
					if name != "" {
						toolCalls = append(toolCalls, ParsedToolCall{
							ToolUseID: tc.Get("id").Str,
							ToolName:  name,
							Category:  NormalizeToolCategory(name),
							InputJSON: tc.Get("function.arguments").Str,
						})
					}
					return true
				})
			}
			hasToolUse := len(toolCalls) > 0

			displayContent := content
			if hasThinking && content == "" {
				displayContent = ""
			}
			if hasThinking {
				displayContent = "[Thinking]\n" + reasoning + "\n[/Thinking]\n" + displayContent
			}

			if displayContent == "" && len(toolCalls) == 0 {
				return true
			}

			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       displayContent,
				Timestamp:     msgTS,
				HasThinking:   hasThinking,
				HasToolUse:    hasToolUse,
				ContentLength: len(content) + len(reasoning),
				ToolCalls:     toolCalls,
			})
			ordinal++

		case "tool":
			toolCallID := msg.Get("tool_call_id").Str
			if toolCallID == "" {
				return true
			}
			content := msg.Get("content").Str
			contentLen := len(content)

			// Preserve tool output as JSON-quoted string so
			// pairToolResults / DecodeContent can surface it in the UI.
			quoted, _ := json.Marshal(content)

			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       "",
				Timestamp:     msgTS,
				ContentLength: contentLen,
				ToolResults: []ParsedToolResult{{
					ToolUseID:     toolCallID,
					ContentRaw:    string(quoted),
					ContentLength: contentLen,
				}},
			})
			ordinal++
		}

		return true
	})

	if len(messages) == 0 {
		return nil, nil, nil
	}

	fullID := "hermes:" + sessionID

	if project == "" {
		if sessionPlatform != "" {
			project = "hermes-" + sessionPlatform
		} else {
			project = "hermes"
		}
	}

	sess := &ParsedSession{
		ID:               fullID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentHermes,
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

	return sess, messages, nil
}

// HermesSessionID extracts the session ID from a Hermes filename.
// "20260403_153620_5a3e2ff1.jsonl" -> "20260403_153620_5a3e2ff1"
func HermesSessionID(name string) string {
	name = strings.TrimSuffix(name, ".jsonl")
	name = strings.TrimSuffix(name, ".json")
	name = strings.TrimPrefix(name, "session_")
	return name
}

// DiscoverHermesSessions finds all JSONL session files under the
// Hermes sessions directory. The directory structure is flat:
// <sessionsDir>/<timestamp>_<hash>.jsonl
func DiscoverHermesSessions(sessionsDir string) []DiscoveredFile {
	if sessionsDir == "" {
		return nil
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile
	jsonlIDs := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}

		jsonlIDs[HermesSessionID(name)] = true
		files = append(files, DiscoveredFile{
			Path:  filepath.Join(sessionsDir, name),
			Agent: AgentHermes,
		})
	}

	// Second pass: add session_*.json files not already covered by .jsonl
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || !strings.HasPrefix(name, "session_") {
			continue
		}
		sid := HermesSessionID(name)
		if jsonlIDs[sid] {
			continue
		}
		files = append(files, DiscoveredFile{
			Path:  filepath.Join(sessionsDir, name),
			Agent: AgentHermes,
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindHermesSourceFile finds a Hermes session file by session ID.
func FindHermesSourceFile(sessionsDir, sessionID string) string {
	if !IsValidSessionID(sessionID) {
		return ""
	}
	candidate := filepath.Join(sessionsDir, sessionID+".jsonl")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	candidate = filepath.Join(sessionsDir, "session_"+sessionID+".json")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

// parseHermesTimestamp parses timestamps in Hermes format.
// Hermes uses ISO 8601 format: "2026-04-03T15:27:21.014566"
// Timestamps without an explicit timezone are interpreted as local time
// (the server's timezone), since Hermes records wall-clock time without
// a UTC offset.
func parseHermesTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Try parsing with microseconds (Hermes default).
	// Use ParseInLocation so naive timestamps are interpreted as local
	// time rather than UTC — Hermes records local wall-clock time.
	t, err := time.ParseInLocation("2006-01-02T15:04:05.999999", s, time.Local)
	if err == nil {
		return t
	}
	// Fallback to standard ISO format (has explicit timezone — Parse is fine).
	t, err = time.Parse(time.RFC3339, s)
	if err == nil {
		return t
	}
	// Try without fractional seconds.
	t, err = time.ParseInLocation("2006-01-02T15:04:05", s, time.Local)
	if err == nil {
		return t
	}
	return time.Time{}
}

// stripHermesSkillPrefix removes the skill injection header that
// Hermes prepends to user messages when a skill is loaded.
// These start with "[SYSTEM: The user has invoked the ..."
//
// The injected format is:
//
//	[SYSTEM: The user has invoked the "<name>" skill...]\n\n
//	---\n<yaml frontmatter>\n---\n<skill body>\n\n
//	[optional setup/supporting-files notes]\n\n
//	The user has provided the following instruction alongside the skill invocation: <message>
//
// We extract the user instruction when present, otherwise return
// "[Skill: <name>]" as a compact placeholder.
func stripHermesSkillPrefix(s string) string {
	const prefix = "[SYSTEM: The user has invoked the \""
	if !strings.HasPrefix(s, prefix) {
		return s
	}

	// Extract skill name from the prefix.
	nameEnd := strings.Index(s[len(prefix):], "\"")
	skillName := ""
	if nameEnd > 0 {
		skillName = s[len(prefix) : len(prefix)+nameEnd]
	}

	// Look for the explicit user instruction marker that Hermes
	// appends after the skill content.
	const instrMarker = "The user has provided the following instruction alongside the skill invocation: "
	if _, after, ok := strings.Cut(s, instrMarker); ok {
		// The user instruction may be followed by an optional
		// "[Runtime note: ...]" block — strip it.
		if rtIdx := strings.Index(after, "\n\n[Runtime note:"); rtIdx >= 0 {
			after = after[:rtIdx]
		}
		after = strings.TrimSpace(after)
		if after != "" {
			return after
		}
	}

	// No explicit user instruction — return skill name placeholder.
	if skillName != "" {
		return "[Skill: " + skillName + "]"
	}
	return s
}
