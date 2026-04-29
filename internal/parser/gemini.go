// ABOUTME: Parses Gemini CLI session JSON files into structured session data.
// ABOUTME: Extracts messages, tool calls, thinking blocks, and token usage.
package parser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// geminiTokens holds token usage counts from a Gemini message.
type geminiTokens struct {
	Input  int
	Output int
	Cached int
}

// extractGeminiTokens reads the tokens object from a Gemini
// message and returns the parsed counts.
func extractGeminiTokens(msg gjson.Result) geminiTokens {
	tok := msg.Get("tokens")
	if !tok.Exists() {
		return geminiTokens{}
	}
	return geminiTokens{
		Input:  int(tok.Get("input").Int()),
		Output: int(tok.Get("output").Int()),
		Cached: int(tok.Get("cached").Int()),
	}
}

// ParseGeminiSession parses a Gemini CLI session JSON file.
// Unlike Claude/Codex JSONL, each Gemini file is a single JSON
// document containing all messages.
func ParseGeminiSession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}

	valid := gjson.ValidBytes(data)
	var root gjson.Result
	if valid {
		root = gjson.ParseBytes(data)
		if root.Get("messages").IsArray() ||
			root.Get("sessionId").Exists() {
			return parseGeminiJSONObject(
				path, project, machine, info, root,
			)
		}
	}
	if bytes.IndexByte(data, '\n') >= 0 {
		return parseGeminiJSONL(
			path, project, machine, info, data,
		)
	}
	return nil, nil, fmt.Errorf("invalid Gemini session in %s", path)
}

func parseGeminiJSONObject(
	path, project, machine string,
	info os.FileInfo,
	root gjson.Result,
) (*ParsedSession, []ParsedMessage, error) {
	sessionID := root.Get("sessionId").Str
	if sessionID == "" {
		return nil, nil, fmt.Errorf(
			"missing sessionId in %s", path,
		)
	}

	startTime := parseTimestamp(root.Get("startTime").Str)
	lastUpdated := parseTimestamp(root.Get("lastUpdated").Str)

	var (
		messages     []ParsedMessage
		firstMessage string
		ordinal      int
	)
	appendMessage := func(msg gjson.Result) {
		parsed, ok := parseGeminiMessage(msg, ordinal)
		if !ok {
			return
		}
		if parsed.Role == RoleUser && firstMessage == "" {
			firstMessage = truncate(
				strings.ReplaceAll(parsed.Content, "\n", " "),
				300,
			)
		}
		messages = append(messages, parsed)
		ordinal++
	}

	root.Get("messages").ForEach(
		func(_, msg gjson.Result) bool {
			appendMessage(msg)
			return true
		},
	)
	return buildGeminiSession(
		path, project, machine, info,
		sessionID, startTime, lastUpdated,
		firstMessage, messages,
	), messages, nil
}

func parseGeminiJSONL(
	path, project, machine string,
	info os.FileInfo,
	data []byte,
) (*ParsedSession, []ParsedMessage, error) {
	var (
		sessionID    string
		startTime    time.Time
		lastUpdated  time.Time
		firstMessage string
		records      = make([]gjson.Result, 0)
		recordIDs    = make(map[string]int)
	)
	for len(data) > 0 {
		line, rest, _ := bytes.Cut(data, []byte("\n"))
		data = rest
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !gjson.ValidBytes(line) {
			// Tolerate malformed lines: Gemini appends to this
			// file while the session is live, so a watcher scan
			// can race a partial trailing write. Matches the
			// Claude JSONL parser's skip-invalid behavior.
			continue
		}
		rec := gjson.ParseBytes(line)
		if id := rec.Get("sessionId").Str; id != "" {
			if sessionID == "" {
				sessionID = id
			}
			if startTime.IsZero() {
				startTime = parseTimestamp(
					rec.Get("startTime").Str,
				)
			}
			if ts := parseTimestamp(
				rec.Get("lastUpdated").Str,
			); ts.After(lastUpdated) {
				lastUpdated = ts
			}
		}
		if ts := parseTimestamp(
			rec.Get("$set.lastUpdated").Str,
		); ts.After(lastUpdated) {
			lastUpdated = ts
		}

		msgType := rec.Get("type").Str
		if msgType != "user" && msgType != "gemini" {
			continue
		}
		msgID := rec.Get("id").Str
		if msgID != "" {
			// Later record with same id replaces the earlier one in
			// place. Assumes Gemini does not reuse ids across roles
			// (user vs gemini); collisions would silently flip the
			// slot's role while keeping its chronological position.
			if idx, ok := recordIDs[msgID]; ok {
				records[idx] = rec
				continue
			}
			recordIDs[msgID] = len(records)
		}
		records = append(records, rec)
	}
	if sessionID == "" {
		return nil, nil, fmt.Errorf(
			"missing sessionId in %s", path,
		)
	}

	messages := make([]ParsedMessage, 0, len(records))
	for _, rec := range records {
		parsed, ok := parseGeminiMessage(rec, len(messages))
		if !ok {
			continue
		}
		if parsed.Role == RoleUser && firstMessage == "" {
			firstMessage = truncate(
				strings.ReplaceAll(parsed.Content, "\n", " "),
				300,
			)
		}
		messages = append(messages, parsed)
	}
	return buildGeminiSession(
		path, project, machine, info,
		sessionID, startTime, lastUpdated,
		firstMessage, messages,
	), messages, nil
}

func parseGeminiMessage(
	msg gjson.Result, ordinal int,
) (ParsedMessage, bool) {
	msgType := msg.Get("type").Str
	if msgType != "user" && msgType != "gemini" {
		return ParsedMessage{}, false
	}

	role := RoleUser
	if msgType == "gemini" {
		role = RoleAssistant
	}
	content, hasThinking, hasToolUse, tcs, trs :=
		extractGeminiContent(msg)
	if strings.TrimSpace(content) == "" {
		return ParsedMessage{}, false
	}

	tok := extractGeminiTokens(msg)
	var tokenUsage json.RawMessage
	tokResult := msg.Get("tokens")
	if tokResult.Exists() {
		tokenUsage = json.RawMessage(tokResult.Raw)
	}
	return ParsedMessage{
		Ordinal:       ordinal,
		Role:          role,
		Content:       content,
		Timestamp:     parseTimestamp(msg.Get("timestamp").Str),
		HasThinking:   hasThinking,
		HasToolUse:    hasToolUse,
		ContentLength: len(content),
		ToolCalls:     tcs,
		ToolResults:   trs,
		Model:         msg.Get("model").String(),
		TokenUsage:    tokenUsage,
		ContextTokens: tok.Input + tok.Cached,
		OutputTokens:  tok.Output,
		HasContextTokens: tokResult.Get("input").Exists() ||
			tokResult.Get("cached").Exists(),
		HasOutputTokens:    tokResult.Get("output").Exists(),
		tokenPresenceKnown: true,
	}, true
}

func buildGeminiSession(
	path, project, machine string,
	info os.FileInfo,
	sessionID string,
	startTime, lastUpdated time.Time,
	firstMessage string,
	messages []ParsedMessage,
) *ParsedSession {
	var userCount int
	for _, m := range messages {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:               "gemini:" + sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentGemini,
		FirstMessage:     firstMessage,
		StartedAt:        startTime,
		EndedAt:          lastUpdated,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}
	accumulateMessageTokenUsage(sess, messages)
	return sess
}

// extractGeminiContent builds readable text from a Gemini
// message, including its content, thoughts, and tool calls.
func extractGeminiContent(
	msg gjson.Result,
) (string, bool, bool, []ParsedToolCall, []ParsedToolResult) {
	var (
		parts       []string
		parsed      []ParsedToolCall
		results     []ParsedToolResult
		hasThinking bool
		hasToolUse  bool
	)

	// Extract thoughts (appear before content chronologically)
	thoughts := msg.Get("thoughts")
	if thoughts.IsArray() {
		thoughts.ForEach(func(_, thought gjson.Result) bool {
			desc := thought.Get("description").Str
			if desc != "" {
				hasThinking = true
				subj := thought.Get("subject").Str
				if subj != "" {
					parts = append(parts,
						fmt.Sprintf(
							"[Thinking]\n%s\n%s\n[/Thinking]",
							subj, desc,
						),
					)
				} else {
					parts = append(parts,
						"[Thinking]\n"+desc+"\n[/Thinking]",
					)
				}
			}
			return true
		})
	}

	// Extract main content (string or Part[] array)
	content := msg.Get("content")
	if content.Type == gjson.String {
		if t := content.Str; t != "" {
			parts = append(parts, t)
		}
	} else if content.IsArray() {
		content.ForEach(func(_, part gjson.Result) bool {
			if t := part.Get("text").Str; t != "" {
				parts = append(parts, t)
			}
			return true
		})
	}

	// Extract tool calls and inline results
	toolCalls := msg.Get("toolCalls")
	if toolCalls.IsArray() {
		toolCalls.ForEach(func(_, tc gjson.Result) bool {
			hasToolUse = true
			name := tc.Get("name").Str
			tcID := tc.Get("id").Str
			if name != "" {
				parsed = append(parsed, ParsedToolCall{
					ToolName:  name,
					Category:  NormalizeToolCategory(name),
					ToolUseID: tcID,
					InputJSON: tc.Get("args").Raw,
				})
				// Extract inline tool results from
				// result[].functionResponse.response.output
				tc.Get("result").ForEach(
					func(_, r gjson.Result) bool {
						output := r.Get(
							"functionResponse.response.output",
						)
						if !output.Exists() {
							return true
						}
						rid := r.Get("functionResponse.id").Str
						if rid == "" {
							rid = tcID
						}
						results = append(results, ParsedToolResult{
							ToolUseID:     rid,
							ContentLength: toolResultContentLength(output),
							ContentRaw:    output.Raw,
						})
						return true
					},
				)
			}
			parts = append(parts, formatGeminiToolCall(tc))
			return true
		})
	}

	return strings.Join(parts, "\n\n"),
		hasThinking, hasToolUse, parsed, results
}

func formatGeminiToolCall(tc gjson.Result) string {
	name := tc.Get("name").Str
	displayName := tc.Get("displayName").Str
	args := tc.Get("args")

	switch name {
	case "read_file":
		return fmt.Sprintf(
			"[Read: %s]", args.Get("file_path").Str,
		)
	case "write_file":
		return fmt.Sprintf(
			"[Write: %s]", args.Get("file_path").Str,
		)
	case "edit_file", "replace":
		return fmt.Sprintf(
			"[Edit: %s]", args.Get("file_path").Str,
		)
	case "run_command", "execute_command", "run_shell_command":
		cmd := args.Get("command").Str
		return fmt.Sprintf("[Bash]\n$ %s", cmd)
	case "list_directory":
		return fmt.Sprintf(
			"[List: %s]", args.Get("dir_path").Str,
		)
	case "search_files", "grep", "grep_search":
		query := args.Get("query").Str
		if query == "" {
			query = args.Get("pattern").Str
		}
		return fmt.Sprintf("[Grep: %s]", query)
	case "glob":
		return fmt.Sprintf(
			"[Glob: %s]", args.Get("pattern").Str,
		)
	default:
		label := displayName
		if label == "" {
			label = name
		}
		return fmt.Sprintf("[Tool: %s]", label)
	}
}

// GeminiSessionID extracts the sessionId field from raw
// Gemini session JSON data without fully parsing.
func GeminiSessionID(data []byte) string {
	if id := gjson.GetBytes(data, "sessionId").Str; id != "" {
		return id
	}
	for len(data) > 0 {
		line, rest, _ := bytes.Cut(data, []byte("\n"))
		data = rest
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !gjson.ValidBytes(line) {
			continue
		}
		if id := gjson.GetBytes(line, "sessionId").Str; id != "" {
			return id
		}
	}
	return ""
}
