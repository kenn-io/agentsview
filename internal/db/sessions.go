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
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrInvalidCursor is returned when a cursor cannot be decoded or verified.
var ErrInvalidCursor = errors.New("invalid cursor")

// ErrSessionExcluded is returned by UpsertSession when the
// session was permanently deleted by the user. Callers should
// skip any follow-up writes (messages, tool_calls) for this session.
var ErrSessionExcluded = errors.New("session excluded")

// ErrSessionTrashed is returned by UpsertSession when the
// session currently exists in the trash. Upload/import callers
// should surface a conflict instead of silently overwriting it.
var ErrSessionTrashed = errors.New("session trashed")

// sessionBaseCols is the column list for standard session queries
// (list, get). Keep in sync with scanSessionRow.
const sessionBaseCols = `id, project, machine, agent,
	first_message, COALESCE(display_name, session_name) AS display_name, started_at, ended_at,
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
	secret_leak_count, secrets_rules_version,
	quality_signal_version,
	short_prompt_count, unstructured_start,
	missing_success_criteria_count,
	missing_verification_count, duplicate_prompt_count,
	no_code_context_count, runaway_tool_loop_count,
	data_version,
	cwd, git_branch, source_session_id, source_version,
	parser_malformed_lines, is_truncated,
	deleted_at, termination_status, created_at`

// sessionPruneCols extends sessionBaseCols with file metadata
// needed by FindPruneCandidates.
const sessionPruneCols = `id, project, machine, agent,
	first_message, COALESCE(display_name, session_name) AS display_name, started_at, ended_at,
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
	secret_leak_count, secrets_rules_version,
	quality_signal_version,
	short_prompt_count, unstructured_start,
	missing_success_criteria_count,
	missing_verification_count, duplicate_prompt_count,
	no_code_context_count, runaway_tool_loop_count,
	data_version,
	cwd, git_branch, source_session_id, source_version,
	parser_malformed_lines, is_truncated,
	deleted_at, termination_status, file_path, file_size, created_at`

// sessionFullCols includes all columns for a complete session record.
const sessionFullCols = `id, project, machine, agent,
	first_message, display_name, session_name, started_at, ended_at,
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
	secret_leak_count, secrets_rules_version,
	quality_signal_version,
	short_prompt_count, unstructured_start,
	missing_success_criteria_count,
	missing_verification_count, duplicate_prompt_count,
	no_code_context_count, runaway_tool_loop_count,
	data_version,
	cwd, git_branch, source_session_id, source_version,
	parser_malformed_lines, is_truncated,
	deleted_at, termination_status, file_path, file_size, file_mtime,
	next_ordinal, last_entry_uuid,
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
		&s.SecretLeakCount, &s.SecretsRulesVersion,
		&s.QualitySignalVersion,
		&s.ShortPromptCount, &s.UnstructuredStart,
		&s.MissingSuccessCriteriaCount,
		&s.MissingVerificationCount, &s.DuplicatePromptCount,
		&s.NoCodeContextCount, &s.RunawayToolLoopCount,
		&s.DataVersion,
		&s.Cwd, &s.GitBranch,
		&s.SourceSessionID, &s.SourceVersion,
		&s.ParserMalformedLines, &s.IsTruncated,
		&s.DeletedAt, &s.TerminationStatus, &s.CreatedAt,
	)
	return s, err
}

const CurrentQualitySignalVersion = 2

// QualitySignals groups persisted deterministic quality-signal
// columns for API callers while keeping the database representation
// scalar and aggregation-friendly.
type QualitySignals struct {
	Version                     int  `json:"version"`
	ShortPromptCount            int  `json:"short_prompt_count"`
	UnstructuredStart           bool `json:"unstructured_start"`
	MissingSuccessCriteriaCount int  `json:"missing_success_criteria_count"`
	MissingVerificationCount    int  `json:"missing_verification_count"`
	DuplicatePromptCount        int  `json:"duplicate_prompt_count"`
	NoCodeContextCount          int  `json:"no_code_context_count"`
	RunawayToolLoopCount        int  `json:"runaway_tool_loop_count"`
}

// StoredQualitySignals returns the grouped API view of persisted
// deterministic quality-signal columns. Version 0 means the row has
// not gone through the Phase 3 signal write/backfill path yet.
func (s Session) StoredQualitySignals() *QualitySignals {
	if s.QualitySignals != nil {
		return s.QualitySignals
	}
	if s.QualitySignalVersion <= 0 {
		return nil
	}
	return &QualitySignals{
		Version:                     s.QualitySignalVersion,
		ShortPromptCount:            s.ShortPromptCount,
		UnstructuredStart:           s.UnstructuredStart,
		MissingSuccessCriteriaCount: s.MissingSuccessCriteriaCount,
		MissingVerificationCount:    s.MissingVerificationCount,
		DuplicatePromptCount:        s.DuplicatePromptCount,
		NoCodeContextCount:          s.NoCodeContextCount,
		RunawayToolLoopCount:        s.RunawayToolLoopCount,
	}
}

// ApplyQualitySignals maps the grouped API representation back to the
// scalar persistence fields used internally.
func (s *Session) ApplyQualitySignals(qs *QualitySignals) {
	s.QualitySignals = qs
	if qs == nil {
		s.QualitySignalVersion = 0
		s.ShortPromptCount = 0
		s.UnstructuredStart = false
		s.MissingSuccessCriteriaCount = 0
		s.MissingVerificationCount = 0
		s.DuplicatePromptCount = 0
		s.NoCodeContextCount = 0
		s.RunawayToolLoopCount = 0
		return
	}
	s.QualitySignalVersion = qs.Version
	s.ShortPromptCount = qs.ShortPromptCount
	s.UnstructuredStart = qs.UnstructuredStart
	s.MissingSuccessCriteriaCount = qs.MissingSuccessCriteriaCount
	s.MissingVerificationCount = qs.MissingVerificationCount
	s.DuplicatePromptCount = qs.DuplicatePromptCount
	s.NoCodeContextCount = qs.NoCodeContextCount
	s.RunawayToolLoopCount = qs.RunawayToolLoopCount
}

// MarshalJSON exposes quality signals as a grouped optional object
// without leaking the scalar persistence columns into the API.
func (s Session) MarshalJSON() ([]byte, error) {
	type sessionAlias Session
	return json.Marshal(struct {
		sessionAlias
		QualitySignals *QualitySignals `json:"quality_signals,omitempty"`
	}{
		sessionAlias:   sessionAlias(s),
		QualitySignals: s.StoredQualitySignals(),
	})
}

// UnmarshalJSON accepts the grouped API quality_signals object and
// restores the scalar fields used by service and persistence code.
func (s *Session) UnmarshalJSON(data []byte) error {
	type sessionAlias Session
	var v struct {
		sessionAlias
		QualitySignals *QualitySignals `json:"quality_signals"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*s = Session(v.sessionAlias)
	s.ApplyQualitySignals(v.QualitySignals)
	return nil
}

// Session represents a row in the sessions table.
type Session struct {
	ID                   string  `json:"id"`
	Project              string  `json:"project"`
	Machine              string  `json:"machine"`
	Agent                string  `json:"agent"`
	FirstMessage         *string `json:"first_message"`
	DisplayName          *string `json:"display_name,omitempty"`
	SessionName          *string `json:"-"`
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
	// QualitySignals mirrors the scalar persistence fields below for API
	// schema and JSON transport.
	QualitySignals              *QualitySignals `json:"quality_signals,omitempty"`
	HasToolCalls                bool            `json:"-"`
	HasContextData              bool            `json:"-"`
	SecretLeakCount             int             `json:"secret_leak_count"`
	SecretsRulesVersion         string          `json:"-"`
	QualitySignalVersion        int             `json:"-"`
	ShortPromptCount            int             `json:"-"`
	UnstructuredStart           bool            `json:"-"`
	MissingSuccessCriteriaCount int             `json:"-"`
	MissingVerificationCount    int             `json:"-"`
	DuplicatePromptCount        int             `json:"-"`
	NoCodeContextCount          int             `json:"-"`
	RunawayToolLoopCount        int             `json:"-"`
	DataVersion                 int             `json:"-"`
	Cwd                         string          `json:"cwd,omitempty"`
	GitBranch                   string          `json:"git_branch,omitempty"`
	SourceSessionID             string          `json:"source_session_id,omitempty"`
	SourceVersion               string          `json:"source_version,omitempty"`
	ParserMalformedLines        int             `json:"parser_malformed_lines,omitempty"`
	IsTruncated                 bool            `json:"is_truncated,omitempty"`

	DeletedAt         *string `json:"deleted_at,omitempty"`
	TerminationStatus *string `json:"termination_status,omitempty"`
	FilePath          *string `json:"file_path,omitempty"`
	FileSize          *int64  `json:"file_size,omitempty"`
	FileMtime         *int64  `json:"file_mtime,omitempty"`
	NextOrdinal       int     `json:"-"`
	LastEntryUUID     *string `json:"-"`
	FileInode         *int64  `json:"file_inode,omitempty"`
	FileDevice        *int64  `json:"file_device,omitempty"`
	FileHash          *string `json:"file_hash,omitempty"`
	LocalModifiedAt   *string `json:"local_modified_at,omitempty"`
	CreatedAt         string  `json:"created_at"`
}

// SessionCursor is the opaque pagination token. EndedAt carries the
// recent-activity value for the default sort (and legacy cursors); Sort/Desc/
// Value generalize keyset pagination to any --sort column. New fields are
// additive so cursors minted before they existed still decode as recent.
type SessionCursor struct {
	EndedAt string `json:"e"`
	ID      string `json:"i"`
	Total   int    `json:"t,omitempty"`
	// Sort is the sort key the cursor was minted under ("" = legacy recent).
	Sort string `json:"k,omitempty"`
	// Desc is the direction the cursor was minted under.
	Desc bool `json:"d,omitempty"`
	// Value is the sort column's value for the page's last row, encoded as a
	// string and re-typed per the sort's kind when comparing.
	Value string `json:"v,omitempty"`
	// Keys carries one keyset term per column for multi-key sorts. When present
	// it is authoritative; the single-key Sort/Desc/Value (and EndedAt) fields
	// are only populated for single-key sorts so older readers still decode.
	Keys []SessionCursorKey `json:"ks,omitempty"`
}

// SessionCursorKey is one column's keyset term inside a multi-key cursor: the
// sort key it was minted under, its direction, and the page's last-row value
// (re-typed per the sort's kind when comparing).
type SessionCursorKey struct {
	Sort  string `json:"k"`
	Desc  bool   `json:"d,omitempty"`
	Value string `json:"v,omitempty"`
}

// resolvedKeys returns the cursor's keyset terms, synthesizing the single-key
// list from the legacy fields when the multi-key Keys slice is absent. A cursor
// with neither Keys nor Sort is a pre-sort legacy token, valid only for the
// default recent-descending order it was always minted under.
func (cur SessionCursor) resolvedKeys() []SessionCursorKey {
	if len(cur.Keys) > 0 {
		return cur.Keys
	}
	if cur.Sort != "" {
		return []SessionCursorKey{{Sort: cur.Sort, Desc: cur.Desc, Value: cur.Value}}
	}
	return []SessionCursorKey{{Sort: defaultSortKey, Desc: true, Value: cur.EndedAt}}
}

// EncodeCursor returns a base64-encoded, HMAC-signed cursor string.
func (db *DB) EncodeCursor(c SessionCursor) string {
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
	AutomatedScope   string   // "", "human", "all", or "automated"
	IncludeChildren  bool     // include subagent sessions (for sidebar grouping)
	IncludeOrphans   bool     // promote orphan child rows to sidebar roots
	Outcome          []string // filter by outcome values
	HealthGrade      []string // filter by health grade values
	MinToolFailures  *int     // minimum tool_failure_signal_count
	HasSecret        bool     // only sessions with current secret_leak_count > 0
	Starred          bool     // only sessions starred by the user
	// SecretsRulesVersions limits HasSecret to sessions scanned by one of these
	// current scanner versions. Empty preserves raw DB semantics for tests and
	// direct store callers that explicitly want unversioned counts.
	SecretsRulesVersions []string
	Cursor               string // opaque cursor from previous page
	Limit                int
	// Termination filters by termination_status:
	//   "" or "all"  → no filter (default)
	//   "clean"      → only sessions with status = 'clean'
	//   "unclean"    → only sessions with status IN
	//                  ('tool_call_pending', 'truncated')
	Termination string
	// Sort is the ordered, structured sort specification: each term is a sort
	// key with an optional per-key direction. When non-empty it is the canonical
	// source of ordering and takes precedence over OrderBy/Descending. This is
	// the field new callers should set to express per-key sort direction.
	Sort []SortKey
	// OrderBy is the legacy single-key shorthand, kept for existing callers. It
	// accepts the same comma-separated "key:dir" spec ParseSortSpec parses and is
	// used only when Sort is empty. "" means recent activity, the default.
	OrderBy string
	// Descending is the legacy fallback direction applied to OrderBy terms that
	// carry no explicit direction. Used only when Sort is empty.
	Descending *bool
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
	"COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, ''), created_at)) AS INTEGER)"

const sidebarActivityExprSQLiteS = "COALESCE(" +
	"NULLIF(s.ended_at, ''), NULLIF(s.started_at, ''), s.created_at)"

func sidebarStarredRootCTE(enabled bool) string {
	if !enabled {
		return ""
	}
	return `,
		eligible_roots(id) AS (
			SELECT DISTINCT t.root_id
			FROM tree t
			JOIN starred_sessions ss ON ss.session_id = t.id
		)`
}

func sidebarStarredRootJoin(enabled bool) string {
	if !enabled {
		return ""
	}
	return "JOIN eligible_roots e ON e.id = t.root_id"
}

// buildCanonicalRootWhere returns a WHERE fragment that identifies canonical root
// sessions for sidebar pagination. Child rows remain nested under their parent
// unless IncludeOrphans explicitly promotes missing-parent child rows to roots.
func buildCanonicalRootWhere(includeOrphans bool) string {
	return BuildCanonicalRootWhere(SQLiteQueryDialect(), "sessions", includeOrphans)
}

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
	b := NewQueryBuilder(SQLiteQueryDialect(), 0)
	pred := terminationPredicate(status, b, func(col string) string {
		return col
	})
	return pred, b.Args()
}

// SessionPage is a page of session results.
type SessionPage struct {
	Sessions   []Session `json:"sessions"`
	NextCursor string    `json:"next_cursor,omitempty"`
	Total      int       `json:"total"`
}

type SidebarSessionIndexRow struct {
	ID                string  `json:"id"`
	ParentSessionID   *string `json:"parent_session_id,omitempty"`
	RelationshipType  string  `json:"relationship_type,omitempty"`
	Project           string  `json:"project"`
	Machine           string  `json:"machine"`
	Agent             string  `json:"agent"`
	DisplayName       *string `json:"display_name,omitempty"`
	StartedAt         *string `json:"started_at"`
	EndedAt           *string `json:"ended_at"`
	CreatedAt         string  `json:"created_at"`
	TerminationStatus *string `json:"termination_status,omitempty"`
	MessageCount      int     `json:"message_count"`
	UserMessageCount  int     `json:"user_message_count"`
	IsAutomated       bool    `json:"is_automated"`
	IsTeammate        bool    `json:"is_teammate"`
}

type SidebarSessionIndex struct {
	Sessions   []SidebarSessionIndexRow `json:"sessions"`
	NextCursor string                   `json:"next_cursor,omitempty"`
	Total      int                      `json:"total"`
}

// buildSessionFilter returns a WHERE clause and args for the
// non-cursor predicates in SessionFilter.
func buildSessionFilter(f SessionFilter) (string, []any) {
	return BuildSessionFilterSQL(f, SQLiteQueryDialect())
}

// ListSessions returns a cursor-paginated list of sessions.
func (db *DB) ListSessions(
	ctx context.Context, f SessionFilter,
) (SessionPage, error) {
	if f.Limit <= 0 || f.Limit > MaxSessionLimit {
		f.Limit = DefaultSessionLimit
	}

	where, args := buildSessionFilter(f)

	dialect := SQLiteQueryDialect()
	rs := ResolveSort(f)

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
	pageBuilder := NewQueryBuilder(dialect, len(args))
	cursorWhere := where
	if f.Cursor != "" {
		vals, err := CursorPredicateValues(cur, rs)
		if err != nil {
			return SessionPage{}, err
		}
		cursorWhere += " AND " + pageBuilder.CursorPredicate(
			rs, f, vals, cur.ID,
		)
	}

	query := "SELECT " + sessionBaseCols +
		" FROM sessions WHERE " + cursorWhere + " " +
		pageBuilder.OrderByClause(rs, f) + " " +
		pageBuilder.Limit(f.Limit+1)
	cursorArgs = append(cursorArgs, pageBuilder.Args()...)

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
		page.NextCursor = db.EncodeCursor(
			NextSessionCursor(&last, rs, total, f),
		)
	}

	return page, nil
}

// GetSidebarSessionIndex returns the skinny session rows needed by
// the sidebar grouper. Paginated calls page root sessions and include
// each root's descendants so grouped sidebar trees stay complete.
func (db *DB) GetSidebarSessionIndex(
	ctx context.Context, f SessionFilter,
) (SidebarSessionIndex, error) {
	f.IncludeChildren = true
	f.IncludeOrphans = true

	if f.Limit > 0 || f.Cursor != "" || f.Starred {
		return db.getSidebarSessionIndexPage(ctx, f)
	}

	f.Cursor = ""
	where, args := buildSessionFilter(f)
	query := `
		SELECT
			id,
			parent_session_id,
			relationship_type,
			project,
			machine,
			agent,
			COALESCE(display_name, session_name) AS display_name,
			started_at,
			ended_at,
			created_at,
			termination_status,
			message_count,
			user_message_count,
			is_automated,
			INSTR(COALESCE(first_message, ''), '<teammate-message') > 0
		FROM sessions
		WHERE ` + where + `
		ORDER BY COALESCE(
			NULLIF(ended_at, ''),
			NULLIF(started_at, ''),
			created_at
		) DESC, id DESC`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return SidebarSessionIndex{},
			fmt.Errorf("querying sidebar session index: %w", err)
	}
	defer rows.Close()

	index := SidebarSessionIndex{
		Sessions: []SidebarSessionIndexRow{},
	}
	for rows.Next() {
		var row SidebarSessionIndexRow
		if err := rows.Scan(
			&row.ID,
			&row.ParentSessionID,
			&row.RelationshipType,
			&row.Project,
			&row.Machine,
			&row.Agent,
			&row.DisplayName,
			&row.StartedAt,
			&row.EndedAt,
			&row.CreatedAt,
			&row.TerminationStatus,
			&row.MessageCount,
			&row.UserMessageCount,
			&row.IsAutomated,
			&row.IsTeammate,
		); err != nil {
			return SidebarSessionIndex{},
				fmt.Errorf("scanning sidebar session index: %w", err)
		}
		index.Sessions = append(index.Sessions, row)
	}
	if err := rows.Err(); err != nil {
		return SidebarSessionIndex{},
			fmt.Errorf("iterating sidebar session index: %w", err)
	}
	index.Total = len(index.Sessions)

	return index, nil
}

func (db *DB) getSidebarSessionIndexPage(
	ctx context.Context, f SessionFilter,
) (SidebarSessionIndex, error) {
	if f.Limit <= 0 || f.Limit > MaxSessionLimit {
		f.Limit = DefaultSessionLimit
	}

	rootFilter := f
	rootFilter.Cursor = ""
	rootFilter.Starred = false
	rootFilter.IncludeChildren = false
	rootWhere, rootArgs := buildSessionBaseFilter(rootFilter)
	canonicalRootWhere := buildCanonicalRootWhere(f.IncludeOrphans)

	var total int
	var cur SessionCursor
	if f.Cursor != "" {
		var err error
		cur, err = db.DecodeCursor(f.Cursor)
		if err != nil {
			return SidebarSessionIndex{}, err
		}
		total = cur.Total
	}
	if total <= 0 {
		if f.Starred {
			countQuery := `
				WITH RECURSIVE root_candidates(id) AS (
					SELECT id
					FROM sessions
					WHERE ` + rootWhere + `
					  AND ` + canonicalRootWhere + `
				),
				tree(root_id, id) AS (
					SELECT id, id FROM root_candidates
					UNION
					SELECT t.root_id, s.id
					FROM sessions s
					JOIN tree t ON s.parent_session_id = t.id
					WHERE s.message_count > 0
					  AND s.deleted_at IS NULL
				),
				eligible_roots(id) AS (
					SELECT DISTINCT t.root_id
					FROM tree t
					JOIN starred_sessions ss ON ss.session_id = t.id
				)
				SELECT COUNT(*) FROM eligible_roots`
			if err := db.getReader().QueryRowContext(
				ctx, countQuery, rootArgs...,
			).Scan(&total); err != nil {
				return SidebarSessionIndex{},
					fmt.Errorf("counting sidebar roots: %w", err)
			}
		} else {
			countQuery := "SELECT COUNT(*) FROM sessions WHERE " +
				rootWhere + " AND " + canonicalRootWhere
			if err := db.getReader().QueryRowContext(
				ctx, countQuery, rootArgs...,
			).Scan(&total); err != nil {
				return SidebarSessionIndex{},
					fmt.Errorf("counting sidebar roots: %w", err)
			}
		}
	}

	pageBuilder := NewQueryBuilder(SQLiteQueryDialect(), len(rootArgs))
	cursorWhere := ""
	if f.Cursor != "" {
		cursorWhere = "WHERE (activity, id) < (" +
			pageBuilder.Add(cur.EndedAt) + ", " +
			pageBuilder.Add(cur.ID) + ")"
	}
	rootQuery := `
		WITH RECURSIVE root_candidates(id) AS (
			SELECT id
			FROM sessions
			WHERE ` + rootWhere + `
			  AND ` + canonicalRootWhere + `
		),
		tree(root_id, id) AS (
			SELECT id, id FROM root_candidates
			UNION
			SELECT t.root_id, s.id
			FROM sessions s
			JOIN tree t ON s.parent_session_id = t.id
			WHERE s.message_count > 0
			  AND s.deleted_at IS NULL
		)
		` + sidebarStarredRootCTE(f.Starred) + `,
		root_activity(id, activity) AS (
			SELECT t.root_id AS id, MAX(` + sidebarActivityExprSQLiteS + `) AS activity
			FROM tree t
			` + sidebarStarredRootJoin(f.Starred) + `
			JOIN sessions s ON s.id = t.id
			GROUP BY t.root_id
		)
		SELECT id, activity
		FROM root_activity
		` + cursorWhere + `
		ORDER BY activity DESC, id DESC
		` + pageBuilder.Limit(f.Limit+1)
	rootQueryArgs := append([]any{}, rootArgs...)
	rootQueryArgs = append(rootQueryArgs, pageBuilder.Args()...)

	rows, err := db.getReader().QueryContext(ctx, rootQuery, rootQueryArgs...)
	if err != nil {
		return SidebarSessionIndex{},
			fmt.Errorf("querying sidebar root page: %w", err)
	}
	defer rows.Close()

	type rootRow struct {
		id       string
		activity string
	}
	roots := []rootRow{}
	for rows.Next() {
		var row rootRow
		if err := rows.Scan(&row.id, &row.activity); err != nil {
			return SidebarSessionIndex{},
				fmt.Errorf("scanning sidebar root page: %w", err)
		}
		roots = append(roots, row)
	}
	if err := rows.Err(); err != nil {
		return SidebarSessionIndex{},
			fmt.Errorf("iterating sidebar root page: %w", err)
	}

	index := SidebarSessionIndex{
		Sessions: []SidebarSessionIndexRow{},
		Total:    total,
	}
	if len(roots) == 0 {
		return index, nil
	}
	selected := roots
	if len(roots) > f.Limit {
		selected = roots[:f.Limit]
		last := selected[f.Limit-1]
		index.NextCursor = db.EncodeCursor(SessionCursor{
			EndedAt: last.activity, ID: last.id, Total: total,
		})
	}

	cteParts := make([]string, 0, len(selected))
	treeArgs := make([]any, 0, len(selected)*2)
	for i, root := range selected {
		if i == 0 {
			cteParts = append(cteParts, "SELECT ? AS id, ? AS ord")
		} else {
			cteParts = append(cteParts, "UNION ALL SELECT ?, ?")
		}
		treeArgs = append(treeArgs, root.id, i)
	}

	treeQuery := `
		WITH RECURSIVE root_page(id, ord) AS (
			` + strings.Join(cteParts, "\n") + `
		),
		tree(id, ord) AS (
			SELECT id, ord FROM root_page
			UNION
			SELECT s.id, t.ord
			FROM sessions s
			JOIN tree t ON s.parent_session_id = t.id
			WHERE s.message_count > 0
			  AND s.deleted_at IS NULL
		),
		ranked_tree(id, ord) AS (
			SELECT id, MIN(ord) AS ord
			FROM tree
			GROUP BY id
		)
		SELECT
			s.id,
			s.parent_session_id,
			s.relationship_type,
			s.project,
			s.machine,
			s.agent,
			COALESCE(s.display_name, s.session_name) AS display_name,
			s.started_at,
			s.ended_at,
			s.created_at,
			s.termination_status,
			s.message_count,
			s.user_message_count,
			s.is_automated,
			INSTR(COALESCE(s.first_message, ''), '<teammate-message') > 0
		FROM sessions s
		JOIN ranked_tree t ON s.id = t.id
		ORDER BY
			t.ord ASC,
			` + sidebarActivityExprSQLiteS + ` DESC,
			s.id DESC`

	rows, err = db.getReader().QueryContext(ctx, treeQuery, treeArgs...)
	if err != nil {
		return SidebarSessionIndex{},
			fmt.Errorf("querying sidebar tree page: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var row SidebarSessionIndexRow
		if err := rows.Scan(
			&row.ID,
			&row.ParentSessionID,
			&row.RelationshipType,
			&row.Project,
			&row.Machine,
			&row.Agent,
			&row.DisplayName,
			&row.StartedAt,
			&row.EndedAt,
			&row.CreatedAt,
			&row.TerminationStatus,
			&row.MessageCount,
			&row.UserMessageCount,
			&row.IsAutomated,
			&row.IsTeammate,
		); err != nil {
			return SidebarSessionIndex{},
				fmt.Errorf("scanning sidebar tree page: %w", err)
		}
		index.Sessions = append(index.Sessions, row)
	}
	if err := rows.Err(); err != nil {
		return SidebarSessionIndex{},
			fmt.Errorf("iterating sidebar tree page: %w", err)
	}

	return index, nil
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
		&s.FirstMessage, &s.DisplayName, &s.SessionName, &s.StartedAt, &s.EndedAt,
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
		&s.SecretLeakCount, &s.SecretsRulesVersion,
		&s.QualitySignalVersion,
		&s.ShortPromptCount, &s.UnstructuredStart,
		&s.MissingSuccessCriteriaCount,
		&s.MissingVerificationCount, &s.DuplicatePromptCount,
		&s.NoCodeContextCount, &s.RunawayToolLoopCount,
		&s.DataVersion,
		&s.Cwd, &s.GitBranch,
		&s.SourceSessionID, &s.SourceVersion,
		&s.ParserMalformedLines, &s.IsTruncated,
		&s.DeletedAt, &s.TerminationStatus, &s.FilePath, &s.FileSize,
		&s.FileMtime, &s.NextOrdinal, &s.LastEntryUUID,
		&s.FileInode, &s.FileDevice,
		&s.FileHash, &s.LocalModifiedAt, &s.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting session full %s: %w", id, err)
	}
	// Expose the visible name (user rename, else agent session name)
	// like the PG and DuckDB GetSessionFull and the sqlite base reads.
	// The coalesce happens post-scan because sessionFullCols is shared
	// with ListSessionsModifiedBetween, whose push consumers must see
	// display_name and session_name unmerged.
	if s.DisplayName == nil {
		s.DisplayName = s.SessionName
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

// IsSessionTrashed returns true if the session ID exists in the trash.
func (db *DB) IsSessionTrashed(id string) bool {
	var n int
	_ = db.getReader().QueryRow(
		"SELECT 1 FROM sessions WHERE id = ? AND deleted_at IS NOT NULL", id,
	).Scan(&n)
	return n == 1
}

// HasTrashedSessionByFilePath returns true when a source path already belongs
// to a trashed row for this agent.
func (db *DB) HasTrashedSessionByFilePath(path, agent string) bool {
	var n int
	_ = db.getReader().QueryRow(
		"SELECT 1 FROM sessions"+
			" WHERE file_path = ? AND agent = ? AND deleted_at IS NOT NULL"+
			" LIMIT 1",
		path, agent,
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
	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("begin purge excluded tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ids, err := sessionIDsTx(
		tx, "id IN (SELECT id FROM excluded_sessions)",
	)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := deleteSessionMessagesTx(tx, id); err != nil {
			return fmt.Errorf(
				"pre-deleting excluded session %s messages: %w",
				id, err,
			)
		}
	}
	if _, err := tx.Exec(
		"DELETE FROM sessions WHERE id IN (SELECT id FROM excluded_sessions)",
	); err != nil {
		return fmt.Errorf("purging excluded sessions: %w", err)
	}
	return tx.Commit()
}

// DeleteParserExcludedSessions removes rows that the current parser
// deliberately excludes, without recording a permanent user deletion
// in excluded_sessions. If the source file later becomes a real
// conversation, sync may import it again.
func (db *DB) DeleteParserExcludedSessions(ids []string) (int, error) {
	if err := db.requireWritable(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return 0, fmt.Errorf("begin parser-excluded delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	deleted := int64(0)
	for _, id := range ids {
		if id == "" {
			continue
		}
		if err := deleteSessionMessagesTx(tx, id); err != nil {
			return 0, fmt.Errorf(
				"pre-deleting parser-excluded session %s messages: %w",
				id, err,
			)
		}
		res, err := tx.Exec(
			"DELETE FROM sessions WHERE id = ?", id,
		)
		if err != nil {
			return 0, fmt.Errorf(
				"deleting parser-excluded session %s: %w",
				id, err,
			)
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit parser-excluded delete: %w", err)
	}
	return int(deleted), nil
}

const upsertSessionSQL = `
		INSERT INTO sessions (
			id, project, machine, agent, first_message, session_name,
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
			next_ordinal, last_entry_uuid,
			file_inode, file_device, file_hash
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project = excluded.project,
			machine = excluded.machine,
			agent = excluded.agent,
			first_message = excluded.first_message,
			-- session_name is always overwritten by re-parse; display_name
			-- is the user override and is only touched by RenameSession.
			session_name = excluded.session_name,
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
			next_ordinal = excluded.next_ordinal,
			last_entry_uuid = excluded.last_entry_uuid,
			file_inode = excluded.file_inode,
			file_device = excluded.file_device,
			file_hash = excluded.file_hash`

func sessionIsAutomated(s Session) bool {
	return s.IsAutomated ||
		(s.UserMessageCount <= 1 &&
			s.FirstMessage != nil &&
			IsAutomatedSession(*s.FirstMessage))
}

func upsertSessionArgs(s Session) []any {
	return []any{
		s.ID, s.Project, s.Machine, s.Agent, s.FirstMessage, s.SessionName,
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
		s.NextOrdinal, s.LastEntryUUID,
		s.FileInode, s.FileDevice, s.FileHash,
	}
}

// UpsertSession inserts or updates a session.
// Sessions that were permanently deleted (in excluded_sessions)
// or currently in the trash are rejected.
func (db *DB) UpsertSession(s Session) error {
	_ = ValidateAndSanitize(&s, nil, nil)

	db.mu.Lock()
	defer db.mu.Unlock()

	// Check exclusion/trash state under the write lock to avoid a race with
	// concurrent DeleteSession/EmptyTrash/RestoreSession.
	var excluded int
	_ = db.getWriter().QueryRow(
		"SELECT 1 FROM excluded_sessions WHERE id = ?", s.ID,
	).Scan(&excluded)
	if excluded == 1 {
		return ErrSessionExcluded
	}
	var trashed int
	_ = db.getWriter().QueryRow(
		"SELECT 1 FROM sessions WHERE id = ? AND deleted_at IS NOT NULL", s.ID,
	).Scan(&trashed)
	if trashed == 1 {
		return ErrSessionTrashed
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

// BumpLocalModifiedAt stamps the current time as local_modified_at so
// incremental PG push picks up metadata changes (e.g. session_name updates
// on the importer skip path) that don't go through the file-based sync path.
func (db *DB) BumpLocalModifiedAt(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		`UPDATE sessions SET local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ? AND deleted_at IS NULL`,
		id,
	)
	return err
}

// RefreshSessionName updates only session_name and bumps local_modified_at
// in a single targeted UPDATE. Use this on re-import skip paths where the
// full UpsertSession is unsafe because the caller does not have a complete
// row to avoid overwriting existing fields with zero values.
func (db *DB) RefreshSessionName(id string, sessionName *string) error {
	if sessionName != nil {
		clean := *sessionName
		var stats ValidationStats
		sanitizeStringField(&clean, &stats)
		sessionName = &clean
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		`UPDATE sessions
		 SET session_name = ?,
		     local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ? AND deleted_at IS NULL`,
		sessionName, id,
	)
	return err
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

// SessionVersionMarker returns a compact marker for one or more
// version fields. Inputs are length-framed so adjacent fields cannot
// collide by concatenation.
func SessionVersionMarker(parts ...string) int64 {
	const (
		offset64 = uint64(14695981039346656037)
		prime64  = uint64(1099511628211)
	)
	h := offset64
	write := func(s string) {
		for _, b := range []byte(s) {
			h ^= uint64(b)
			h *= prime64
		}
	}
	for _, part := range parts {
		write(fmt.Sprintf("%d:", len(part)))
		write(part)
	}
	return int64(h)
}

// GetSessionVersion returns the message count and a compact version
// marker for change detection in SSE watchers.
func (db *DB) GetSessionVersion(
	id string,
) (count int, version int64, ok bool) {
	var fileMtime int64
	var fileHash, localModifiedAt string
	err := db.getReader().QueryRow(
		"SELECT message_count, COALESCE(file_mtime, 0),"+
			" COALESCE(file_hash, ''), COALESCE(local_modified_at, '')"+
			" FROM sessions WHERE id = ?",
		id,
	).Scan(&count, &fileMtime, &fileHash, &localModifiedAt)
	if err != nil {
		return 0, 0, false
	}
	return count, SessionVersionMarker(
		fmt.Sprintf("%d", fileMtime),
		fileHash,
		localModifiedAt,
	), true
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
	NextOrdinal          int
	LastEntryUUID        string
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

type IncrementalSessionUpdate struct {
	EndedAt              *string
	MsgCount             int
	UserMsgCount         int
	FileSize             int64
	FileMtime            int64
	FileHash             *string
	NextOrdinal          int
	LastEntryUUID        string
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
		 WHERE file_path = ?
		   AND deleted_at IS NULL`, path,
	).Scan(&count)
	if err != nil || count != 1 {
		return nil, false
	}

	var info IncrementalInfo
	var fs, fm, fi, fd sql.NullInt64
	var firstMsg, lastEntryUUID sql.NullString
	err = db.getReader().QueryRow(
		`SELECT id, file_size, file_mtime,
			next_ordinal, last_entry_uuid,
			file_inode, file_device,
			message_count, user_message_count,
			first_message,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens
		 FROM sessions
		 WHERE file_path = ?
		   AND deleted_at IS NULL`,
		path,
	).Scan(
		&info.ID, &fs, &fm, &info.NextOrdinal, &lastEntryUUID, &fi, &fd,
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
	if lastEntryUUID.Valid {
		info.LastEntryUUID = lastEntryUUID.String
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
// user_message_count, file_size, file_mtime, optional file_hash, and token
// aggregates. All values are absolute (not deltas) so the
// update is idempotent on retry.
//
// is_automated is recomputed from the stored transcript's first
// user message (falling back to first_message for legacy rows)
// and the new user_message_count so that classifier additions
// reach rows that only ever take the incremental path. Without
// this, a row whose first parse predates a new pattern would stay
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
func updateSessionIncrementalTx(
	tx *sql.Tx, id string, update IncrementalSessionUpdate,
) error {
	var lastEntryUUID any
	if update.LastEntryUUID != "" {
		lastEntryUUID = update.LastEntryUUID
	}
	result, err := tx.Exec(`
		UPDATE sessions SET
			ended_at = COALESCE(?, ended_at),
			message_count = ?,
			user_message_count = ?,
			file_size = ?,
			file_mtime = ?,
			file_hash = COALESCE(?, file_hash),
			next_ordinal = ?,
			last_entry_uuid = ?,
			total_output_tokens = ?,
			peak_context_tokens = ?,
			has_total_output_tokens = ?,
			has_peak_context_tokens = ?,
			termination_status = NULL
		WHERE id = ?`,
		update.EndedAt, update.MsgCount, update.UserMsgCount,
		update.FileSize, update.FileMtime,
		update.FileHash,
		update.NextOrdinal, lastEntryUUID,
		update.TotalOutputTokens, update.PeakContextTokens,
		update.HasTotalOutputTokens, update.HasPeakContextTokens, id,
	)
	if err != nil {
		return fmt.Errorf(
			"incremental update session %s: %w", id, err,
		)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(
			"incremental update session %s rows affected: %w", id, err,
		)
	}
	if rows != 1 {
		return fmt.Errorf(
			"incremental update session %s: updated %d rows", id, rows,
		)
	}
	return nil
}

func (db *DB) UpdateSessionIncremental(
	id string, update IncrementalSessionUpdate,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning incremental update tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	err = updateSessionIncrementalTx(tx, id, update)
	if err != nil {
		return err
	}
	if err := updateSessionAutomationFromMessagesTx(tx, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing incremental update tx: %w", err)
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

// GetProjectByPath returns the stored project for the newest
// non-deleted session matching file_path.
func (db *DB) GetProjectByPath(path string) (project string, ok bool) {
	err := db.getReader().QueryRow(
		"SELECT project FROM sessions"+
			" WHERE file_path = ?"+
			" AND deleted_at IS NULL"+
			" ORDER BY file_mtime DESC LIMIT 1",
		path,
	).Scan(&project)
	if err != nil {
		return "", false
	}
	return project, true
}

// GetFileHashByPath returns the stored file_hash for the session
// matching file_path, preferring the most recently modified row.
// The bool is false when no row exists or the column is NULL. Used
// by the Shelley skip to compare a per-conversation content
// fingerprint alongside file_mtime.
func (db *DB) GetFileHashByPath(path string) (hash string, ok bool) {
	var h sql.NullString
	err := db.getReader().QueryRow(
		"SELECT file_hash FROM sessions"+
			" WHERE file_path = ?"+
			" ORDER BY file_mtime DESC LIMIT 1",
		path,
	).Scan(&h)
	if err != nil {
		return "", false
	}
	return h.String, h.Valid
}

// ListSessionIDsByFilePath returns non-deleted session IDs for a source path
// and agent. Used by parsers whose canonical session ID can change while the
// underlying source file remains the same.
func (db *DB) ListSessionIDsByFilePath(path, agent string) ([]string, error) {
	rows, err := db.getReader().Query(
		"SELECT id FROM sessions"+
			" WHERE file_path = ? AND agent = ? AND deleted_at IS NULL"+
			" ORDER BY id",
		path, agent,
	)
	if err != nil {
		return nil, fmt.Errorf("listing session IDs by file path: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning session ID by file path: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating session IDs by file path: %w", err)
	}
	return ids, nil
}

const storedSourcePathHintRootBatchSize = 100

// ListStoredSourcePathHints returns active source paths for agent whose stored
// file_path falls under any watched root. It is used by provider changed-path
// comparison to avoid losing sessions when the changed path is a sidecar or a
// root-scoped database event rather than the exact persisted source path.
func (db *DB) ListStoredSourcePathHints(
	agent string,
	roots []string,
) ([]string, error) {
	if agent == "" {
		return nil, nil
	}
	roots = normalizeStoredSourcePathHintRoots(roots)
	if len(roots) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{})
	var hints []string
	for start := 0; start < len(roots); start += storedSourcePathHintRootBatchSize {
		end := min(start+storedSourcePathHintRootBatchSize, len(roots))
		batch := roots[start:end]
		query, args := storedSourcePathHintQuery(agent, batch)
		rows, err := db.getReader().Query(query, args...)
		if err != nil {
			return nil, fmt.Errorf("listing stored source path hints: %w", err)
		}
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scanning stored source path hint: %w", err)
			}
			path = cleanStoredSourcePathHint(path)
			if !storedSourcePathHintInAnyRoot(path, batch) {
				continue
			}
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			hints = append(hints, path)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("closing stored source path hint rows: %w", err)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterating stored source path hints: %w", err)
		}
	}
	sort.Strings(hints)
	return hints, nil
}

func normalizeStoredSourcePathHintRoots(roots []string) []string {
	seen := make(map[string]struct{}, len(roots))
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		root = cleanStoredSourcePathHint(root)
		if root == "" || root == "." {
			continue
		}
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		out = append(out, root)
	}
	sort.Strings(out)
	return out
}

func storedSourcePathHintQuery(agent string, roots []string) (string, []any) {
	clauses := make([]string, 0, len(roots))
	args := []any{agent}
	for _, root := range roots {
		root = cleanStoredSourcePathHint(root)
		if root == "" || root == "." {
			continue
		}
		likeRoot := sqliteLikeEscape(root)
		clauses = append(clauses,
			`(file_path = ? OR
			  file_path LIKE ? ESCAPE '!' OR
			  file_path LIKE ? ESCAPE '!')`,
		)
		args = append(args,
			root,
			likeRoot+string(filepath.Separator)+"%",
			likeRoot+"#%",
		)
	}
	if len(clauses) == 0 {
		return `SELECT file_path FROM sessions WHERE 0`, nil
	}
	query := `SELECT file_path
		FROM sessions
		WHERE agent = ?
		  AND file_path IS NOT NULL
		  AND deleted_at IS NULL
		  AND (` + strings.Join(clauses, " OR ") + `)
		ORDER BY file_path`
	return query, args
}

func cleanStoredSourcePathHint(path string) string {
	return filepath.Clean(path)
}

func storedSourcePathHintInAnyRoot(path string, roots []string) bool {
	for _, root := range roots {
		if storedSourcePathHintInRoot(path, root) {
			return true
		}
	}
	return false
}

func storedSourcePathHintInRoot(path, root string) bool {
	path = cleanStoredSourcePathHint(path)
	root = cleanStoredSourcePathHint(root)
	if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
		return true
	}
	suffix, ok := strings.CutPrefix(path, root+"#")
	return ok &&
		storedSourcePathHintAllowsVirtualSuffix(root) &&
		suffix != "" &&
		!strings.ContainsAny(suffix, `/\`)
}

func storedSourcePathHintAllowsVirtualSuffix(root string) bool {
	return filepath.Ext(root) != ""
}

func sqliteLikeEscape(value string) string {
	value = strings.ReplaceAll(value, `!`, `!!`)
	value = strings.ReplaceAll(value, `%`, `!%`)
	value = strings.ReplaceAll(value, `_`, `!_`)
	return value
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

	aliasIDs, err := sessionAliasIDsTx(tx, "id = ?", id)
	if err != nil {
		return err
	}
	if err := deleteSessionMessagesTx(tx, id); err != nil {
		return fmt.Errorf(
			"pre-deleting session %s messages: %w",
			id, err,
		)
	}

	res, err := tx.Exec(
		"DELETE FROM sessions WHERE id = ?", id,
	)
	if err != nil {
		return fmt.Errorf("deleting session %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		if err := excludeSessionIDTx(tx, id); err != nil {
			return fmt.Errorf("excluding session %s: %w", id, err)
		}
		for _, aliasID := range aliasIDs {
			if err := excludeSessionIDTx(tx, aliasID); err != nil {
				return fmt.Errorf(
					"excluding session alias %s: %w", aliasID, err,
				)
			}
		}
	}
	return tx.Commit()
}

func excludeSessionIDTx(tx *sql.Tx, id string) error {
	_, err := tx.Exec(
		"INSERT OR IGNORE INTO excluded_sessions (id) VALUES (?)",
		id,
	)
	return err
}

func sessionAliasIDsTx(tx *sql.Tx, where string, args ...any) ([]string, error) {
	rows, err := tx.Query(
		"SELECT id, agent, file_path FROM sessions WHERE "+where,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("loading session alias state: %w", err)
	}
	defer rows.Close()

	var aliases []string
	for rows.Next() {
		var id, agent string
		var filePath sql.NullString
		if err := rows.Scan(&id, &agent, &filePath); err != nil {
			return nil, fmt.Errorf("scanning session alias state: %w", err)
		}
		if aliasID := vibeFallbackAliasID(id, agent, filePath); aliasID != "" {
			aliases = append(aliases, aliasID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating session alias state: %w", err)
	}
	return aliases, nil
}

func sessionIDsTx(tx *sql.Tx, where string, args ...any) ([]string, error) {
	rows, err := tx.Query(
		"SELECT id FROM sessions WHERE "+where,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("loading session ids: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning session id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating session ids: %w", err)
	}
	return ids, nil
}

func vibeFallbackAliasID(id, agent string, filePath sql.NullString) string {
	if agent != "vibe" || !filePath.Valid || filePath.String == "" {
		return ""
	}
	dir := filepath.Base(filepath.Dir(filePath.String))
	if !strings.HasPrefix(dir, "session_") {
		return ""
	}
	fallbackID := "vibe:" + dir
	if idx := strings.LastIndex(id, "vibe:"); idx > 0 {
		fallbackID = id[:idx] + fallbackID
	}
	if fallbackID == id {
		return ""
	}
	return fallbackID
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

	res, err := tx.Exec(
		`UPDATE sessions
		 SET deleted_at = deleted_at
		 WHERE id = ? AND deleted_at IS NOT NULL`,
		id,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"locking trashed session %s: %w", id, err,
		)
	}
	locked, _ := res.RowsAffected()
	if locked == 0 {
		return 0, nil
	}
	aliasIDs, err := sessionAliasIDsTx(
		tx, "id = ? AND deleted_at IS NOT NULL", id,
	)
	if err != nil {
		return 0, err
	}
	if err := deleteSessionMessagesTx(tx, id); err != nil {
		return 0, fmt.Errorf(
			"pre-deleting trashed session %s messages: %w",
			id, err,
		)
	}

	res, err = tx.Exec(
		"DELETE FROM sessions WHERE id = ? AND deleted_at IS NOT NULL",
		id,
	)
	if err != nil {
		return 0, fmt.Errorf("deleting trashed session %s: %w", id, err)
	}
	n, _ := res.RowsAffected()

	// Record in exclusion list so sync doesn't re-import.
	if err := excludeSessionIDTx(tx, id); err != nil {
		return 0, fmt.Errorf("excluding session %s: %w", id, err)
	}
	for _, aliasID := range aliasIDs {
		if err := excludeSessionIDTx(tx, aliasID); err != nil {
			return 0, fmt.Errorf(
				"excluding session alias %s: %w", aliasID, err,
			)
		}
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
	return EscapeLikePattern(s)
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
			&s.SecretLeakCount, &s.SecretsRulesVersion,
			&s.QualitySignalVersion,
			&s.ShortPromptCount, &s.UnstructuredStart,
			&s.MissingSuccessCriteriaCount,
			&s.MissingVerificationCount, &s.DuplicatePromptCount,
			&s.NoCodeContextCount, &s.RunawayToolLoopCount,
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
// Pass nil to clear a custom name (reverts to session_name or first_message).
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

	if _, err := tx.Exec(
		`UPDATE sessions
		 SET deleted_at = deleted_at
		 WHERE deleted_at IS NOT NULL`,
	); err != nil {
		return 0, fmt.Errorf("locking trashed sessions: %w", err)
	}

	aliasIDs, err := sessionAliasIDsTx(tx, "deleted_at IS NOT NULL")
	if err != nil {
		return 0, err
	}
	ids, err := sessionIDsTx(tx, "deleted_at IS NOT NULL")
	if err != nil {
		return 0, err
	}

	// Record all trashed session IDs before deleting.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO excluded_sessions (id)
		 SELECT id FROM sessions WHERE deleted_at IS NOT NULL`,
	); err != nil {
		return 0, fmt.Errorf("excluding trashed sessions: %w", err)
	}
	for _, aliasID := range aliasIDs {
		if err := excludeSessionIDTx(tx, aliasID); err != nil {
			return 0, fmt.Errorf(
				"excluding trashed session alias %s: %w", aliasID, err,
			)
		}
	}
	for _, id := range ids {
		if err := deleteSessionMessagesTx(tx, id); err != nil {
			return 0, fmt.Errorf(
				"pre-deleting trashed session %s messages: %w",
				id, err,
			)
		}
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
	if err := db.requireWritable(); err != nil {
		return 0, err
	}
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

		aliasIDs, err := sessionAliasIDsTx(
			tx, "id IN ("+placeholders+")", args...,
		)
		if err != nil {
			return 0, err
		}

		// Exclude only IDs that exist before we delete them.
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO excluded_sessions (id) "+
				"SELECT id FROM sessions WHERE id IN ("+placeholders+")",
			args...,
		); err != nil {
			return 0, fmt.Errorf("excluding batch: %w", err)
		}
		for _, aliasID := range aliasIDs {
			if err := excludeSessionIDTx(tx, aliasID); err != nil {
				return 0, fmt.Errorf(
					"excluding batch session alias %s: %w", aliasID, err,
				)
			}
		}
		for _, id := range batch {
			if err := deleteSessionMessagesTx(tx, id); err != nil {
				return 0, fmt.Errorf(
					"pre-deleting batch session %s messages: %w",
					id, err,
				)
			}
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
			&s.FirstMessage, &s.DisplayName, &s.SessionName, &s.StartedAt, &s.EndedAt,
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
			&s.SecretLeakCount, &s.SecretsRulesVersion,
			&s.QualitySignalVersion,
			&s.ShortPromptCount, &s.UnstructuredStart,
			&s.MissingSuccessCriteriaCount,
			&s.MissingVerificationCount, &s.DuplicatePromptCount,
			&s.NoCodeContextCount, &s.RunawayToolLoopCount,
			&s.DataVersion,
			&s.Cwd, &s.GitBranch,
			&s.SourceSessionID, &s.SourceVersion,
			&s.ParserMalformedLines, &s.IsTruncated,
			&s.DeletedAt, &s.TerminationStatus, &s.FilePath, &s.FileSize,
			&s.FileMtime, &s.NextOrdinal, &s.LastEntryUUID,
			&s.FileInode, &s.FileDevice,
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
