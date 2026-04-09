package parser

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WarpSession bundles a parsed session with its messages.
type WarpSession struct {
	Session  ParsedSession
	Messages []ParsedMessage
}

// WarpSessionMeta is lightweight metadata for a session,
// used to detect changes without parsing messages.
type WarpSessionMeta struct {
	SessionID   string
	VirtualPath string
	FileMtime   int64 // last_modified_at as UnixNano
}

// ListWarpSessionMeta returns lightweight metadata for all
// conversations without parsing exchanges. Used by the sync
// engine to detect which sessions have changed.
func ListWarpSessionMeta(
	dbPath string,
) ([]WarpSessionMeta, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	db, err := openWarpDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(
		`SELECT conversation_id, last_modified_at
		 FROM agent_conversations`,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"listing warp conversations: %w", err,
		)
	}
	defer rows.Close()

	var metas []WarpSessionMeta
	for rows.Next() {
		var id string
		var lastModified string
		if err := rows.Scan(
			&id, &lastModified,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning warp session meta: %w", err,
			)
		}
		mtime := parseWarpTimestamp(lastModified).UnixNano()
		metas = append(metas, WarpSessionMeta{
			SessionID:   id,
			VirtualPath: dbPath + "#" + id,
			FileMtime:   mtime,
		})
	}
	return metas, rows.Err()
}

// ParseWarpDB opens the Warp SQLite database read-only and
// returns all conversations with messages.
func ParseWarpDB(
	dbPath, machine string,
) ([]WarpSession, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	db, err := openWarpDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	convos, err := loadWarpConversations(db)
	if err != nil {
		return nil, fmt.Errorf(
			"loading warp conversations: %w", err,
		)
	}

	var results []WarpSession
	for _, c := range convos {
		parsed, msgs, err := buildWarpSession(
			db, c, dbPath, machine,
		)
		if err != nil {
			log.Printf(
				"warp conversation %s: %v", c.id, err,
			)
			continue
		}
		if parsed == nil {
			continue
		}
		results = append(results, WarpSession{
			Session:  *parsed,
			Messages: msgs,
		})
	}
	return results, nil
}

// ParseWarpSession parses a single conversation by ID from
// the Warp database.
func ParseWarpSession(
	dbPath, conversationID, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil, fmt.Errorf(
			"warp db not found: %s", dbPath,
		)
	}

	db, err := openWarpDB(dbPath)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()

	c, err := loadOneWarpConversation(db, conversationID)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading warp conversation %s: %w",
			conversationID, err,
		)
	}

	return buildWarpSession(db, c, dbPath, machine)
}

func openWarpDB(dbPath string) (*sql.DB, error) {
	dsn := dbPath +
		"?mode=ro&_journal_mode=WAL&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf(
			"opening warp db %s: %w", dbPath, err,
		)
	}
	return db, nil
}

// warpConversationRow is a row from agent_conversations.
type warpConversationRow struct {
	id               string
	conversationData string
	lastModifiedAt   string
}

func loadWarpConversations(
	db *sql.DB,
) ([]warpConversationRow, error) {
	rows, err := db.Query(`
		SELECT conversation_id,
		       COALESCE(conversation_data, '{}'),
		       last_modified_at
		FROM agent_conversations
		ORDER BY last_modified_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var convos []warpConversationRow
	for rows.Next() {
		var c warpConversationRow
		if err := rows.Scan(
			&c.id, &c.conversationData, &c.lastModifiedAt,
		); err != nil {
			return nil, err
		}
		convos = append(convos, c)
	}
	return convos, rows.Err()
}

func loadOneWarpConversation(
	db *sql.DB, conversationID string,
) (warpConversationRow, error) {
	row := db.QueryRow(`
		SELECT conversation_id,
		       COALESCE(conversation_data, '{}'),
		       last_modified_at
		FROM agent_conversations
		WHERE conversation_id = ?
	`, conversationID)

	var c warpConversationRow
	err := row.Scan(
		&c.id, &c.conversationData, &c.lastModifiedAt,
	)
	return c, err
}

// warpExchangeRow is a row from ai_queries.
type warpExchangeRow struct {
	exchangeID  string
	startTS     string
	input       string
	modelID     string
	workingDir  string
	outputStatus string
}

func loadWarpExchanges(
	db *sql.DB, conversationID string,
) ([]warpExchangeRow, error) {
	rows, err := db.Query(`
		SELECT exchange_id, start_ts,
		       COALESCE(input, '[]'),
		       COALESCE(model_id, ''),
		       COALESCE(working_directory, ''),
		       COALESCE(output_status, '')
		FROM ai_queries
		WHERE conversation_id = ?
		ORDER BY start_ts
	`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var exchanges []warpExchangeRow
	for rows.Next() {
		var e warpExchangeRow
		if err := rows.Scan(
			&e.exchangeID, &e.startTS, &e.input,
			&e.modelID, &e.workingDir, &e.outputStatus,
		); err != nil {
			return nil, err
		}
		exchanges = append(exchanges, e)
	}
	return exchanges, rows.Err()
}

func buildWarpSession(
	db *sql.DB,
	c warpConversationRow,
	dbPath, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	exchanges, err := loadWarpExchanges(db, c.id)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading exchanges for %s: %w", c.id, err,
		)
	}

	if len(exchanges) == 0 {
		return nil, nil, nil
	}

	var (
		parsed       []ParsedMessage
		firstMsg     string
		startedAt    time.Time
		endedAt      time.Time
		project      string
		cwd          string
		ordinal      int
		userCount    int
		lastModel    string
	)

	for _, e := range exchanges {
		ts := parseWarpTimestamp(e.startTS)
		if startedAt.IsZero() {
			startedAt = ts
		}
		endedAt = ts
		if e.modelID != "" {
			lastModel = normalizeWarpModel(e.modelID)
		}
		if e.workingDir != "" && cwd == "" {
			cwd = e.workingDir
		}

		queryText := extractWarpQueryText(e.input)
		if queryText == "" {
			// Exchange without user input (assistant
			// tool call / intermediate step). Count it
			// for timing but don't create a message.
			continue
		}

		// User message.
		parsed = append(parsed, ParsedMessage{
			Ordinal:       ordinal,
			Role:          RoleUser,
			Content:       queryText,
			Timestamp:     ts,
			ContentLength: len(queryText),
			Model:         lastModel,
		})
		ordinal++
		userCount++

		if firstMsg == "" {
			firstMsg = truncate(
				strings.ReplaceAll(queryText, "\n", " "),
				300,
			)
		}
	}

	if len(parsed) == 0 {
		return nil, nil, nil
	}

	// Extract project from working directory.
	if cwd != "" {
		project = ExtractProjectFromCwd(cwd)
	}
	if project == "" {
		project = "unknown"
	}

	// Parse conversation metadata for token usage.
	meta := parseWarpConversationMeta(c.conversationData)

	// Synthesize tool call messages from aggregate stats.
	toolMsgs := synthesizeWarpToolMessages(
		meta, endedAt, lastModel, &ordinal,
	)
	parsed = append(parsed, toolMsgs...)

	sess := &ParsedSession{
		ID:               "warp:" + c.id,
		Project:          project,
		Machine:          machine,
		Agent:            AgentWarp,
		Cwd:              cwd,
		FirstMessage:     firstMsg,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(parsed),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  dbPath + "#" + c.id,
			Mtime: parseWarpTimestamp(c.lastModifiedAt).UnixNano(),
		},
	}

	// Token usage from conversation metadata.
	if meta.totalTokens > 0 {
		sess.HasTotalOutputTokens = true
		sess.TotalOutputTokens = meta.totalTokens
	}

	return sess, parsed, nil
}

// warpConversationMeta holds parsed metadata from
// conversation_data JSON.
type warpConversationMeta struct {
	totalTokens int
	toolStats   warpToolStats
}

type warpToolStats struct {
	RunCommand           int
	ReadFiles            int
	SearchCodebase       int
	Grep                 int
	FileGlob             int
	ApplyFileDiff        int
	WriteLongRunning     int
	ReadMCPResource      int
	CallMCPTool          int
	SuggestPlan          int
	SuggestCreatePlan    int
	ReadShellCmdOutput   int
	UseComputer          int
}

func parseWarpConversationMeta(data string) warpConversationMeta {
	var meta warpConversationMeta
	if data == "" || data == "{}" {
		return meta
	}

	var raw struct {
		Usage struct {
			TokenUsage []struct {
				WarpTokens int `json:"warp_tokens"`
				BYOKTokens int `json:"byok_tokens"`
			} `json:"token_usage"`
			ToolUsage struct {
				RunCommand       struct{ Count int } `json:"run_command_stats"`
				ReadFiles        struct{ Count int } `json:"read_files_stats"`
				SearchCodebase   struct{ Count int } `json:"search_codebase_stats"`
				Grep             struct{ Count int } `json:"grep_stats"`
				FileGlob         struct{ Count int } `json:"file_glob_stats"`
				ApplyFileDiff    struct {
					Count int `json:"count"`
				} `json:"apply_file_diff_stats"`
				WriteLongRunning struct{ Count int } `json:"write_to_long_running_shell_command_stats"`
				ReadMCPResource  struct{ Count int } `json:"read_mcp_resource_stats"`
				CallMCPTool      struct{ Count int } `json:"call_mcp_tool_stats"`
				SuggestPlan      struct{ Count int } `json:"suggest_plan_stats"`
				SuggestCreatePlan struct{ Count int } `json:"suggest_create_plan_stats"`
				ReadShellOutput  struct{ Count int } `json:"read_shell_command_output_stats"`
				UseComputer      struct{ Count int } `json:"use_computer_stats"`
			} `json:"tool_usage_metadata"`
		} `json:"conversation_usage_metadata"`
	}

	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return meta
	}

	for _, tu := range raw.Usage.TokenUsage {
		meta.totalTokens += tu.WarpTokens + tu.BYOKTokens
	}

	ts := raw.Usage.ToolUsage
	meta.toolStats = warpToolStats{
		RunCommand:         ts.RunCommand.Count,
		ReadFiles:          ts.ReadFiles.Count,
		SearchCodebase:     ts.SearchCodebase.Count,
		Grep:               ts.Grep.Count,
		FileGlob:           ts.FileGlob.Count,
		ApplyFileDiff:      ts.ApplyFileDiff.Count,
		WriteLongRunning:   ts.WriteLongRunning.Count,
		ReadMCPResource:    ts.ReadMCPResource.Count,
		CallMCPTool:        ts.CallMCPTool.Count,
		SuggestPlan:        ts.SuggestPlan.Count,
		SuggestCreatePlan:  ts.SuggestCreatePlan.Count,
		ReadShellCmdOutput: ts.ReadShellOutput.Count,
		UseComputer:        ts.UseComputer.Count,
	}

	return meta
}

// synthesizeWarpToolMessages creates assistant messages with
// tool call metadata from aggregate tool usage stats. This
// gives agentsview accurate tool category breakdowns even
// though Warp doesn't persist individual tool call records.
func synthesizeWarpToolMessages(
	meta warpConversationMeta,
	ts time.Time,
	model string,
	ordinal *int,
) []ParsedMessage {
	type toolEntry struct {
		name  string
		count int
	}
	entries := []toolEntry{
		{"run_command", meta.toolStats.RunCommand},
		{"read_files", meta.toolStats.ReadFiles},
		{"search_codebase", meta.toolStats.SearchCodebase},
		{"grep", meta.toolStats.Grep},
		{"file_glob", meta.toolStats.FileGlob},
		{"apply_file_diff", meta.toolStats.ApplyFileDiff},
		{"write_to_long_running_shell_command", meta.toolStats.WriteLongRunning},
		{"read_mcp_resource", meta.toolStats.ReadMCPResource},
		{"call_mcp_tool", meta.toolStats.CallMCPTool},
		{"suggest_plan", meta.toolStats.SuggestPlan},
		{"suggest_create_plan", meta.toolStats.SuggestCreatePlan},
		{"read_shell_command_output", meta.toolStats.ReadShellCmdOutput},
		{"use_computer", meta.toolStats.UseComputer},
	}

	var msgs []ParsedMessage
	for _, e := range entries {
		for range e.count {
			category := NormalizeToolCategory(e.name)
			content := fmt.Sprintf("[%s]", category)
			msgs = append(msgs, ParsedMessage{
				Ordinal:       *ordinal,
				Role:          RoleAssistant,
				Content:       content,
				Timestamp:     ts,
				HasToolUse:    true,
				ContentLength: len(content),
				Model:         model,
				ToolCalls: []ParsedToolCall{{
					ToolName: e.name,
					Category: category,
				}},
			})
			*ordinal++
		}
	}
	return msgs
}

// extractWarpQueryText extracts the user's query text from the
// ai_queries input JSON. The format is:
// [{"Query":{"text":"...","context":[...]}}]
func extractWarpQueryText(input string) string {
	input = strings.TrimSpace(input)
	if input == "" || input == "[]" {
		return ""
	}

	var items []json.RawMessage
	if err := json.Unmarshal([]byte(input), &items); err != nil {
		return ""
	}
	if len(items) == 0 {
		return ""
	}

	var wrapper struct {
		Query struct {
			Text string `json:"text"`
		} `json:"Query"`
	}
	if err := json.Unmarshal(items[0], &wrapper); err != nil {
		return ""
	}
	return strings.TrimSpace(wrapper.Query.Text)
}

// normalizeWarpModel cleans up Warp model IDs for display.
func normalizeWarpModel(modelID string) string {
	// Warp uses IDs like "auto-genius", "auto" — keep as-is.
	return modelID
}

// parseWarpTimestamp parses timestamps from Warp's SQLite DB.
// Format: "2026-04-07 08:55:40" or with fractional seconds.
func parseWarpTimestamp(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}

	// Try with fractional seconds first.
	for _, layout := range []string{
		"2006-01-02 15:04:05.000000",
		"2006-01-02 15:04:05",
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// FindWarpDBPath returns the path to warp.sqlite inside the
// given directory, or "" if it doesn't exist.
func FindWarpDBPath(dir string) string {
	candidate := filepath.Join(dir, "warp.sqlite")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}
