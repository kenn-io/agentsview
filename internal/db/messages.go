package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/wesm/agentsview/internal/parser"
)

const (
	selectMessageCols = `id, session_id, ordinal, role, content,
		timestamp, has_thinking, has_tool_use, content_length,
		is_system,
		model, token_usage, context_tokens, output_tokens,
		has_context_tokens, has_output_tokens`

	insertMessageCols = `session_id, ordinal, role, content,
		timestamp, has_thinking, has_tool_use, content_length,
		is_system,
		model, token_usage, context_tokens, output_tokens,
		has_context_tokens, has_output_tokens`

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
	MessageID           int64             `json:"-"`
	SessionID           string            `json:"-"`
	ToolName            string            `json:"tool_name"`
	Category            string            `json:"category"`
	ToolUseID           string            `json:"tool_use_id,omitempty"`
	InputJSON           string            `json:"input_json,omitempty"`
	SkillName           string            `json:"skill_name,omitempty"`
	ResultContentLength int               `json:"result_content_length,omitempty"`
	ResultContent       string            `json:"result_content,omitempty"`
	SubagentSessionID   string            `json:"subagent_session_id,omitempty"`
	ResultEvents        []ToolResultEvent `json:"result_events,omitempty"`
}

// ToolResult holds a tool_result content block for pairing.
type ToolResult struct {
	ToolUseID     string
	ContentLength int
	ContentRaw    string // raw JSON of the content field; decode lazily
}

// ToolResultEvent represents a canonical chronological result update.
type ToolResultEvent struct {
	ToolUseID         string `json:"tool_use_id,omitempty"`
	AgentID           string `json:"agent_id,omitempty"`
	SubagentSessionID string `json:"subagent_session_id,omitempty"`
	Source            string `json:"source"`
	Status            string `json:"status"`
	Content           string `json:"content"`
	ContentLength     int    `json:"content_length"`
	Timestamp         string `json:"timestamp,omitempty"`
	EventIndex        int    `json:"event_index"`
}

// Message represents a row in the messages table.
type Message struct {
	ID               int64           `json:"id"`
	SessionID        string          `json:"session_id"`
	Ordinal          int             `json:"ordinal"`
	Role             string          `json:"role"`
	Content          string          `json:"content"`
	Timestamp        string          `json:"timestamp"`
	HasThinking      bool            `json:"has_thinking"`
	HasToolUse       bool            `json:"has_tool_use"`
	ContentLength    int             `json:"content_length"`
	Model            string          `json:"model"`
	TokenUsage       json.RawMessage `json:"token_usage,omitempty"`
	ContextTokens    int             `json:"context_tokens"`
	OutputTokens     int             `json:"output_tokens"`
	HasContextTokens bool            `json:"has_context_tokens"`
	HasOutputTokens  bool            `json:"has_output_tokens"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolResults      []ToolResult    `json:"-"`         // transient, for pairing
	IsSystem         bool            `json:"is_system"` // persisted, filters search/analytics
}

// TokenPresence reports whether context/output token fields were
// present in stored message metadata. It preserves explicit flags,
// falls back to non-zero numeric values for legacy rows, and inspects
// raw token_usage payload keys to preserve zero-valued coverage.
func (m Message) TokenPresence() (bool, bool) {
	return parser.InferTokenPresence(
		m.TokenUsage, m.ContextTokens, m.OutputTokens,
		m.HasContextTokens, m.HasOutputTokens,
	)
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

// insertMessagesTx batch-inserts messages within an existing
// transaction. Returns a slice of message IDs parallel to the
// input msgs slice. The caller must hold db.mu.
func (db *DB) insertMessagesTx(
	tx *sql.Tx, msgs []Message,
) ([]int64, error) {
	stmt, err := tx.Prepare(fmt.Sprintf(`
		INSERT INTO messages (%s)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, insertMessageCols))
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
			m.HasContextTokens, m.HasOutputTokens,
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

func insertToolResultEventsTx(
	tx *sql.Tx, rows []toolResultEventRow,
) error {
	if len(rows) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`
		INSERT INTO tool_result_events
			(session_id, tool_call_message_ordinal, call_index,
			 tool_use_id, agent_id, subagent_session_id,
			 source, status, content, content_length,
			 timestamp, event_index)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing tool_result_events insert: %w", err)
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.Exec(
			r.SessionID, r.MessageOrdinal, r.CallIndex,
			nilIfEmpty(r.Event.ToolUseID),
			nilIfEmpty(r.Event.AgentID),
			nilIfEmpty(r.Event.SubagentSessionID),
			r.Event.Source, r.Event.Status,
			r.Event.Content,
			r.Event.ContentLength,
			nilIfEmpty(r.Event.Timestamp),
			r.Event.EventIndex,
		); err != nil {
			return fmt.Errorf(
				"inserting tool_result_event %q/%q: %w",
				r.Event.Source, r.Event.Status, err,
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
	events := resolveToolResultEvents(msgs)
	if err := insertToolResultEventsTx(tx, events); err != nil {
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

// savedPin captures the minimal pin state needed to re-attach a pin
// after a full message replacement. The ordinal acts as a stable key
// because session files are append-only and message ordinals never
// change for existing messages.
type savedPin struct {
	ordinal   int
	note      *string
	createdAt string
}

// ReplaceSessionMessages deletes existing and inserts new messages
// in a single transaction. Any existing pins are preserved by
// re-attaching them to the new message rows that share the same
// ordinal (pins for ordinals that no longer exist are dropped).
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

	// Save existing pins before deletion. The ON DELETE CASCADE on
	// pinned_messages.message_id would otherwise wipe them when
	// messages are deleted below.
	pinRows, err := tx.Query(
		"SELECT ordinal, note, created_at FROM pinned_messages WHERE session_id = ?",
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("saving pins: %w", err)
	}
	defer pinRows.Close()
	var pins []savedPin
	for pinRows.Next() {
		var sp savedPin
		if err := pinRows.Scan(&sp.ordinal, &sp.note, &sp.createdAt); err != nil {
			return fmt.Errorf("scanning pin: %w", err)
		}
		pins = append(pins, sp)
	}
	if err := pinRows.Err(); err != nil {
		return fmt.Errorf("iterating pins: %w", err)
	}

	if _, err := tx.Exec(
		"DELETE FROM tool_calls WHERE session_id = ?",
		sessionID,
	); err != nil {
		return fmt.Errorf("deleting old tool_calls: %w", err)
	}
	if _, err := tx.Exec(
		"DELETE FROM tool_result_events WHERE session_id = ?",
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"deleting old tool_result_events: %w", err,
		)
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
		events := resolveToolResultEvents(msgs)
		if err := insertToolResultEventsTx(tx, events); err != nil {
			return err
		}
	}

	// Re-attach saved pins by matching ordinal to the newly inserted
	// messages. Pins for ordinals that no longer exist are silently
	// dropped (the message was removed from the session).
	for _, sp := range pins {
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO pinned_messages
				(session_id, message_id, ordinal, note, created_at)
			SELECT ?, m.id, m.ordinal, ?, ?
			FROM messages m
			WHERE m.session_id = ? AND m.ordinal = ?`,
			sessionID, sp.note, sp.createdAt, sessionID, sp.ordinal,
		); err != nil {
			return fmt.Errorf("restoring pin ord=%d: %w", sp.ordinal, err)
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
	if err := db.attachToolResultEvents(ctx, msgs); err != nil {
		return err
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

func (db *DB) attachToolResultEvents(
	ctx context.Context, msgs []Message,
) error {
	if len(msgs) == 0 {
		return nil
	}

	sessionID := msgs[0].SessionID
	ordToIdx := make(map[int]int, len(msgs))
	ordinals := make([]int, 0, len(msgs))
	for i, m := range msgs {
		ordToIdx[m.Ordinal] = i
		ordinals = append(ordinals, m.Ordinal)
	}
	for i := 0; i < len(ordinals); i += attachToolCallBatchSize {
		end := min(i+attachToolCallBatchSize, len(ordinals))
		if err := db.attachToolResultEventsBatch(
			ctx, msgs, ordToIdx, sessionID, ordinals[i:end],
		); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) attachToolResultEventsBatch(
	ctx context.Context,
	msgs []Message,
	ordToIdx map[int]int,
	sessionID string,
	ordinals []int,
) error {
	if len(ordinals) == 0 {
		return nil
	}

	args := []any{sessionID}
	placeholders := make([]string, len(ordinals))
	for i, ord := range ordinals {
		args = append(args, ord)
		placeholders[i] = "?"
	}

	query := fmt.Sprintf(`
		SELECT tool_call_message_ordinal, call_index,
			tool_use_id, agent_id, subagent_session_id,
			source, status, content, content_length,
			timestamp, event_index
		FROM tool_result_events
		WHERE session_id = ? AND tool_call_message_ordinal IN (%s)
		ORDER BY tool_call_message_ordinal, call_index, event_index`,
		strings.Join(placeholders, ","))

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying tool_result_events: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			msgOrdinal int
			callIndex  int
			ev         ToolResultEvent
			toolUseID  sql.NullString
			agentID    sql.NullString
			subID      sql.NullString
			timestamp  sql.NullString
		)
		if err := rows.Scan(
			&msgOrdinal, &callIndex,
			&toolUseID, &agentID, &subID,
			&ev.Source, &ev.Status, &ev.Content,
			&ev.ContentLength, &timestamp, &ev.EventIndex,
		); err != nil {
			return fmt.Errorf("scanning tool_result_event: %w", err)
		}
		if toolUseID.Valid {
			ev.ToolUseID = toolUseID.String
		}
		if agentID.Valid {
			ev.AgentID = agentID.String
		}
		if subID.Valid {
			ev.SubagentSessionID = subID.String
		}
		if timestamp.Valid {
			ev.Timestamp = timestamp.String
		}
		idx, ok := ordToIdx[msgOrdinal]
		if !ok {
			continue
		}
		if callIndex < 0 || callIndex >= len(msgs[idx].ToolCalls) {
			continue
		}
		msgs[idx].ToolCalls[callIndex].ResultEvents = append(
			msgs[idx].ToolCalls[callIndex].ResultEvents,
			ev,
		)
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
			&m.HasContextTokens, &m.HasOutputTokens,
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

// MessageTokenFingerprint returns an exact ordered fingerprint of
// stored token metadata for a session's messages. Used by PG push
// fast-paths to detect token metadata changes without rewriting
// unchanged sessions.
func (db *DB) MessageTokenFingerprint(sessionID string) (string, error) {
	rows, err := db.getReader().Query(
		`SELECT ordinal, model, token_usage, context_tokens,
			output_tokens, has_context_tokens, has_output_tokens
		 FROM messages
		 WHERE session_id = ?
		 ORDER BY ordinal ASC`,
		sessionID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var ordinal, contextTokens, outputTokens int
		var model, tokenUsage string
		var hasContextTokens, hasOutputTokens bool
		if err := rows.Scan(
			&ordinal, &model, &tokenUsage, &contextTokens,
			&outputTokens, &hasContextTokens, &hasOutputTokens,
		); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%d|%d:%s|%d:%s|%d|%d|%t|%t;",
			ordinal,
			len(model), model,
			len(tokenUsage), tokenUsage,
			contextTokens, outputTokens,
			hasContextTokens, hasOutputTokens,
		)
	}
	return b.String(), rows.Err()
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

// SystemMessageFingerprint returns the ordered, comma-separated list of
// ordinals for system messages in a session (e.g. "0,2,5"). This is an
// exact fingerprint of the system-message ordinal set: any reclassification
// of which messages are system — even when counts, sums, or sums-of-squares
// remain equal — produces a different string. Used by the PG push fast-path.
func (db *DB) SystemMessageFingerprint(sessionID string) (string, error) {
	var v sql.NullString
	err := db.getReader().QueryRow(
		`SELECT GROUP_CONCAT(ordinal, ',')
		 FROM (
		   SELECT ordinal FROM messages
		   WHERE session_id = ? AND is_system = 1
		   ORDER BY ordinal
		 )`,
		sessionID,
	).Scan(&v)
	if err != nil {
		return "", err
	}
	return v.String, nil
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
		&m.HasContextTokens, &m.HasOutputTokens,
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

type toolResultEventRow struct {
	SessionID      string
	MessageOrdinal int
	CallIndex      int
	Event          ToolResultEvent
}

func resolveToolResultEvents(msgs []Message) []toolResultEventRow {
	var rows []toolResultEventRow
	for _, m := range msgs {
		for callIndex, tc := range m.ToolCalls {
			for eventIndex, ev := range tc.ResultEvents {
				ev.EventIndex = eventIndex
				if ev.ContentLength == 0 {
					ev.ContentLength = len(ev.Content)
				}
				if ev.ToolUseID == "" {
					ev.ToolUseID = tc.ToolUseID
				}
				if ev.SubagentSessionID == "" {
					ev.SubagentSessionID = tc.SubagentSessionID
				}
				rows = append(rows, toolResultEventRow{
					SessionID:      m.SessionID,
					MessageOrdinal: m.Ordinal,
					CallIndex:      callIndex,
					Event:          ev,
				})
			}
		}
	}
	return rows
}
