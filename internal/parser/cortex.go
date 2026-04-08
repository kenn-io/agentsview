package parser

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// cortexBackupRe matches backup filenames like
// <uuid>.back.<timestamp>.json — these must be skipped.
var cortexBackupRe = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.back\.`,
)

// cortexSessionJSON is the top-level structure of a Cortex
// session file (<uuid>.json). When the session has grown beyond
// the in-process write limit, Cortex splits the conversation into
// a companion <uuid>.history.jsonl file and this JSON stores only
// metadata (no "history" key).
type cortexSessionJSON struct {
	SessionID        string            `json:"session_id"`
	Title            string            `json:"title"`
	History          []cortexMessage   `json:"history"`
	ConnectionName   string            `json:"connection_name"`
	WorkingDirectory string            `json:"working_directory"`
	GitRoot          string            `json:"git_root"`
	GitBranch        string            `json:"git_branch"`
	CreatedAt        string            `json:"created_at"`
	LastUpdated      string            `json:"last_updated"`
	HistoryLength    int               `json:"history_length"`
	SessionType      string            `json:"session_type"`
	PermissionCache  map[string]string `json:"permission_cache"`
}

// cortexMessage represents a single turn in the conversation history.
type cortexMessage struct {
	Role         string               `json:"role"`
	ID           string               `json:"id"`
	Content      []cortexContentBlock `json:"content"`
	UserSentTime string               `json:"user_sent_time"`
}

// cortexContentBlock is a single block inside a message's content array.
// Cortex uses a nested structure: tool_use and tool_result are wrapped
// inside an object under the key matching the block type.
type cortexContentBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	InternalOnly *bool             `json:"internalOnly"`
	IsUserPrompt *bool             `json:"is_user_prompt"`
	MessageID    string            `json:"message_id"`
	ToolUse      *cortexToolUse    `json:"tool_use"`
	ToolResult   *cortexToolResult `json:"tool_result"`
}

// cortexToolUse is the payload for a tool_use content block.
type cortexToolUse struct {
	ToolUseID string          `json:"tool_use_id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

// cortexToolResult is the payload for a tool_result content block.
type cortexToolResult struct {
	Name      string               `json:"name"`
	ToolUseID string               `json:"tool_use_id"`
	Content   []cortexContentBlock `json:"content"`
	Status    string               `json:"status"`
}

// isCortexInternalBlock reports whether a content block should be
// suppressed. Cortex marks injected system context with
// internalOnly=true and/or wraps it in <system-reminder> tags.
func isCortexInternalBlock(b cortexContentBlock) bool {
	if b.InternalOnly != nil && *b.InternalOnly {
		return true
	}
	if strings.Contains(b.Text, "<system-reminder>") {
		return true
	}
	return false
}

// extractCortexText extracts display text from a Cortex message,
// filtering internal-only blocks and system reminders.
func extractCortexText(msg cortexMessage) string {
	var parts []string
	for _, b := range msg.Content {
		if b.Type != "text" {
			continue
		}
		if isCortexInternalBlock(b) {
			continue
		}
		t := strings.TrimSpace(b.Text)
		if t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// hasRealUserContent reports whether a user message contains at
// least one non-internal text block.
func hasRealUserContent(msg cortexMessage) bool {
	for _, b := range msg.Content {
		if b.Type == "text" && !isCortexInternalBlock(b) {
			t := strings.TrimSpace(b.Text)
			if t != "" {
				return true
			}
		}
	}
	return false
}

// parseCortexMessages converts a slice of raw cortexMessage values
// into ParsedMessage entries. It skips:
//   - the entire first user turn (it contains only system reminders),
//     unless the message has actual user text
//   - user messages that consist solely of tool_result blocks
//
// Returns messages and the first real user prompt string.
func parseCortexMessages(
	history []cortexMessage,
	timestamps map[string]time.Time,
) ([]ParsedMessage, string) {
	var msgs []ParsedMessage
	var firstMessage string
	ordinal := 0

	for i, msg := range history {
		role := RoleType(msg.Role)
		if role != RoleUser && role != RoleAssistant {
			continue
		}

		// For user messages, skip turns that only contain
		// internal/system-reminder content.
		if role == RoleUser && i == 0 && !hasRealUserContent(msg) {
			continue
		}

		// Determine timestamp: prefer user_sent_time on user messages,
		// fall back to timestamps map (keyed by message ID).
		var ts time.Time
		if msg.UserSentTime != "" {
			ts, _ = time.Parse(time.RFC3339Nano, msg.UserSentTime)
			if ts.IsZero() {
				ts, _ = time.Parse(time.RFC3339, msg.UserSentTime)
			}
		}
		if ts.IsZero() {
			ts = timestamps[msg.ID]
		}

		// Build the display content string.
		text := extractCortexText(msg)

		// Collect tool calls (from assistant messages).
		var toolCalls []ParsedToolCall
		hasToolUse := false
		for _, b := range msg.Content {
			if b.Type == "tool_use" && b.ToolUse != nil {
				hasToolUse = true
				tu := b.ToolUse
				cat := NormalizeToolCategory(tu.Name)
				inputJSON := ""
				if len(tu.Input) > 0 && string(tu.Input) != "null" {
					inputJSON = string(tu.Input)
				}
				toolCalls = append(toolCalls, ParsedToolCall{
					ToolUseID: tu.ToolUseID,
					ToolName:  tu.Name,
					Category:  cat,
					InputJSON: inputJSON,
				})
			}
		}

		// Collect tool results (from user messages carrying tool_result blocks).
		var toolResults []ParsedToolResult
		for _, b := range msg.Content {
			if b.Type == "tool_result" && b.ToolResult != nil {
				tr := b.ToolResult
				raw, _ := json.Marshal(tr.Content)
				toolResults = append(toolResults, ParsedToolResult{
					ToolUseID:     tr.ToolUseID,
					ContentRaw:    string(raw),
					ContentLength: len(raw),
				})
			}
		}

		// User messages that only contain tool results are responses
		// to prior tool calls — include them but with empty content.
		if role == RoleUser &&
			text == "" &&
			len(toolCalls) == 0 &&
			len(toolResults) == 0 {
			continue
		}

		// Capture first real user prompt.
		if role == RoleUser && firstMessage == "" && text != "" {
			firstMessage = truncate(
				strings.ReplaceAll(text, "\n", " "), 300,
			)
		}

		// Build the content for display: use text if available,
		// otherwise synthesize from tool calls.
		content := text
		if content == "" && len(toolCalls) > 0 {
			var labels []string
			for _, tc := range toolCalls {
				labels = append(labels, formatCortexToolHeader(tc))
			}
			content = strings.Join(labels, "\n")
		}

		if content == "" && len(toolResults) > 0 {
			content = fmt.Sprintf("[%d tool result(s)]", len(toolResults))
		}

		if content == "" {
			continue
		}

		msgs = append(msgs, ParsedMessage{
			Ordinal:       ordinal,
			Role:          role,
			Content:       content,
			Timestamp:     ts,
			HasToolUse:    hasToolUse,
			ContentLength: len(content),
			ToolCalls:     toolCalls,
			ToolResults:   toolResults,
		})
		ordinal++
	}

	return msgs, firstMessage
}

// formatCortexToolHeader renders a one-line label for a tool call,
// mirroring the style used by the Claude and Codex parsers.
func formatCortexToolHeader(tc ParsedToolCall) string {
	if tc.InputJSON == "" {
		return formatToolHeader(tc.Category, tc.ToolName)
	}
	detail := cortexToolDetail(tc.ToolName, tc.InputJSON)
	return formatToolHeader(tc.Category, detail)
}

// cortexToolDetail extracts a concise label from a tool's input JSON.
func cortexToolDetail(name, inputJSON string) string {
	if !strings.HasPrefix(strings.TrimSpace(inputJSON), "{") {
		return name
	}
	input := make(map[string]json.RawMessage)
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return name
	}
	getString := func(keys ...string) string {
		for _, k := range keys {
			v, ok := input[k]
			if !ok {
				continue
			}
			var s string
			if err := json.Unmarshal(v, &s); err == nil {
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			}
		}
		return ""
	}
	switch name {
	case "bash":
		if cmd := getString("command", "cmd"); cmd != "" {
			first, _, _ := strings.Cut(cmd, "\n")
			return truncate("$ "+strings.TrimSpace(first), 200)
		}
	case "read":
		if p := getString("file_path", "path"); p != "" {
			return p
		}
	case "write":
		if p := getString("file_path", "path"); p != "" {
			return p
		}
	case "edit":
		if p := getString("file_path", "path"); p != "" {
			return p
		}
	case "grep":
		if pat := getString("pattern"); pat != "" {
			return pat
		}
	case "glob":
		if pat := getString("pattern"); pat != "" {
			if path := getString("path"); path != "" {
				return pat + " in " + path
			}
			return pat
		}
	case "web_fetch":
		if url := getString("url"); url != "" {
			return url
		}
	case "snowflake_sql_execute":
		if q := getString("query", "sql"); q != "" {
			first, _, _ := strings.Cut(q, "\n")
			return truncate(strings.TrimSpace(first), 200)
		}
	}
	return name
}

// parseCortexTimestamps builds a map from message ID to timestamp
// by scanning the history JSONL file. This is used for sessions that
// store their history in a companion .history.jsonl file, which
// contains no explicit time field other than what's in tool results
// or user_sent_time — but the JSONL append order gives us ordering.
//
// For now we return an empty map; timestamps will come from user_sent_time.
func parseCortexTimestamps(_ string) map[string]time.Time {
	return make(map[string]time.Time)
}

// ParseCortexSession parses a Cortex session from its .json metadata
// file. If the file contains an embedded "history" array, it is used
// directly. If no history is embedded (the split-file format), the
// companion .history.jsonl file is read instead.
func ParseCortexSession(
	path, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}

	var meta cortexSessionJSON
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if meta.SessionID == "" {
		return nil, nil, nil
	}

	// Choose history source: prefer embedded, fall back to JSONL.
	history := meta.History
	histFile := strings.TrimSuffix(path, ".json") + ".history.jsonl"

	if len(history) == 0 {
		loaded, err := readCortexHistoryJSONL(histFile)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf(
				"read history %s: %w", histFile, err,
			)
		}
		history = loaded
	}

	if len(history) == 0 {
		return nil, nil, nil
	}

	msgs, firstMessage := parseCortexMessages(
		history,
		parseCortexTimestamps(histFile),
	)

	startedAt, _ := time.Parse(time.RFC3339Nano, meta.CreatedAt)
	if startedAt.IsZero() {
		startedAt, _ = time.Parse(time.RFC3339, meta.CreatedAt)
	}
	endedAt, _ := time.Parse(time.RFC3339Nano, meta.LastUpdated)
	if endedAt.IsZero() {
		endedAt, _ = time.Parse(time.RFC3339, meta.LastUpdated)
	}

	// Derive timestamps from messages when meta fields are absent.
	for _, m := range msgs {
		if !m.Timestamp.IsZero() {
			if startedAt.IsZero() || m.Timestamp.Before(startedAt) {
				startedAt = m.Timestamp
			}
			if m.Timestamp.After(endedAt) {
				endedAt = m.Timestamp
			}
		}
	}

	project := extractCortexProject(meta)

	// Use the title if it's not the default auto-generated one.
	displayName := ""
	if meta.Title != "" &&
		!strings.HasPrefix(meta.Title, "Chat for session:") {
		displayName = meta.Title
	}

	userCount := 0
	for _, m := range msgs {
		if m.Role == RoleUser {
			userCount++
		}
	}

	// Always use the discovered .json file for File metadata, even
	// when we read from .history.jsonl. The sync engine tracks the
	// .json file for skip/hash logic, so this must match.
	sess := &ParsedSession{
		ID:               "cortex:" + meta.SessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentCortex,
		Cwd:              meta.WorkingDirectory,
		FirstMessage:     firstMessage,
		DisplayName:      displayName,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(msgs),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}

	return sess, msgs, nil
}

// readCortexHistoryJSONL reads a .history.jsonl file and returns the
// messages it contains. Each line is a JSON-encoded cortexMessage.
func readCortexHistoryJSONL(
	path string,
) ([]cortexMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var msgs []cortexMessage
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg cortexMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Role == "" {
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

// extractCortexProject derives a project name from session metadata,
// using the working directory or git root as the source.
func extractCortexProject(meta cortexSessionJSON) string {
	cwd := meta.WorkingDirectory
	if cwd == "" {
		cwd = meta.GitRoot
	}
	branch := meta.GitBranch
	if proj := ExtractProjectFromCwdWithBranch(cwd, branch); proj != "" {
		return proj
	}
	return "unknown"
}

// IsCortexBackupFile reports whether a filename is a Cortex backup
// file (e.g. <uuid>.back.<timestamp>.json) that should be ignored
// during discovery.
func IsCortexBackupFile(name string) bool {
	return cortexBackupRe.MatchString(name)
}

// IsCortexSessionFile reports whether name is a primary Cortex
// session metadata file: a UUID followed by ".json" (but not a
// backup or .history.jsonl file).
func IsCortexSessionFile(name string) bool {
	if !strings.HasSuffix(name, ".json") {
		return false
	}
	if IsCortexBackupFile(name) {
		return false
	}
	stem := strings.TrimSuffix(name, ".json")
	return IsValidSessionID(stem)
}

// DiscoverCortexSessions finds all primary session metadata files
// in the Cortex conversations directory (~/.snowflake/cortex/conversations).
// Backup files (*.back.*.json) are silently skipped. Both embedded-history
// sessions (<uuid>.json with a "history" key) and split sessions
// (<uuid>.json + <uuid>.history.jsonl) are returned as a single entry
// pointing to the .json metadata file.
func DiscoverCortexSessions(
	conversationsDir string,
) []DiscoveredFile {
	if conversationsDir == "" {
		return nil
	}

	entries, err := os.ReadDir(conversationsDir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !IsCortexSessionFile(name) {
			continue
		}
		files = append(files, DiscoveredFile{
			Path:  filepath.Join(conversationsDir, name),
			Agent: AgentCortex,
		})
	}

	return files
}

// FindCortexSourceFile locates a Cortex session file by UUID. Accepts
// both the raw UUID and the prefixed "cortex:<uuid>" form. Returns the
// path to the .json metadata file if found, otherwise "".
func FindCortexSourceFile(
	conversationsDir, sessionID string,
) string {
	// Strip "cortex:" prefix before validation — callers may
	// pass the full prefixed ID.
	sessionID = strings.TrimPrefix(sessionID, "cortex:")
	if conversationsDir == "" || !IsValidSessionID(sessionID) {
		return ""
	}

	candidate := filepath.Join(conversationsDir, sessionID+".json")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}
