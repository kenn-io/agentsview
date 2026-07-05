package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/agentsview/internal/parser"
)

const (
	selectMessageCols = `id, session_id, ordinal, role, content,
		thinking_text,
		COALESCE(timestamp, '') AS timestamp,
		has_thinking, has_tool_use, content_length,
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
	toolCallInsertRowsPerStmt        = 83 // 12 params per row (999/12 = 83)
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
	FilePath            string            `json:"-"`
	CallIndex           int               `json:"-"`
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

// MessageWindow parameterises GetMessagesWindow. Exactly one retrieval
// mode: Around non-nil = symmetric window; otherwise linear from/limit.
type MessageWindow struct {
	From   *int
	Limit  int
	Asc    bool
	Around *int
	Before int // used only with Around; default handled by caller
	After  int
	Roles  []string // empty = all roles
}

// GetMessagesWindow returns messages for a session using either linear
// pagination (mirroring GetMessages, optionally role-filtered) or a
// symmetric window centered on an ordinal (Around/Before/After). Around
// mode always includes the anchor row even when its own role is excluded
// by Roles; the before/after counts are taken after applying the role
// filter, so they count role-matching messages rather than raw ordinal
// distance from the anchor.
func (db *DB) GetMessagesWindow(
	ctx context.Context, sessionID string, w MessageWindow,
) ([]Message, error) {
	if w.Around != nil {
		return db.getMessagesAroundAnchor(ctx, sessionID, w)
	}
	from := 0
	if w.From != nil {
		from = *w.From
	}
	if len(w.Roles) == 0 {
		return db.GetMessages(ctx, sessionID, from, w.Limit, w.Asc)
	}
	return db.getMessagesLinearRoleFiltered(
		ctx, sessionID, from, w.Limit, w.Asc, w.Roles,
	)
}

// getMessagesLinearRoleFiltered is GetMessages plus an "AND role IN (...)"
// predicate, used when MessageWindow.Roles is non-empty.
func (db *DB) getMessagesLinearRoleFiltered(
	ctx context.Context,
	sessionID string, from, limit int, asc bool, roles []string,
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
	roleClause, roleArgs := roleFilterClause(roles)
	query := fmt.Sprintf(`
		SELECT %s
		FROM messages
		WHERE session_id = ? AND ordinal %s ?%s
		ORDER BY ordinal %s
		LIMIT ?`, selectMessageCols, op, roleClause, dir)
	args := append([]any{sessionID, from}, roleArgs...)
	args = append(args, limit)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying role-filtered messages: %w", err)
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

// getMessagesAroundAnchor implements MessageWindow's Around mode: three
// queries (before/anchor/after) merged into one ascending slice. The
// anchor query has no role predicate so the anchor row is always present;
// before/after apply the role filter (when set) before taking Before/After
// rows, so the counts reflect role-matching messages, not raw ordinals.
func (db *DB) getMessagesAroundAnchor(
	ctx context.Context, sessionID string, w MessageWindow,
) ([]Message, error) {
	anchor := *w.Around
	beforeLimit := max(w.Before, 0)
	afterLimit := max(w.After, 0)
	roleClause, roleArgs := roleFilterClause(w.Roles)

	beforeQuery := fmt.Sprintf(`
		SELECT %s FROM messages
		WHERE session_id = ? AND ordinal < ?%s
		ORDER BY ordinal DESC LIMIT ?`, selectMessageCols, roleClause)
	beforeArgs := append([]any{sessionID, anchor}, roleArgs...)
	beforeArgs = append(beforeArgs, beforeLimit)
	before, err := db.queryMessageRows(ctx, beforeQuery, beforeArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying before-window messages: %w", err)
	}
	slices.Reverse(before)

	anchorQuery := fmt.Sprintf(`
		SELECT %s FROM messages WHERE session_id = ? AND ordinal = ?`,
		selectMessageCols)
	anchorMsgs, err := db.queryMessageRows(ctx, anchorQuery, sessionID, anchor)
	if err != nil {
		return nil, fmt.Errorf("querying anchor message: %w", err)
	}

	afterQuery := fmt.Sprintf(`
		SELECT %s FROM messages
		WHERE session_id = ? AND ordinal > ?%s
		ORDER BY ordinal ASC LIMIT ?`, selectMessageCols, roleClause)
	afterArgs := append([]any{sessionID, anchor}, roleArgs...)
	afterArgs = append(afterArgs, afterLimit)
	after, err := db.queryMessageRows(ctx, afterQuery, afterArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying after-window messages: %w", err)
	}

	msgs := make([]Message, 0, len(before)+len(anchorMsgs)+len(after))
	msgs = append(msgs, before...)
	msgs = append(msgs, anchorMsgs...)
	msgs = append(msgs, after...)
	if err := db.attachToolCalls(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// queryMessageRows runs query and scans the resulting message rows,
// without attaching tool calls (callers batch that across the merged set).
func (db *DB) queryMessageRows(
	ctx context.Context, query string, args ...any,
) ([]Message, error) {
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// roleFilterClause returns an "AND role IN (...)" clause and its bind
// args for the given roles, or ("", nil) when roles is empty.
func roleFilterClause(roles []string) (string, []any) {
	if len(roles) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(roles))
	args := make([]any, len(roles))
	for i, r := range roles {
		placeholders[i] = "?"
		args[i] = r
	}
	return " AND role IN (" + strings.Join(placeholders, ",") + ")", args
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

// EmbeddableMessage is one user/assistant message eligible for vector
// indexing (role user|assistant, is_system = 0, not system-prefixed).
type EmbeddableMessage struct {
	SessionID  string
	SourceUUID string
	Ordinal    int
	Content    string
}

// ScanEmbeddableMessages streams the embeddable universe (user/assistant
// messages that are not is_system and not system-prefixed, per
// SystemPrefixSQL) from non-trashed sessions, ordered by (session_id,
// ordinal), invoking fn once per row. since != "" restricts the scan to
// sessions with ended_at >= since (RFC3339 or RFC3339Nano) for incremental
// refresh, comparing parsed timestamps rather than raw strings via SQLite's
// datetime() so mixed fractional-second precision doesn't produce a wrong
// ordering; "" scans every session. maxEnded returns the maximum
// sessions.ended_at seen across the scanned rows (as its original raw
// string), or "" when the scan produced no rows.
func (db *DB) ScanEmbeddableMessages(
	ctx context.Context, since string, fn func(EmbeddableMessage) error,
) (maxEnded string, err error) {
	query := `
		SELECT m.session_id, m.source_uuid, m.ordinal, m.content, s.ended_at
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE m.role IN ('user', 'assistant')
		  AND m.is_system = 0
		  AND s.deleted_at IS NULL
		  AND ` + SystemPrefixSQL("m.content", "m.role") + `
		` + optionalSinceClause(since) + `
		ORDER BY m.session_id, m.ordinal`

	args := []any{}
	if since != "" {
		args = append(args, since)
	}

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("scanning embeddable messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var m EmbeddableMessage
		var ended sql.NullString
		if err := rows.Scan(
			&m.SessionID, &m.SourceUUID, &m.Ordinal, &m.Content, &ended,
		); err != nil {
			return "", fmt.Errorf("scanning embeddable message: %w", err)
		}
		if ended.Valid && endedAfter(ended.String, maxEnded) {
			maxEnded = ended.String
		}
		if err := fn(m); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterating embeddable messages: %w", err)
	}
	return maxEnded, nil
}

// optionalSinceClause returns the AND clause restricting the embeddable scan
// to sessions with ended_at >= since (or ended_at IS NULL), or "" when since
// is unset. It compares via SQLite's datetime() rather than raw string
// ordering: RFC3339Nano's variable fractional-second precision (e.g.
// ended_at values are sometimes stored with milliseconds, sometimes
// without) makes lexicographic comparison wrong, since "...00.123Z" sorts
// before "...00Z". datetime() truncates to second granularity, so a session
// whose true ended_at is a few hundred milliseconds before since may be
// re-scanned; that overlap is harmless because Refresh's upserts are
// idempotent.
//
// A NULL ended_at (a session still in progress, or one whose parser never
// set it) always matches: excluding it would make its messages invisible to
// every incremental scan until a full (since="") rebuild happens to catch
// it, even though re-scanning an unchanged session is just a cheap no-op
// mirror upsert. A legacy empty-string ended_at (the pre-NULLIF-migration
// "unset" sentinel this repo's other read queries guard against, e.g.
// sessions.go's COALESCE(NULLIF(ended_at, ""), ...) chains) must be treated
// the same way via NULLIF(s.ended_at, ""): without it, "" is neither NULL
// nor >= since, so a changed legacy session would never be rescanned again
// once any watermark exists.
func optionalSinceClause(since string) string {
	if since == "" {
		return ""
	}
	return "AND (NULLIF(s.ended_at, '') IS NULL OR " +
		"datetime(NULLIF(s.ended_at, '')) >= datetime(?))"
}

// endedAfter reports whether candidate is chronologically after current,
// comparing parsed RFC3339/RFC3339Nano timestamps rather than raw strings
// so variable fractional-second precision can't produce a wrong ordering.
// An empty current is always considered older. Falls back to a
// lexicographic comparison if either value fails to parse.
func endedAfter(candidate, current string) bool {
	if current == "" {
		return true
	}
	c, errC := parseEndedAt(candidate)
	cur, errCur := parseEndedAt(current)
	if errC != nil || errCur != nil {
		return candidate > current
	}
	return c.After(cur)
}

// parseEndedAt parses an ended_at value, trying RFC3339Nano (which also
// accepts plain RFC3339) first and falling back to strict RFC3339.
func parseEndedAt(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
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
			args = append(args, id)
			args = append(args, messageInsertArgs(m)...)
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
	args := make([]any, 0, len(calls)*12)
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
			nilIfEmpty(tc.FilePath),
			tc.CallIndex,
		)
	}
	query := `
		INSERT INTO tool_calls
			(message_id, session_id, tool_name, category,
			 tool_use_id, input_json, skill_name,
			 result_content_length, result_content, subagent_session_id,
			 file_path, call_index)
		VALUES ` + multiRowPlaceholders(len(calls), 12)
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
	if err := db.requireWritable(); err != nil {
		return err
	}
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
	for _, sessionID := range messageSessionIDs(msgs) {
		if err := setSessionAutomationFromMessagesTx(
			tx, sessionID,
		); err != nil {
			return err
		}
		if err := invalidateSessionSignalsTx(tx, sessionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// invalidateSessionSignalsTx zeroes quality_signal_version so the
// startup backfill treats the session as stale. Every transaction
// that changes message content but refreshes derived signals and
// secret findings through separate follow-up writes must call this:
// if those writes never land (crash, DB closed under a resync swap),
// the zeroed version keeps the session eligible for recompute instead
// of freezing pre-write derived data as current. Only the signal
// update itself restores the version.
func invalidateSessionSignalsTx(tx *sql.Tx, sessionID string) error {
	if _, err := tx.Exec(
		"UPDATE sessions SET quality_signal_version = 0 WHERE id = ?",
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"invalidating signal version for %s: %w", sessionID, err,
		)
	}
	return nil
}

func writeMessagesTx(tx *sql.Tx, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
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
	return nil
}

func (db *DB) WriteSessionIncremental(
	sessionID string, msgs []Message, update IncrementalSessionUpdate,
) error {
	t := time.Now()
	defer func() {
		if d := time.Since(t); d > slowOpThreshold {
			log.Printf(
				"db: WriteSessionIncremental (%d msgs): %s",
				len(msgs), d.Round(time.Millisecond),
			)
		}
	}()

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning incremental write tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := writeMessagesTx(tx, msgs); err != nil {
		return err
	}
	if err := updateSessionIncrementalTx(tx, sessionID, update); err != nil {
		return err
	}
	if err := updateSessionAutomationFromMessagesTx(tx, sessionID); err != nil {
		return err
	}
	if err := invalidateSessionSignalsTx(tx, sessionID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing incremental write tx: %w", err)
	}
	return nil
}

func messageSessionIDs(msgs []Message) []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0, 1)
	for _, m := range msgs {
		if m.SessionID == "" {
			continue
		}
		if _, ok := seen[m.SessionID]; ok {
			continue
		}
		seen[m.SessionID] = struct{}{}
		ids = append(ids, m.SessionID)
	}
	return ids
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
	msgs = append([]Message(nil), msgs...)
	_ = ValidateAndSanitize(nil, msgs, nil)

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

	// Prefer an in-place diff (append/merge shapes from streaming
	// syncs) so unchanged rows keep their rowids, pins, and FTS
	// entries; fall back to the full delete+reinsert for
	// truncations, reorders, and wholesale rewrites.
	plan, useDiff := db.planStoredMessageDiff(sessionID, msgs)

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if useDiff {
		if err := applySessionMessageDiffTx(tx, sessionID, plan); err != nil {
			return err
		}
	} else if err := replaceSessionMessagesTx(tx, sessionID, msgs); err != nil {
		return err
	}
	// A full message replacement re-normalizes every row, so clear the
	// incremental-append marker parse-diff reads (see resetIncrementalMarkerTx).
	if err := resetIncrementalMarkerTx(tx, sessionID); err != nil {
		return err
	}
	if err := updateSessionAutomationFromMessagesTx(tx, sessionID); err != nil {
		return err
	}
	// The new messages invalidate any findings scanned from the old content, so
	// clear them and reset the scan state (empty version => secrets scan
	// --backfill re-scans). ReplaceSessionContent does not call this method; it
	// supplies fresh findings via replaceSecretFindingsTx directly.
	if err := replaceSecretFindingsTx(tx, sessionID, nil, 0, ""); err != nil {
		return err
	}
	if err := invalidateSessionSignalsTx(tx, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// replaceSessionMessagesTx performs the full message-replace sequence within
// an existing transaction: saves pins, deletes old tool_calls /
// tool_result_events / messages (with FTS optimisation), inserts new messages
// + tool_calls + tool_result_events, then restores pins. Caller owns the lock
// and transaction lifecycle.
func replaceSessionMessagesTx(
	tx *sql.Tx, sessionID string, msgs []Message,
) error {
	pins, err := savePinsTx(tx, sessionID)
	if err != nil {
		return err
	}

	if err := deleteSessionMessagesTx(tx, sessionID); err != nil {
		return err
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

	return restorePinsTx(tx, sessionID, pins)
}

func sessionHasFTSTx(tx *sql.Tx) (bool, error) {
	var ftsCount int
	if err := tx.QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='table' AND name='messages_fts'`,
	).Scan(&ftsCount); err != nil {
		return false, fmt.Errorf("probing fts table: %w", err)
	}
	return ftsCount > 0, nil
}

func deleteSessionMessageRowsTx(
	tx *sql.Tx, sessionID string,
) error {
	hasFTS, err := sessionHasFTSTx(tx)
	if err != nil {
		return err
	}

	if hasFTS {
		// Bulk-delete the FTS entries first so the later row delete
		// does not re-tokenize large message blobs through messages_ad.
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
	return nil
}

func deleteSessionMessagesTx(tx *sql.Tx, sessionID string) error {
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
	return deleteSessionMessageRowsTx(tx, sessionID)
}

// ReplaceSessionContent atomically replaces a session's messages, signal
// columns, and secret findings in one transaction, so the derived data can
// never diverge from the messages it was computed from.
func (db *DB) ReplaceSessionContent(
	sessionID string, msgs []Message,
	signals SessionSignalUpdate, findings []SecretFinding,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Same diff-vs-full decision as ReplaceSessionMessages: this is
	// the hot path for streaming chunk-merge full-parse fallbacks.
	plan, useDiff := db.planStoredMessageDiff(sessionID, msgs)

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if useDiff {
		if err := applySessionMessageDiffTx(tx, sessionID, plan); err != nil {
			return err
		}
	} else if err := replaceSessionMessagesTx(tx, sessionID, msgs); err != nil {
		return err
	}
	// Every message row is now the full-parse shape, so this row is no
	// longer incremental-append skew: clear the marker parse-diff reads.
	if err := resetIncrementalMarkerTx(tx, sessionID); err != nil {
		return err
	}
	if err := updateSessionAutomationFromMessagesTx(tx, sessionID); err != nil {
		return err
	}
	if err := updateSessionSignalsTx(tx, sessionID, signals); err != nil {
		return err
	}
	// replaceSecretFindingsTx is the sole writer of secret_leak_count/
	// secrets_rules_version (updateSessionSignalsTx leaves them untouched), so
	// the count cannot diverge from the findings it summarizes.
	if err := replaceSecretFindingsTx(tx, sessionID, findings,
		signals.SecretLeakCount, signals.SecretsRulesVersion); err != nil {
		return err
	}
	return tx.Commit()
}

func updateSessionAutomationFromMessagesTx(
	tx *sql.Tx, sessionID string,
) error {
	want, rowAutomated, ok, err := sessionAutomationStateTx(
		tx, sessionID,
	)
	if err != nil || !ok {
		return err
	}
	if want == rowAutomated {
		return nil
	}
	return setSessionAutomationTx(tx, sessionID, want)
}

func setSessionAutomationFromMessagesTx(
	tx *sql.Tx, sessionID string,
) error {
	want, rowAutomated, ok, err := sessionAutomationStateTx(
		tx, sessionID,
	)
	if err != nil || !ok || !want || rowAutomated {
		return err
	}
	return setSessionAutomationTx(tx, sessionID, true)
}

func sessionAutomationStateTx(
	tx *sql.Tx, sessionID string,
) (want, rowAutomated, ok bool, err error) {
	var (
		firstMessage     sql.NullString
		firstUserMessage sql.NullString
		userMsgCount     int
	)
	err = tx.QueryRow(`
		SELECT
			s.first_message,
			s.user_message_count,
			s.is_automated,
			(
				SELECT m.content
				FROM messages m
				WHERE m.session_id = s.id
				  AND m.role = 'user'
				  AND m.is_system = 0
				  AND TRIM(m.content) <> ''
				ORDER BY m.ordinal
				LIMIT 1
			) AS first_user_message
		FROM sessions s
		WHERE s.id = ?`,
		sessionID,
	).Scan(
		&firstMessage, &userMsgCount,
		&rowAutomated, &firstUserMessage,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, false, nil
	}
	if err != nil {
		return false, false, false, fmt.Errorf(
			"reading automation candidate for %s: %w",
			sessionID, err,
		)
	}

	want = isAutomatedFromTextCandidates(
		userMsgCount, firstUserMessage, firstMessage,
	)
	return want, rowAutomated, true, nil
}

func setSessionAutomationTx(
	tx *sql.Tx, sessionID string, isAutomated bool,
) error {
	if _, err := tx.Exec(`
		UPDATE sessions
		   SET is_automated = ?,
		       local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ?`,
		isAutomated, sessionID,
	); err != nil {
		return fmt.Errorf(
			"updating is_automated from messages for %s: %w",
			sessionID, err,
		)
	}
	return nil
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
			result_content_length, result_content, subagent_session_id,
			file_path, call_index
		FROM tool_calls
		WHERE message_id IN (%s)
		ORDER BY message_id, call_index`,
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
		var filePath sql.NullString
		var resultLen sql.NullInt64
		var callIndex sql.NullInt64
		if err := rows.Scan(
			&tc.MessageID, &tc.SessionID,
			&tc.ToolName, &tc.Category,
			&toolUseID, &inputJSON, &skillName,
			&resultLen, &resultContent, &subagentSessionID,
			&filePath, &callIndex,
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
		if filePath.Valid {
			tc.FilePath = filePath.String
		}
		if callIndex.Valid {
			tc.CallIndex = int(callIndex.Int64)
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

// SanitizeUTF8 strips NUL bytes, replaces invalid UTF-8 sequences,
// and removes control runes (other than \n, \t, \r). PostgreSQL
// enforces strict UTF-8 and rejects NUL in text columns, so the push
// boundary applies this to every parser-derived string; the local
// fingerprint builders below apply it too so local fingerprints stay
// comparable to PG-readback fingerprints when a stored row carries
// NUL bytes.
//
// The control-rune strip runs per rune, not per byte: C1 controls
// (U+0080..U+009F) are valid two-byte UTF-8 and survive
// strings.ToValidUTF8, so a terminal escape such as ESC ]0;...BEL
// embedded in parsed content would otherwise persist intact. This
// function is the single sanitization seam shared by the write path
// (sync.validateAndSanitize), the local fingerprint builders, and the
// PG push/readback path, so it MUST stay idempotent:
// SanitizeUTF8(SanitizeUTF8(s)) == SanitizeUTF8(s). The byte-level
// NUL strip is retained because it is the pg-push breaker fix and a
// raw NUL must be removed before strings.ToValidUTF8 (which treats it
// as valid).
func SanitizeUTF8(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ToValidUTF8(s, "")
	// Fast path: skip the rune scan and allocation when the string
	// carries no control runes to strip.
	if strings.IndexFunc(s, isStrippableControl) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isStrippableControl(r) {
			return -1
		}
		return r
	}, s)
}

// isStrippableControl reports whether r is a control rune that
// SanitizeUTF8 removes. Newline, tab, and carriage return are
// preserved because they are legitimate whitespace in message
// content; every other control rune (C0 below U+0020, DEL, and the
// C1 block U+0080..U+009F) is stripped.
func isStrippableControl(r rune) bool {
	if r == '\n' || r == '\t' || r == '\r' {
		return false
	}
	return unicode.IsControl(r)
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
		// Sanitize before measuring: the PG-readback
		// fingerprint sees values sanitized at insert time,
		// so raw values (e.g. NUL bytes from a corrupt parse)
		// would never match and the fast path would rewrite
		// the session on every push.
		model = SanitizeUTF8(model)
		tokenUsage = SanitizeUTF8(tokenUsage)
		claudeMsgID = SanitizeUTF8(claudeMsgID)
		claudeReqID = SanitizeUTF8(claudeReqID)
		srcType = SanitizeUTF8(srcType)
		srcSubtype = SanitizeUTF8(srcSubtype)
		srcUUID = SanitizeUTF8(srcUUID)
		srcParentUUID = SanitizeUTF8(srcParentUUID)
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

// MessageContentHashFingerprint returns an exact ordered fingerprint
// of per-message body content: ordinal, the stored content_length
// column, and a SHA-256 over the sanitized content. Parse-diff and PG
// push use it alongside the aggregate MessageContentFingerprint
// (sum/max/min of content_length), which cannot see equal-length body
// rewrites or per-message length changes whose aggregates collide.
func (db *DB) MessageContentHashFingerprint(sessionID string) (string, error) {
	rows, err := db.getReader().Query(
		`SELECT ordinal, content, content_length
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
		var ordinal, contentLength int
		var content string
		if err := rows.Scan(
			&ordinal, &content, &contentLength,
		); err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte(SanitizeUTF8(content)))
		fmt.Fprintf(&b, "%d|%d|%x;", ordinal, contentLength, sum)
	}
	return b.String(), rows.Err()
}

// MessageRoleTimeFingerprint returns an exact ordered fingerprint of
// per-message role and timestamp for a session's messages. The
// parse-diff comparator uses it as a tier-1 fast path alongside
// MessageTokenFingerprint, which deliberately excludes these two
// columns; without it, role-only or timestamp-only parser drift would
// never trigger the tier-2 row comparison. Role is sanitized to mirror
// the tier-2 compare in messageMetadataDiff; timestamp is compared raw
// there, so it stays raw here. timestamp is nullable, so a NULL is
// coalesced to the empty string to match both the in-memory twin (which
// emits "" for a zero-value Go timestamp) and the tier-2 read path
// (selectMessageCols coalesces the same way); without it a single
// imported NULL row would error here and abort the whole parse-diff run.
func (db *DB) MessageRoleTimeFingerprint(sessionID string) (string, error) {
	return db.MessageRoleTimeFingerprintWithTimestampNormalizer(
		sessionID, nil,
	)
}

// MessageRoleTimeFingerprintWithTimestampNormalizer returns the same
// fingerprint as MessageRoleTimeFingerprint after applying normalizeTimestamp
// to each timestamp value. It lets callers compare against stores that preserve
// a different timestamp representation while keeping the query and field
// ordering identical to the raw parse-diff fingerprint.
func (db *DB) MessageRoleTimeFingerprintWithTimestampNormalizer(
	sessionID string,
	normalizeTimestamp func(string) string,
) (string, error) {
	rows, err := db.getReader().Query(
		`SELECT ordinal, role, COALESCE(timestamp, '')
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
		var ordinal int
		var role, timestamp string
		if err := rows.Scan(&ordinal, &role, &timestamp); err != nil {
			return "", err
		}
		role = SanitizeUTF8(role)
		if normalizeTimestamp != nil {
			timestamp = normalizeTimestamp(timestamp)
		}
		fmt.Fprintf(&b, "%d|%d:%s|%d:%s;",
			ordinal, len(role), role, len(timestamp), timestamp)
	}
	return b.String(), rows.Err()
}

// MessageFlagsFingerprint returns an exact ordered fingerprint of the
// per-message flag and thinking columns that the token, role/time, and
// content fingerprints do not cover: is_system, has_thinking,
// has_tool_use, and a SHA-256 over the sanitized thinking_text. The
// parse-diff comparator uses it as a tier-1 fast path so a parser change
// confined to these columns still triggers the tier-2 row comparison.
// PG push uses it with a PostgreSQL-side twin to avoid skipping
// metadata-only rewrites.
func (db *DB) MessageFlagsFingerprint(sessionID string) (string, error) {
	rows, err := db.getReader().Query(
		`SELECT ordinal, is_system, has_thinking, has_tool_use,
			thinking_text
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
		var ordinal int
		var isSystem, hasThinking, hasToolUse bool
		var thinkingText string
		if err := rows.Scan(
			&ordinal, &isSystem, &hasThinking, &hasToolUse, &thinkingText,
		); err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte(SanitizeUTF8(thinkingText)))
		fmt.Fprintf(&b, "%d|%t|%t|%t|%x;",
			ordinal, isSystem, hasThinking, hasToolUse, sum)
	}
	return b.String(), rows.Err()
}

// ToolCallParseDiffFingerprint returns an exact ordered fingerprint of a
// session's parser-owned tool_call columns: tool_name, category,
// tool_use_id, a SHA-256 over input_json, skill_name,
// subagent_session_id, result_content_length, and file_path. The
// database-assigned id/message_id/session_id columns are excluded, and
// result_content (the possibly blocked body) is represented only by its
// length, mirroring the sizes-not-bodies rule the message content
// fingerprint follows. The
// sibling tool_result_events rows are not fingerprinted: the
// blocked-category config clears them wholesale, so comparing them would
// be config-sensitive; result_content_length already captures their
// summarized size.
// Rows are ordered by the owning message's ordinal then tool_calls.id
// (insertion order within a message). The parse-diff comparator uses it
// as a tier-1 fast path so tool-call drift that moves none of the
// message fingerprints still triggers the tier-2 comparison. Not used by
// the PG push fast-path.
func (db *DB) ToolCallParseDiffFingerprint(sessionID string) (string, error) {
	rows, err := db.getReader().Query(
		`SELECT m.ordinal, tc.tool_name, tc.category, tc.tool_use_id,
			tc.input_json, tc.skill_name, tc.subagent_session_id,
			tc.result_content_length, COALESCE(tc.file_path, '')
		 FROM tool_calls tc
		 JOIN messages m ON m.id = tc.message_id
		 WHERE tc.session_id = ?
		 ORDER BY m.ordinal ASC, tc.id ASC`,
		sessionID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var ordinal int
		var resultLen sql.NullInt64
		var toolName, category, filePath string
		var toolUseID, inputJSON, skillName, subagentSessionID sql.NullString
		if err := rows.Scan(
			&ordinal, &toolName, &category, &toolUseID,
			&inputJSON, &skillName, &subagentSessionID, &resultLen,
			&filePath,
		); err != nil {
			return "", err
		}
		toolName = SanitizeUTF8(toolName)
		category = SanitizeUTF8(category)
		tu := SanitizeUTF8(toolUseID.String)
		skill := SanitizeUTF8(skillName.String)
		sub := SanitizeUTF8(subagentSessionID.String)
		fp := SanitizeUTF8(filePath)
		sum := sha256.Sum256([]byte(SanitizeUTF8(inputJSON.String)))
		fmt.Fprintf(&b,
			"%d|%d:%s|%d:%s|%d:%s|%x|%d:%s|%d:%s|%d|%d:%s;",
			ordinal,
			len(toolName), toolName,
			len(category), category,
			len(tu), tu,
			sum,
			len(skill), skill,
			len(sub), sub,
			int(resultLen.Int64),
			len(fp), fp,
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

// ToolCallFingerprint returns an exact ordered fingerprint of persisted
// tool-call fields that can change without changing the message body or the
// tool-call count. Used by PG push fast-paths to avoid skipping parser
// changes that only affect tool metadata or inputs.
func (db *DB) ToolCallFingerprint(sessionID string) (string, error) {
	rows, err := db.getReader().Query(
		`SELECT m.ordinal, tc.tool_name, tc.category,
			COALESCE(tc.tool_use_id, ''), COALESCE(tc.input_json, ''),
			COALESCE(tc.skill_name, ''),
			COALESCE(tc.subagent_session_id, ''),
			COALESCE(tc.result_content_length, 0),
			COALESCE(tc.result_content, ''),
			COALESCE(tc.file_path, '')
		 FROM tool_calls tc
		 JOIN messages m ON m.id = tc.message_id
		 WHERE tc.session_id = ?
		 ORDER BY m.ordinal ASC, tc.id ASC`,
		sessionID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	lastMessageOrdinal := -1
	callIndex := 0
	for rows.Next() {
		var messageOrdinal, resultContentLength int
		var toolName, category, toolUseID, inputJSON string
		var skillName, subagentSessionID, resultContent, filePath string
		if err := rows.Scan(
			&messageOrdinal, &toolName, &category,
			&toolUseID, &inputJSON, &skillName, &subagentSessionID,
			&resultContentLength, &resultContent, &filePath,
		); err != nil {
			return "", err
		}
		if messageOrdinal == lastMessageOrdinal {
			callIndex++
		} else {
			lastMessageOrdinal = messageOrdinal
			callIndex = 0
		}
		toolName = SanitizeUTF8(toolName)
		category = SanitizeUTF8(category)
		toolUseID = SanitizeUTF8(toolUseID)
		inputJSON = SanitizeUTF8(inputJSON)
		skillName = SanitizeUTF8(skillName)
		subagentSessionID = SanitizeUTF8(subagentSessionID)
		resultContent = SanitizeUTF8(resultContent)
		filePath = SanitizeUTF8(filePath)
		fmt.Fprintf(&b,
			"%d|%d|%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d|%d:%s|%d:%s;",
			messageOrdinal, callIndex,
			len(toolName), toolName,
			len(category), category,
			len(toolUseID), toolUseID,
			len(inputJSON), inputJSON,
			len(skillName), skillName,
			len(subagentSessionID), subagentSessionID,
			resultContentLength,
			len(resultContent), resultContent,
			len(filePath), filePath,
		)
	}
	return b.String(), rows.Err()
}

// ToolResultEventFingerprintWithTimestampNormalizer returns an exact ordered
// fingerprint of persisted tool-result event fields. The optional timestamp
// normalizer lets push backends compare SQLite text timestamps with target
// stores that round or reformat timestamp values on insert.
func (db *DB) ToolResultEventFingerprintWithTimestampNormalizer(
	sessionID string,
	normalizeTimestamp func(string) string,
) (string, error) {
	rows, err := db.getReader().Query(
		`SELECT tool_call_message_ordinal, call_index, event_index,
			COALESCE(tool_use_id, ''), COALESCE(agent_id, ''),
			COALESCE(subagent_session_id, ''), source, status,
			content, content_length, COALESCE(timestamp, '')
		 FROM tool_result_events
		 WHERE session_id = ?
		 ORDER BY tool_call_message_ordinal ASC, call_index ASC, event_index ASC`,
		sessionID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var messageOrdinal, callIndex, eventIndex, contentLength int
		var toolUseID, agentID, subagentSessionID string
		var source, status, content, timestamp string
		if err := rows.Scan(
			&messageOrdinal, &callIndex, &eventIndex,
			&toolUseID, &agentID, &subagentSessionID,
			&source, &status, &content, &contentLength, &timestamp,
		); err != nil {
			return "", err
		}
		if normalizeTimestamp != nil {
			timestamp = normalizeTimestamp(timestamp)
		}
		toolUseID = SanitizeUTF8(toolUseID)
		agentID = SanitizeUTF8(agentID)
		subagentSessionID = SanitizeUTF8(subagentSessionID)
		source = SanitizeUTF8(source)
		status = SanitizeUTF8(status)
		content = SanitizeUTF8(content)
		contentSum := sha256.Sum256([]byte(content))
		fmt.Fprintf(
			&b,
			"%d|%d|%d|%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d|%x|%d:%s;",
			messageOrdinal, callIndex, eventIndex,
			len(toolUseID), toolUseID,
			len(agentID), agentID,
			len(subagentSessionID), subagentSessionID,
			len(source), source,
			len(status), status,
			contentLength,
			contentSum,
			len(timestamp), timestamp,
		)
	}
	return b.String(), rows.Err()
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
// the parallel IDs slice from insertMessagesTx. CallIndex is derived
// from each call's position within its message, so callers (sync,
// importer) need not prepopulate it. Panics if len(ids) != len(msgs)
// since that indicates a caller bug.
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
		for callIdx, tc := range m.ToolCalls {
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
				FilePath:            tc.FilePath,
				CallIndex:           callIdx,
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
