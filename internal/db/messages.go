package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	selectMessageCols = `id, session_id, ordinal, role, content,
		timestamp, has_thinking, has_tool_use, content_length,
		is_system,
		model, token_usage, context_tokens, output_tokens`

	insertMessageCols = `session_id, ordinal, role, content,
		timestamp, has_thinking, has_tool_use, content_length,
		is_system,
		model, token_usage, context_tokens, output_tokens`

	// DefaultMessageLimit is the default number of messages returned.
	DefaultMessageLimit = 100
	// MaxMessageLimit is the maximum number of messages returned.
	MaxMessageLimit = 1000

	// Keep query parameter counts conservative so large sessions
	// do not exceed SQLite variable limits when hydrating tool calls.
	attachToolCallBatchSize = 500
)

// ToolCall represents a single tool invocation stored in
// the tool_calls table.
type ToolCall struct {
	MessageID           int64  `json:"-"`
	SessionID           string `json:"-"`
	ToolName            string `json:"tool_name"`
	Category            string `json:"category"`
	ToolUseID           string `json:"tool_use_id,omitempty"`
	InputJSON           string `json:"input_json,omitempty"`
	SkillName           string `json:"skill_name,omitempty"`
	ResultContentLength int    `json:"result_content_length,omitempty"`
	ResultContent       string `json:"result_content,omitempty"`
	SubagentSessionID   string `json:"subagent_session_id,omitempty"`
}

// ToolResult holds a tool_result content block for pairing.
type ToolResult struct {
	ToolUseID     string
	ContentLength int
	ContentRaw    string // raw JSON of the content field; decode lazily
}

// Message represents a row in the messages table.
type Message struct {
	ID            int64           `json:"id"`
	SessionID     string          `json:"session_id"`
	Ordinal       int             `json:"ordinal"`
	Role          string          `json:"role"`
	Content       string          `json:"content"`
	Timestamp     string          `json:"timestamp"`
	HasThinking   bool            `json:"has_thinking"`
	HasToolUse    bool            `json:"has_tool_use"`
	ContentLength int             `json:"content_length"`
	Model         string          `json:"model"`
	TokenUsage    json.RawMessage `json:"token_usage,omitempty"`
	ContextTokens int             `json:"context_tokens"`
	OutputTokens  int             `json:"output_tokens"`
	ToolCalls     []ToolCall      `json:"tool_calls,omitempty"`
	ToolResults   []ToolResult    `json:"-"`         // transient, for pairing
	IsSystem      bool            `json:"is_system"` // persisted, filters search/analytics
}

// MinimapEntry is a lightweight message summary for minimap rendering.
type MinimapEntry struct {
	Ordinal       int    `json:"ordinal"`
	Role          string `json:"role"`
	ContentLength int    `json:"content_length"`
	HasThinking   bool   `json:"has_thinking"`
	HasToolUse    bool   `json:"has_tool_use"`
}

// GetMessages returns paginated messages for a session.
// from: starting ordinal (inclusive)
// limit: max messages to return
// asc: true for ascending ordinal order, false for descending
func (db *DB) GetMessages(
	ctx context.Context,
	sessionID string, from, limit int, asc bool,
) ([]Message, error) {
	if limit <= 0 || limit > MaxMessageLimit {
		limit = DefaultMessageLimit
	}

	dir := "ASC"
	op := ">="
	if !asc {
		dir = "DESC"
		op = "<="
	}

	query := fmt.Sprintf(`
		SELECT %s
		FROM messages
		WHERE session_id = ? AND ordinal %s ?
		ORDER BY ordinal %s
		LIMIT ?`, selectMessageCols, op, dir)

	rows, err := db.getReader().QueryContext(
		ctx, query, sessionID, from, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying messages: %w", err)
	}
	defer rows.Close()
	msgs, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if err := db.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// GetAllMessages returns all messages for a session ordered by ordinal.
func (db *DB) GetAllMessages(
	ctx context.Context, sessionID string,
) ([]Message, error) {
	rows, err := db.getReader().QueryContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM messages
		WHERE session_id = ?
		ORDER BY ordinal ASC`, selectMessageCols), sessionID)
	if err != nil {
		return nil, fmt.Errorf("querying all messages: %w", err)
	}
	defer rows.Close()
	msgs, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if err := db.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// GetMinimap returns lightweight metadata for all messages in a session.
func (db *DB) GetMinimap(
	ctx context.Context, sessionID string,
) ([]MinimapEntry, error) {
	return db.GetMinimapFrom(ctx, sessionID, 0)
}

// GetMinimapFrom returns lightweight metadata for messages in a
// session starting at ordinal >= from.
func (db *DB) GetMinimapFrom(
	ctx context.Context, sessionID string, from int,
) ([]MinimapEntry, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT ordinal, role, content_length, has_thinking, has_tool_use
		FROM messages
		WHERE session_id = ? AND ordinal >= ?
		ORDER BY ordinal ASC`, sessionID, from)
	if err != nil {
		return nil, fmt.Errorf("querying minimap: %w", err)
	}
	defer rows.Close()

	var entries []MinimapEntry
	for rows.Next() {
		var e MinimapEntry
		if err := rows.Scan(
			&e.Ordinal, &e.Role, &e.ContentLength,
			&e.HasThinking, &e.HasToolUse,
		); err != nil {
			return nil, fmt.Errorf("scanning minimap entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// SampleMinimap downsamples entries to at most max points while
// preserving ordering and both endpoints.
func SampleMinimap(
	entries []MinimapEntry, max int,
) []MinimapEntry {
	if max <= 0 || len(entries) <= max {
		return entries
	}
	if max == 1 {
		return []MinimapEntry{entries[0]}
	}

	sampled := make([]MinimapEntry, 0, max)
	lastIdx := len(entries) - 1
	den := max - 1
	for i := range max {
		idx := (i * lastIdx) / den
		sampled = append(sampled, entries[idx])
	}
	return sampled
}

// insertMessagesTx batch-inserts messages within an existing
// transaction. Returns a slice of message IDs parallel to the
// input msgs slice. The caller must hold db.mu.
func (db *DB) insertMessagesTx(
	tx *sql.Tx, msgs []Message,
) ([]int64, error) {
	stmt, err := tx.Prepare(fmt.Sprintf(`
		INSERT INTO messages (%s)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, insertMessageCols))
	if err != nil {
		return nil, fmt.Errorf("preparing insert: %w", err)
	}
	defer stmt.Close()

	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		res, err := stmt.Exec(
			m.SessionID, m.Ordinal, m.Role, m.Content,
			m.Timestamp, m.HasThinking, m.HasToolUse,
			m.ContentLength, m.IsSystem,
			m.Model, string(m.TokenUsage),
			m.ContextTokens, m.OutputTokens,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"inserting message ord=%d: %w", m.Ordinal, err,
			)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf(
				"last insert id ord=%d: %w", m.Ordinal, err,
			)
		}
		ids[i] = id
	}
	return ids, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// insertToolCallsTx batch-inserts tool calls within an
// existing transaction.
func insertToolCallsTx(
	tx *sql.Tx, calls []ToolCall,
) error {
	if len(calls) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`
		INSERT INTO tool_calls
			(message_id, session_id, tool_name, category,
			 tool_use_id, input_json, skill_name,
			 result_content_length, result_content, subagent_session_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing tool_calls insert: %w", err)
	}
	defer stmt.Close()

	for _, tc := range calls {
		if _, err := stmt.Exec(
			tc.MessageID, tc.SessionID,
			tc.ToolName, tc.Category,
			nilIfEmpty(tc.ToolUseID),
			nilIfEmpty(tc.InputJSON),
			nilIfEmpty(tc.SkillName),
			nilIfZero(tc.ResultContentLength),
			nilIfEmpty(tc.ResultContent),
			nilIfEmpty(tc.SubagentSessionID),
		); err != nil {
			return fmt.Errorf(
				"inserting tool_call %q: %w", tc.ToolName, err,
			)
		}
	}
	return nil
}

const slowOpThreshold = 100 * time.Millisecond

// InsertMessages batch-inserts messages for a session.
func (db *DB) InsertMessages(msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	t := time.Now()
	defer func() {
		if d := time.Since(t); d > slowOpThreshold {
			log.Printf(
				"db: InsertMessages (%d msgs): %s",
				len(msgs), d.Round(time.Millisecond),
			)
		}
	}()

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ids, err := db.insertMessagesTx(tx, msgs)
	if err != nil {
		return err
	}

	toolCalls := resolveToolCalls(msgs, ids)
	if err := insertToolCallsTx(tx, toolCalls); err != nil {
		return err
	}
	return tx.Commit()
}

// MaxOrdinal returns the highest ordinal for a session,
// or -1 if the session has no messages.
func (db *DB) MaxOrdinal(sessionID string) int {
	var n sql.NullInt64
	err := db.getReader().QueryRow(
		"SELECT MAX(ordinal) FROM messages"+
			" WHERE session_id = ?",
		sessionID,
	).Scan(&n)
	if err != nil || !n.Valid {
		return -1
	}
	return int(n.Int64)
}

// ReplaceSessionMessages deletes existing and inserts new messages
// in a single transaction.
func (db *DB) ReplaceSessionMessages(
	sessionID string, msgs []Message,
) error {
	t := time.Now()
	defer func() {
		if d := time.Since(t); d > slowOpThreshold {
			log.Printf(
				"db: ReplaceSessionMessages %s (%d msgs): %s",
				sessionID, len(msgs),
				d.Round(time.Millisecond),
			)
		}
	}()

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		"DELETE FROM tool_calls WHERE session_id = ?",
		sessionID,
	); err != nil {
		return fmt.Errorf("deleting old tool_calls: %w", err)
	}

	if _, err := tx.Exec(
		"DELETE FROM messages WHERE session_id = ?", sessionID,
	); err != nil {
		return fmt.Errorf("deleting old messages: %w", err)
	}

	if len(msgs) > 0 {
		ids, err := db.insertMessagesTx(tx, msgs)
		if err != nil {
			return err
		}
		toolCalls := resolveToolCalls(msgs, ids)
		if err := insertToolCallsTx(tx, toolCalls); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// attachToolCalls loads tool_calls for the given messages
// and attaches them to each message's ToolCalls field.
func (db *DB) attachToolCalls(
	ctx context.Context, msgs []Message,
) error {
	if len(msgs) == 0 {
		return nil
	}

	idToIdx := make(map[int64]int, len(msgs))
	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
		idToIdx[m.ID] = i
	}

	for i := 0; i < len(ids); i += attachToolCallBatchSize {
		end := min(i+attachToolCallBatchSize, len(ids))
		if err := db.attachToolCallsBatch(
			ctx, msgs, idToIdx, ids[i:end],
		); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) attachToolCallsBatch(
	ctx context.Context,
	msgs []Message,
	idToIdx map[int64]int,
	batch []int64,
) error {
	if len(batch) == 0 {
		return nil
	}

	args := make([]any, len(batch))
	placeholders := make([]string, len(batch))
	for i, id := range batch {
		args[i] = id
		placeholders[i] = "?"
	}

	query := fmt.Sprintf(`
		SELECT message_id, session_id, tool_name, category,
			tool_use_id, input_json, skill_name,
			result_content_length, result_content, subagent_session_id
		FROM tool_calls
		WHERE message_id IN (%s)
		ORDER BY id`,
		strings.Join(placeholders, ","))

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying tool_calls: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var tc ToolCall
		var toolUseID, inputJSON, skillName sql.NullString
		var subagentSessionID, resultContent sql.NullString
		var resultLen sql.NullInt64
		if err := rows.Scan(
			&tc.MessageID, &tc.SessionID,
			&tc.ToolName, &tc.Category,
			&toolUseID, &inputJSON, &skillName,
			&resultLen, &resultContent, &subagentSessionID,
		); err != nil {
			return fmt.Errorf("scanning tool_call: %w", err)
		}
		if toolUseID.Valid {
			tc.ToolUseID = toolUseID.String
		}
		if inputJSON.Valid {
			tc.InputJSON = inputJSON.String
		}
		if skillName.Valid {
			tc.SkillName = skillName.String
		}
		if resultLen.Valid {
			tc.ResultContentLength = int(resultLen.Int64)
		}
		if resultContent.Valid {
			tc.ResultContent = resultContent.String
		}
		if subagentSessionID.Valid {
			tc.SubagentSessionID = subagentSessionID.String
		}

		if idx, ok := idToIdx[tc.MessageID]; ok {
			msgs[idx].ToolCalls = append(
				msgs[idx].ToolCalls, tc,
			)
		}
	}
	return rows.Err()
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var msgs []Message
	for rows.Next() {
		var m Message
		var tokenUsage string
		err := rows.Scan(
			&m.ID, &m.SessionID, &m.Ordinal, &m.Role,
			&m.Content, &m.Timestamp,
			&m.HasThinking, &m.HasToolUse, &m.ContentLength,
			&m.IsSystem,
			&m.Model, &tokenUsage,
			&m.ContextTokens, &m.OutputTokens,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning message: %w", err)
		}
		if tokenUsage != "" {
			m.TokenUsage = json.RawMessage(tokenUsage)
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// MessageCount returns the number of messages for a session.
func (db *DB) MessageCount(sessionID string) (int, error) {
	var count int
	err := db.getReader().QueryRow(
		"SELECT COUNT(*) FROM messages WHERE session_id = ?",
		sessionID,
	).Scan(&count)
	return count, err
}

// MessageContentFingerprint returns a lightweight fingerprint of all
// messages for a session, computed as the sum, max, and min of
// content_length values.
func (db *DB) MessageContentFingerprint(sessionID string) (sum, max, min int64, err error) {
	err = db.getReader().QueryRow(
		"SELECT COALESCE(SUM(content_length), 0), COALESCE(MAX(content_length), 0), COALESCE(MIN(content_length), 0) FROM messages WHERE session_id = ?",
		sessionID,
	).Scan(&sum, &max, &min)
	return sum, max, min, err
}

// ToolCallCount returns the number of tool_calls rows for a session.
func (db *DB) ToolCallCount(sessionID string) (int, error) {
	var n int
	err := db.getReader().QueryRow(
		"SELECT COUNT(*) FROM tool_calls WHERE session_id = ?",
		sessionID,
	).Scan(&n)
	return n, err
}

// ToolCallContentFingerprint returns the sum of result_content_length
// values for a session's tool calls, used as a lightweight content
// change detector.
func (db *DB) ToolCallContentFingerprint(sessionID string) (int64, error) {
	var sum int64
	err := db.getReader().QueryRow(
		"SELECT COALESCE(SUM(result_content_length), 0) FROM tool_calls WHERE session_id = ?",
		sessionID,
	).Scan(&sum)
	return sum, err
}

// GetMessageByOrdinal returns a single message by session ID and ordinal.
func (db *DB) GetMessageByOrdinal(
	sessionID string, ordinal int,
) (*Message, error) {
	row := db.getReader().QueryRow(fmt.Sprintf(`
		SELECT %s
		FROM messages
		WHERE session_id = ? AND ordinal = ?`, selectMessageCols),
		sessionID, ordinal)

	var m Message
	var tokenUsage string
	err := row.Scan(
		&m.ID, &m.SessionID, &m.Ordinal, &m.Role,
		&m.Content, &m.Timestamp,
		&m.HasThinking, &m.HasToolUse, &m.ContentLength,
		&m.IsSystem,
		&m.Model, &tokenUsage,
		&m.ContextTokens, &m.OutputTokens,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if tokenUsage != "" {
		m.TokenUsage = json.RawMessage(tokenUsage)
	}
	return &m, nil
}

// resolveToolCalls builds ToolCall rows from messages using
// the parallel IDs slice from insertMessagesTx. Panics if
// len(ids) != len(msgs) since that indicates a caller bug.
func resolveToolCalls(
	msgs []Message, ids []int64,
) []ToolCall {
	if len(ids) != len(msgs) {
		panic(fmt.Sprintf(
			"resolveToolCalls: len(ids)=%d != len(msgs)=%d",
			len(ids), len(msgs),
		))
	}
	var calls []ToolCall
	for i, m := range msgs {
		for _, tc := range m.ToolCalls {
			calls = append(calls, ToolCall{
				MessageID:           ids[i],
				SessionID:           m.SessionID,
				ToolName:            tc.ToolName,
				Category:            tc.Category,
				ToolUseID:           tc.ToolUseID,
				InputJSON:           tc.InputJSON,
				SkillName:           tc.SkillName,
				ResultContentLength: tc.ResultContentLength,
				ResultContent:       tc.ResultContent,
				SubagentSessionID:   tc.SubagentSessionID,
			})
		}
	}
	return calls
}
