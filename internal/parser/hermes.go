// ABOUTME: Parses Hermes Agent JSONL session files into structured session data.
// ABOUTME: Handles Hermes's OpenAI-style message format with session_meta header,
// ABOUTME: user/assistant/tool roles, and function-call tool invocations.
package parser

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
)

type hermesStateSession struct {
	id               string
	source           string
	model            string
	parentSessionID  string
	startedAt        time.Time
	endedAt          time.Time
	messageCount     int
	inputTokens      int
	outputTokens     int
	cacheReadTokens  int
	cacheWriteTokens int
	reasoningTokens  int
	estimatedCost    sql.NullFloat64
	actualCost       sql.NullFloat64
	costStatus       string
	costSource       string
	title            string
	apiCallCount     int
}

type hermesStateMessage struct {
	role                string
	content             string
	toolCallID          string
	toolCalls           string
	timestamp           time.Time
	finishReason        string
	reasoning           string
	reasoningContent    string
	reasoningDetails    string
	codexReasoningItems string
	codexMessageItems   string
}

// ParseHermesArchive parses a Hermes root directory. If a state.db is
// present, it uses that database for session metadata and usage while
// selecting the richest available message stream. Without state.db it
// falls back to the transcript-file parser.
func ParseHermesArchive(root, project, machine string) ([]ParseResult, error) {
	stateDB, sessionsDir, ok := hermesStatePaths(root)
	if !ok {
		return parseHermesTranscriptArchive(root, project, machine)
	}

	results, err := parseHermesStateDB(
		stateDB, sessionsDir, project, machine,
	)
	if err == nil {
		return results, nil
	}
	log.Printf(
		"hermes: state db parse failed for %s: %v; falling back to transcripts",
		stateDB, err,
	)
	return parseHermesTranscriptArchive(
		sessionsDir, project, machine,
	)
}

func parseHermesTranscriptArchive(
	root, project, machine string,
) ([]ParseResult, error) {
	var results []ParseResult
	for _, file := range discoverHermesTranscriptFiles(root) {
		fileProject := file.Project
		if project != "" {
			fileProject = project
		}
		sess, msgs, err := ParseHermesSession(
			file.Path, fileProject, machine,
		)
		if err != nil {
			return nil, err
		}
		if sess != nil {
			results = append(results, ParseResult{
				Session: *sess, Messages: msgs,
			})
		}
	}
	return results, nil
}

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
			isCompact := isHermesCompactBoundary(displayContent)

			if firstMsg == "" && displayContent != "" && !isCompact {
				firstMsg = truncate(
					strings.ReplaceAll(displayContent, "\n", " "),
					300,
				)
			}

			messages = append(messages, ParsedMessage{
				Ordinal:           ordinal,
				Role:              RoleUser,
				Content:           displayContent,
				Timestamp:         ts,
				ContentLength:     len(content),
				IsSystem:          isCompact,
				SourceType:        sourceTypeIf(isCompact, "system"),
				SourceSubtype:     sourceTypeIf(isCompact, "compact_boundary"),
				IsCompactBoundary: isCompact,
			})
			ordinal++
			if !isCompact {
				realUserCount++
			}

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
			isCompact := isHermesCompactBoundary(displayContent)

			if firstMsg == "" && displayContent != "" && !isCompact {
				firstMsg = truncate(
					strings.ReplaceAll(displayContent, "\n", " "),
					300,
				)
			}

			messages = append(messages, ParsedMessage{
				Ordinal:           ordinal,
				Role:              RoleUser,
				Content:           displayContent,
				Timestamp:         msgTS,
				ContentLength:     len(content),
				IsSystem:          isCompact,
				SourceType:        sourceTypeIf(isCompact, "system"),
				SourceSubtype:     sourceTypeIf(isCompact, "compact_boundary"),
				IsCompactBoundary: isCompact,
			})
			ordinal++
			if !isCompact {
				realUserCount++
			}

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

func hermesStatePaths(root string) (stateDB, sessionsDir string, ok bool) {
	if root == "" {
		return "", "", false
	}
	info, err := os.Stat(root)
	if err == nil && !info.IsDir() &&
		filepath.Base(root) == "state.db" {
		dir := filepath.Dir(root)
		return root, filepath.Join(dir, "sessions"), true
	}
	if st := filepath.Join(root, "state.db"); IsRegularFile(st) {
		return st, filepath.Join(root, "sessions"), true
	}
	if filepath.Base(root) == "sessions" {
		parent := filepath.Dir(root)
		st := filepath.Join(parent, "state.db")
		if IsRegularFile(st) {
			return st, root, true
		}
	}
	return "", "", false
}

func parseHermesStateDB(
	stateDB, sessionsDir, project, machine string,
) ([]ParseResult, error) {
	conn, err := sql.Open("sqlite3", "file:"+stateDB+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open hermes state db: %w", err)
	}
	defer conn.Close()

	sessions, err := readHermesStateSessions(conn)
	if err != nil {
		return nil, err
	}
	messages, err := readHermesStateMessages(conn)
	if err != nil {
		return nil, err
	}

	var results []ParseResult
	seen := make(map[string]struct{}, len(sessions))
	for _, ss := range sessions {
		res, ok := buildHermesStateResult(
			ss, messages[ss.id], sessionsDir, stateDB, project, machine,
		)
		if ok {
			results = append(results, res)
			seen[ss.id] = struct{}{}
		}
	}
	for _, file := range discoverHermesTranscriptFiles(sessionsDir) {
		rawID := HermesSessionID(filepath.Base(file.Path))
		if _, ok := seen[rawID]; ok {
			continue
		}
		sess, msgs, err := ParseHermesSession(
			file.Path, file.Project, machine,
		)
		if err != nil {
			return nil, err
		}
		if sess != nil {
			results = append(results, ParseResult{
				Session: *sess, Messages: msgs,
			})
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Session.ID < results[j].Session.ID
	})
	return results, nil
}

func readHermesStateSessions(
	conn *sql.DB,
) ([]hermesStateSession, error) {
	rows, err := conn.Query(`
		SELECT id, source, COALESCE(model, ''),
			COALESCE(parent_session_id, ''), started_at,
			COALESCE(ended_at, 0), COALESCE(message_count, 0),
			COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
			COALESCE(cache_read_tokens, 0),
			COALESCE(cache_write_tokens, 0),
			COALESCE(reasoning_tokens, 0),
			estimated_cost_usd, actual_cost_usd,
			COALESCE(cost_status, ''), COALESCE(cost_source, ''),
			COALESCE(title, ''), COALESCE(api_call_count, 0)
		FROM sessions
		ORDER BY started_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query hermes sessions: %w", err)
	}
	defer rows.Close()

	var out []hermesStateSession
	for rows.Next() {
		var ss hermesStateSession
		var started, ended float64
		if err := rows.Scan(
			&ss.id, &ss.source, &ss.model,
			&ss.parentSessionID, &started, &ended,
			&ss.messageCount, &ss.inputTokens, &ss.outputTokens,
			&ss.cacheReadTokens, &ss.cacheWriteTokens,
			&ss.reasoningTokens, &ss.estimatedCost, &ss.actualCost,
			&ss.costStatus, &ss.costSource, &ss.title,
			&ss.apiCallCount,
		); err != nil {
			return nil, fmt.Errorf("scan hermes session: %w", err)
		}
		ss.startedAt = hermesUnixTime(started)
		ss.endedAt = hermesUnixTime(ended)
		out = append(out, ss)
	}
	return out, rows.Err()
}

func readHermesStateMessages(
	conn *sql.DB,
) (map[string][]hermesStateMessage, error) {
	rows, err := conn.Query(`
		SELECT session_id, role, COALESCE(content, ''),
			COALESCE(tool_call_id, ''), COALESCE(tool_calls, ''),
			timestamp, COALESCE(finish_reason, ''),
			COALESCE(reasoning, ''), COALESCE(reasoning_content, ''),
			COALESCE(reasoning_details, ''),
			COALESCE(codex_reasoning_items, ''),
			COALESCE(codex_message_items, '')
		FROM messages
		ORDER BY session_id ASC, timestamp ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query hermes messages: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]hermesStateMessage)
	for rows.Next() {
		var sid string
		var hm hermesStateMessage
		var ts float64
		if err := rows.Scan(
			&sid, &hm.role, &hm.content, &hm.toolCallID,
			&hm.toolCalls, &ts, &hm.finishReason,
			&hm.reasoning, &hm.reasoningContent,
			&hm.reasoningDetails, &hm.codexReasoningItems,
			&hm.codexMessageItems,
		); err != nil {
			return nil, fmt.Errorf("scan hermes message: %w", err)
		}
		hm.timestamp = hermesUnixTime(ts)
		out[sid] = append(out[sid], hm)
	}
	return out, rows.Err()
}

func buildHermesStateResult(
	ss hermesStateSession, stateMessages []hermesStateMessage,
	sessionsDir, stateDB, project, machine string,
) (ParseResult, bool) {
	jsonPath := filepath.Join(sessionsDir, "session_"+ss.id+".json")
	jsonlPath := filepath.Join(sessionsDir, ss.id+".jsonl")

	var sess *ParsedSession
	var msgs []ParsedMessage
	var err error
	selectedPath := stateDB
	if IsRegularFile(jsonPath) {
		sess, msgs, err = parseHermesJSONSession(jsonPath, project, machine)
		if err == nil && sess != nil &&
			hermesMessageQuality(msgs) >= hermesStateQuality(stateMessages) {
			selectedPath = jsonPath
		} else {
			sess, msgs = nil, nil
		}
	}
	if sess == nil && IsRegularFile(jsonlPath) {
		sess, msgs, err = parseHermesJSONLSession(jsonlPath, project, machine)
		if err == nil && sess != nil &&
			(hermesMessageQuality(msgs) >= hermesStateQuality(stateMessages) || len(stateMessages) == 0) {
			selectedPath = jsonlPath
		} else {
			sess, msgs = nil, nil
		}
	}
	usageEvents := hermesUsageEvents(ss, "hermes:"+ss.id)
	if sess == nil {
		msgs = convertHermesStateMessages(stateMessages)
		if len(msgs) == 0 && len(usageEvents) == 0 {
			return ParseResult{}, false
		}
		sess = &ParsedSession{
			ID:               "hermes:" + ss.id,
			Agent:            AgentHermes,
			Machine:          machine,
			StartedAt:        ss.startedAt,
			EndedAt:          ss.endedAt,
			MessageCount:     len(msgs),
			UserMessageCount: countHermesUsers(msgs),
			FirstMessage:     firstHermesMessage(msgs),
		}
	}

	applyHermesStateMetadata(sess, ss, selectedPath, project)
	return ParseResult{
		Session:     *sess,
		Messages:    msgs,
		UsageEvents: usageEvents,
	}, true
}

func applyHermesStateMetadata(
	sess *ParsedSession, ss hermesStateSession, selectedPath, project string,
) {
	sess.ID = "hermes:" + ss.id
	sess.Agent = AgentHermes
	if project != "" {
		sess.Project = project
	} else if ss.source != "" {
		sess.Project = "hermes-" + ss.source
	} else if sess.Project == "" {
		sess.Project = "hermes"
	}
	if !ss.startedAt.IsZero() {
		sess.StartedAt = ss.startedAt
	}
	if !ss.endedAt.IsZero() {
		sess.EndedAt = ss.endedAt
	}
	if ss.parentSessionID != "" {
		sess.ParentSessionID = "hermes:" + ss.parentSessionID
		sess.RelationshipType = RelContinuation
	}
	sess.SourceSessionID = ss.id
	sess.SourceVersion = "hermes-state-db"
	sess.SessionName = ss.title

	// Populate the session-aggregate token columns from Hermes's own
	// authoritative state.db accounting. These feed the session list,
	// session detail, and stats portfolio, which read the aggregate
	// fields directly and do not fall back to usage_events. The
	// transcript paths yield 0 here (per-message token_count is 0 in
	// state.db), so these values are strictly better; set them
	// unconditionally when present. PeakContextTokens uses the
	// cumulative input + cache_read approximation (matching forge's
	// convention); a truer last-prompt peak lives only in sessions.json.
	if ss.outputTokens > 0 {
		sess.TotalOutputTokens = ss.outputTokens
		sess.HasTotalOutputTokens = true
	}
	if ctx := ss.inputTokens + ss.cacheReadTokens; ctx > 0 {
		sess.PeakContextTokens = ctx
		sess.HasPeakContextTokens = true
	}
	sess.aggregateTokenPresenceKnown =
		sess.HasTotalOutputTokens || sess.HasPeakContextTokens

	if selectedPath != "" {
		if info, err := os.Stat(selectedPath); err == nil {
			sess.File = FileInfo{
				Path:  selectedPath,
				Size:  info.Size(),
				Mtime: info.ModTime().UnixNano(),
			}
		}
	}
}

// hermesHasCostSource reports whether a Hermes cost_source represents a
// real cost determination. "none" (and empty) mean Hermes had no basis
// for the figure, so a $0 it pairs with cost_status "included" is a
// default placeholder rather than a confident free-usage signal.
func hermesHasCostSource(costSource string) bool {
	return costSource != "" && costSource != "none"
}

func hermesUsageEvents(
	ss hermesStateSession, sessionID string,
) []ParsedUsageEvent {
	if ss.model == "" {
		return nil
	}
	if ss.inputTokens == 0 && ss.outputTokens == 0 &&
		ss.cacheReadTokens == 0 && ss.cacheWriteTokens == 0 &&
		ss.reasoningTokens == 0 && !ss.estimatedCost.Valid &&
		!ss.actualCost.Valid {
		return nil
	}
	// Only emit a cost_usd when Hermes actually knows it. Otherwise
	// leave it nil so agentsview prices the row from its own model
	// catalog. A "included" cost_status is a genuine known $0 only when
	// a real cost_source backs it; Hermes also emits "included" with
	// cost_source "none" (or empty) as a default for models it does not
	// price (e.g. gpt-5.5), which is NOT a confident $0 and must fall
	// through to catalog pricing. Likewise "unknown"/empty with a 0
	// estimate is not a real figure and must not masquerade as $0.
	var cost *float64
	switch {
	case ss.actualCost.Valid:
		v := ss.actualCost.Float64
		cost = &v
	case ss.costStatus == "included" && hermesHasCostSource(ss.costSource):
		zero := 0.0
		cost = &zero
	case ss.estimatedCost.Valid && ss.estimatedCost.Float64 > 0:
		v := ss.estimatedCost.Float64
		cost = &v
	}
	return []ParsedUsageEvent{{
		SessionID:                sessionID,
		Source:                   "session",
		Model:                    ss.model,
		InputTokens:              max(ss.inputTokens, 0),
		OutputTokens:             max(ss.outputTokens, 0),
		CacheCreationInputTokens: max(ss.cacheWriteTokens, 0),
		CacheReadInputTokens:     max(ss.cacheReadTokens, 0),
		ReasoningTokens:          max(ss.reasoningTokens, 0),
		CostUSD:                  cost,
		CostStatus:               ss.costStatus,
		CostSource:               ss.costSource,
		OccurredAt:               timeString(ss.endedAt, ss.startedAt),
		DedupKey:                 "session:" + sessionID,
	}}
}

func convertHermesStateMessages(
	stateMessages []hermesStateMessage,
) []ParsedMessage {
	msgs := make([]ParsedMessage, 0, len(stateMessages))
	for _, hm := range stateMessages {
		ordinal := len(msgs)
		switch hm.role {
		case "user":
			content := strings.TrimSpace(hm.content)
			if content == "" {
				continue
			}
			display := stripHermesSkillPrefix(content)
			isCompact := isHermesCompactBoundary(display)
			msgs = append(msgs, ParsedMessage{
				Ordinal:           ordinal,
				Role:              RoleUser,
				Content:           display,
				Timestamp:         hm.timestamp,
				ContentLength:     len(content),
				IsSystem:          isCompact,
				SourceType:        sourceTypeIf(isCompact, "system"),
				SourceSubtype:     sourceTypeIf(isCompact, "compact_boundary"),
				IsCompactBoundary: isCompact,
			})
		case "assistant":
			content := strings.TrimSpace(hm.content)
			reasoning := firstNonEmptyHermes(
				hm.reasoning, hm.reasoningContent,
				hm.reasoningDetails, hm.codexReasoningItems,
			)
			display := content
			hasThinking := reasoning != ""
			if hasThinking {
				display = "[Thinking]\n" + reasoning +
					"\n[/Thinking]\n" + display
			}
			var toolCalls []ParsedToolCall
			if gjson.Valid(hm.toolCalls) {
				gjson.Parse(hm.toolCalls).ForEach(
					func(_, tc gjson.Result) bool {
						name := tc.Get("function.name").Str
						if name == "" {
							name = tc.Get("name").Str
						}
						if name != "" {
							toolCalls = append(toolCalls, ParsedToolCall{
								ToolUseID: tc.Get("id").Str,
								ToolName:  name,
								Category:  NormalizeToolCategory(name),
								InputJSON: tc.Get("function.arguments").Str,
							})
						}
						return true
					},
				)
			}
			if display == "" && len(toolCalls) == 0 {
				continue
			}
			msgs = append(msgs, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       display,
				Timestamp:     hm.timestamp,
				HasThinking:   hasThinking,
				HasToolUse:    len(toolCalls) > 0,
				ContentLength: len(content) + len(reasoning),
				ToolCalls:     toolCalls,
			})
		case "tool":
			if hm.toolCallID == "" {
				continue
			}
			quoted, _ := json.Marshal(hm.content)
			msgs = append(msgs, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Timestamp:     hm.timestamp,
				ContentLength: len(hm.content),
				ToolResults: []ParsedToolResult{{
					ToolUseID:     hm.toolCallID,
					ContentRaw:    string(quoted),
					ContentLength: len(hm.content),
				}},
			})
		}
	}
	return msgs
}

func hermesMessageQuality(msgs []ParsedMessage) int {
	score := len(msgs) * 1000
	for _, msg := range msgs {
		score += len(msg.Content)
		if len(msg.ToolCalls) > 0 {
			score += 100
		}
		if msg.HasThinking {
			score += 50
		}
	}
	return score
}

func hermesStateQuality(msgs []hermesStateMessage) int {
	score := len(msgs) * 1000
	for _, msg := range msgs {
		score += len(msg.content)
		if msg.toolCalls != "" {
			score += 100
		}
		if firstNonEmptyHermes(
			msg.reasoning, msg.reasoningContent,
			msg.reasoningDetails, msg.codexReasoningItems,
		) != "" {
			score += 50
		}
	}
	return score
}

func hermesUnixTime(v float64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	sec, frac := math.Modf(v)
	return time.Unix(int64(sec), int64(frac*1_000_000_000)).UTC()
}

func timeString(primary, fallback time.Time) string {
	if !primary.IsZero() {
		return primary.Format(time.RFC3339Nano)
	}
	if !fallback.IsZero() {
		return fallback.Format(time.RFC3339Nano)
	}
	return ""
}

func countHermesUsers(msgs []ParsedMessage) int {
	count := 0
	for _, msg := range msgs {
		if msg.Role == RoleUser && !msg.IsSystem &&
			len(msg.ToolResults) == 0 && strings.TrimSpace(msg.Content) != "" {
			count++
		}
	}
	return count
}

func firstHermesMessage(msgs []ParsedMessage) string {
	for _, msg := range msgs {
		if msg.Role == RoleUser && !msg.IsSystem &&
			strings.TrimSpace(msg.Content) != "" {
			return truncate(
				strings.ReplaceAll(msg.Content, "\n", " "),
				300,
			)
		}
	}
	return ""
}

func isHermesCompactBoundary(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "[CONTEXT COMPACTION - REFERENCE ONLY]") ||
		strings.HasPrefix(s, "[CONTEXT COMPACTION – REFERENCE ONLY]")
}

func sourceTypeIf(ok bool, value string) string {
	if ok {
		return value
	}
	return ""
}

func firstNonEmptyHermes(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// HermesSessionID extracts the session ID from a Hermes filename.
// "20260403_153620_5a3e2ff1.jsonl" -> "20260403_153620_5a3e2ff1"
func HermesSessionID(name string) string {
	name = strings.TrimSuffix(name, ".jsonl")
	name = strings.TrimSuffix(name, ".json")
	name = strings.TrimPrefix(name, "session_")
	return name
}

// DiscoverHermesSessions finds Hermes session sources. When a sibling
// state.db exists, it prefers that archive root; otherwise it returns
// transcript files from the sessions directory.
func DiscoverHermesSessions(sessionsDir string) []DiscoveredFile {
	if sessionsDir == "" {
		return nil
	}
	if stateDB, _, ok := hermesStatePaths(sessionsDir); ok {
		return []DiscoveredFile{{
			Path:  stateDB,
			Agent: AgentHermes,
		}}
	}
	childSessions := filepath.Join(sessionsDir, "sessions")
	if info, err := os.Stat(childSessions); err == nil && info.IsDir() {
		return discoverHermesTranscriptFiles(childSessions)
	}
	return discoverHermesTranscriptFiles(sessionsDir)
}

func discoverHermesTranscriptFiles(sessionsDir string) []DiscoveredFile {
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
