package parser

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// OpenCodeSession bundles a parsed session with its messages.
type OpenCodeSession struct {
	Session  ParsedSession
	Messages []ParsedMessage
}

// OpenCodeSessionMeta is lightweight metadata for a session,
// used to detect changes without parsing messages or parts.
type OpenCodeSessionMeta struct {
	SessionID   string
	VirtualPath string
	FileMtime   int64
}

// ListOpenCodeSessionMeta returns lightweight metadata for
// all sessions without parsing messages or parts. Used by
// the sync engine to detect which sessions have changed.
func ListOpenCodeSessionMeta(
	dbPath string,
) ([]OpenCodeSessionMeta, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(
		"SELECT id, time_updated FROM session",
	)
	if err != nil {
		return nil, fmt.Errorf(
			"listing opencode sessions: %w", err,
		)
	}
	defer rows.Close()

	var metas []OpenCodeSessionMeta
	for rows.Next() {
		var id string
		var timeUpdated int64
		if err := rows.Scan(
			&id, &timeUpdated,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning opencode session meta: %w", err,
			)
		}
		metas = append(metas, OpenCodeSessionMeta{
			SessionID:   id,
			VirtualPath: dbPath + "#" + id,
			FileMtime:   timeUpdated * 1_000_000,
		})
	}
	return metas, rows.Err()
}

// ParseOpenCodeDB opens the OpenCode SQLite database read-only
// and returns all sessions with messages.
func ParseOpenCodeDB(
	dbPath, machine string,
) ([]OpenCodeSession, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	projects, err := loadOpenCodeProjects(db)
	if err != nil {
		return nil, fmt.Errorf(
			"loading opencode projects: %w", err,
		)
	}

	sessions, err := loadOpenCodeSessions(db)
	if err != nil {
		return nil, fmt.Errorf(
			"loading opencode sessions: %w", err,
		)
	}

	var results []OpenCodeSession
	for _, s := range sessions {
		worktree := projects[s.projectID]
		parsed, msgs, err := buildOpenCodeSession(
			db, s, worktree, dbPath, machine,
		)
		if err != nil {
			log.Printf(
				"opencode session %s: %v", s.id, err,
			)
			continue
		}
		if parsed == nil {
			continue
		}
		results = append(results, OpenCodeSession{
			Session:  *parsed,
			Messages: msgs,
		})
	}
	return results, nil
}

// ParseOpenCodeSession parses a single session by ID from the
// OpenCode database.
func ParseOpenCodeSession(
	dbPath, sessionID, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil, fmt.Errorf(
			"opencode db not found: %s", dbPath,
		)
	}

	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()

	projects, err := loadOpenCodeProjects(db)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading opencode projects: %w", err,
		)
	}

	s, err := loadOneOpenCodeSession(db, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading opencode session %s: %w",
			sessionID, err,
		)
	}

	worktree := projects[s.projectID]
	return buildOpenCodeSession(
		db, s, worktree, dbPath, machine,
	)
}

func openOpenCodeDB(dbPath string) (*sql.DB, error) {
	dsn := dbPath +
		"?mode=ro&_journal_mode=WAL&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf(
			"opening opencode db %s: %w", dbPath, err,
		)
	}
	return db, nil
}

// openCodeProject is a row from the opencode project table.
type openCodeProject struct {
	id       string
	worktree string
}

func loadOpenCodeProjects(
	db *sql.DB,
) (map[string]string, error) {
	rows, err := db.Query(
		"SELECT id, worktree FROM project",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	projects := make(map[string]string)
	for rows.Next() {
		var p openCodeProject
		if err := rows.Scan(&p.id, &p.worktree); err != nil {
			return nil, err
		}
		projects[p.id] = p.worktree
	}
	return projects, rows.Err()
}

// openCodeSessionRow is a row from the opencode session table.
type openCodeSessionRow struct {
	id          string
	projectID   string
	parentID    string
	title       string
	timeCreated int64
	timeUpdated int64
}

func loadOpenCodeSessions(
	db *sql.DB,
) ([]openCodeSessionRow, error) {
	rows, err := db.Query(`
		SELECT s.id, s.project_id,
		       COALESCE(s.parent_id, ''),
		       COALESCE(s.title, ''),
		       s.time_created, s.time_updated
		FROM session s
		ORDER BY s.time_created
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []openCodeSessionRow
	for rows.Next() {
		var s openCodeSessionRow
		if err := rows.Scan(
			&s.id, &s.projectID, &s.parentID,
			&s.title, &s.timeCreated, &s.timeUpdated,
		); err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func loadOneOpenCodeSession(
	db *sql.DB, sessionID string,
) (openCodeSessionRow, error) {
	row := db.QueryRow(`
		SELECT s.id, s.project_id,
		       COALESCE(s.parent_id, ''),
		       COALESCE(s.title, ''),
		       s.time_created, s.time_updated
		FROM session s
		WHERE s.id = ?
	`, sessionID)

	var s openCodeSessionRow
	err := row.Scan(
		&s.id, &s.projectID, &s.parentID,
		&s.title, &s.timeCreated, &s.timeUpdated,
	)
	return s, err
}

// openCodeMessageRow is a row from the opencode message table.
// The role is extracted from the JSON data column.
type openCodeMessageRow struct {
	id          string
	data        string
	timeCreated int64
}

// openCodeMessageData holds the fields we extract from the
// message data JSON blob.
type openCodeMessageData struct {
	Role string `json:"role"`
}

// openCodePartRow is a row from the opencode part table.
// The part type is extracted from the JSON data column.
type openCodePartRow struct {
	id          string
	messageID   string
	data        string
	timeCreated int64
}

func loadOpenCodeMessages(
	db *sql.DB, sessionID string,
) ([]openCodeMessageRow, error) {
	rows, err := db.Query(`
		SELECT id, data, time_created
		FROM message
		WHERE session_id = ?
		ORDER BY time_created
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []openCodeMessageRow
	for rows.Next() {
		var m openCodeMessageRow
		if err := rows.Scan(
			&m.id, &m.data, &m.timeCreated,
		); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func loadOpenCodeParts(
	db *sql.DB, sessionID string,
) (map[string][]openCodePartRow, error) {
	rows, err := db.Query(`
		SELECT p.id, p.message_id,
		       COALESCE(p.data, '{}'),
		       p.time_created
		FROM part p
		WHERE p.session_id = ?
		ORDER BY p.time_created
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	parts := make(map[string][]openCodePartRow)
	for rows.Next() {
		var p openCodePartRow
		if err := rows.Scan(
			&p.id, &p.messageID,
			&p.data, &p.timeCreated,
		); err != nil {
			return nil, err
		}
		parts[p.messageID] = append(
			parts[p.messageID], p,
		)
	}
	return parts, rows.Err()
}

func buildOpenCodeSession(
	db *sql.DB,
	s openCodeSessionRow,
	worktree, dbPath, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	msgs, err := loadOpenCodeMessages(db, s.id)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading messages for %s: %w", s.id, err,
		)
	}

	parts, err := loadOpenCodeParts(db, s.id)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading parts for %s: %w", s.id, err,
		)
	}

	var (
		parsed       []ParsedMessage
		firstMsg     string
		hasUserOrAst bool
		ordinal      int
	)

	// Prefer OpenCode's LLM-generated title when available.
	// Skip default placeholders that match OpenCode's exact
	// format: "New session - " or "Child session - " followed
	// by an ISO-8601 timestamp.
	if s.title != "" && !isOpenCodeDefaultTitle(s.title) {
		firstMsg = truncate(s.title, 300)
	}

	for _, m := range msgs {
		var md openCodeMessageData
		if json.Unmarshal([]byte(m.data), &md) != nil {
			continue
		}
		role := normalizeOpenCodeRole(md.Role)
		if role == "" {
			continue
		}
		hasUserOrAst = true

		msgParts := parts[m.id]
		sort.Slice(msgParts, func(a, b int) bool {
			return msgParts[a].timeCreated <
				msgParts[b].timeCreated
		})

		pm := buildOpenCodeMessage(
			ordinal, role, m.timeCreated, msgParts,
		)
		if strings.TrimSpace(pm.Content) == "" &&
			!pm.HasToolUse {
			continue
		}

		if role == RoleUser && firstMsg == "" {
			firstMsg = truncate(
				strings.ReplaceAll(pm.Content, "\n", " "),
				300,
			)
		}

		parsed = append(parsed, pm)
		ordinal++
	}

	if !hasUserOrAst || len(parsed) == 0 {
		return nil, nil, nil
	}

	project := ExtractProjectFromCwd(worktree)
	if project == "" {
		project = "unknown"
	}

	parentID := ""
	if s.parentID != "" {
		parentID = "opencode:" + s.parentID
	}

	startedAt := millisToTime(s.timeCreated)
	endedAt := millisToTime(s.timeUpdated)

	userCount := 0
	for _, m := range parsed {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:               "opencode:" + s.id,
		Project:          project,
		Machine:          machine,
		Agent:            AgentOpenCode,
		ParentSessionID:  parentID,
		FirstMessage:     firstMsg,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(parsed),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  dbPath + "#" + s.id,
			Mtime: s.timeUpdated * 1_000_000,
		},
	}

	return sess, parsed, nil
}

// openCodeDefaultTitleRe matches the exact placeholder format
// OpenCode uses before the LLM generates a real title:
// "New session - 2026-03-22T10:00:00.000Z" or
// "Child session - 2026-03-22T10:00:00.000Z".
var openCodeDefaultTitleRe = regexp.MustCompile(
	`^(New session|Child session) - \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`,
)

func isOpenCodeDefaultTitle(title string) bool {
	return openCodeDefaultTitleRe.MatchString(title)
}

func normalizeOpenCodeRole(role string) RoleType {
	switch role {
	case "user":
		return RoleUser
	case "assistant":
		return RoleAssistant
	default:
		return ""
	}
}

func buildOpenCodeMessage(
	ordinal int,
	role RoleType,
	timeCreatedMs int64,
	parts []openCodePartRow,
) ParsedMessage {
	var (
		texts       []string
		toolCalls   []ParsedToolCall
		hasThinking bool
		hasToolUse  bool
	)

	for _, p := range parts {
		partType := extractOpenCodePartType(p.data)
		switch partType {
		case "text":
			text := extractOpenCodeText(p.data)
			if text != "" {
				texts = append(texts, text)
			}
		case "tool":
			hasToolUse = true
			tc := extractOpenCodeToolCall(p.data)
			if tc.ToolName != "" {
				toolCalls = append(toolCalls, tc)
			}
		case "reasoning":
			text := extractOpenCodeText(p.data)
			if text != "" {
				hasThinking = true
				texts = append(texts,
					"[Thinking]\n"+text+"\n[/Thinking]")
			}
		}
		// skip step-start, step-finish, patch, etc.
	}

	content := strings.Join(texts, "\n")
	return ParsedMessage{
		Ordinal:       ordinal,
		Role:          role,
		Content:       content,
		Timestamp:     millisToTime(timeCreatedMs),
		HasThinking:   hasThinking,
		HasToolUse:    hasToolUse,
		ContentLength: len(content),
		ToolCalls:     toolCalls,
	}
}

// openCodePartTypeData extracts just the type from a part's
// JSON data blob.
type openCodePartTypeData struct {
	Type string `json:"type"`
}

func extractOpenCodePartType(data string) string {
	var d openCodePartTypeData
	if json.Unmarshal([]byte(data), &d) != nil {
		return ""
	}
	return d.Type
}

// openCodeTextData is the JSON structure for a text part's data.
type openCodeTextData struct {
	Content string `json:"content"`
	Text    string `json:"text"`
}

func extractOpenCodeText(data string) string {
	var d openCodeTextData
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return ""
	}
	if d.Content != "" {
		return d.Content
	}
	return d.Text
}

// openCodeToolData is the JSON structure for a tool part's data.
type openCodeToolData struct {
	ToolName string          `json:"tool"`
	CallID   string          `json:"callID"`
	State    json.RawMessage `json:"state"`
}

// openCodeToolState holds the nested state of a tool call.
type openCodeToolState struct {
	Input json.RawMessage `json:"input"`
}

func extractOpenCodeToolCall(data string) ParsedToolCall {
	var d openCodeToolData
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return ParsedToolCall{}
	}

	var inputJSON string
	if len(d.State) > 0 {
		var state openCodeToolState
		if err := json.Unmarshal(d.State, &state); err == nil {
			if len(state.Input) > 0 {
				inputJSON = string(state.Input)
			}
		}
	}

	return ParsedToolCall{
		ToolUseID: d.CallID,
		ToolName:  d.ToolName,
		Category:  NormalizeToolCategory(d.ToolName),
		InputJSON: inputJSON,
	}
}

func millisToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
