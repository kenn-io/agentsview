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
		thinking_text,
		timestamp, has_thinking, has_tool_use, content_length,
		is_system,
		model, token_usage, context_tokens, output_tokens,
		has_context_tokens, has_output_tokens,
		claude_message_id, claude_request_id,
		source_type, source_subtype, source_uuid,
		source_parent_uuid, is_sidechain, is_compact_boundary`

	insertMessageCols = `session_id, ordinal, role, content,
		thinking_text,
		timestamp, has_thinking, has_tool_use, content_length,
		is_system,
		model, token_usage, context_tokens, output_tokens,
		has_context_tokens, has_output_tokens,
		claude_message_id, claude_request_id,
		source_type, source_subtype, source_uuid,
		source_parent_uuid, is_sidechain, is_compact_boundary`

	// DefaultMessageLimit is the default number of messages returned.
	DefaultMessageLimit = 100
	// MaxMessageLimit is the maximum number of messages returned.
	MaxMessageLimit = 1000

	// Keep query parameter counts conservative so large sessions
	// do not exceed SQLite variable limits when hydrating tool calls.
	attachToolCallBatchSize = 500

	// Keep multi-row INSERT statements below SQLite's historic
	// 999-variable limit so binaries built against older SQLite
	// versions still work.
	messageInsertRowsPerStmt         = 39 // 25 params per row
	toolCallInsertRowsPerStmt        = 90 // 10 params per row
	toolResultEventInsertRowsPerStmt = 80 // 12 params per row
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
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	Ordinal   int    `json:"ordinal"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	// ThinkingText holds the concatenated text of all thinking
	// blocks for this message; "" if none.
	ThinkingText      string          `json:"thinking_text"`
	Timestamp         string          `json:"timestamp"`
	HasThinking       bool            `json:"has_thinking"`
	HasToolUse        bool            `json:"has_tool_use"`
	ContentLength     int             `json:"content_length"`
	Model             string          `json:"model"`
	TokenUsage        json.RawMessage `json:"token_usage,omitempty"`
	ContextTokens     int             `json:"context_tokens"`
	OutputTokens      int             `json:"output_tokens"`
	HasContextTokens  bool            `json:"has_context_tokens"`
	HasOutputTokens   bool            `json:"has_output_tokens"`
	ClaudeMessageID   string          `json:"claude_message_id,omitempty"`
	ClaudeRequestID   string          `json:"claude_request_id,omitempty"`
	ToolCalls         []ToolCall      `json:"tool_calls,omitempty"`
	ToolResults       []ToolResult    `json:"-"`         // transient, for pairing
	IsSystem          bool            `json:"is_system"` // persisted, filters search/analytics
	SourceType        string          `json:"source_type,omitempty"`
	SourceSubtype     string          `json:"source_subtype,omitempty"`
	SourceUUID        string          `json:"source_uuid,omitempty"`
	SourceParentUUID  string          `json:"source_parent_uuid,omitempty"`
	IsSidechain       bool            `json:"is_sidechain,omitempty"`
	IsCompactBoundary bool            `json:"is_compact_boundary,omitempty"`
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
func insertMessagesTx(
	tx *sql.Tx, msgs []Message,
) ([]int64, error) {
	ids := make([]int64, len(msgs))
	nextID, err := nextMessageIDTx(tx)
	if err != nil {
		return nil, err
	}

	for start := 0; start < len(msgs); start += messageInsertRowsPerStmt {
		end := min(start+messageInsertRowsPerStmt, len(msgs))
		batch := msgs[start:end]
		args := make([]any, 0, len(batch)*25)
		for i, m := range batch {
			id := nextID + int64(start+i)
			ids[start+i] = id
			args = append(args,
				id,
				m.SessionID, m.Ordinal, m.Role, m.Content,
				m.ThinkingText,
				m.Timestamp, m.HasThinking, m.HasToolUse,
				m.ContentLength, m.IsSystem,
				m.Model, string(m.TokenUsage),
				m.ContextTokens, m.OutputTokens,
				m.HasContextTokens, m.HasOutputTokens,
				m.ClaudeMessageID, m.ClaudeRequestID,
				m.SourceType, m.SourceSubtype, m.SourceUUID,
				m.SourceParentUUID, m.IsSidechain, m.IsCompactBoundary,
			)
		}
		query := fmt.Sprintf(
			"INSERT INTO messages (id, %s) VALUES %s",
			insertMessageCols,
			multiRowPlaceholders(len(batch), 25),
		)
		if _, err := tx.Exec(query, args...); err != nil {
			first := batch[0].Ordinal
			last := batch[len(batch)-1].Ordinal
			return nil, fmt.Errorf(
				"inserting messages ord=%d..%d: %w",
				first, last, err,
			)
		}
	}
	return ids, nil
}

func nextMessageIDTx(tx *sql.Tx) (int64, error) {
	var n sql.NullInt64
	if err := tx.QueryRow("SELECT MAX(id) FROM messages").Scan(&n); err != nil {
		return 0, fmt.Errorf("reading next message id: %w", err)
	}
	if !n.Valid {
		return 1, nil
	}
	return n.Int64 + 1, nil
}

func multiRowPlaceholders(rows, cols int) string {
	var b strings.Builder
	for i := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for j := range cols {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('?')
		}
		b.WriteByte(')')
	}
	return b.String()
}

func insertToolCallsChunkTx(
	tx *sql.Tx, calls []ToolCall,
) error {
	args := make([]any, 0, len(calls)*10)
	for _, tc := range calls {
		args = append(args,
			tc.MessageID, tc.SessionID,
			tc.ToolName, tc.Category,
			nilIfEmpty(tc.ToolUseID),
			nilIfEmpty(tc.InputJSON),
			nilIfEmpty(tc.SkillName),
			nilIfZero(tc.ResultContentLength),
			nilIfEmpty(tc.ResultContent),
			nilIfEmpty(tc.SubagentSessionID),
		)
	}
	query := `
		INSERT INTO tool_calls
			(message_id, session_id, tool_name, category,
			 tool_use_id, input_json, skill_name,
			 result_content_length, result_content, subagent_session_id)
		VALUES ` + multiRowPlaceholders(len(calls), 10)
	if _, err := tx.Exec(query, args...); err != nil {
		return fmt.Errorf(
			"inserting tool_calls batch (%d rows): %w",
			len(calls), err,
		)
	}
	return nil
}

func insertToolResultEventsChunkTx(
	tx *sql.Tx, rows []toolResultEventRow,
) error {
	args := make([]any, 0, len(rows)*12)
	for _, r := range rows {
		args = append(args,
			r.SessionID, r.MessageOrdinal, r.CallIndex,
			nilIfEmpty(r.Event.ToolUseID),
			nilIfEmpty(r.Event.AgentID),
			nilIfEmpty(r.Event.SubagentSessionID),
			r.Event.Source, r.Event.Status,
			r.Event.Content,
			r.Event.ContentLength,
			nilIfEmpty(r.Event.Timestamp),
			r.Event.EventIndex,
		)
	}
	query := `
		INSERT INTO tool_result_events
			(session_id, tool_call_message_ordinal, call_index,
			 tool_use_id, agent_id, subagent_session_id,
			 source, status, content, content_length,
			 timestamp, event_index)
		VALUES ` + multiRowPlaceholders(len(rows), 12)
	if _, err := tx.Exec(query, args...); err != nil {
		return fmt.Errorf(
			"inserting tool_result_events batch (%d rows): %w",
			len(rows), err,
		)
	}
	return nil
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
	for start := 0; start < len(calls); start += toolCallInsertRowsPerStmt {
		end := min(start+toolCallInsertRowsPerStmt, len(calls))
		if err := insertToolCallsChunkTx(tx, calls[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func insertToolResultEventsTx(
	tx *sql.Tx, rows []toolResultEventRow,
) error {
	for start := 0; start < len(rows); start += toolResultEventInsertRowsPerStmt {
		end := min(start+toolResultEventInsertRowsPerStmt, len(rows))
		if err := insertToolResultEventsChunkTx(tx, rows[start:end]); err != nil {
			return err
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

	ids, err := insertMessagesTx(tx, msgs)
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

// LastClaudeMessageID returns the claude_message_id of the
// highest-ordinal assistant message in a session whose
// claude_message_id is non-empty, or "" if none exists. The sync
// engine uses this to detect cross-sync splits of a single
// streaming response (next sync's first appended assistant entry
// shares the message.id of the previously-stored last assistant).
func (db *DB) LastClaudeMessageID(sessionID string) string {
	var s sql.NullString
	err := db.getReader().QueryRow(
		`SELECT claude_message_id FROM messages
		 WHERE session_id = ?
		   AND role = 'assistant'
		   AND claude_message_id != ''
		 ORDER BY ordinal DESC
		 LIMIT 1`,
		sessionID,
	).Scan(&s)
	if err != nil || !s.Valid {
		return ""
	}
	return s.String
}

// savedPin captures the minimal pin state needed to re-attach a pin
// after a full message replacement. source_uuid is the preferred
// identifier because it survives rewrites where the ordinal stream
// shifts (e.g. when newly-emitted compact-boundary messages are
// inserted between previously-seen rows). The ordinal is kept as a
// fallback for legacy pins on rows that lack a source_uuid.
type savedPin struct {
	sourceUUID string
	ordinal    int
	note       *string
	createdAt  string
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

	pins, err := savePinsTx(tx, sessionID)
	if err != nil {
		return err
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

	// FTS5 is optional (the module may be missing in the runtime).
	// Probe sqlite_master so the bulk-delete + trigger-swap dance
	// only runs when there's actually an FTS table to maintain.
	var ftsCount int
	if err := tx.QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='table' AND name='messages_fts'`,
	).Scan(&ftsCount); err != nil {
		return fmt.Errorf("probing fts table: %w", err)
	}
	hasFTS := ftsCount > 0

	if hasFTS {
		// Bulk-delete the FTS index entries up-front in a single SQL
		// statement, then drop the per-row messages_ad trigger so the
		// upcoming DELETE FROM messages doesn't re-fire the FTS5
		// 'delete' command for every row. With large sessions
		// (thousands of rows where a single content blob can be many
		// MB) the per-row trigger path is dominated by FTS
		// tokenization and stalls the writer for minutes; the bulk
		// INSERT...SELECT path is effectively flat. The trigger is
		// restored before the transaction is allowed to commit.
		if _, err := tx.Exec(
			`INSERT INTO messages_fts(messages_fts, rowid, content)
			 SELECT 'delete', id, content
			 FROM messages WHERE session_id = ?`,
			sessionID,
		); err != nil {
			return fmt.Errorf("bulk-deleting fts entries: %w", err)
		}
		if _, err := tx.Exec(
			"DROP TRIGGER IF EXISTS messages_ad",
		); err != nil {
			return fmt.Errorf("dropping messages_ad trigger: %w", err)
		}
	}
	if _, err := tx.Exec(
		"DELETE FROM messages WHERE session_id = ?", sessionID,
	); err != nil {
		return fmt.Errorf("deleting old messages: %w", err)
	}
	if hasFTS {
		if _, err := tx.Exec(messagesADTriggerDDL); err != nil {
			return fmt.Errorf("restoring messages_ad trigger: %w", err)
		}
	}

	if len(msgs) > 0 {
		ids, err := insertMessagesTx(tx, msgs)
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

	if err := restorePinsTx(tx, sessionID, pins); err != nil {
		return err
	}

	return tx.Commit()
}

func savePinsTx(tx *sql.Tx, sessionID string) ([]savedPin, error) {
	// Save existing pins before deletion. The ON DELETE CASCADE on
	// pinned_messages.message_id would otherwise wipe them when
	// messages are deleted below. source_uuid comes from the joined
	// message row; LEFT JOIN keeps pins on legacy rows whose
	// message_id no longer resolves cleanly.
	pinRows, err := tx.Query(`
		SELECT p.ordinal, COALESCE(m.source_uuid, ''),
			p.note, p.created_at
		FROM pinned_messages p
		LEFT JOIN messages m ON m.id = p.message_id
		WHERE p.session_id = ?`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("saving pins: %w", err)
	}
	defer pinRows.Close()
	var pins []savedPin
	for pinRows.Next() {
		var sp savedPin
		if err := pinRows.Scan(
			&sp.ordinal, &sp.sourceUUID, &sp.note, &sp.createdAt,
		); err != nil {
			return nil, fmt.Errorf("scanning pin: %w", err)
		}
		pins = append(pins, sp)
	}
	if err := pinRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pins: %w", err)
	}
	return pins, nil
}

func restorePinsTx(
	tx *sql.Tx, sessionID string, pins []savedPin,
) error {
	// Re-attach saved pins. Prefer source_uuid (stable across
	// ordinal-shifting rewrites) and fall back to ordinal for
	// legacy pins whose source row predates the source_uuid column.
	// Pins whose row no longer exists by either key are silently
	// dropped.
	for _, sp := range pins {
		if sp.sourceUUID != "" {
			res, err := tx.Exec(`
				INSERT OR IGNORE INTO pinned_messages
					(session_id, message_id, ordinal, note, created_at)
				SELECT ?, m.id, m.ordinal, ?, ?
				FROM messages m
				WHERE m.session_id = ? AND m.source_uuid = ?`,
				sessionID, sp.note, sp.createdAt, sessionID, sp.sourceUUID,
			)
			if err != nil {
				return fmt.Errorf(
					"restoring pin uuid=%s: %w", sp.sourceUUID, err,
				)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				continue
			}
		}
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
	return nil
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
			&m.Content, &m.ThinkingText, &m.Timestamp,
			&m.HasThinking, &m.HasToolUse, &m.ContentLength,
			&m.IsSystem,
			&m.Model, &tokenUsage,
			&m.ContextTokens, &m.OutputTokens,
			&m.HasContextTokens, &m.HasOutputTokens,
			&m.ClaudeMessageID, &m.ClaudeRequestID,
			&m.SourceType, &m.SourceSubtype, &m.SourceUUID,
			&m.SourceParentUUID, &m.IsSidechain, &m.IsCompactBoundary,
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
// unchanged sessions. Includes the source-tracking columns so
// metadata-only changes invalidate the fast path.
func (db *DB) MessageTokenFingerprint(sessionID string) (string, error) {
	rows, err := db.getReader().Query(
		`SELECT ordinal, model, token_usage, context_tokens,
			output_tokens, has_context_tokens, has_output_tokens,
			claude_message_id, claude_request_id,
			source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain, is_compact_boundary
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
		var claudeMsgID, claudeReqID string
		var srcType, srcSubtype, srcUUID, srcParentUUID string
		var isSidechain, isCompactBoundary bool
		if err := rows.Scan(
			&ordinal, &model, &tokenUsage, &contextTokens,
			&outputTokens, &hasContextTokens, &hasOutputTokens,
			&claudeMsgID, &claudeReqID,
			&srcType, &srcSubtype, &srcUUID, &srcParentUUID,
			&isSidechain, &isCompactBoundary,
		); err != nil {
			return "", err
		}
		fmt.Fprintf(&b,
			"%d|%d:%s|%d:%s|%d|%d|%t|%t|%s|%s|"+
				"%d:%s|%d:%s|%d:%s|%d:%s|%t|%t;",
			ordinal,
			len(model), model,
			len(tokenUsage), tokenUsage,
			contextTokens, outputTokens,
			hasContextTokens, hasOutputTokens,
			claudeMsgID, claudeReqID,
			len(srcType), srcType,
			len(srcSubtype), srcSubtype,
			len(srcUUID), srcUUID,
			len(srcParentUUID), srcParentUUID,
			isSidechain, isCompactBoundary,
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
		&m.Content, &m.ThinkingText, &m.Timestamp,
		&m.HasThinking, &m.HasToolUse, &m.ContentLength,
		&m.IsSystem,
		&m.Model, &tokenUsage,
		&m.ContextTokens, &m.OutputTokens,
		&m.HasContextTokens, &m.HasOutputTokens,
		&m.ClaudeMessageID, &m.ClaudeRequestID,
		&m.SourceType, &m.SourceSubtype, &m.SourceUUID,
		&m.SourceParentUUID, &m.IsSidechain, &m.IsCompactBoundary,
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
