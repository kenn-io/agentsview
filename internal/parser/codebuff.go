// ABOUTME: Parses Codebuff/Freebuff chat-messages.json session files into
// ABOUTME: structured session data. Both agents share the same on-disk layout
// ABOUTME: under ~/.config/manicode/projects/<project>/chats/<timestamp>/.
// ABOUTME: The agent type (codebuff vs freebuff) is determined from the
// ABOUTME: agentType field in run-state.json, and agentType is also
// ABOUTME: surfaced as the session's UsageEvent.Model so the daily usage
// ABOUTME: report can bucket similar sessions by template while leaving the literal
// LLM (server-selected and not persisted on disk) unknown.
package parser

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/money"
)

// codebuffSessionDir contains the session timestamp directory path and
// the project hint derived from the parent directory name.
type codebuffSessionDir struct {
	Path        string
	ProjectHint string
}

// parseCodebuffSession parses a single codebuff/freebuff session directory
// and returns the parsed session with messages.
func parseCodebuffSession(
	dir string,
	projectHint string,
	machine string,
) (*ParsedSession, []ParsedMessage, error) {
	chatMessagesPath := filepath.Join(dir, "chat-messages.json")
	runStatePath := filepath.Join(dir, "run-state.json")
	chatMetaPath := filepath.Join(dir, "chat-meta.json")

	// Read run-state.json for model, token, agent-type, and skills data.
	rs, err := readCodebuffRunState(runStatePath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("read run-state %s: %w", runStatePath, err)
	}

	// Read chat-meta.json for session name and timing hints.
	meta := readCodebuffChatMeta(chatMetaPath)

	// Read and parse the chat messages.
	data, err := os.ReadFile(chatMessagesPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read chat-messages %s: %w", chatMessagesPath, err)
	}
	if !gjson.ValidBytes(data) {
		return nil, nil, fmt.Errorf("decode %s: invalid json", chatMessagesPath)
	}

	// Session ID is the timestamp directory name (ISO 8601).
	sessionID := filepath.Base(dir)
	sessionDate := parseCodebuffSessionDate(sessionID)

	msgs, startedAt, endedAt, err := parseCodebuffMessages(data, sessionDate)
	if err != nil {
		return nil, nil, fmt.Errorf("parse chat-messages %s: %w", chatMessagesPath, err)
	}

	// Enrich tool calls with skill names by matching against the skills
	// catalog available to this session (run-state.json.fileContext.skills).
	// Codebuff/Freebuff invoke skills through generic tool calls (e.g.
	// run_terminal_command) rather than a dedicated Skill tool, so a tool
	// call is attributed to a skill when its name or input references a
	// known skill from the catalog.
	codebuffAttachSkillNames(msgs, rs.Skills)

	// Build session name from first user prompt.
	firstMsg := ""
	for _, msg := range msgs {
		if msg.Role == RoleUser && !msg.IsSystem &&
			strings.TrimSpace(msg.Content) != "" {
			firstMsg = truncate(
				strings.ReplaceAll(msg.Content, "\n", " "),
				300,
			)
			break
		}
	}
	if firstMsg == "" && meta.FirstPrompt != "" {
		firstMsg = truncate(
			strings.ReplaceAll(meta.FirstPrompt, "\n", " "),
			300,
		)
	}

	// Session name from first prompt (better than directory name).
	sessionName := firstMsg
	if len(sessionName) > 80 {
		sessionName = truncate(sessionName, 77)
	}
	if sessionName == "" {
		if rs.Cwd != "" {
			sessionName = filepath.Base(rs.Cwd)
		} else {
			sessionName = projectHint
		}
	}

	// Determine agent type from run-state agentType field.
	// Sessions with "free" in the agentType are Freebuff, others are Codebuff.
	// Both share the same on-disk layout; the parser splits them by type
	// so the UI can filter each agent independently.
	agent := AgentCodebuff
	agentLabel := "Codebuff"
	if strings.Contains(strings.ToLower(rs.AgentType), "free") {
		agent = AgentFreebuff
		agentLabel = "Freebuff"
	}

	// Count user messages.
	userMsgCount := 0
	for _, msg := range msgs {
		if msg.Role == RoleUser && !msg.IsSystem &&
			strings.TrimSpace(msg.Content) != "" {
			userMsgCount++
		}
	}
	messageCount := len(msgs)

	// If no messages from the transcript, use meta counts.
	if messageCount == 0 {
		messageCount = meta.MessageCount
		if meta.MessageCount > 0 {
			userMsgCount = 1 // at least one user prompt
		}
	}

	// Mark meta-derived counts authoritative. When the on-disk transcript is
	// empty but chat-meta.json reports a count, the meta totals are the
	// parser's only source for the session's user-visible counts. Without
	// this flag the sync engine's applySessionTokenTotalsFromMessages pass
	// would recompute counts from the empty parsed-message slice and
	// overwrite the meta totals with zero, hiding the session from any UI
	// that filters on nonzero counts. Set the flag only in the fallback case
	// (not when the transcript already provided counts) so the sync engine
	// keeps reconciling message-derived counts for sessions with real
	// transcripts.
	countsAuthoritative := len(msgs) == 0 && meta.MessageCount > 0

	// Source file identity: use chat-messages.json as the primary source.
	info, err := os.Stat(chatMessagesPath)
	fileInfo := FileInfo{
		Path: chatMessagesPath,
	}
	if err == nil {
		fileInfo.Size = info.Size()
		fileInfo.Mtime = info.ModTime().UnixNano()
	}

	// Use projectHint (the storage directory name) for the session ID
	// to ensure stability. The cwd-derived project name can change if
	// the git root changes, which would break source lookup and cause
	// session ID instability.
	projectID := projectHint
	if projectID == "" {
		projectID = "unknown"
	}
	fullID := string(agent) + ":" + projectID + ":" + sessionID

	// Derive display project from run-state cwd for UI display.
	// Use ExtractProjectFromCwd (git-root aware) rather than
	// GetProjectName because rs.Cwd is a full absolute path, not
	// a Claude-style encoded project name.
	project := projectHint
	if rs.Cwd != "" {
		if p := ExtractProjectFromCwd(rs.Cwd); p != "" {
			project = p
		}
	}

	// Fall back StartedAt/EndedAt for sessions whose transcript carries
	// no parseable message timestamps (empty chat-messages.json, or
	// messages without timestamp fields). Analytics and sorting need a
	// real timestamp; fall back to the session directory date, then the
	// source mtime as a last resort.
	if startedAt.IsZero() && !sessionDate.IsZero() {
		startedAt = sessionDate
	}
	if startedAt.IsZero() && fileInfo.Mtime > 0 {
		startedAt = time.Unix(0, fileInfo.Mtime)
	}
	if endedAt.IsZero() && !startedAt.IsZero() {
		endedAt = startedAt
	}

	sess := &ParsedSession{
		ID:                  fullID,
		Project:             project,
		Machine:             machine,
		Agent:               agent,
		AgentLabel:          agentLabel,
		Cwd:                 rs.Cwd,
		FirstMessage:        firstMsg,
		SessionName:         sessionName,
		StartedAt:           startedAt,
		EndedAt:             endedAt,
		MessageCount:        messageCount,
		UserMessageCount:    userMsgCount,
		CountsAuthoritative: countsAuthoritative,
		SourceSessionID:     sessionID,
		SourceVersion:       "codebuff-chat-v1",
		File:                fileInfo,
	}

	// contextTokenCount from run-state.json is the final per-step context
	// count, not the peak. Compaction can make the final value lower than
	// the true peak, so we cannot reliably derive PeakContextTokens from
	// this value. Leave peak context unavailable.

	// Emit usage event for reported credits. The actual LLM is
	// unknown (selected server-side, can change mid-session), so we
	// attribute the cost to the agent template (e.g. "base2-deepseek",
	// "base2-free-minimax-m3") rather than the agent name. The
	// template is granular enough to bucket similar sessions
	// separately in the daily model breakdown of the usage report
	// while remaining non-empty so the ue.model != '' eligibility
	// filter accepts the row. Skip emitting when the template is
	// missing so sessions with empty agentType don't surface as
	// empty-model rows. Freebuff vs codebuff distinction is kept in
	// the agent breakdown via sess.Agent. One codebuff credit =
	// $0.01 = 10_000 microdollars, rounded to the nearest microdollar
	// with halves away from zero.
	if rs.CreditsUsed > 0 && rs.AgentType != "" {
		cost, costErr := money.FromFloatDollars(rs.CreditsUsed * 0.01)
		if costErr != nil {
			cost = money.Money{}
		}
		// Determine occurred_at: prefer message timestamps, then fall
		// back to the session directory timestamp, then source mtime.
		occurredAt := startedAt
		if !endedAt.IsZero() {
			occurredAt = endedAt
		}
		if occurredAt.IsZero() && !sessionDate.IsZero() {
			occurredAt = sessionDate
		}
		if occurredAt.IsZero() && fileInfo.Mtime > 0 {
			occurredAt = time.Unix(0, fileInfo.Mtime)
		}
		sess.UsageEvents = []ParsedUsageEvent{{
			SessionID:  fullID,
			Source:     "session",
			Model:      rs.AgentType,
			OccurredAt: occurredAt.Format(time.RFC3339Nano),
			Cost:       &cost,
			CostStatus: "reported",
			CostSource: "session",
			DedupKey:   "session:" + fullID,
		}}
	}

	return sess, msgs, nil
}

// codebuffRunState holds extracted fields from run-state.json.
type codebuffRunState struct {
	AgentType         string
	ContextTokenCount int
	CreditsUsed       float64
	Cwd               string
	Skills            []codebuffSkill
}

// codebuffSkill is a single skill entry from the session's skill catalog
// (run-state.json sessionState.fileContext.skills). The catalog lists the
// skills available to the agent during the session.
type codebuffSkill struct {
	Name        string
	Description string
	FilePath    string
	Content     string
}

func readCodebuffRunState(path string) (codebuffRunState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return codebuffRunState{}, err
	}
	if !gjson.ValidBytes(data) {
		return codebuffRunState{}, fmt.Errorf("invalid json in %s", path)
	}

	mas := gjson.GetBytes(data, "sessionState.mainAgentState")
	rs := codebuffRunState{
		AgentType:         mas.Get("agentType").Str,
		ContextTokenCount: int(mas.Get("contextTokenCount").Int()),
		CreditsUsed:       mas.Get("creditsUsed").Float(),
		Cwd: gjson.GetBytes(data,
			"sessionState.fileContext.cwd").Str,
	}
	rs.Skills = parseCodebuffSkills(data)
	return rs, nil
}

// parseCodebuffSkills extracts the skill catalog from run-state.json
// (sessionState.fileContext.skills). The field is a JSON object keyed by
// skill name; each value carries name, description, optional content, and
// filePath. Returns an empty slice when no skills are present.
func parseCodebuffSkills(data []byte) []codebuffSkill {
	skills := gjson.GetBytes(data, "sessionState.fileContext.skills")
	if !skills.Exists() || !skills.IsObject() {
		return nil
	}
	var out []codebuffSkill
	skills.ForEach(func(key, val gjson.Result) bool {
		name := val.Get("name").Str
		if name == "" {
			name = key.Str
		}
		out = append(out, codebuffSkill{
			Name:        name,
			Description: val.Get("description").Str,
			FilePath:    val.Get("filePath").Str,
			Content:     val.Get("content").Str,
		})
		return true
	})
	return out
}

// codebuffAttachSkillNames attributes tool calls to skills from the
// session's skill catalog. Codebuff/Freebuff do not emit a dedicated Skill
// tool; skills are invoked through generic tools (e.g. run_terminal_command)
// whose input names the skill, or through a tool literally named "Skill".
// A tool call is attributed when its tool name matches a skill, or its input
// JSON references a known skill name.
func codebuffAttachSkillNames(msgs []ParsedMessage, skills []codebuffSkill) {
	if len(skills) == 0 {
		return
	}
	// byName maps the lowercased skill name to the catalog's canonical
	// casing so attribution can match case-insensitively while always
	// reporting the catalog spelling.
	byName := make(map[string]string, len(skills))
	for _, s := range skills {
		byName[strings.ToLower(s.Name)] = s.Name
	}
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			tc := &msgs[i].ToolCalls[j]
			if tc.SkillName != "" {
				continue
			}
			// Explicit Skill tool.
			if strings.EqualFold(tc.ToolName, "Skill") ||
				strings.EqualFold(tc.ToolName, "skill") {
				tc.SkillName = gjson.Get(tc.InputJSON, "skill").Str
				if tc.SkillName == "" {
					tc.SkillName = gjson.Get(tc.InputJSON, "name").Str
				}
				if tc.SkillName == "" {
					tc.SkillName = tc.ToolName
				}
				continue
			}
			// Tool name itself is a skill name.
			if _, ok := byName[strings.ToLower(tc.ToolName)]; ok {
				tc.SkillName = tc.ToolName
				continue
			}
			// Input JSON references a known skill name.
			if name := codebuffSkillNameFromInput(tc.InputJSON, byName); name != "" {
				tc.SkillName = name
			}
		}
	}
}

// codebuffSkillNameFromInput scans raw tool input JSON for a reference to a
// known skill name. It matches the skill name as a quoted JSON string value
// or as a standalone token (e.g. inside a shell command). byName maps the
// lowercased skill name to its canonical catalog casing; matches always
// return the canonical casing. When multiple catalog skills appear as
// tokens, the winner is deterministic: the first in lowercase-sorted
// order. Returns "" when no known skill is referenced.
func codebuffSkillNameFromInput(inputJSON string, byName map[string]string) string {
	if inputJSON == "" {
		return ""
	}
	// Direct JSON string/object match on common skill-carrying keys.
	for _, key := range []string{"skill", "name", "skill_name", "command", "prompt"} {
		v := gjson.Get(inputJSON, key).Str
		if v != "" {
			if canonical, ok := byName[strings.ToLower(v)]; ok {
				return canonical
			}
		}
	}
	// Fall back to scanning for any known skill name as a whole token.
	// Sort the candidate names so the winner is deterministic when two
	// catalog skills both appear in the input.
	lower := strings.ToLower(inputJSON)
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if containsSkillToken(lower, name) {
			return byName[name]
		}
	}
	return ""
}

// containsSkillToken reports whether lower contains name as a whole
// alphanumeric token (word-boundary match). It splits lower on
// non-alphanumeric runes and compares each token against name.
// This avoids false positives from substring matching (e.g. "go"
// matching "going" or "cargo").
func containsSkillToken(lower, name string) bool {
	if name == "" {
		return false
	}
	var buf []byte
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			buf = append(buf, c)
		} else {
			if string(buf) == name {
				return true
			}
			buf = buf[:0]
		}
	}
	return string(buf) == name
}

// codebuffChatMeta holds extracted fields from chat-meta.json.
type codebuffChatMeta struct {
	MessageCount int
	FirstPrompt  string
	MessagesSize int64
}

func readCodebuffChatMeta(path string) codebuffChatMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return codebuffChatMeta{}
	}
	if !gjson.ValidBytes(data) {
		return codebuffChatMeta{}
	}
	return codebuffChatMeta{
		MessageCount: int(gjson.GetBytes(data, "messageCount").Int()),
		FirstPrompt:  gjson.GetBytes(data, "firstPrompt").Str,
		MessagesSize: gjson.GetBytes(data, "messagesSize").Int(),
	}
}

// IsCodebuffTimestamp reports whether s matches one of the
// on-disk session-directory timestamp shapes that parseCodebuffSession
// treats as the bare session ID suffix of the canonical
// "agent:<project>:<ts>" id. Used by the session-get resolver to
// distinguish a Codebuff/Freebuff timestamp from a bare UUID
// (Codex, Copilot, Gemini, ...) that the generic prefix resolver
// handles. Returns true when s parses as any of the four ISO-8601
// forms accepted by parseCodebuffSessionDate:
//
//   - "2026-07-16T00-09-00.236Z"   (full ISO with millis and Z)
//   - "2026-07-16T00-09-00Z"       (full ISO without millis)
//   - "2026-07-16T00-09-00.123"    (full ISO without Z)
//   - "2026-07-16"                 (basic ISO date only)
//
// Everything else (UUIDs, numeric Unix epochs, free-form strings)
// returns false so the generic resolver path stays open. The
// predicate is purely syntactic — no FS walk — so it is cheap on
// --server and --pg transports where a FS scan is wasted work.
func IsCodebuffTimestamp(s string) bool {
	return !parseCodebuffSessionDate(s).IsZero()
}

// parseCodebuffSessionDate parses the session directory name as an ISO 8601
// timestamp. The directory name format is "2026-07-16T00-09-00.236Z".
// The returned time is always in the local timezone so that time-only
// message timestamps (HH:MM PM) combine correctly with the date.
func parseCodebuffSessionDate(sessionID string) time.Time {
	// Try full ISO format with milliseconds and Z suffix.
	if ts, err := time.Parse("2006-01-02T15-04-05.999Z", sessionID); err == nil {
		return ts.In(time.Local)
	}
	// Try without milliseconds.
	if ts, err := time.Parse("2006-01-02T15-04-05Z", sessionID); err == nil {
		return ts.In(time.Local)
	}
	// Try with milliseconds, no Z. Interpret as local time since
	// codebuff records wall-clock timestamps without a UTC offset.
	if ts, err := time.ParseInLocation("2006-01-02T15-04-05.999", sessionID, time.Local); err == nil {
		return ts
	}
	// Try basic ISO date only.
	if ts, err := time.ParseInLocation("2006-01-02", sessionID, time.Local); err == nil {
		return ts
	}
	return time.Time{}
}

// parseCodebuffMessages parses chat-messages.json data into ParsedMessages.
// sessionDate provides the date context for time-only timestamps. Message
// Model stays empty: the LLM is selected server-side per agentType template
// and is not persisted in the on-disk format.
func parseCodebuffMessages(
	data []byte, sessionDate time.Time,
) ([]ParsedMessage, time.Time, time.Time, error) {
	root := gjson.ParseBytes(data)
	if !root.IsArray() {
		return nil, time.Time{}, time.Time{},
			fmt.Errorf("chat-messages.json root is not an array")
	}

	var (
		messages  []ParsedMessage
		startedAt time.Time
		endedAt   time.Time
		ordinal   int
		// Track the current date for cross-midnight sessions. Start with
		// the session directory date and advance when time-of-day wraps
		// past midnight.
		currentDate = sessionDate
		prevHour    = -1
	)
	// Seed the rollover state from the session creation time-of-day so
	// the first time-only message can roll past midnight. A session
	// created late in the local evening (directory
	// 2026-07-17T06-58-00.000Z = 23:58 July 16 in UTC-7) whose first
	// message reads "12:01 AM" belongs to the next local calendar day;
	// without the seed, prevHour stays -1 until the second message and
	// the first message would be stamped ~24h before the session
	// started, skewing StartedAt.
	if !sessionDate.IsZero() {
		prevHour = sessionDate.Hour()
	}

	root.ForEach(func(_, msg gjson.Result) bool {
		variant := msg.Get("variant").Str
		ts := parseCodebuffTimestamp(
			msg.Get("timestamp").Str, currentDate,
		)

		// Detect midnight rollover for time-only timestamps only.
		// RFC3339 timestamps retain their timezone, so their hours
		// should not be compared with local time-only timestamps.
		rawTS := strings.TrimSpace(msg.Get("timestamp").Str)
		isTimeOnly := !strings.Contains(rawTS, "T") &&
			!strings.Contains(rawTS, "-") &&
			strings.Contains(rawTS, ":")
		if !ts.IsZero() && prevHour >= 0 && isTimeOnly {
			if ts.Hour() < prevHour {
				currentDate = currentDate.AddDate(0, 0, 1)
				// Re-parse with the advanced date.
				ts = parseCodebuffTimestamp(
					msg.Get("timestamp").Str, currentDate,
				)
			}
		}
		// Only track prevHour for time-only timestamps to avoid
		// incorrect rollover when formats are mixed. Reset prevHour
		// when a non-time-only timestamp is encountered, and also
		// anchor currentDate to the absolute timestamp's local
		// calendar date so subsequent time-only messages don't get
		// assigned to the previous session directory date across
		// midnight.
		if !ts.IsZero() {
			if isTimeOnly {
				prevHour = ts.Hour()
			} else {
				prevHour = -1
				tsLocal := ts.In(currentDate.Location())
				tsDate := time.Date(
					tsLocal.Year(), tsLocal.Month(), tsLocal.Day(),
					0, 0, 0, 0, currentDate.Location(),
				)
				if !tsDate.Equal(currentDate) {
					currentDate = tsDate
				}
			}
		}

		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(endedAt) {
				endedAt = ts
			}
		}

		switch variant {
		case "user":
			content := strings.TrimSpace(msg.Get("content").Str)
			// User messages can also carry blocks (e.g. images).
			// Collect image references from blocks to append to content.
			if blocks := msg.Get("blocks"); blocks.IsArray() {
				blocks.ForEach(func(_, block gjson.Result) bool {
					if block.Get("type").Str == "image" {
						filename := block.Get("filename").Str
						if filename != "" {
							content += "\n[Image: " + filename + "]"
						} else {
							content += "\n[Image attached]"
						}
					}
					return true
				})
				content = strings.TrimSpace(content)
			}
			if content == "" {
				return true
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       content,
				Timestamp:     ts,
				ContentLength: len(content),
			})
			ordinal++

		case "ai":
			parsed := parseCodebuffAIMessage(msg, ts)
			if len(parsed) == 0 {
				return true
			}
			for i := range parsed {
				parsed[i].Ordinal = ordinal
				ordinal++
			}
			messages = append(messages, parsed...)

		case "error":
			// Error messages from the upstream CLI (API failures, rate
			// limits, country blocks). Emit as a system message so the
			// error is visible in the transcript.
			content := strings.TrimSpace(msg.Get("content").Str)
			if content == "" {
				return true
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleSystem,
				Content:       content,
				Timestamp:     ts,
				ContentLength: len(content),
				IsSystem:      true,
			})
			ordinal++
		}

		return true
	})

	return messages, startedAt, endedAt, nil
}

// parseCodebuffAIMessage parses an AI-variant message into one or more
// ParsedMessages. AI messages contain blocks: text (reasoning or regular),
// tool calls, and subagent invocations. Blocks are processed sequentially
// to preserve the interleaving order of text, tools, and results.
func parseCodebuffAIMessage(
	msg gjson.Result,
	ts time.Time,
) []ParsedMessage {
	blocks := msg.Get("blocks")
	if !blocks.IsArray() {
		return nil
	}

	// textEntry tracks a text block with its type to preserve interleaving.
	type textEntry struct {
		content  string
		isReason bool
	}
	var (
		out         []ParsedMessage
		textBuf     []textEntry
		toolCalls   []ParsedToolCall
		toolResults []ParsedToolResult
	)

	// flushText emits accumulated text entries in order, grouping
	// consecutive entries of the same type.
	flushText := func() {
		if len(textBuf) == 0 {
			return
		}
		// Group consecutive entries of the same type.
		var thinkingParts, regularParts []string
		for _, entry := range textBuf {
			if entry.isReason {
				// Flush regular text before starting a thinking block.
				if len(regularParts) > 0 {
					text := strings.Join(regularParts, "\n\n")
					out = append(out, ParsedMessage{
						Role:          RoleAssistant,
						Content:       text,
						Timestamp:     ts,
						ContentLength: len(text),
					})
					regularParts = nil
				}
				thinkingParts = append(thinkingParts, entry.content)
			} else {
				// Flush thinking before starting regular text.
				if len(thinkingParts) > 0 {
					thinkingText := strings.Join(thinkingParts, "\n\n")
					out = append(out, ParsedMessage{
						Role:          RoleAssistant,
						Content:       "[Thinking]\n" + thinkingText + "\n[/Thinking]",
						ThinkingText:  thinkingText,
						HasThinking:   true,
						Timestamp:     ts,
						ContentLength: len(thinkingText),
					})
					thinkingParts = nil
				}
				regularParts = append(regularParts, entry.content)
			}
		}
		// Flush any remaining.
		if len(thinkingParts) > 0 {
			thinkingText := strings.Join(thinkingParts, "\n\n")
			out = append(out, ParsedMessage{
				Role:          RoleAssistant,
				Content:       "[Thinking]\n" + thinkingText + "\n[/Thinking]",
				ThinkingText:  thinkingText,
				HasThinking:   true,
				Timestamp:     ts,
				ContentLength: len(thinkingText),
			})
		}
		if len(regularParts) > 0 {
			text := strings.Join(regularParts, "\n\n")
			out = append(out, ParsedMessage{
				Role:          RoleAssistant,
				Content:       text,
				Timestamp:     ts,
				ContentLength: len(text),
			})
		}
		textBuf = nil
	}

	// flushTools emits accumulated tool calls as a single assistant message,
	// then emits each tool result as a user message.
	flushTools := func() {
		if len(toolCalls) > 0 {
			out = append(out, ParsedMessage{
				Role:       RoleAssistant,
				Timestamp:  ts,
				HasToolUse: true,
				ToolCalls:  toolCalls,
			})
			toolCalls = nil
		}
		for _, tr := range toolResults {
			out = append(out, ParsedMessage{
				Role:          RoleUser,
				Timestamp:     ts,
				ToolResults:   []ParsedToolResult{tr},
				ContentLength: tr.ContentLength,
			})
		}
		toolResults = nil
	}

	// Track whether we're currently accumulating tool calls to batch
	// consecutive tool blocks together.
	inToolRun := false

	blocks.ForEach(func(_, block gjson.Result) bool {
		blockType := block.Get("type").Str
		isTool := blockType == "tool" || blockType == "agent"

		// Flush on transition away from a tool run.
		if inToolRun && !isTool {
			flushText()
			flushTools()
			inToolRun = false
		}

		switch blockType {
		case "text":
			// Flush accumulated tools before text to preserve ordering.
			if len(toolCalls) > 0 {
				flushTools()
			}
			textType := block.Get("textType").Str
			content := block.Get("content").Str
			if strings.TrimSpace(content) == "" {
				return true
			}
			isReason := textType == "reasoning"
			textBuf = append(textBuf, textEntry{content: content, isReason: isReason})

		case "tool":
			if !inToolRun {
				flushText()
				inToolRun = true
			}
			tc := parseCodebuffToolCall(block)
			if tc != nil {
				toolCalls = append(toolCalls, *tc)
				if output := block.Get("output"); output.Exists() {
					toolResults = append(toolResults, ParsedToolResult{
						ToolUseID:     tc.ToolUseID,
						ContentRaw:    output.Raw,
						ContentLength: len(output.Raw),
					})
				}
			}

		case "agent":
			if !inToolRun {
				flushText()
				inToolRun = true
			}

			agentType := block.Get("agentType").Str
			agentName := block.Get("agentName").Str
			agentID := block.Get("agentId").Str
			agentStatus := block.Get("status").Str

			inputParts := map[string]any{
				"agentType": agentType,
				"agentName": agentName,
			}
			if params := block.Get("params"); params.Exists() &&
				params.Raw != "null" {
				inputParts["params"] = params.Value()
			}
			if prompt := block.Get("initialPrompt"); prompt.Exists() &&
				prompt.Str != "" {
				inputParts["prompt"] = prompt.Str
			}
			// The agent's lifecycle status (spawned, complete, ...) used to
			// be rendered in the assistant text for the block; now that
			// agent output is emitted as a linked ParsedToolResult, carry
			// the status in the tool-call input so it stays visible in the
			// parsed session.
			status := agentStatus
			if status == "" {
				status = "spawned"
			}
			inputParts["status"] = status

			inputJSON, _ := json.Marshal(inputParts, json.Deterministic(true))

			tc := ParsedToolCall{
				ToolUseID: agentID,
				ToolName:  agentType,
				Category:  "Task",
				InputJSON: string(inputJSON),
			}
			toolCalls = append(toolCalls, tc)

			// Emit agent output as a linked ParsedToolResult rather
			// than an ordinary assistant text message. Representing
			// the output as a tool result lets the configured result-
			// content blocking system (BlockedResultCategories)
			// strip it when the Task category is blocked. Without
			// this, agent-block output stored as ordinary assistant
			// text survives blocking and retains content the operator
			// explicitly configured agentsview not to store.
			if output := block.Get("content"); output.Exists() && output.Str != "" {
				toolResults = append(toolResults, ParsedToolResult{
					ToolUseID:     agentID,
					ContentRaw:    output.Raw,
					ContentLength: len(output.Raw),
				})
			}

		case "mode-divider":
			flushText()
			flushTools()
			mode := block.Get("mode").Str
			if mode != "" {
				// Emit system blocks immediately, not deferred.
				out = append(out, ParsedMessage{
					Role:          RoleSystem,
					Content:       "[Mode: " + mode + "]",
					Timestamp:     ts,
					ContentLength: len("[Mode: " + mode + "]"),
					IsSystem:      true,
				})
			}

		case "plan":
			flushText()
			flushTools()
			content := block.Get("content").Str
			if strings.TrimSpace(content) != "" {
				// Emit system blocks immediately, not deferred.
				out = append(out, ParsedMessage{
					Role:          RoleSystem,
					Content:       "[Plan]\n" + content,
					Timestamp:     ts,
					ContentLength: len("[Plan]\n" + content),
					IsSystem:      true,
				})
			}

		case "ask-user":
			flushText()
			flushTools()
			questions := block.Get("questions")
			if questions.IsArray() {
				var parts []string
				questions.ForEach(func(_, q gjson.Result) bool {
					questionText := q.Get("question").Str
					if strings.TrimSpace(questionText) != "" {
						parts = append(parts, "[Agent asked] "+questionText)
					}
					return true
				})
				if len(parts) > 0 {
					content := strings.Join(parts, "\n")
					out = append(out, ParsedMessage{
						Role:          RoleSystem,
						Content:       content,
						Timestamp:     ts,
						ContentLength: len(content),
						IsSystem:      true,
					})
				}
			}

		case "image":
			if block.Get("filename").Str != "" {
				textBuf = append(textBuf, textEntry{
					content:  "[Image: " + block.Get("filename").Str + "]",
					isReason: false,
				})
			} else {
				textBuf = append(textBuf, textEntry{
					content:  "[Image attached]",
					isReason: false,
				})
			}
		}
		return true
	})

	// Flush any remaining accumulated content.
	flushText()
	flushTools()

	if len(out) == 0 {
		return nil
	}
	return out
}

// parseCodebuffToolCall extracts a ParsedToolCall from a tool block.
func parseCodebuffToolCall(block gjson.Result) *ParsedToolCall {
	toolName := block.Get("toolName").Str
	if toolName == "" {
		return nil
	}
	toolCallID := block.Get("toolCallId").Str
	input := block.Get("input")

	inputJSON := ""
	if input.Exists() && input.Raw != "" && input.Raw != "null" {
		inputJSON = input.Raw
	}

	return &ParsedToolCall{
		ToolUseID: toolCallID,
		ToolName:  toolName,
		Category:  NormalizeToolCategory(toolName),
		InputJSON: inputJSON,
	}
}

// parseCodebuffTimestamp parses a timestamp string. Codebuff/freebuff
// messages use "HH:MM PM" format with the date provided by the session
// directory name. The sessionDate carries the date context.
func parseCodebuffTimestamp(s string, sessionDate time.Time) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}

	// ISO format (used by newer builds or subagent messages).
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return ts
	}
	if ts, err := time.Parse("2006-01-02T15:04:05.999Z07:00", s); err == nil {
		return ts
	}

	// "HH:MM PM" format: combine with the session date.
	if ts, err := time.Parse("03:04 PM", s); err == nil {
		if sessionDate.IsZero() {
			return time.Time{}
		}
		// Combine date from session with time from message.
		return time.Date(
			sessionDate.Year(),
			sessionDate.Month(),
			sessionDate.Day(),
			ts.Hour(),
			ts.Minute(),
			0, 0,
			sessionDate.Location(),
		)
	}

	return time.Time{}
}

// discoverCodebuffSessions finds all session directories under a root.
// root is the parent projects directory (~/.config/manicode/projects).
// Sessions live under <root>/<project>/chats/<timestamp>/.
func discoverCodebuffSessions(root string) []codebuffSessionDir {
	var dirs []codebuffSessionDir
	_ = codebuffDiscoverEach(context.Background(), root, func(match singleFileMatch) error {
		dirs = append(dirs, codebuffSessionDir{
			Path:        filepath.Dir(match.Path),
			ProjectHint: match.ProjectHint,
		})
		return nil
	})
	return dirs
}

// codebuffDiscoverEach streams session discoveries, yielding each match
// as it is found. This avoids materializing the entire archive in memory.
func codebuffDiscoverEach(
	ctx context.Context, root string, yield func(singleFileMatch) error,
) error {
	// Stream project directories.
	return streamDirectoryEntries(ctx, root, func(projectEntry os.DirEntry) error {
		if !projectEntry.IsDir() {
			return nil
		}
		projectName := projectEntry.Name()
		chatsDir := filepath.Join(root, projectName, "chats")
		// Stream session directories within each project.
		return streamDirectoryEntries(ctx, chatsDir, func(sessionEntry os.DirEntry) error {
			if !sessionEntry.IsDir() {
				return nil
			}
			dir := filepath.Join(chatsDir, sessionEntry.Name())
			chatPath := filepath.Join(dir, "chat-messages.json")
			if !IsRegularFile(chatPath) {
				return nil
			}
			return yield(singleFileMatch{
				Path:        chatPath,
				ProjectHint: projectName,
			})
		})
	})
}

// codebuffProjectFromPath extracts the project name from a session
// file path. The path is rooted under ~/.config/manicode/projects/.
func codebuffProjectFromPath(path string) string {
	// Path is: <root>/<project>/chats/<timestamp>/chat-messages.json
	// We want the <project> component.
	// Walk up from chat-messages.json: dir=/timestamp, parent=chats, grandparent=<project>
	dir := filepath.Dir(path)            // <timestamp>
	chatsDir := filepath.Dir(dir)        // chats
	projectDir := filepath.Dir(chatsDir) // <project>
	return filepath.Base(projectDir)
}
