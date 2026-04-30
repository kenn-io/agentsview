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
	has_total_output_tokens, has_peak_context_tokens,
	is_automated,
	tool_failure_signal_count, tool_retry_count,
	edit_churn_count, consecutive_failure_max,
	outcome, outcome_confidence,
	ended_with_role, final_failure_streak,
	signals_pending_since,
	compaction_count, mid_task_compaction_count,
	context_pressure_max,
	health_score, health_grade,
	has_tool_calls, has_context_data,
	data_version,
	cwd, git_branch, source_session_id, source_version,
	parser_malformed_lines, is_truncated,
	deleted_at, termination_status, created_at`

// sessionPruneCols extends sessionBaseCols with file metadata
// needed by FindPruneCandidates.
const sessionPruneCols = `id, project, machine, agent,
	first_message, display_name, started_at, ended_at,
	message_count, user_message_count,
	parent_session_id, relationship_type,
	total_output_tokens, peak_context_tokens,
	has_total_output_tokens, has_peak_context_tokens,
	is_automated,
	tool_failure_signal_count, tool_retry_count,
	edit_churn_count, consecutive_failure_max,
	outcome, outcome_confidence,
	ended_with_role, final_failure_streak,
	signals_pending_since,
	compaction_count, mid_task_compaction_count,
	context_pressure_max,
	health_score, health_grade,
	has_tool_calls, has_context_data,
	data_version,
	cwd, git_branch, source_session_id, source_version,
	parser_malformed_lines, is_truncated,
	deleted_at, termination_status, file_path, file_size, created_at`

// sessionFullCols includes all columns for a complete session record.
const sessionFullCols = `id, project, machine, agent,
	first_message, display_name, started_at, ended_at,
	message_count, user_message_count,
	parent_session_id, relationship_type,
	total_output_tokens, peak_context_tokens,
	has_total_output_tokens, has_peak_context_tokens,
	is_automated,
	tool_failure_signal_count, tool_retry_count,
	edit_churn_count, consecutive_failure_max,
	outcome, outcome_confidence,
	ended_with_role, final_failure_streak,
	signals_pending_since,
	compaction_count, mid_task_compaction_count,
	context_pressure_max,
	health_score, health_grade,
	has_tool_calls, has_context_data,
	data_version,
	cwd, git_branch, source_session_id, source_version,
	parser_malformed_lines, is_truncated,
	deleted_at, termination_status, file_path, file_size, file_mtime,
	file_inode, file_device,
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
		&s.HasTotalOutputTokens, &s.HasPeakContextTokens,
		&s.IsAutomated,
		&s.ToolFailureSignalCount, &s.ToolRetryCount,
		&s.EditChurnCount, &s.ConsecutiveFailureMax,
		&s.Outcome, &s.OutcomeConfidence,
		&s.EndedWithRole, &s.FinalFailureStreak,
		&s.SignalsPendingSince,
		&s.CompactionCount, &s.MidTaskCompactionCount,
		&s.ContextPressureMax,
		&s.HealthScore, &s.HealthGrade,
		&s.HasToolCalls, &s.HasContextData,
		&s.DataVersion,
		&s.Cwd, &s.GitBranch,
		&s.SourceSessionID, &s.SourceVersion,
		&s.ParserMalformedLines, &s.IsTruncated,
		&s.DeletedAt, &s.TerminationStatus, &s.CreatedAt,
	)
	return s, err
}

// Session represents a row in the sessions table.
type Session struct {
	ID                   string  `json:"id"`
	Project              string  `json:"project"`
	Machine              string  `json:"machine"`
	Agent                string  `json:"agent"`
	FirstMessage         *string `json:"first_message"`
	DisplayName          *string `json:"display_name,omitempty"`
	StartedAt            *string `json:"started_at"`
	EndedAt              *string `json:"ended_at"`
	MessageCount         int     `json:"message_count"`
	UserMessageCount     int     `json:"user_message_count"`
	ParentSessionID      *string `json:"parent_session_id,omitempty"`
	RelationshipType     string  `json:"relationship_type,omitempty"`
	TotalOutputTokens    int     `json:"total_output_tokens"`
	PeakContextTokens    int     `json:"peak_context_tokens"`
	HasTotalOutputTokens bool    `json:"has_total_output_tokens"`
	HasPeakContextTokens bool    `json:"has_peak_context_tokens"`
	IsAutomated          bool    `json:"is_automated"`

	// Session signals (computed from messages/tool_calls).
	ToolFailureSignalCount int      `json:"tool_failure_signal_count"`
	ToolRetryCount         int      `json:"tool_retry_count"`
	EditChurnCount         int      `json:"edit_churn_count"`
	ConsecutiveFailureMax  int      `json:"consecutive_failure_max"`
	Outcome                string   `json:"outcome"`
	OutcomeConfidence      string   `json:"outcome_confidence"`
	EndedWithRole          string   `json:"ended_with_role"`
	FinalFailureStreak     int      `json:"final_failure_streak"`
	SignalsPendingSince    *string  `json:"signals_pending_since,omitempty"`
	CompactionCount        int      `json:"compaction_count"`
	MidTaskCompactionCount int      `json:"mid_task_compaction_count"`
	ContextPressureMax     *float64 `json:"context_pressure_max,omitempty"`
	HealthScore            *int     `json:"health_score,omitempty"`
	HealthGrade            *string  `json:"health_grade,omitempty"`
	HasToolCalls           bool     `json:"-"`
	HasContextData         bool     `json:"-"`
	DataVersion            int      `json:"-"`
	Cwd                    string   `json:"cwd,omitempty"`
	GitBranch              string   `json:"git_branch,omitempty"`
	SourceSessionID        string   `json:"source_session_id,omitempty"`
	SourceVersion          string   `json:"source_version,omitempty"`
	ParserMalformedLines   int      `json:"parser_malformed_lines,omitempty"`
	IsTruncated            bool     `json:"is_truncated,omitempty"`

	DeletedAt         *string `json:"deleted_at,omitempty"`
	TerminationStatus *string `json:"termination_status,omitempty"`
	FilePath          *string `json:"file_path,omitempty"`
	FileSize          *int64  `json:"file_size,omitempty"`
	FileMtime         *int64  `json:"file_mtime,omitempty"`
	FileInode         *int64  `json:"file_inode,omitempty"`
	FileDevice        *int64  `json:"file_device,omitempty"`
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
	Project          string
	ExcludeProject   string // exclude sessions with this project name
	Machine          string
	Agent            string
	Date             string   // exact date YYYY-MM-DD
	DateFrom         string   // range start (inclusive)
	DateTo           string   // range end (inclusive)
	ActiveSince      string   // ISO-8601 timestamp; filters on most recent activity
	MinMessages      int      // message_count >= N (0 = no filter)
	MaxMessages      int      // message_count <= N (0 = no filter)
	MinUserMessages  int      // user_message_count >= N (0 = no filter)
	ExcludeOneShot   bool     // exclude sessions with user_message_count <= 1
	ExcludeAutomated bool     // exclude sessions where is_automated = 1
	IncludeChildren  bool     // include subagent sessions (for sidebar grouping)
	Outcome          []string // filter by outcome values
	HealthGrade      []string // filter by health grade values
	MinToolFailures  *int     // minimum tool_failure_signal_count
	Cursor           string   // opaque cursor from previous page
	Limit            int
	// Termination filters by termination_status:
	//   "" or "all"  → no filter (default)
	//   "clean"      → only sessions with status = 'clean'
	//   "unclean"    → only sessions with status IN
	//                  ('tool_call_pending', 'truncated')
	Termination string
}

// activeWindow is the freshness window for "active" sessions
// (last activity within this duration).
const activeWindow = 10 * time.Minute

// staleWindow is the upper bound for "stale" sessions. Past this
// idle duration with an orphan tool call, the session is "unclean".
const staleWindow = 60 * time.Minute

// activityExprSQLite computes seconds-since-epoch of the most
// recent activity timestamp. Used by both sessions and analytics
// filters when classifying by status.
const activityExprSQLite = "CAST(strftime('%s', " +
	"COALESCE(ended_at, started_at, created_at)) AS INTEGER)"

// buildTerminationPredSQLite returns a WHERE fragment and args for
// the multi-state termination filter (active / stale / unclean).
// The status value may be comma-separated to OR multiple states
// (e.g. "stale,unclean"). Returns ("", nil) when empty or "all".
//
// Stale and unclean both require a parser red flag
// (tool_call_pending or truncated). Sessions classified as clean
// or with NULL termination_status never appear under those
// filters — the parser-side classifier is the only positive
// signal that something is wrong. Active is purely time-based:
// any session written to in the last activeWindow qualifies.
func buildTerminationPredSQLite(status string) (string, []any) {
	if status == "" || status == "all" {
		return "", nil
	}
	now := time.Now().Unix()
	activeCutoff := now - int64(activeWindow.Seconds())
	staleCutoff := now - int64(staleWindow.Seconds())
	const flagged = "termination_status IN ('tool_call_pending', 'truncated')"

	parts := strings.Split(status, ",")
	preds := make([]string, 0, len(parts))
	args := make([]any, 0, len(parts)*2)
	for _, p := range parts {
		switch strings.TrimSpace(p) {
		case "active":
			preds = append(preds, activityExprSQLite+" > ?")
			args = append(args, activeCutoff)
		case "stale":
			preds = append(preds, "("+activityExprSQLite+" > ? AND "+
				activityExprSQLite+" <= ? AND "+flagged+")")
			args = append(args, staleCutoff, activeCutoff)
		case "unclean":
			preds = append(preds, "("+activityExprSQLite+" <= ? AND "+flagged+")")
			args = append(args, staleCutoff)
		case "clean":
			preds = append(preds, "termination_status = 'clean'")
		case "awaiting_user":
			preds = append(preds, "termination_status = 'awaiting_user'")
		}
	}
	if len(preds) == 0 {
		return "", nil
	}
	if len(preds) == 1 {
		return preds[0], args
	}
	return "(" + strings.Join(preds, " OR ") + ")", args
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
		machines := strings.Split(f.Machine, ",")
		if len(machines) == 1 {
			filterPreds = append(filterPreds, "machine = ?")
			filterArgs = append(filterArgs, machines[0])
		} else {
			placeholders := make(
				[]string, len(machines),
			)
			for i, m := range machines {
				placeholders[i] = "?"
				filterArgs = append(filterArgs, m)
			}
			filterPreds = append(filterPreds,
				"machine IN ("+
					strings.Join(placeholders, ",")+
					")")
		}
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
	if pred, args := buildTerminationPredSQLite(f.Termination); pred != "" {
		filterPreds = append(filterPreds, pred)
		filterArgs = append(filterArgs, args...)
	}
	// "" and "all" add no predicate.

	// ExcludeOneShot is handled separately from filterPreds
	// when IncludeChildren is true. Children (subagents, forks)
	// are almost always one-shot by nature and must not be
	// excluded. The one-shot filter applies only to root
	// sessions that match the filter directly.
	// When ExcludeOneShot is true but automated sessions are
	// included, exempt them from the one-shot filter — automated
	// sessions are single-turn by definition, so a strict
	// user_message_count > 1 predicate would always hide them.
	oneShotPred := ""
	if f.ExcludeOneShot {
		pred := "user_message_count > 1"
		if !f.ExcludeAutomated {
			pred = "(user_message_count > 1 OR is_automated = 1)"
		}
		if f.IncludeChildren {
			oneShotPred = pred
		} else {
			filterPreds = append(filterPreds, pred)
		}
	}

	if f.ExcludeAutomated {
		filterPreds = append(filterPreds, "is_automated = 0")
	}

	if len(f.Outcome) > 0 {
		placeholders := make([]string, len(f.Outcome))
		for i, v := range f.Outcome {
			placeholders[i] = "?"
			filterArgs = append(filterArgs, v)
		}
		filterPreds = append(filterPreds,
			"outcome IN ("+strings.Join(placeholders, ",")+")")
	}
	if len(f.HealthGrade) > 0 {
		placeholders := make([]string, len(f.HealthGrade))
		for i, v := range f.HealthGrade {
			placeholders[i] = "?"
			filterArgs = append(filterArgs, v)
		}
		filterPreds = append(filterPreds,
			"health_grade IN ("+
				strings.Join(placeholders, ",")+
				")")
	}
	if f.MinToolFailures != nil {
		filterPreds = append(filterPreds,
			"tool_failure_signal_count >= ?")
		filterArgs = append(filterArgs, *f.MinToolFailures)
	}

	// Simple case: children not included — basePreds already
	// carries the relationship_type guard, so subagent/fork
	// rows are dropped and no OR-branch is needed.
	if !f.IncludeChildren {
		allPreds := append(basePreds, filterPreds...)
		if oneShotPred != "" {
			allPreds = append(allPreds, oneShotPred)
		}
		return strings.Join(allPreds, " AND "), filterArgs
	}

	// IncludeChildren: compute the transitive closure of rows
	// reachable from qualifying roots via parent_session_id,
	// then restrict the outer query to that set. A plain
	// single-level parent subquery is not sufficient — a
	// subagent that passes the user filters can appear in
	// that subquery and drag its own children through as fake
	// roots, even when the subagent itself is filtered out by
	// the relationship guard. The CTE invariant "every
	// included row has a full parent chain back to a
	// rootMatch-passing root" handles this at any depth.
	baseWhere := strings.Join(basePreds, " AND ")

	rootMatchParts := append([]string{}, filterPreds...)
	if oneShotPred != "" {
		rootMatchParts = append(rootMatchParts, oneShotPred)
	}
	rootMatchParts = append(rootMatchParts,
		"relationship_type NOT IN ('subagent', 'fork')")
	rootMatch := strings.Join(rootMatchParts, " AND ")

	// UNION (not UNION ALL) in the recursive step deduplicates
	// and guards against cyclic parent chains. Depth in real
	// data is 1-2, so the perf cost is negligible.
	cte := "WITH RECURSIVE tree(id) AS (" +
		"SELECT id FROM sessions" +
		" WHERE message_count > 0 AND deleted_at IS NULL AND " +
		rootMatch +
		" UNION " +
		"SELECT s.id FROM sessions s" +
		" JOIN tree t ON s.parent_session_id = t.id" +
		" WHERE s.message_count > 0 AND s.deleted_at IS NULL" +
		") SELECT id FROM tree"

	where := baseWhere + " AND id IN (" + cte + ")"
	return where, filterArgs
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
		&s.HasTotalOutputTokens, &s.HasPeakContextTokens,
		&s.IsAutomated,
		&s.ToolFailureSignalCount, &s.ToolRetryCount,
		&s.EditChurnCount, &s.ConsecutiveFailureMax,
		&s.Outcome, &s.OutcomeConfidence,
		&s.EndedWithRole, &s.FinalFailureStreak,
		&s.SignalsPendingSince,
		&s.CompactionCount, &s.MidTaskCompactionCount,
		&s.ContextPressureMax,
		&s.HealthScore, &s.HealthGrade,
		&s.HasToolCalls, &s.HasContextData,
		&s.DataVersion,
		&s.Cwd, &s.GitBranch,
		&s.SourceSessionID, &s.SourceVersion,
		&s.ParserMalformedLines, &s.IsTruncated,
		&s.DeletedAt, &s.TerminationStatus, &s.FilePath, &s.FileSize,
		&s.FileMtime, &s.FileInode, &s.FileDevice,
		&s.FileHash, &s.LocalModifiedAt, &s.CreatedAt,
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

const upsertSessionSQL = `
		INSERT INTO sessions (
			id, project, machine, agent, first_message, display_name,
			started_at, ended_at, message_count,
			user_message_count, parent_session_id,
			relationship_type,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens,
			is_automated,
			termination_status,
			cwd, git_branch, source_session_id,
			source_version, parser_malformed_lines,
			is_truncated,
			file_path, file_size, file_mtime,
			file_inode, file_device, file_hash
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			has_total_output_tokens = excluded.has_total_output_tokens,
			has_peak_context_tokens = excluded.has_peak_context_tokens,
			is_automated = excluded.is_automated,
			termination_status = excluded.termination_status,
			cwd = excluded.cwd,
			git_branch = excluded.git_branch,
			source_session_id = excluded.source_session_id,
			source_version = excluded.source_version,
			parser_malformed_lines = excluded.parser_malformed_lines,
			is_truncated = excluded.is_truncated,
			file_path = excluded.file_path,
			file_size = excluded.file_size,
			file_mtime = excluded.file_mtime,
			file_inode = excluded.file_inode,
			file_device = excluded.file_device,
			file_hash = excluded.file_hash`

func sessionIsAutomated(s Session) bool {
	return s.UserMessageCount <= 1 &&
		s.FirstMessage != nil &&
		IsAutomatedSession(*s.FirstMessage)
}

func upsertSessionArgs(s Session) []any {
	return []any{
		s.ID, s.Project, s.Machine, s.Agent, s.FirstMessage, s.DisplayName,
		s.StartedAt, s.EndedAt, s.MessageCount,
		s.UserMessageCount, s.ParentSessionID,
		s.RelationshipType,
		s.TotalOutputTokens, s.PeakContextTokens,
		s.HasTotalOutputTokens, s.HasPeakContextTokens,
		sessionIsAutomated(s),
		s.TerminationStatus,
		s.Cwd, s.GitBranch, s.SourceSessionID,
		s.SourceVersion, s.ParserMalformedLines,
		s.IsTruncated,
		s.FilePath, s.FileSize, s.FileMtime,
		s.FileInode, s.FileDevice, s.FileHash,
	}
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

	// data_version is intentionally NOT advanced here. The
	// caller must call SetSessionDataVersion only after the
	// associated message rewrite succeeds, so a transient
	// failure to write messages doesn't mark the file as
	// up-to-date and starve the rewrite on the next sync.
	// New rows are seeded with 0 (the default) and bumped to
	// the current version once their messages land.
	_, err := db.getWriter().Exec(
		upsertSessionSQL,
		upsertSessionArgs(s)...,
	)
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
		" AND deleted_at IS NULL" +
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

// FindSessionIDsByPartial returns up to limit session IDs that
// contain the given substring. Used by CLI lookups so users can
// reference sessions by a short prefix shown in list output.
// Excludes soft-deleted sessions.
func (db *DB) FindSessionIDsByPartial(
	ctx context.Context, partial string, limit int,
) ([]string, error) {
	if partial == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT id FROM sessions
		 WHERE id LIKE ? AND deleted_at IS NULL
		 ORDER BY COALESCE(
		     NULLIF(ended_at, ''),
		     NULLIF(started_at, ''),
		     created_at
		 ) DESC
		 LIMIT ?`,
		"%"+partial+"%", limit,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"finding sessions by partial id %q: %w",
			partial, err,
		)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf(
				"scanning session id: %w", err,
			)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FindSessionIDsByRawSuffix returns up to limit session IDs whose
// stored id is either the exact raw input or the raw input
// preceded by an agent prefix (e.g. "codex:<uuid>"). The suffix
// comparison uses SUBSTR rather than LIKE so that SQL wildcard
// characters ('_' and '%') present in session IDs (which permit
// underscores) are compared literally instead of matching any
// character. Results are sorted by most recently active first.
// Excludes soft-deleted sessions.
func (db *DB) FindSessionIDsByRawSuffix(
	ctx context.Context, raw string, limit int,
) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT id FROM sessions
		 WHERE (id = ?1
		        OR SUBSTR(id, -(LENGTH(?1) + 1)) = ':' || ?1)
		   AND deleted_at IS NULL
		 ORDER BY (id = ?1) DESC,
		          COALESCE(
		              NULLIF(ended_at, ''),
		              NULLIF(started_at, ''),
		              created_at
		          ) DESC
		 LIMIT ?2`,
		raw, limit,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"finding sessions by raw suffix %q: %w",
			raw, err,
		)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf(
				"scanning session id: %w", err,
			)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetSessionDataVersion returns the data_version for a session.
// Returns 0 when the session does not exist.
func (db *DB) GetSessionDataVersion(id string) int {
	var v int
	err := db.getReader().QueryRow(
		"SELECT data_version FROM sessions WHERE id = ?", id,
	).Scan(&v)
	if err != nil {
		return 0
	}
	return v
}

// SetSessionDataVersion stamps the parser data_version on a
// session row. Call this only after the associated message
// rewrite has succeeded -- skipping it on failure ensures the
// next sync re-parses the file instead of treating it as
// already current. Bumps local_modified_at so the change
// propagates through the next pg push.
func (db *DB) SetSessionDataVersion(id string, version int) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		`UPDATE sessions SET
			data_version = ?,
			local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ?`,
		version, id,
	)
	if err != nil {
		return fmt.Errorf(
			"setting data_version for %s: %w", id, err,
		)
	}
	return nil
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
// re-parsing of an append-only session file. FirstMessage is
// the currently stored preview text; the sync engine uses it to
// decide whether the Claude parser's skip-command path has left
// the preview empty and a full parse should be forced.
type IncrementalInfo struct {
	ID                   string
	FileSize             int64
	FileMtime            int64
	FileInode            int64
	FileDevice           int64
	MsgCount             int
	UserMsgCount         int
	FirstMessage         string
	TotalOutputTokens    int
	PeakContextTokens    int
	HasTotalOutputTokens bool
	HasPeakContextTokens bool
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
	var fs, fm, fi, fd sql.NullInt64
	var firstMsg sql.NullString
	err = db.getReader().QueryRow(
		`SELECT id, file_size, file_mtime,
			file_inode, file_device,
			message_count, user_message_count,
			first_message,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens
		 FROM sessions WHERE file_path = ?`,
		path,
	).Scan(
		&info.ID, &fs, &fm, &fi, &fd,
		&info.MsgCount, &info.UserMsgCount,
		&firstMsg,
		&info.TotalOutputTokens, &info.PeakContextTokens,
		&info.HasTotalOutputTokens, &info.HasPeakContextTokens,
	)
	if err != nil {
		return nil, false
	}
	if firstMsg.Valid {
		info.FirstMessage = firstMsg.String
	}
	if fs.Valid {
		info.FileSize = fs.Int64
	}
	if fm.Valid {
		info.FileMtime = fm.Int64
	}
	if fi.Valid {
		info.FileInode = fi.Int64
	}
	if fd.Valid {
		info.FileDevice = fd.Int64
	}
	info.HasTotalOutputTokens =
		info.HasTotalOutputTokens || info.TotalOutputTokens != 0
	info.HasPeakContextTokens =
		info.HasPeakContextTokens || info.PeakContextTokens != 0
	return &info, true
}

// UpdateSessionIncremental updates only the fields that change
// during an incremental append: ended_at, message_count,
// user_message_count, file_size, file_mtime, and token
// aggregates. All values are absolute (not deltas) so the
// update is idempotent on retry.
//
// is_automated is recomputed from the current first_message and
// the new user_message_count so that classifier additions reach
// rows that only ever take the incremental path. Without this,
// a row whose first parse predates a new pattern would stay
// is_automated=0 indefinitely (UpsertSession sets the flag once
// at insert; the incremental path never re-evaluates it).
//
// termination_status is cleared to NULL on every incremental
// write. The classifier needs the full message slice to reach the
// right verdict (orphan tool calls, awaiting_user, etc.) and the
// incremental path only sees the new tail. Leaving the previous
// classification in place would surface stale "tool_call_pending"
// or "awaiting_user" indicators in the UI for up to 15 minutes
// (the periodic full-resync interval) after the user appended a
// resolving result or sent a new message. Clearing makes the
// session render with the time-based StatusDot tier (working /
// idle / quiet) until the next full sync reclassifies.
func (db *DB) UpdateSessionIncremental(
	id string,
	endedAt *string,
	msgCount, userMsgCount int,
	fileSize, fileMtime int64,
	totalOutputTokens, peakContextTokens int,
	hasTotalOutputTokens, hasPeakContextTokens bool,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	isAutomated := false
	if userMsgCount <= 1 {
		var fm sql.NullString
		err := db.getReader().QueryRow(
			"SELECT first_message FROM sessions WHERE id = ?", id,
		).Scan(&fm)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf(
				"reading first_message for incremental update %s: %w",
				id, err,
			)
		}
		if fm.Valid {
			isAutomated = IsAutomatedSession(fm.String)
		}
	}

	_, err := db.getWriter().Exec(`
		UPDATE sessions SET
			ended_at = COALESCE(?, ended_at),
			message_count = ?,
			user_message_count = ?,
			is_automated = ?,
			file_size = ?,
			file_mtime = ?,
			total_output_tokens = ?,
			peak_context_tokens = ?,
			has_total_output_tokens = ?,
			has_peak_context_tokens = ?,
			termination_status = NULL
		WHERE id = ?`,
		endedAt, msgCount, userMsgCount, isAutomated,
		fileSize, fileMtime,
		totalOutputTokens, peakContextTokens,
		hasTotalOutputTokens, hasPeakContextTokens, id,
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

// GetDataVersionByPath returns the minimum data_version for
// sessions matching a file_path. Returns 0 when no session
// exists for the path.
func (db *DB) GetDataVersionByPath(path string) int {
	var v int
	err := db.getReader().QueryRow(
		"SELECT MIN(data_version) FROM sessions"+
			" WHERE file_path = ?", path,
	).Scan(&v)
	if err != nil {
		return 0
	}
	return v
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
	ctx context.Context,
	excludeOneShot, excludeAutomated bool,
) ([]ProjectInfo, error) {
	q := `SELECT project, COUNT(*) as session_count
		FROM sessions
		WHERE message_count > 0
		  AND relationship_type NOT IN ('subagent', 'fork')
		  AND deleted_at IS NULL`
	if excludeOneShot {
		if !excludeAutomated {
			q += " AND (user_message_count > 1 OR is_automated = 1)"
		} else {
			q += " AND user_message_count > 1"
		}
	}
	if excludeAutomated {
		q += " AND is_automated = 0"
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
	ctx context.Context,
	excludeOneShot, excludeAutomated bool,
) ([]AgentInfo, error) {
	q := `SELECT agent, COUNT(*) as session_count
		FROM sessions
		WHERE message_count > 0 AND agent <> ''
		  AND deleted_at IS NULL
		  AND relationship_type NOT IN ('subagent', 'fork')`
	if excludeOneShot {
		if !excludeAutomated {
			q += " AND (user_message_count > 1 OR is_automated = 1)"
		} else {
			q += " AND user_message_count > 1"
		}
	}
	if excludeAutomated {
		q += " AND is_automated = 0"
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
	ctx context.Context,
	excludeOneShot, excludeAutomated bool,
) ([]string, error) {
	q := "SELECT DISTINCT machine FROM sessions WHERE deleted_at IS NULL"
	if excludeOneShot {
		if !excludeAutomated {
			q += " AND (user_message_count > 1 OR is_automated = 1)"
		} else {
			q += " AND user_message_count > 1"
		}
	}
	if excludeAutomated {
		q += " AND is_automated = 0"
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
			&s.HasTotalOutputTokens, &s.HasPeakContextTokens,
			&s.IsAutomated,
			&s.ToolFailureSignalCount, &s.ToolRetryCount,
			&s.EditChurnCount, &s.ConsecutiveFailureMax,
			&s.Outcome, &s.OutcomeConfidence,
			&s.EndedWithRole, &s.FinalFailureStreak,
			&s.SignalsPendingSince,
			&s.CompactionCount, &s.MidTaskCompactionCount,
			&s.ContextPressureMax,
			&s.HealthScore, &s.HealthGrade,
			&s.HasToolCalls, &s.HasContextData,
			&s.DataVersion,
			&s.Cwd, &s.GitBranch,
			&s.SourceSessionID, &s.SourceVersion,
			&s.ParserMalformedLines, &s.IsTruncated,
			&s.DeletedAt, &s.TerminationStatus, &s.FilePath, &s.FileSize, &s.CreatedAt,
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
	projects, excludeProjects []string,
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
	if len(projects) > 0 {
		placeholders := make([]string, len(projects))
		for i, p := range projects {
			placeholders[i] = "?"
			args = append(args, p)
		}
		where = append(where, "project IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(excludeProjects) > 0 {
		placeholders := make([]string, len(excludeProjects))
		for i, p := range excludeProjects {
			placeholders[i] = "?"
			args = append(args, p)
		}
		where = append(where, "project NOT IN ("+strings.Join(placeholders, ", ")+")")
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
			&s.HasTotalOutputTokens, &s.HasPeakContextTokens,
			&s.IsAutomated,
			&s.ToolFailureSignalCount, &s.ToolRetryCount,
			&s.EditChurnCount, &s.ConsecutiveFailureMax,
			&s.Outcome, &s.OutcomeConfidence,
			&s.EndedWithRole, &s.FinalFailureStreak,
			&s.SignalsPendingSince,
			&s.CompactionCount, &s.MidTaskCompactionCount,
			&s.ContextPressureMax,
			&s.HealthScore, &s.HealthGrade,
			&s.HasToolCalls, &s.HasContextData,
			&s.DataVersion,
			&s.Cwd, &s.GitBranch,
			&s.SourceSessionID, &s.SourceVersion,
			&s.ParserMalformedLines, &s.IsTruncated,
			&s.DeletedAt, &s.TerminationStatus, &s.FilePath, &s.FileSize,
			&s.FileMtime, &s.FileInode, &s.FileDevice,
			&s.FileHash, &s.LocalModifiedAt, &s.CreatedAt,
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
