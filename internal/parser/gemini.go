// ABOUTME: Parses Gemini CLI session JSON files into structured session data.
// ABOUTME: Extracts messages, tool calls, thinking blocks, and token usage.
package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

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

	if !gjson.ValidBytes(data) {
		return nil, nil, fmt.Errorf("invalid JSON in %s", path)
	}

	root := gjson.ParseBytes(data)

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

	root.Get("messages").ForEach(
		func(_, msg gjson.Result) bool {
			msgType := msg.Get("type").Str
			if msgType != "user" && msgType != "gemini" {
				return true
			}

			ts := parseTimestamp(msg.Get("timestamp").Str)

			role := RoleUser
			if msgType == "gemini" {
				role = RoleAssistant
			}

			content, hasThinking, hasToolUse, tcs, trs :=
				extractGeminiContent(msg)
			if strings.TrimSpace(content) == "" {
				return true
			}

			if role == RoleUser && firstMessage == "" {
				firstMessage = truncate(
					strings.ReplaceAll(content, "\n", " "),
					300,
				)
			}

			tok := extractGeminiTokens(msg)
			var tokenUsage json.RawMessage
			tokResult := msg.Get("tokens")
			if tokResult.Exists() {
				tokenUsage = json.RawMessage(tokResult.Raw)
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          role,
				Content:       content,
				Timestamp:     ts,
				HasThinking:   hasThinking,
				HasToolUse:    hasToolUse,
				ContentLength: len(content),
				ToolCalls:     tcs,
				ToolResults:   trs,
				Model:         msg.Get("model").String(),
				TokenUsage:    tokenUsage,
				ContextTokens: tok.Input + tok.Cached,
				OutputTokens:  tok.Output,
			})
			ordinal++
			return true
		},
	)

	var (
		userCount   int
		totalOut    int
		peakContext int
	)
	for _, m := range messages {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
		}
		totalOut += m.OutputTokens
		if m.ContextTokens > peakContext {
			peakContext = m.ContextTokens
		}
	}

	sess := &ParsedSession{
		ID:                "gemini:" + sessionID,
		Project:           project,
		Machine:           machine,
		Agent:             AgentGemini,
		FirstMessage:      firstMessage,
		StartedAt:         startTime,
		EndedAt:           lastUpdated,
		MessageCount:      len(messages),
		UserMessageCount:  userCount,
		TotalOutputTokens: totalOut,
		PeakContextTokens: peakContext,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}

	return sess, messages, nil
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
	return gjson.GetBytes(data, "sessionId").Str
}
