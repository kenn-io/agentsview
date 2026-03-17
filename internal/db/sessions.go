package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidCursor is returned when a cursor cannot be decoded or verified.
var ErrInvalidCursor = errors.New("invalid cursor")

// ErrSessionExcluded is returned by UpsertSession when the
// session was permanently deleted by the user. Callers should
// skip any follow-up writes (messages, tool_calls) for this session.
var ErrSessionExcluded = errors.New("session excluded")

// sessionBaseCols is the column list for standard session queries
// (list, get). Keep in sync with scanSessionRow.
const sessionBaseCols = `id, project, machine, agent,
	first_message, display_name, started_at, ended_at,
	message_count, user_message_count,
	parent_session_id, relationship_type,
	total_output_tokens, peak_context_tokens,
	deleted_at, created_at`

// sessionPruneCols extends sessionBaseCols with file metadata
// needed by FindPruneCandidates.
const sessionPruneCols = `id, project, machine, agent,
	first_message, display_name, started_at, ended_at,
	message_count, user_message_count,
	parent_session_id, relationship_type,
	total_output_tokens, peak_context_tokens,
	deleted_at, file_path, file_size, created_at`

// sessionFullCols includes all columns for a complete session record.
const sessionFullCols = `id, project, machine, agent,
	first_message, display_name, started_at, ended_at,
	message_count, user_message_count,
	parent_session_id, relationship_type,
	total_output_tokens, peak_context_tokens,
	deleted_at, file_path, file_size, file_mtime,
	file_hash, local_modified_at, created_at`

const (
	// DefaultSessionLimit is the default number of sessions returned.
	DefaultSessionLimit = 200
	// MaxSessionLimit is the maximum number of sessions returned.
	MaxSessionLimit = 500
)

// rowScanner is satisfied by both *sql.Row and *sql.Rows,
// allowing a single scan helper for both.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanSessionRow scans sessionBaseCols into a Session.
func scanSessionRow(rs rowScanner) (Session, error) {
	var s Session
	err := rs.Scan(
		&s.ID, &s.Project, &s.Machine, &s.Agent,
		&s.FirstMessage, &s.DisplayName, &s.StartedAt, &s.EndedAt,
		&s.MessageCount, &s.UserMessageCount,
		&s.ParentSessionID, &s.RelationshipType,
		&s.TotalOutputTokens, &s.PeakContextTokens,
		&s.DeletedAt, &s.CreatedAt,
	)
	return s, err
}

// Session represents a row in the sessions table.
type Session struct {
	ID                string  `json:"id"`
	Project           string  `json:"project"`
	Machine           string  `json:"machine"`
	Agent             string  `json:"agent"`
	FirstMessage      *string `json:"first_message"`
	DisplayName       *string `json:"display_name,omitempty"`
	StartedAt         *string `json:"started_at"`
	EndedAt           *string `json:"ended_at"`
	MessageCount      int     `json:"message_count"`
	UserMessageCount  int     `json:"user_message_count"`
	ParentSessionID   *string `json:"parent_session_id,omitempty"`
	RelationshipType  string  `json:"relationship_type,omitempty"`
	TotalOutputTokens int     `json:"total_output_tokens"`
	PeakContextTokens int     `json:"peak_context_tokens"`
	DeletedAt         *string `json:"deleted_at,omitempty"`
	FilePath          *string `json:"file_path,omitempty"`
	FileSize          *int64  `json:"file_size,omitempty"`
	FileMtime         *int64  `json:"file_mtime,omitempty"`
	FileHash          *string `json:"file_hash,omitempty"`
	LocalModifiedAt   *string `json:"local_modified_at,omitempty"`
	CreatedAt         string  `json:"created_at"`
}

// SessionCursor is the opaque pagination token.
type SessionCursor struct {
	EndedAt string `json:"e"`
	ID      string `json:"i"`
	Total   int    `json:"t,omitempty"`
}

// EncodeCursor returns a base64-encoded cursor string.
func (db *DB) EncodeCursor(endedAt, id string, total ...int) string {
	t := 0
	if len(total) > 0 {
		t = total[0]
	}
	c := SessionCursor{EndedAt: endedAt, ID: id, Total: t}
	data, _ := json.Marshal(c)

	db.cursorMu.RLock()
	mac := hmac.New(sha256.New, db.cursorSecret)
	db.cursorMu.RUnlock()

	mac.Write(data)
	sig := mac.Sum(nil)

	return base64.RawURLEncoding.EncodeToString(data) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// DecodeCursor parses a base64-encoded cursor string.
func (db *DB) DecodeCursor(s string) (SessionCursor, error) {
	parts := strings.Split(s, ".")
	if len(parts) == 1 {
		// Legacy cursor (unsigned). Trust nothing about the Total.
		data, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			return SessionCursor{}, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
		}
		var c SessionCursor
		if err := json.Unmarshal(data, &c); err != nil {
			return SessionCursor{}, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
		}
		c.Total = 0 // Force re-computation
		return c, nil
	} else if len(parts) != 2 {
		return SessionCursor{}, fmt.Errorf("%w: invalid format", ErrInvalidCursor)
	}

	payload := parts[0]
	sigStr := parts[1]

	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return SessionCursor{}, fmt.Errorf("%w: invalid payload: %v", ErrInvalidCursor, err)
	}

	sig, err := base64.RawURLEncoding.DecodeString(sigStr)
	if err != nil {
		return SessionCursor{}, fmt.Errorf("%w: invalid signature encoding: %v", ErrInvalidCursor, err)
	}

	db.cursorMu.RLock()
	mac := hmac.New(sha256.New, db.cursorSecret)
	db.cursorMu.RUnlock()

	mac.Write(data)
	expectedSig := mac.Sum(nil)

	if !hmac.Equal(sig, expectedSig) {
		return SessionCursor{}, fmt.Errorf("%w: signature mismatch", ErrInvalidCursor)
	}

	var c SessionCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return SessionCursor{}, fmt.Errorf("%w: invalid json: %v", ErrInvalidCursor, err)
	}
	return c, nil
}

// SessionFilter specifies how to query sessions.
type SessionFilter struct {
	Project         string
	ExcludeProject  string // exclude sessions with this project name
	Machine         string
	Agent           string
	Date            string // exact date YYYY-MM-DD
	DateFrom        string // range start (inclusive)
	DateTo          string // range end (inclusive)
	ActiveSince     string // ISO-8601 timestamp; filters on most recent activity
	MinMessages     int    // message_count >= N (0 = no filter)
	MaxMessages     int    // message_count <= N (0 = no filter)
	MinUserMessages int    // user_message_count >= N (0 = no filter)
	ExcludeOneShot  bool   // exclude sessions with user_message_count <= 1
	IncludeChildren bool   // include subagent sessions (for sidebar grouping)
	Cursor          string // opaque cursor from previous page
	Limit           int
}

// SessionPage is a page of session results.
type SessionPage struct {
	Sessions   []Session `json:"sessions"`
	NextCursor string    `json:"next_cursor,omitempty"`
	Total      int       `json:"total"`
}

// buildSessionFilter returns a WHERE clause and args for the
// non-cursor predicates in SessionFilter.
func buildSessionFilter(f SessionFilter) (string, []any) {
	// Base predicates apply to every row.
	basePreds := []string{
		"message_count > 0",
		"deleted_at IS NULL",
	}
	if !f.IncludeChildren {
		basePreds = append(basePreds,
			"relationship_type NOT IN ('subagent', 'fork')")
	}

	// Filter predicates narrow results based on user criteria.
	// When IncludeChildren is true these only apply to root
	// sessions; children are included via a subquery on their
	// parent instead.
	var filterPreds []string
	var filterArgs []any

	if f.Project != "" {
		filterPreds = append(filterPreds, "project = ?")
		filterArgs = append(filterArgs, f.Project)
	}
	if f.ExcludeProject != "" {
		filterPreds = append(filterPreds, "project != ?")
		filterArgs = append(filterArgs, f.ExcludeProject)
	}
	if f.Machine != "" {
		filterPreds = append(filterPreds, "machine = ?")
		filterArgs = append(filterArgs, f.Machine)
	}
	if f.Agent != "" {
		agents := strings.Split(f.Agent, ",")
		if len(agents) == 1 {
			filterPreds = append(filterPreds, "agent = ?")
			filterArgs = append(filterArgs, agents[0])
		} else {
			placeholders := make(
				[]string, len(agents),
			)
			for i, a := range agents {
				placeholders[i] = "?"
				filterArgs = append(filterArgs, a)
			}
			filterPreds = append(filterPreds,
				"agent IN ("+
					strings.Join(placeholders, ",")+
					")")
		}
	}
	if f.Date != "" {
		filterPreds = append(filterPreds,
			"date(COALESCE(NULLIF(started_at, ''), created_at)) = ?")
		filterArgs = append(filterArgs, f.Date)
	}
	if f.DateFrom != "" {
		filterPreds = append(filterPreds,
			"date(COALESCE(NULLIF(started_at, ''), created_at)) >= ?")
		filterArgs = append(filterArgs, f.DateFrom)
	}
	if f.DateTo != "" {
		filterPreds = append(filterPreds,
			"date(COALESCE(NULLIF(started_at, ''), created_at)) <= ?")
		filterArgs = append(filterArgs, f.DateTo)
	}
	if f.ActiveSince != "" {
		filterPreds = append(filterPreds,
			"COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, ''), created_at) >= ?")
		filterArgs = append(filterArgs, f.ActiveSince)
	}
	if f.MinMessages > 0 {
		filterPreds = append(filterPreds, "message_count >= ?")
		filterArgs = append(filterArgs, f.MinMessages)
	}
	if f.MaxMessages > 0 {
		filterPreds = append(filterPreds, "message_count <= ?")
		filterArgs = append(filterArgs, f.MaxMessages)
	}
	if f.MinUserMessages > 0 {
		filterPreds = append(filterPreds, "user_message_count >= ?")
		filterArgs = append(filterArgs, f.MinUserMessages)
	}

	// ExcludeOneShot is handled separately from filterPreds
	// when IncludeChildren is true. Children (subagents, forks)
	// are almost always one-shot by nature and must not be
	// excluded. The one-shot filter applies only to root
	// sessions that match the filter directly.
	oneShotPred := ""
	if f.ExcludeOneShot {
		if f.IncludeChildren {
			oneShotPred = "user_message_count > 1"
		} else {
			filterPreds = append(filterPreds,
				"user_message_count > 1")
		}
	}

	// Simple case: no IncludeChildren or no user filters.
	hasFilters := len(filterPreds) > 0 || oneShotPred != ""
	if !f.IncludeChildren || !hasFilters {
		allPreds := append(basePreds, filterPreds...)
		return strings.Join(allPreds, " AND "), filterArgs
	}

	// IncludeChildren + filters: match the filter directly,
	// or be a child of a session that matches the filter.
	// This scopes children to their parent's filter match
	// instead of including all children in the database.
	baseWhere := strings.Join(basePreds, " AND ")

	// Root match: must pass all filter predicates + one-shot.
	rootMatchParts := append([]string{}, filterPreds...)
	if oneShotPred != "" {
		rootMatchParts = append(rootMatchParts, oneShotPred)
	}
	rootMatch := strings.Join(rootMatchParts, " AND ")

	// Subquery for parent inclusion: same criteria as root
	// match so only children of qualifying parents appear.
	subqWhere := "message_count > 0 AND deleted_at IS NULL"
	if rootMatch != "" {
		subqWhere += " AND " + rootMatch
	}

	where := baseWhere + " AND (" + rootMatch +
		" OR parent_session_id IN" +
		" (SELECT id FROM sessions WHERE " + subqWhere + "))"

	// Args appear twice: outer root match + subquery.
	allArgs := make([]any, 0, len(filterArgs)*2)
	allArgs = append(allArgs, filterArgs...)
	allArgs = append(allArgs, filterArgs...)
	return where, allArgs
}

// ListSessions returns a cursor-paginated list of sessions.
func (db *DB) ListSessions(
	ctx context.Context, f SessionFilter,
) (SessionPage, error) {
	if f.Limit <= 0 || f.Limit > MaxSessionLimit {
		f.Limit = DefaultSessionLimit
	}

	where, args := buildSessionFilter(f)

	var total int
	var cur SessionCursor
	if f.Cursor != "" {
		var err error
		cur, err = db.DecodeCursor(f.Cursor)
		if err != nil {
			return SessionPage{}, err
		}
		total = cur.Total
	}
	// Total count applies filters but not cursor. To avoid
	// re-counting on every pagination request, newer cursors carry
	// the first-page total and we reuse it here.
	if total <= 0 {
		countQuery := "SELECT COUNT(*) FROM sessions WHERE " + where
		if err := db.getReader().QueryRowContext(
			ctx, countQuery, args...,
		).Scan(&total); err != nil {
			return SessionPage{},
				fmt.Errorf("counting sessions: %w", err)
		}
	}

	// Paginated results
	cursorArgs := append([]any{}, args...)
	cursorWhere := where
	if f.Cursor != "" {
		cursorWhere += ` AND (
				COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, ''), created_at), id
			) < (?, ?)`
		cursorArgs = append(cursorArgs, cur.EndedAt, cur.ID)
	}

	query := "SELECT " + sessionBaseCols +
		" FROM sessions WHERE " + cursorWhere + `
		ORDER BY COALESCE(
			NULLIF(ended_at, ''),
			NULLIF(started_at, ''),
			created_at
		) DESC, id DESC
		LIMIT ?`
	cursorArgs = append(cursorArgs, f.Limit+1)

	rows, err := db.getReader().QueryContext(ctx, query, cursorArgs...)
	if err != nil {
		return SessionPage{},
			fmt.Errorf("querying sessions: %w", err)
	}
	defer rows.Close()

	sessions, err := scanSessionRows(rows)
	if err != nil {
		return SessionPage{}, err
	}

	page := SessionPage{Sessions: sessions, Total: total}
	if len(sessions) > f.Limit {
		page.Sessions = sessions[:f.Limit]
		last := page.Sessions[f.Limit-1]
		ea := last.CreatedAt
		if last.StartedAt != nil && *last.StartedAt != "" {
			ea = *last.StartedAt
		}
		if last.EndedAt != nil && *last.EndedAt != "" {
			ea = *last.EndedAt
		}
		page.NextCursor = db.EncodeCursor(ea, last.ID, total)
	}

	return page, nil
}

// GetSession returns a single session by ID, excluding
// soft-deleted (trashed) sessions.
func (db *DB) GetSession(
	ctx context.Context, id string,
) (*Session, error) {
	row := db.getReader().QueryRowContext(
		ctx,
		"SELECT "+sessionBaseCols+" FROM sessions WHERE id = ? AND deleted_at IS NULL",
		id,
	)

	s, err := scanSessionRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting session %s: %w", id, err)
	}
	return &s, nil
}

// GetSessionFull returns a single session by ID with all file metadata.
func (db *DB) GetSessionFull(
	ctx context.Context, id string,
) (*Session, error) {
	row := db.getReader().QueryRowContext(
		ctx,
		"SELECT "+sessionFullCols+" FROM sessions WHERE id = ?",
		id,
	)

	var s Session
	err := row.Scan(
		&s.ID, &s.Project, &s.Machine, &s.Agent,
		&s.FirstMessage, &s.DisplayName, &s.StartedAt, &s.EndedAt,
		&s.MessageCount, &s.UserMessageCount,
		&s.ParentSessionID, &s.RelationshipType,
		&s.TotalOutputTokens, &s.PeakContextTokens,
		&s.DeletedAt, &s.FilePath, &s.FileSize,
		&s.FileMtime, &s.FileHash, &s.LocalModifiedAt, &s.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting session full %s: %w", id, err)
	}
	return &s, nil
}

// IsSessionExcluded returns true if the session ID was
// permanently deleted by the user.
func (db *DB) IsSessionExcluded(id string) bool {
	var n int
	_ = db.getReader().QueryRow(
		"SELECT 1 FROM excluded_sessions WHERE id = ?", id,
	).Scan(&n)
	return n == 1
}

// PurgeExcludedSessions removes any session rows whose IDs
// appear in excluded_sessions. Used after a resync to clean
// up sessions that were synced before their exclusion was
// recorded.
func (db *DB) PurgeExcludedSessions() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		"DELETE FROM sessions WHERE id IN (SELECT id FROM excluded_sessions)",
	)
	return err
}

// UpsertSession inserts or updates a session.
// Sessions that were permanently deleted (in excluded_sessions)
// are silently skipped.
func (db *DB) UpsertSession(s Session) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Check exclusion under the write lock to avoid a race with
	// concurrent DeleteSession/EmptyTrash.
	var excluded int
	_ = db.getWriter().QueryRow(
		"SELECT 1 FROM excluded_sessions WHERE id = ?", s.ID,
	).Scan(&excluded)
	if excluded == 1 {
		return ErrSessionExcluded
	}

	_, err := db.getWriter().Exec(`
		INSERT INTO sessions (
			id, project, machine, agent, first_message,
			started_at, ended_at, message_count,
			user_message_count, parent_session_id,
			relationship_type,
			total_output_tokens, peak_context_tokens,
			file_path, file_size, file_mtime, file_hash
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project = excluded.project,
			machine = excluded.machine,
			agent = excluded.agent,
			first_message = excluded.first_message,
			started_at = excluded.started_at,
			ended_at = excluded.ended_at,
			message_count = excluded.message_count,
			user_message_count = excluded.user_message_count,
			parent_session_id = excluded.parent_session_id,
			relationship_type = excluded.relationship_type,
			total_output_tokens = excluded.total_output_tokens,
			peak_context_tokens = excluded.peak_context_tokens,
			file_path = excluded.file_path,
			file_size = excluded.file_size,
			file_mtime = excluded.file_mtime,
			file_hash = excluded.file_hash`,
		s.ID, s.Project, s.Machine, s.Agent, s.FirstMessage,
		s.StartedAt, s.EndedAt, s.MessageCount,
		s.UserMessageCount, s.ParentSessionID,
		s.RelationshipType,
		s.TotalOutputTokens, s.PeakContextTokens,
		s.FilePath, s.FileSize, s.FileMtime, s.FileHash)
	if err != nil {
		return fmt.Errorf("upserting session %s: %w", s.ID, err)
	}
	return nil
}

// GetChildSessions returns sessions whose parent_session_id
// matches the given parentID, ordered by started_at ascending.
func (db *DB) GetChildSessions(
	ctx context.Context, parentID string,
) ([]Session, error) {
	query := "SELECT " + sessionBaseCols +
		" FROM sessions WHERE parent_session_id = ?" +
		" ORDER BY started_at"
	rows, err := db.getReader().QueryContext(ctx, query, parentID)
	if err != nil {
		return nil, fmt.Errorf(
			"querying child sessions for %s: %w", parentID, err,
		)
	}
	defer rows.Close()

	return scanSessionRows(rows)
}

// LinkSubagentSessions sets parent_session_id and
// relationship_type on sessions that are referenced by
// tool_calls.subagent_session_id. Updates sessions that either
// have no parent yet or have a non-subagent relationship (e.g.
// a Zencoder session classified as "continuation" from header
// parentId that is actually a spawned subagent).
func (db *DB) LinkSubagentSessions() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.getWriter().Exec(`
		UPDATE sessions
		SET parent_session_id = (
			SELECT tc.session_id
			FROM tool_calls tc
			WHERE tc.subagent_session_id = sessions.id
			LIMIT 1
		),
		relationship_type = 'subagent'
		WHERE relationship_type != 'subagent'
		AND EXISTS (
			SELECT 1 FROM tool_calls tc
			WHERE tc.subagent_session_id = sessions.id
		)`)
	if err != nil {
		return fmt.Errorf("linking subagent sessions: %w", err)
	}
	return nil
}

// GetSessionFileInfo returns file_size and file_mtime for a
// session. Used for fast skip checks during sync.
func (db *DB) GetSessionFileInfo(
	id string,
) (size int64, mtime int64, ok bool) {
	var s, m sql.NullInt64
	err := db.getReader().QueryRow(
		"SELECT file_size, file_mtime FROM sessions WHERE id = ?",
		id,
	).Scan(&s, &m)
	if err != nil {
		return 0, 0, false
	}
	return s.Int64, m.Int64, true
}

// GetSessionFilePath returns the stored file_path for a session,
// or empty string if not found or NULL.
func (db *DB) GetSessionFilePath(id string) string {
	var fp sql.NullString
	err := db.getReader().QueryRow(
		"SELECT file_path FROM sessions WHERE id = ?", id,
	).Scan(&fp)
	if err != nil || !fp.Valid {
		return ""
	}
	return fp.String
}

// GetSessionMessageCount returns the message_count for a
// session. Returns (0, false) when the session does not exist.
func (db *DB) GetSessionMessageCount(
	id string,
) (count int, ok bool) {
	err := db.getReader().QueryRow(
		"SELECT message_count FROM sessions WHERE id = ?",
		id,
	).Scan(&count)
	if err != nil {
		return 0, false
	}
	return count, true
}

// GetSessionVersion returns the message count and file mtime
// for change detection in SSE watchers.
func (db *DB) GetSessionVersion(
	id string,
) (count int, fileMtime int64, ok bool) {
	err := db.getReader().QueryRow(
		"SELECT message_count, COALESCE(file_mtime, 0)"+
			" FROM sessions WHERE id = ?",
		id,
	).Scan(&count, &fileMtime)
	if err != nil {
		return 0, 0, false
	}
	return count, fileMtime, true
}

// IncrementalInfo holds the data needed for incremental
// re-parsing of an append-only session file.
type IncrementalInfo struct {
	ID                string
	FileSize          int64
	MsgCount          int
	UserMsgCount      int
	TotalOutputTokens int
	PeakContextTokens int
}

// GetSessionForIncremental returns session state needed for
// incremental parsing, looked up by file_path. Returns false
// when the path is unknown or maps to multiple sessions (e.g.
// Claude DAG forks), since incremental parsing cannot update
// multiple sessions from a single append.
func (db *DB) GetSessionForIncremental(
	path string,
) (*IncrementalInfo, bool) {
	// Bail out if the file maps to more than one session
	// (Claude fork/subagent splits).
	var count int
	err := db.getReader().QueryRow(
		`SELECT COUNT(*) FROM sessions
		 WHERE file_path = ?`, path,
	).Scan(&count)
	if err != nil || count != 1 {
		return nil, false
	}

	var info IncrementalInfo
	var fs sql.NullInt64
	err = db.getReader().QueryRow(
		`SELECT id, file_size, message_count,
			user_message_count,
			total_output_tokens, peak_context_tokens
		 FROM sessions WHERE file_path = ?`,
		path,
	).Scan(
		&info.ID, &fs, &info.MsgCount, &info.UserMsgCount,
		&info.TotalOutputTokens, &info.PeakContextTokens,
	)
	if err != nil {
		return nil, false
	}
	if fs.Valid {
		info.FileSize = fs.Int64
	}
	return &info, true
}

// UpdateSessionIncremental updates only the fields that change
// during an incremental append: ended_at, message_count,
// user_message_count, file_size, file_mtime, and token
// aggregates. All values are absolute (not deltas) so the
// update is idempotent on retry.
func (db *DB) UpdateSessionIncremental(
	id string,
	endedAt *string,
	msgCount, userMsgCount int,
	fileSize, fileMtime int64,
	totalOutputTokens, peakContextTokens int,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.getWriter().Exec(`
		UPDATE sessions SET
			ended_at = COALESCE(?, ended_at),
			message_count = ?,
			user_message_count = ?,
			file_size = ?,
			file_mtime = ?,
			total_output_tokens = ?,
			peak_context_tokens = ?
		WHERE id = ?`,
		endedAt, msgCount, userMsgCount,
		fileSize, fileMtime,
		totalOutputTokens, peakContextTokens, id,
	)
	if err != nil {
		return fmt.Errorf(
			"incremental update session %s: %w", id, err,
		)
	}
	return nil
}

// GetFileInfoByPath returns file_size and file_mtime for a
// session identified by file_path. Used for codex/gemini files
// where the session ID requires parsing.
func (db *DB) GetFileInfoByPath(
	path string,
) (size int64, mtime int64, ok bool) {
	var s, m sql.NullInt64
	err := db.getReader().QueryRow(
		"SELECT file_size, file_mtime FROM sessions"+
			" WHERE file_path = ?"+
			" ORDER BY file_mtime DESC LIMIT 1",
		path,
	).Scan(&s, &m)
	if err != nil {
		return 0, 0, false
	}
	return s.Int64, m.Int64, true
}

// ResetAllMtimes zeroes file_mtime for every session, forcing
// the next sync to re-process all files regardless of whether
// their size+mtime matches what was previously stored.
func (db *DB) ResetAllMtimes() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		"UPDATE sessions SET file_mtime = 0",
	)
	if err != nil {
		return fmt.Errorf("resetting mtimes: %w", err)
	}
	return nil
}

// DeleteSession removes a session and its messages (cascading).
// The session ID is recorded in excluded_sessions so the sync
// engine does not re-import it from disk. Both operations run
// in a single transaction. The exclusion is only written when
// a session row was actually deleted, preventing ghost entries
// for non-existent IDs.
func (db *DB) DeleteSession(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	w := db.getWriter()

	tx, err := w.Begin()
	if err != nil {
		return fmt.Errorf("begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(
		"DELETE FROM sessions WHERE id = ?", id,
	)
	if err != nil {
		return fmt.Errorf("deleting session %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO excluded_sessions (id) VALUES (?)",
			id,
		); err != nil {
			return fmt.Errorf("excluding session %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// DeleteSessionIfTrashed atomically deletes a session only if it
// is currently in the trash (deleted_at IS NOT NULL). Returns the
// number of rows affected. This avoids a TOCTOU race between
// checking deleted_at and performing the delete.
func (db *DB) DeleteSessionIfTrashed(id string) (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	w := db.getWriter()

	tx, err := w.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin delete-if-trashed tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Only delete if the session is currently trashed.
	res, err := tx.Exec(
		"DELETE FROM sessions WHERE id = ? AND deleted_at IS NOT NULL",
		id,
	)
	if err != nil {
		return 0, fmt.Errorf("deleting trashed session %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, nil
	}

	// Record in exclusion list so sync doesn't re-import.
	if _, err := tx.Exec(
		"INSERT OR IGNORE INTO excluded_sessions (id) VALUES (?)", id,
	); err != nil {
		return 0, fmt.Errorf("excluding session %s: %w", id, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit delete-if-trashed: %w", err)
	}
	return n, nil
}

// GetProjects returns project names with session counts.
func (db *DB) GetProjects(
	ctx context.Context, excludeOneShot bool,
) ([]ProjectInfo, error) {
	q := `SELECT project, COUNT(*) as session_count
		FROM sessions
		WHERE message_count > 0
		  AND relationship_type NOT IN ('subagent', 'fork')
		  AND deleted_at IS NULL`
	if excludeOneShot {
		q += " AND user_message_count > 1"
	}
	q += " GROUP BY project ORDER BY project"
	rows, err := db.getReader().QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("querying projects: %w", err)
	}
	defer rows.Close()

	var projects []ProjectInfo
	for rows.Next() {
		var p ProjectInfo
		if err := rows.Scan(&p.Name, &p.SessionCount); err != nil {
			return nil, fmt.Errorf("scanning project: %w", err)
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// ProjectInfo holds a project name and its session count.
type ProjectInfo struct {
	Name         string `json:"name"`
	SessionCount int    `json:"session_count"`
}

// GetAgents returns distinct agent names with session counts.
func (db *DB) GetAgents(
	ctx context.Context, excludeOneShot bool,
) ([]AgentInfo, error) {
	q := `SELECT agent, COUNT(*) as session_count
		FROM sessions
		WHERE message_count > 0 AND agent <> ''
		  AND deleted_at IS NULL
		  AND relationship_type NOT IN ('subagent', 'fork')`
	if excludeOneShot {
		q += " AND user_message_count > 1"
	}
	q += " GROUP BY agent ORDER BY agent"
	rows, err := db.getReader().QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("querying agents: %w", err)
	}
	defer rows.Close()

	agents := []AgentInfo{}
	for rows.Next() {
		var a AgentInfo
		if err := rows.Scan(&a.Name, &a.SessionCount); err != nil {
			return nil, fmt.Errorf("scanning agent: %w", err)
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// AgentInfo holds an agent name and its session count.
type AgentInfo struct {
	Name         string `json:"name"`
	SessionCount int    `json:"session_count"`
}

// GetMachines returns distinct machine names.
func (db *DB) GetMachines(
	ctx context.Context, excludeOneShot bool,
) ([]string, error) {
	q := "SELECT DISTINCT machine FROM sessions WHERE deleted_at IS NULL"
	if excludeOneShot {
		q += " AND user_message_count > 1"
	}
	q += " ORDER BY machine"
	rows, err := db.getReader().QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	machines := []string{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		machines = append(machines, m)
	}
	return machines, rows.Err()
}

// scanSessionRows iterates rows and scans each using
// scanSessionRow.
func scanSessionRows(rows *sql.Rows) ([]Session, error) {
	sessions := []Session{}
	for rows.Next() {
		s, err := scanSessionRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning session: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// PruneFilter defines criteria for finding sessions to prune.
// Filters combine with AND. At least one must be set.
type PruneFilter struct {
	Project      string // substring match (LIKE '%x%')
	MaxMessages  *int   // user messages <= N (nil = no filter)
	Before       string // ended_at < date (YYYY-MM-DD)
	FirstMessage string // first_message LIKE 'prefix%'
}

// HasFilters reports whether at least one filter is set.
func (f PruneFilter) HasFilters() bool {
	return f.Project != "" ||
		f.MaxMessages != nil ||
		f.Before != "" ||
		f.FirstMessage != ""
}

// escapeLike escapes SQL LIKE wildcard characters so user
// input is matched literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`, `%`, `\%`, `_`, `\_`,
	)
	return r.Replace(s)
}

// FindPruneCandidates returns sessions matching all filter
// criteria. Returns full Session rows including file metadata.
func (db *DB) FindPruneCandidates(
	f PruneFilter,
) ([]Session, error) {
	if !f.HasFilters() {
		return nil, fmt.Errorf("at least one filter is required")
	}

	where := "deleted_at IS NULL"
	args := []any{}

	if f.Project != "" {
		where += ` AND project LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(f.Project)+"%")
	}
	if f.MaxMessages != nil {
		where += ` AND (SELECT COUNT(*) FROM messages
			WHERE messages.session_id = sessions.id
			AND messages.role = 'user'
			AND messages.is_system = 0) <= ?`
		args = append(args, *f.MaxMessages)
	}
	if f.Before != "" {
		where += " AND COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, ''), created_at) < ?"
		args = append(args, f.Before)
	}
	if f.FirstMessage != "" {
		where += ` AND first_message LIKE ? ESCAPE '\'`
		args = append(args, escapeLike(f.FirstMessage)+"%")
	}

	// Exclude sessions that are parents of other sessions.
	where += ` AND NOT EXISTS (
		SELECT 1 FROM sessions AS child
		WHERE child.parent_session_id = sessions.id)`

	query := "SELECT " + sessionPruneCols +
		" FROM sessions WHERE " + where + `
		ORDER BY COALESCE(
			NULLIF(ended_at, ''),
			NULLIF(started_at, ''),
			created_at
		) DESC`

	rows, err := db.getReader().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("finding prune candidates: %w", err)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var s Session
		err := rows.Scan(
			&s.ID, &s.Project, &s.Machine, &s.Agent,
			&s.FirstMessage, &s.DisplayName, &s.StartedAt, &s.EndedAt,
			&s.MessageCount, &s.UserMessageCount,
			&s.ParentSessionID, &s.RelationshipType,
			&s.TotalOutputTokens, &s.PeakContextTokens,
			&s.DeletedAt, &s.FilePath, &s.FileSize, &s.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning prune candidate: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// SoftDeleteSession marks a session as deleted by setting deleted_at.
func (db *DB) SoftDeleteSession(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		`UPDATE sessions
		 SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'),
		     local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ? AND deleted_at IS NULL`, id,
	)
	return err
}

// RestoreSession clears deleted_at, making the session visible again.
// Returns the number of rows affected (0 if session doesn't exist
// or is not in trash).
func (db *DB) RestoreSession(id string) (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	res, err := db.getWriter().Exec(
		`UPDATE sessions
		 SET deleted_at = NULL,
		     local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ? AND deleted_at IS NOT NULL`,
		id,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RenameSession sets or clears the display_name for a session.
// Pass nil to clear a custom name (reverts to first_message).
func (db *DB) RenameSession(id string, displayName *string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		`UPDATE sessions
		 SET display_name = ?,
		     local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ? AND deleted_at IS NULL`,
		displayName, id,
	)
	return err
}

// ListTrashedSessions returns sessions that have been soft-deleted.
func (db *DB) ListTrashedSessions(
	ctx context.Context,
) ([]Session, error) {
	query := "SELECT " + sessionBaseCols +
		" FROM sessions WHERE deleted_at IS NOT NULL" +
		" ORDER BY deleted_at DESC LIMIT 500"
	rows, err := db.getReader().QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("querying trashed sessions: %w", err)
	}
	defer rows.Close()
	return scanSessionRows(rows)
}

// EmptyTrash permanently deletes all soft-deleted sessions.
// Session IDs are recorded in excluded_sessions so the sync
// engine does not re-import them. Both operations run in a
// single transaction to prevent ghost exclusions when the
// delete fails. Returns the count of deleted rows.
func (db *DB) EmptyTrash() (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	w := db.getWriter()

	tx, err := w.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin empty-trash tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Record all trashed session IDs before deleting.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO excluded_sessions (id)
		 SELECT id FROM sessions WHERE deleted_at IS NOT NULL`,
	); err != nil {
		return 0, fmt.Errorf("excluding trashed sessions: %w", err)
	}
	res, err := tx.Exec(
		"DELETE FROM sessions WHERE deleted_at IS NOT NULL",
	)
	if err != nil {
		return 0, fmt.Errorf("emptying trash: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit empty-trash: %w", err)
	}
	return int(n), nil
}

// DeleteSessions removes multiple sessions by ID in a single
// transaction. Batches operations in groups of 500 to stay
// under SQLite variable limits. Deleted IDs are recorded in
// excluded_sessions so the sync engine does not re-import
// them. Returns count of deleted rows.
func (db *DB) DeleteSessions(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	total := 0
	const batchSize = 500
	for i := 0; i < len(ids); i += batchSize {
		end := min(i+batchSize, len(ids))
		batch := ids[i:end]

		args := make([]any, len(batch))
		for j, id := range batch {
			args[j] = id
		}
		placeholders := strings.Repeat(",?", len(batch))[1:]

		// Exclude only IDs that exist before we delete them.
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO excluded_sessions (id) "+
				"SELECT id FROM sessions WHERE id IN ("+placeholders+")",
			args...,
		); err != nil {
			return 0, fmt.Errorf("excluding batch: %w", err)
		}

		res, err := tx.Exec(
			"DELETE FROM sessions WHERE id IN ("+placeholders+")",
			args...,
		)
		if err != nil {
			return 0, fmt.Errorf("deleting batch: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing transaction: %w", err)
	}
	return total, nil
}

// ListSessionsModifiedBetween returns all sessions created or
// modified after since and at or before until.
//
// Uses file_mtime (nanoseconds since epoch from the source file)
// as the primary modification signal so that active sessions with
// new messages are detected even when ended_at has not changed.
// Falls back to session timestamps for rows without file_mtime.
//
// Precision note: file_mtime is compared as nanosecond integers,
// while text timestamps are normalized to millisecond precision
// (strftime '%f' -> 3 decimal places). Sub-millisecond differences
// in text timestamp fields are therefore truncated.
func (db *DB) ListSessionsModifiedBetween(
	ctx context.Context, since, until string,
) ([]Session, error) {
	query := "SELECT " + sessionFullCols + " FROM sessions"
	var (
		args  []any
		where []string
	)
	if since != "" {
		sinceTime, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			return nil, fmt.Errorf(
				"parsing since timestamp %q: %w", since, err,
			)
		}
		sinceText := sinceTime.UTC().Format("2006-01-02T15:04:05.000Z")
		sinceNano := sinceTime.UnixNano()
		where = append(where, `(file_mtime > ?
			OR `+sqliteSyncTimestampExpr(colLocalModifiedAt)+` > ?
			OR `+sqliteSyncTimestampExpr(colBestTimestamp)+` > ?
			OR `+sqliteSyncTimestampExpr(colCreatedAt)+` > ?)`)
		args = append(args, sinceNano, sinceText, sinceText, sinceText)
	}
	if until != "" {
		untilTime, err := time.Parse(time.RFC3339Nano, until)
		if err != nil {
			return nil, fmt.Errorf(
				"parsing until timestamp %q: %w", until, err,
			)
		}
		untilText := untilTime.UTC().Format("2006-01-02T15:04:05.000Z")
		untilNano := untilTime.UnixNano()
		// COALESCE(file_mtime, -1) maps NULL to -1, which is always
		// <= untilNano. This is intentional: rows without file_mtime
		// should pass the upper-bound check and fall through to the
		// timestamp comparisons below. The since clause omits COALESCE
		// so that NULL file_mtime does not satisfy > sinceNano.
		where = append(where, `(COALESCE(file_mtime, -1) <= ?
			AND COALESCE(`+sqliteSyncTimestampExpr(colLocalModifiedAt)+`, '') <= ?
			AND `+sqliteSyncTimestampExpr(colBestTimestamp)+` <= ?
			AND `+sqliteSyncTimestampExpr(colCreatedAt)+` <= ?)`)
		args = append(args, untilNano, untilText, untilText, untilText)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY created_at`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"listing sessions modified since %s: %w",
			since, err,
		)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var s Session
		err := rows.Scan(
			&s.ID, &s.Project, &s.Machine, &s.Agent,
			&s.FirstMessage, &s.DisplayName, &s.StartedAt, &s.EndedAt,
			&s.MessageCount, &s.UserMessageCount,
			&s.ParentSessionID, &s.RelationshipType,
			&s.TotalOutputTokens, &s.PeakContextTokens,
			&s.DeletedAt, &s.FilePath, &s.FileSize,
			&s.FileMtime, &s.FileHash, &s.LocalModifiedAt, &s.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning session: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// trustedSQLiteExpr is a string type for SQL expressions known to be safe
// (literals, column references). Using a distinct type prevents accidental
// injection of user input, mirroring the trustedSQL pattern in pgsync/time.go.
type trustedSQLiteExpr string

const (
	colLocalModifiedAt trustedSQLiteExpr = "NULLIF(local_modified_at, '')"
	colBestTimestamp   trustedSQLiteExpr = `COALESCE(
				NULLIF(ended_at, ''),
				NULLIF(started_at, ''),
				created_at
			)`
	colCreatedAt trustedSQLiteExpr = "created_at"
)

func sqliteSyncTimestampExpr(expr trustedSQLiteExpr) string {
	return "strftime('%Y-%m-%dT%H:%M:%fZ', " + string(expr) + ")"
}
