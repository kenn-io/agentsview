package parser

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

type grokSummaryFields struct {
	Summary       string
	FirstPrompt   string
	ModelID       string
	CreatedAt     string
	UpdatedAt     string
	LastActiveAt  string
	Hostname      string
	NumMessages   int
	WorktreeLabel string
	GitRootDir    string
	Cwd           string
	HeadBranch    string
}

type grokSignalMetrics struct {
	TotalOutputTokens int
	PeakContextTokens int
	UserMessageCount  int
	HasUserMessages   bool
}

func ParseGrokSummary(
	path, projectHint, machine string,
) (ParseResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ParseResult{}, fmt.Errorf("stat %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ParseResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	if !gjson.ValidBytes(data) {
		return ParseResult{}, fmt.Errorf("decode %s: invalid json", path)
	}

	summary := decodeGrokSummary(data)
	rawID := filepath.Base(filepath.Dir(path))
	if !IsValidSessionID(rawID) {
		return ParseResult{}, fmt.Errorf("invalid grok session id for %s", path)
	}
	sessionDir := filepath.Dir(path)
	signals, err := parseGrokSignals(filepath.Join(sessionDir, "signals.json"))
	if err != nil {
		return ParseResult{}, err
	}

	project, cwd := grokProjectAndCwd(summary, projectHint)
	startedAt := grokParseTime(summary.CreatedAt)
	endedAt := grokEndedAt(summary)

	messages, malformed, transcriptErr := parseGrokChatHistory(
		filepath.Join(sessionDir, "chat_history.jsonl"),
	)
	if transcriptErr != nil && !os.IsNotExist(transcriptErr) {
		return ParseResult{}, transcriptErr
	}

	firstPrompt := ""
	for _, msg := range messages {
		if msg.Role == RoleUser && strings.TrimSpace(msg.Content) != "" {
			firstPrompt = strings.TrimSpace(msg.Content)
			break
		}
	}
	if firstPrompt == "" {
		firstPrompt = strings.TrimSpace(summary.FirstPrompt)
	}
	// Current Grok Build stores the searchable prompt text in
	// session_summary / generated_title rather than firstPrompt.
	if firstPrompt == "" {
		firstPrompt = strings.TrimSpace(summary.Summary)
	}

	userMessageCount := 0
	messageCount := 0
	countsAuthoritative := false
	transcriptFidelity := TranscriptFidelityFull
	sourceVersion := "grok-chat-v1"

	if len(messages) > 0 {
		for _, msg := range messages {
			messageCount++
			// Tool-result carrier rows are RoleUser with empty content so the
			// sync engine can pair them onto tool calls and then drop them.
			if msg.Role == RoleUser && !msg.IsSystem &&
				strings.TrimSpace(msg.Content) != "" {
				userMessageCount++
			}
		}
	} else {
		// Fall back to summary-only when chat_history is missing/empty.
		transcriptFidelity = TranscriptFidelitySummary
		sourceVersion = "grok-summary-v1"
		countsAuthoritative = true
		switch {
		case signals.HasUserMessages:
			userMessageCount = signals.UserMessageCount
		case firstPrompt != "":
			userMessageCount = 1
		}
		messageCount = max(summary.NumMessages, userMessageCount)
		if firstPrompt != "" {
			messages = []ParsedMessage{{
				Role:      RoleUser,
				Content:   firstPrompt,
				Timestamp: startedAt,
			}}
		}
	}

	result := ParseResult{
		Session: ParsedSession{
			ID:                 "grok:" + rawID,
			Project:            project,
			Machine:            machine,
			Agent:              AgentGrok,
			Cwd:                cwd,
			GitBranch:          summary.HeadBranch,
			SourceSessionID:    rawID,
			SourceVersion:      sourceVersion,
			TranscriptFidelity: transcriptFidelity,
			MalformedLines:     malformed,
			FirstMessage: truncate(
				strings.ReplaceAll(firstPrompt, "\n", " "),
				300,
			),
			SessionName:         strings.TrimSpace(summary.Summary),
			StartedAt:           startedAt,
			EndedAt:             endedAt,
			MessageCount:        messageCount,
			UserMessageCount:    userMessageCount,
			CountsAuthoritative: countsAuthoritative,
			File: FileInfo{
				Path:  path,
				Size:  info.Size(),
				Mtime: info.ModTime().UnixNano(),
			},
		},
		Messages: messages,
	}
	if signals.TotalOutputTokens > 0 {
		result.Session.TotalOutputTokens = signals.TotalOutputTokens
		result.Session.HasTotalOutputTokens = true
	}
	if signals.PeakContextTokens > 0 {
		result.Session.PeakContextTokens = signals.PeakContextTokens
		result.Session.HasPeakContextTokens = true
	}
	result.Session.aggregateTokenPresenceKnown =
		result.Session.HasTotalOutputTokens ||
			result.Session.HasPeakContextTokens
	return result, nil
}

func parseGrokChatHistory(path string) ([]ParsedMessage, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	lr := newLineReader(f, maxLineSize)
	defer releaseLineReader(lr)

	var (
		messages     []ParsedMessage
		malformed    int
		pendingThink string
		hasPending   bool
		ordinal      int
	)

	flushThinking := func() {
		if !hasPending {
			return
		}
		messages = append(messages, ParsedMessage{
			Ordinal:       ordinal,
			Role:          RoleAssistant,
			Content:       "[Thinking]\n" + pendingThink + "\n[/Thinking]",
			ThinkingText:  pendingThink,
			HasThinking:   true,
			ContentLength: len(pendingThink),
		})
		ordinal++
		pendingThink = ""
		hasPending = false
	}

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !gjson.Valid(line) {
			malformed++
			continue
		}
		root := gjson.Parse(line)
		switch root.Get("type").Str {
		case "system":
			// System prompts are vendor boilerplate; skip them.
			continue

		case "user":
			flushThinking()
			content := grokUserContent(root.Get("content"))
			if content == "" {
				// Meta-only injections (user_info / system-reminder /
				// skills catalog) are not real user turns.
				continue
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       content,
				ContentLength: len(content),
			})
			ordinal++

		case "reasoning":
			text := grokReasoningText(root)
			if text == "" {
				continue
			}
			if hasPending {
				pendingThink += "\n\n" + text
			} else {
				pendingThink = text
				hasPending = true
			}

		case "assistant":
			content := strings.TrimSpace(root.Get("content").Str)
			toolCalls := grokToolCalls(root.Get("tool_calls"))
			if content == "" && len(toolCalls) == 0 && !hasPending {
				continue
			}
			msg := ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       content,
				ContentLength: len(content),
				Model:         strings.TrimSpace(root.Get("model_id").Str),
				ToolCalls:     toolCalls,
				HasToolUse:    len(toolCalls) > 0,
			}
			if hasPending {
				msg.HasThinking = true
				msg.ThinkingText = pendingThink
				msg.Content = "[Thinking]\n" + pendingThink + "\n[/Thinking]\n" + content
				msg.ContentLength = len(pendingThink) + len(content)
				pendingThink = ""
				hasPending = false
			}
			messages = append(messages, msg)
			ordinal++

		case "tool_result":
			flushThinking()
			toolCallID := strings.TrimSpace(root.Get("tool_call_id").Str)
			if toolCallID == "" {
				continue
			}
			content := root.Get("content")
			contentRaw := content.Raw
			contentLen := toolResultContentLength(content)
			if content.Type == gjson.String {
				// Preserve tool output as JSON-quoted string so
				// pairToolResults / DecodeContent can surface it.
				quoted, _ := json.Marshal(content.Str)
				contentRaw = string(quoted)
				contentLen = len(content.Str)
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				ContentLength: contentLen,
				ToolResults: []ParsedToolResult{{
					ToolUseID:     toolCallID,
					ContentRaw:    contentRaw,
					ContentLength: contentLen,
				}},
			})
			ordinal++

		default:
			// Unknown entry types are ignored but not treated as malformed.
			continue
		}
	}
	if err := lr.Err(); err != nil {
		return nil, malformed, fmt.Errorf("reading %s: %w", path, err)
	}
	flushThinking()
	return messages, malformed, nil
}

func grokUserContent(content gjson.Result) string {
	var text string
	switch {
	case content.Type == gjson.String:
		text = content.Str
	case content.IsArray():
		var parts []string
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").Str == "text" {
				if t := part.Get("text").Str; t != "" {
					parts = append(parts, t)
				}
			}
			return true
		})
		text = strings.Join(parts, "\n")
	default:
		return ""
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	// Prefer the explicit user query when Grok wraps prompts.
	if extracted := extractUserQuery(strings.Split(text, "\n")); extracted != text {
		return extracted
	}
	if strings.Contains(text, "<user_query>") {
		return extractUserQuery(strings.Split(text, "\n"))
	}
	// Strip injected context blocks; keep any remaining real prompt text.
	// Meta-only payloads collapse to empty and are skipped by the caller.
	return grokStripMetaUserBlocks(text)
}

// grokStripMetaUserBlocks removes recognized Grok context-injection blocks
// (user_info, git_status, system-reminder, agent_skills, mcp_servers) while
// preserving any surrounding user text. Mixed payloads therefore keep the
// real prompt instead of being discarded wholesale.
func grokStripMetaUserBlocks(text string) string {
	for _, tag := range []string{
		"user_info",
		"git_status",
		"system-reminder",
		"agent_skills",
		"mcp_servers",
	} {
		text = grokStripXMLTagBlock(text, tag)
	}
	return strings.TrimSpace(text)
}

// grokStripXMLTagBlock removes every <tag>...</tag> span from text. An
// unclosed opening tag drops the remainder of the string from that point.
func grokStripXMLTagBlock(text, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	for {
		start := strings.Index(text, open)
		if start < 0 {
			return text
		}
		rest := text[start+len(open):]
		endRel := strings.Index(rest, close)
		if endRel < 0 {
			return strings.TrimSpace(text[:start])
		}
		end := start + len(open) + endRel + len(close)
		text = text[:start] + text[end:]
	}
}

func grokReasoningText(root gjson.Result) string {
	summary := root.Get("summary")
	if summary.IsArray() {
		var parts []string
		summary.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").Str == "summary_text" {
				if t := strings.TrimSpace(part.Get("text").Str); t != "" {
					parts = append(parts, t)
				}
			}
			return true
		})
		return strings.Join(parts, "\n\n")
	}
	return strings.TrimSpace(root.Get("content").Str)
}

func grokToolCalls(arr gjson.Result) []ParsedToolCall {
	if !arr.IsArray() {
		return nil
	}
	var out []ParsedToolCall
	arr.ForEach(func(_, tc gjson.Result) bool {
		name := firstNonEmptyJSONLString(
			tc.Get("name").Str,
			tc.Get("function.name").Str,
		)
		if name == "" {
			return true
		}
		inputJSON := grokToolCallInputJSON(tc)
		out = append(out, ParsedToolCall{
			ToolUseID: firstNonEmptyJSONLString(tc.Get("id").Str, tc.Get("tool_call_id").Str),
			ToolName:  name,
			Category:  NormalizeToolCategory(name),
			InputJSON: inputJSON,
			SkillName: inferToolSkillName(name, inputJSON),
		})
		return true
	})
	return out
}

// grokToolCallInputJSON picks the first present arguments field and normalizes
// JSON-encoded string values (OpenAI-style function.arguments) to the raw
// object JSON so path extraction and skill inference can read them.
func grokToolCallInputJSON(tc gjson.Result) string {
	for _, path := range []string{"arguments", "function.arguments", "input"} {
		args := tc.Get(path)
		if !args.Exists() {
			continue
		}
		if args.Type == gjson.String {
			// Unwrap JSON-encoded strings (OpenAI-style); plain text stays as-is.
			return args.Str
		}
		if raw := args.Raw; raw != "" && raw != "null" {
			return raw
		}
	}
	return ""
}

// grokSummaryMessageCount prefers the chat-transcript count over the broader
// event counter. Current Grok Build stores both: num_chat_messages is the
// transcript-shaped total AgentsView should surface, while num_messages also
// includes non-chat events and would inflate summary-only sessions.
func grokSummaryMessageCount(root gjson.Result) int {
	for _, path := range []string{
		"num_chat_messages",
		"num_messages",
		"numMessages",
	} {
		if v := root.Get(path); v.Exists() {
			return int(v.Int())
		}
	}
	return 0
}

func decodeGrokSummary(data []byte) grokSummaryFields {
	root := gjson.ParseBytes(data)
	return grokSummaryFields{
		Summary: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("session_summary").String()),
			strings.TrimSpace(root.Get("generated_title").String()),
			strings.TrimSpace(root.Get("summary").String()),
		),
		FirstPrompt: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("firstPrompt").String()),
			strings.TrimSpace(root.Get("first_prompt").String()),
		),
		ModelID: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("current_model_id").String()),
			strings.TrimSpace(root.Get("modelId").String()),
			strings.TrimSpace(root.Get("model_id").String()),
		),
		CreatedAt: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("created_at").String()),
			strings.TrimSpace(root.Get("createdAt").String()),
		),
		UpdatedAt: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("updated_at").String()),
			strings.TrimSpace(root.Get("updatedAt").String()),
		),
		LastActiveAt: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("last_active_at").String()),
			strings.TrimSpace(root.Get("lastActiveAt").String()),
		),
		Hostname:    strings.TrimSpace(root.Get("hostname").String()),
		NumMessages: grokSummaryMessageCount(root),
		WorktreeLabel: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("worktreeLabel").String()),
			strings.TrimSpace(root.Get("worktree_label").String()),
		),
		GitRootDir: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("git_root_dir").String()),
			strings.TrimSpace(root.Get("gitRootDir").String()),
		),
		Cwd: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("info.cwd").String()),
			strings.TrimSpace(root.Get("cwd").String()),
		),
		HeadBranch: firstNonEmptyJSONLString(
			strings.TrimSpace(root.Get("head_branch").String()),
			strings.TrimSpace(root.Get("headBranch").String()),
			strings.TrimSpace(root.Get("git.branch").String()),
		),
	}
}

func grokProjectAndCwd(
	summary grokSummaryFields, projectHint string,
) (project, cwd string) {
	cwd = firstNonEmptyJSONLString(
		strings.TrimSpace(summary.Cwd),
		strings.TrimSpace(summary.GitRootDir),
	)
	if cwd != "" {
		if p := ExtractProjectFromCwdWithBranch(cwd, summary.HeadBranch); p != "" {
			return p, cwd
		}
	}

	// Prefer the vendor-provided worktree label when present (legacy
	// camelCase summary schema). Fall back to the path-derived hint.
	if label := strings.TrimSpace(summary.WorktreeLabel); label != "" {
		return label, cwd
	}

	hint := strings.TrimSpace(projectHint)
	if decoded, err := url.PathUnescape(hint); err == nil && decoded != "" {
		hint = decoded
	}
	if hint != "" {
		if p := ExtractProjectFromCwdWithBranch(hint, summary.HeadBranch); p != "" {
			return p, cwd
		}
		if p := GetProjectName(hint); p != "" {
			return p, cwd
		}
	}
	return "", cwd
}

func parseGrokSignals(path string) (grokSignalMetrics, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return grokSignalMetrics{}, nil
	}
	if err != nil {
		return grokSignalMetrics{}, fmt.Errorf("read %s: %w", path, err)
	}
	if !gjson.ValidBytes(data) {
		return grokSignalMetrics{}, fmt.Errorf("decode %s: invalid json", path)
	}
	root := gjson.ParseBytes(data)
	metrics := grokSignalMetrics{
		TotalOutputTokens: grokFirstPositiveInt(
			data,
			"tokenUsage.totalOutputTokens",
			"usage.totalOutputTokens",
			"outputTokens",
			"totalOutputTokens",
		),
		PeakContextTokens: grokFirstPositiveInt(
			data,
			"tokenUsage.peakContextTokens",
			"usage.peakContextTokens",
			"peakContextTokens",
			"contextTokens",
			"contextTokensUsed",
		),
	}
	if userCount := root.Get("userMessageCount"); userCount.Exists() {
		metrics.HasUserMessages = true
		if n := int(userCount.Int()); n > 0 {
			metrics.UserMessageCount = n
		}
	}
	return metrics, nil
}

func grokFirstPositiveInt(data []byte, paths ...string) int {
	for _, path := range paths {
		value := gjson.GetBytes(data, path)
		if !value.Exists() {
			continue
		}
		if n := int(value.Int()); n > 0 {
			return n
		}
	}
	return 0
}

func grokParseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return ts
}

func grokEndedAt(summary grokSummaryFields) time.Time {
	for _, value := range []string{
		summary.LastActiveAt,
		summary.UpdatedAt,
		summary.CreatedAt,
	} {
		if ts := grokParseTime(value); !ts.IsZero() {
			return ts
		}
	}
	return time.Time{}
}
