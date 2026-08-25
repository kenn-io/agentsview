package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	pathpkg "path"
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

// legacyDeletionCauseSourceMissing identifies rows written before source
// availability was separated from user deletion. It is used only to migrate
// or copy an older archive without preserving the accidental trash state.
const legacyDeletionCauseSourceMissing = "source_missing"

// subagentParentRepairQueueStateKey is the temporary JSON queue used by early
// builds of the nested-hierarchy change. RepairQueuedSubagentParents migrates
// it into subagent_parent_repair_queue before processing queued children.
const subagentParentRepairQueueStateKey = "subagent_parent_repair_queue_v1"

// sessionBaseCols is the column list for standard session queries
// (list, get). Keep in sync with scanSessionRow.
const sessionBaseCols = `id, project, machine, agent,
	agent_label, entrypoint, session_kind,
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
	transcript_fidelity,
	parser_malformed_lines, is_truncated,
	deleted_at, termination_status, transcript_revision, created_at,
	EXISTS (
		SELECT 1 FROM session_project_assignments spa
		WHERE spa.session_id = sessions.id
	) AS project_assigned`

// sessionPruneCols extends sessionBaseCols with file metadata
// needed by FindPruneCandidates.
const sessionPruneCols = `id, project, machine, agent,
	agent_label, entrypoint, session_kind,
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
	transcript_fidelity,
	parser_malformed_lines, is_truncated,
	deleted_at, termination_status, transcript_revision,
	file_path, file_size, created_at`

// sessionFullCols includes all columns for a complete session record.
const sessionFullCols = `id, project, machine, agent,
	agent_label, entrypoint, session_kind,
	first_message, display_name, session_name, started_at, ended_at,
	message_count, user_message_count,
	parent_session_id, parser_parent_session_id, relationship_type,
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
	transcript_fidelity,
	parser_malformed_lines, is_truncated,
	last_write_incremental,
	deleted_at, deletion_cause, source_missing_at,
	termination_status, file_path, file_size, file_mtime,
	next_ordinal, last_entry_uuid,
	file_inode, file_device,
	file_hash, local_modified_at, transcript_revision, created_at,
	EXISTS (
		SELECT 1 FROM session_project_assignments spa
		WHERE spa.session_id = sessions.id
	) AS project_assigned`

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
	return scanSessionRowWithSource(rs, false)
}

// scanSessionRowWithSource scans sessionBaseCols and an optional trailing
// file_path into a Session.
func scanSessionRowWithSource(rs rowScanner, includeSource bool) (Session, error) {
	var s Session
	targets := []any{
		&s.ID, &s.Project, &s.Machine, &s.Agent,
		&s.AgentLabel, &s.Entrypoint, &s.SessionKind,
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
		&s.TranscriptFidelity,
		&s.ParserMalformedLines, &s.IsTruncated,
		&s.DeletedAt, &s.TerminationStatus,
		&s.TranscriptRevision, &s.CreatedAt, &s.ProjectAssigned,
	}
	if includeSource {
		targets = append(targets, &s.FilePath)
	}
	err := rs.Scan(targets...)
	return s, err
}

const CurrentQualitySignalVersion = 3

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
		sessionAlias   `json:",inline"`
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
		sessionAlias   `json:",inline"`
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
	ID                    string  `json:"id"`
	Project               string  `json:"project"`
	Machine               string  `json:"machine"`
	Agent                 string  `json:"agent"`
	AgentLabel            string  `json:"agent_label,omitempty"`
	Entrypoint            string  `json:"entrypoint,omitempty"`
	SessionKind           string  `json:"session_kind,omitempty"`
	FirstMessage          *string `json:"first_message"`
	DisplayName           *string `json:"display_name,omitempty"`
	SessionName           *string `json:"-"`
	StartedAt             *string `json:"started_at"`
	EndedAt               *string `json:"ended_at"`
	MessageCount          int     `json:"message_count"`
	UserMessageCount      int     `json:"user_message_count"`
	ParentSessionID       *string `json:"parent_session_id,omitempty"`
	ParserParentSessionID *string `json:"-"`
	RelationshipType      string  `json:"relationship_type,omitempty"`
	TotalOutputTokens     int     `json:"total_output_tokens"`
	PeakContextTokens     int     `json:"peak_context_tokens"`
	HasTotalOutputTokens  bool    `json:"has_total_output_tokens"`
	HasPeakContextTokens  bool    `json:"has_peak_context_tokens"`
	IsAutomated           bool    `json:"is_automated"`

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
	ProjectAssigned             bool            `json:"project_assigned,omitempty"`
	SourceSessionID             string          `json:"source_session_id,omitempty"`
	SourceVersion               string          `json:"source_version,omitempty"`
	TranscriptFidelity          string          `json:"transcript_fidelity,omitempty"`
	ParserMalformedLines        int             `json:"parser_malformed_lines,omitzero"`
	IsTruncated                 bool            `json:"is_truncated,omitzero"`

	DeletedAt         *string `json:"deleted_at,omitempty"`
	DeletionCause     *string `json:"-"`
	SourceMissingAt   *string `json:"-"`
	TerminationStatus *string `json:"termination_status,omitempty"`
	FilePath          *string `json:"file_path,omitempty"`
	FileSize          *int64  `json:"file_size,omitempty"`
	FileMtime         *int64  `json:"file_mtime,omitempty"`
	NextOrdinal       int     `json:"-"`
	LastEntryUUID     *string `json:"-"`
	// ClaudeLinearParse is SQLite-only sync bookkeeping: whether the
	// Claude full parser fell back to linear processing for this file
	// (nil = unknown/legacy or non-Claude). Linearity is monotonic
	// across appends, so the incremental parser skips fork detection
	// for linear-bound transcripts. Not mirrored to PG/DuckDB.
	ClaudeLinearParse *bool `json:"-"`
	// LastWriteIncremental is SQLite-only sync bookkeeping (like
	// NextOrdinal): true when the last write to this row went through
	// the incremental-append path (updateSessionIncrementalTx) instead
	// of a full re-normalization (upsertSessionArgs, which always resets
	// it to false). It is consumed only by parse-diff to classify benign
	// incremental-vs-full skew and is json:"-" so it never leaks through
	// the HTTP session API. Deliberately not mirrored to PG/DuckDB: their
	// push column lists omit the whole sync-bookkeeping cluster.
	LastWriteIncremental bool    `json:"-"`
	FileInode            *int64  `json:"file_inode,omitempty"`
	FileDevice           *int64  `json:"file_device,omitempty"`
	FileHash             *string `json:"file_hash,omitempty"`
	LocalModifiedAt      *string `json:"local_modified_at,omitempty"`
	TranscriptRevision   *string `json:"transcript_revision,omitempty"`
	CreatedAt            string  `json:"created_at"`

	// PreserveSessionName is transient write intent. Parser write paths set it
	// when a provider supplied no authoritative title signal, so an upsert
	// retains an existing session_name while a new row can still use the
	// parser's fallback name.
	PreserveSessionName bool `json:"-"`
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
	Project        string
	ExcludeProject string // exclude sessions with this project name
	Machine        string
	// GitBranch is a branchListSep-joined list of opaque (project, branch) tokens (EncodeBranchFilterToken).
	GitBranch       string
	Agent           string
	Date            string // date overlapped by session activity, YYYY-MM-DD
	DateFrom        string // activity range start (inclusive)
	DateTo          string // activity range end (inclusive)
	Timezone        string // IANA timezone for date filters; empty means UTC
	ActiveSince     string // ISO-8601 timestamp; filters on most recent activity
	MinMessages     int    // message_count >= N (0 = no filter)
	MaxMessages     int    // message_count <= N (0 = no filter)
	MinUserMessages int    // user_message_count >= N (0 = no filter)
	ExcludeOneShot  bool   // exclude sessions with user_message_count <= 1
	// ChildExemptOneShot carves child sessions (a sidebar-child
	// relationship_type or a non-empty parent_session_id) out of the
	// ExcludeOneShot gate. Set only by the semantic/hybrid content-search
	// session scope: nearly all non-automated subagent transcripts carry a
	// single user message, so the one-shot gate would otherwise drop the
	// subordinate units the Scope filter exists to govern. Top-level
	// sessions keep the one-shot exclusion unchanged; every other caller
	// (session list, substring/regex/fts search) leaves this false.
	ChildExemptOneShot bool
	ExcludeAutomated   bool     // exclude sessions where is_automated = 1
	AutomatedScope     string   // "", "human", "all", or "automated"
	IncludeChildren    bool     // include subagent sessions (for sidebar grouping)
	IncludeOrphans     bool     // promote orphan child rows to sidebar roots
	IncludeSource      bool     // include the session source file path in list rows
	Outcome            []string // filter by outcome values
	HealthGrade        []string // filter by health grade values
	MinToolFailures    *int     // minimum tool_failure_signal_count
	HasSecret          bool     // only sessions with current secret_leak_count > 0
	Starred            bool     // only sessions starred by the user
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
	ID                 string  `json:"id"`
	ParentSessionID    *string `json:"parent_session_id,omitempty"`
	RelationshipType   string  `json:"relationship_type,omitempty"`
	Project            string  `json:"project"`
	Machine            string  `json:"machine"`
	Agent              string  `json:"agent"`
	AgentLabel         string  `json:"agent_label,omitempty"`
	Entrypoint         string  `json:"entrypoint,omitempty"`
	SessionKind        string  `json:"session_kind,omitempty"`
	DisplayName        *string `json:"display_name,omitempty"`
	StartedAt          *string `json:"started_at"`
	EndedAt            *string `json:"ended_at"`
	CreatedAt          string  `json:"created_at"`
	TerminationStatus  *string `json:"termination_status,omitempty"`
	MessageCount       int     `json:"message_count"`
	UserMessageCount   int     `json:"user_message_count"`
	TranscriptRevision *string `json:"transcript_revision,omitempty"`
	IsAutomated        bool    `json:"is_automated"`
	IsTeammate         bool    `json:"is_teammate"`
}

type SidebarSessionIndex struct {
	Sessions   []SidebarSessionIndexRow `json:"sessions"`
	NextCursor string                   `json:"next_cursor,omitempty"`
	// Total counts canonical root groups matching the filter. Sessions may
	// contain additional descendant rows needed to render those groups.
	Total int `json:"total"`
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

	columns := sessionBaseCols
	if f.IncludeSource {
		columns += ", file_path"
	}
	query := "SELECT " + columns +
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

	sessions, err := scanSessionRowsWithSource(rows, f.IncludeSource)
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
	rootFilter := f
	rootFilter.IncludeChildren = false
	rootWhere, rootArgs := buildSessionBaseFilter(rootFilter)
	canonicalRootWhere := buildCanonicalRootWhere(f.IncludeOrphans)
	var total int
	countQuery := "SELECT COUNT(*) FROM sessions WHERE " +
		rootWhere + " AND " + canonicalRootWhere
	if err := db.getReader().QueryRowContext(
		ctx, countQuery, rootArgs...,
	).Scan(&total); err != nil {
		return SidebarSessionIndex{},
			fmt.Errorf("counting sidebar roots: %w", err)
	}

	where, args := buildSessionFilter(f)
	query := `
		SELECT
			id,
			parent_session_id,
			relationship_type,
			project,
			machine,
			agent,
			agent_label,
			entrypoint,
			session_kind,
			COALESCE(display_name, session_name) AS display_name,
			started_at,
			ended_at,
			created_at,
			termination_status,
			message_count,
			user_message_count,
			transcript_revision,
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
		Total:    total,
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
			&row.AgentLabel,
			&row.Entrypoint,
			&row.SessionKind,
			&row.DisplayName,
			&row.StartedAt,
			&row.EndedAt,
			&row.CreatedAt,
			&row.TerminationStatus,
			&row.MessageCount,
			&row.UserMessageCount,
			&row.TranscriptRevision,
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
	childAutomationPred := automationScopePredicate(f, SQLiteQueryDialect(), "s")
	childAutomationWhere := ""
	if childAutomationPred != "" {
		childAutomationWhere = " AND " + childAutomationPred
	}

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
					  ` + childAutomationWhere + `
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
			  ` + childAutomationWhere + `
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
			  ` + childAutomationWhere + `
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
			s.agent_label,
			s.entrypoint,
			s.session_kind,
			COALESCE(s.display_name, s.session_name) AS display_name,
			s.started_at,
			s.ended_at,
			s.created_at,
			s.termination_status,
			s.message_count,
			s.user_message_count,
			s.transcript_revision,
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
			&row.AgentLabel,
			&row.Entrypoint,
			&row.SessionKind,
			&row.DisplayName,
			&row.StartedAt,
			&row.EndedAt,
			&row.CreatedAt,
			&row.TerminationStatus,
			&row.MessageCount,
			&row.UserMessageCount,
			&row.TranscriptRevision,
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
	s, err := db.getSessionFullUncoalesced(ctx, id)
	if err != nil || s == nil {
		return s, err
	}
	// Expose the visible name (user rename, else agent session name)
	// like the PG and DuckDB GetSessionFull and the sqlite base reads.
	// The coalesce happens post-scan because sessionFullCols is shared
	// with ListSessionsModifiedBetween, whose push consumers must see
	// display_name and session_name unmerged.
	if s.DisplayName == nil {
		s.DisplayName = s.SessionName
	}
	return s, nil
}

// GetArtifactExportSession returns raw user- and agent-owned session names so
// canonical manifests do not publish session_name as a user display_name.
func (db *DB) GetArtifactExportSession(
	ctx context.Context, id string,
) (*Session, error) {
	return db.getSessionFullUncoalesced(ctx, id)
}

func (db *DB) getSessionFullUncoalesced(
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
		&s.AgentLabel, &s.Entrypoint, &s.SessionKind,
		&s.FirstMessage, &s.DisplayName, &s.SessionName, &s.StartedAt, &s.EndedAt,
		&s.MessageCount, &s.UserMessageCount,
		&s.ParentSessionID, &s.ParserParentSessionID, &s.RelationshipType,
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
		&s.TranscriptFidelity,
		&s.ParserMalformedLines, &s.IsTruncated,
		&s.LastWriteIncremental,
		&s.DeletedAt, &s.DeletionCause, &s.SourceMissingAt,
		&s.TerminationStatus, &s.FilePath, &s.FileSize,
		&s.FileMtime, &s.NextOrdinal, &s.LastEntryUUID,
		&s.FileInode, &s.FileDevice,
		&s.FileHash, &s.LocalModifiedAt,
		&s.TranscriptRevision, &s.CreatedAt, &s.ProjectAssigned,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting session full %s: %w", id, err)
	}
	return &s, nil
}

// GetSessionName returns the raw agent-provided session name without loading
// the rest of the session row. A NULL name is reported as an empty string for
// an existing row; found distinguishes that case from a missing session.
func (db *DB) GetSessionName(
	ctx context.Context, id string,
) (name string, found bool, err error) {
	var stored sql.NullString
	err = db.getReader().QueryRowContext(
		ctx,
		"SELECT session_name FROM sessions WHERE id = ?",
		id,
	).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("getting session name %s: %w", id, err)
	}
	return stored.String, true, nil
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
		"SELECT 1 FROM sessions WHERE id = ?"+
			" AND deleted_at IS NOT NULL", id,
	).Scan(&n)
	return n == 1
}

// HasTrashedSessionByFilePath returns true when a source path already belongs
// to a trashed row for this agent.
func (db *DB) HasTrashedSessionByFilePath(path, agent string) bool {
	var n int
	_ = db.getReader().QueryRow(
		"SELECT 1 FROM sessions"+
			" WHERE file_path = ? AND agent = ?"+
			" AND deleted_at IS NOT NULL"+
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

const insertSessionSQL = `
		INSERT INTO sessions (
			id, project, machine, agent, first_message, session_name,
			agent_label, entrypoint, session_kind,
			started_at, ended_at, message_count,
			user_message_count, parent_session_id,
			parser_parent_session_id,
			relationship_type,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens,
			is_automated,
			termination_status,
			cwd, git_branch, source_session_id,
			source_version, transcript_fidelity,
			parser_malformed_lines,
			is_truncated,
			last_write_incremental,
			file_path, file_size, file_mtime,
			next_ordinal, last_entry_uuid, claude_linear_parse,
			file_inode, file_device, file_hash
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// insertSessionIfAbsentSQL inserts a session only when its id does not already
// exist, leaving an existing row untouched.
const insertSessionIfAbsentSQL = insertSessionSQL + `
		ON CONFLICT(id) DO NOTHING`

const upsertSessionBaseSQL = insertSessionSQL + `
		ON CONFLICT(id) DO UPDATE SET
			project = excluded.project,
			machine = excluded.machine,
			agent = excluded.agent,
			agent_label = excluded.agent_label,
			entrypoint = excluded.entrypoint,
			session_kind = excluded.session_kind,
			first_message = excluded.first_message,
			-- Parser writes resolve session_name before this statement: an
			-- explicit title (including blank) replaces it, while an absent
			-- provider signal may carry the existing value forward. display_name
			-- is the user override and is only touched by RenameSession.
			session_name = excluded.session_name,
			started_at = excluded.started_at,
			ended_at = excluded.ended_at,
			message_count = excluded.message_count,
			user_message_count = excluded.user_message_count,
			parent_session_id = excluded.parent_session_id,
			parser_parent_session_id = excluded.parser_parent_session_id,
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
			transcript_fidelity = excluded.transcript_fidelity,
			parser_malformed_lines = excluded.parser_malformed_lines,
			is_truncated = excluded.is_truncated,
			-- last_write_incremental is deliberately NOT touched on conflict.
			-- A bare upsert rewrites only the session row, not the message
			-- rows, so it is not a re-normalization: the append-only full-parse
			-- path (Claude/Codex, ReplaceMessages=false) upserts the session and
			-- appends new messages while leaving earlier incrementally written
			-- rows in place. Clearing the marker here would make parse-diff
			-- report that still-present benign skew as real drift. The marker is
			-- reset only by a genuine full message replacement
			-- (resetIncrementalMarkerTx), and seeded false on fresh INSERT.
			file_path = excluded.file_path,
			file_size = excluded.file_size,
			file_mtime = excluded.file_mtime,
			next_ordinal = excluded.next_ordinal,
			last_entry_uuid = excluded.last_entry_uuid,
			-- COALESCE keeps a known linearity verdict when a caller
			-- upserts without one (e.g. non-parse session writers).
			claude_linear_parse = COALESCE(
				excluded.claude_linear_parse, sessions.claude_linear_parse),
			file_inode = excluded.file_inode,
			file_device = excluded.file_device,
			file_hash = excluded.file_hash`

const upsertSessionSQL = upsertSessionBaseSQL + `,
			source_missing_at = NULL`

func sessionIsAutomated(s Session) bool {
	return s.IsAutomated ||
		IsAutomatedSessionMetadata(s.Agent, s.SessionKind) ||
		(s.UserMessageCount <= 1 &&
			s.FirstMessage != nil &&
			IsAutomatedSession(*s.FirstMessage))
}

func parserParentSessionID(s Session) *string {
	if s.ParserParentSessionID != nil {
		return s.ParserParentSessionID
	}
	return s.ParentSessionID
}

func upsertSessionArgs(s Session) []any {
	return []any{
		s.ID, s.Project, s.Machine, s.Agent, s.FirstMessage, s.SessionName,
		s.AgentLabel, s.Entrypoint, s.SessionKind,
		s.StartedAt, s.EndedAt, s.MessageCount,
		s.UserMessageCount, s.ParentSessionID, parserParentSessionID(s),
		s.RelationshipType,
		s.TotalOutputTokens, s.PeakContextTokens,
		s.HasTotalOutputTokens, s.HasPeakContextTokens,
		sessionIsAutomated(s),
		s.TerminationStatus,
		s.Cwd, s.GitBranch, s.SourceSessionID,
		s.SourceVersion, s.TranscriptFidelity,
		s.ParserMalformedLines,
		s.IsTruncated,
		// last_write_incremental is seeded false on fresh INSERT: a brand-new
		// row starts fully normalized. On conflict the column is left as-is
		// (see upsertSessionSQL) because a bare upsert does not re-normalize
		// the stored messages; only a full message replacement clears it.
		false,
		s.FilePath, s.FileSize, s.FileMtime,
		s.NextOrdinal, s.LastEntryUUID, s.ClaudeLinearParse,
		s.FileInode, s.FileDevice, s.FileHash,
	}
}

// UpsertSession inserts or updates a session.
// Sessions that were permanently deleted (in excluded_sessions)
// or currently in the trash are rejected.
func (db *DB) UpsertSession(s Session) error {
	_, err := db.upsertSession(s, true)
	return err
}

// UpsertSessionPendingContent inserts or updates the session row without
// clearing source-missing state. Full content writers call
// ClearSessionSourceMissing only after every required dependent write lands.
// The returned bool reports whether the row was source-missing before the
// upsert, so callers can replace rather than append its retained content.
func (db *DB) UpsertSessionPendingContent(s Session) (bool, error) {
	result, err := db.upsertSession(s, false)
	return result.sourceMissing, err
}

func (db *DB) upsertSession(
	s Session, reviveSourceMissing bool,
) (sessionUpsertResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	writer := db.getWriter()
	return upsertSessionExec(
		writer.Exec,
		writer.QueryRow,
		s,
		reviveSourceMissing,
	)
}

type sessionUpsertResult struct {
	inserted        bool
	sourceMissing   bool
	previousProject string
	currentProject  string
}

func upsertSessionExec(
	exec func(string, ...any) (sql.Result, error),
	queryRow func(string, ...any) rowScanner,
	s Session,
	reviveSourceMissing bool,
) (sessionUpsertResult, error) {
	_ = ValidateAndSanitize(&s, nil, nil)

	// Check exclusion/trash state under the write lock to avoid a race with
	// concurrent DeleteSession/EmptyTrash/RestoreSession.
	var excluded int
	err := queryRow(
		"SELECT 1 FROM excluded_sessions WHERE id = ?", s.ID,
	).Scan(&excluded)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return sessionUpsertResult{},
			fmt.Errorf("checking exclusion for %s: %w", s.ID, err)
	}
	if excluded == 1 {
		return sessionUpsertResult{}, ErrSessionExcluded
	}
	var previousProject string
	var previousSessionName sql.NullString
	var deletedAt, sourceMissingAt sql.NullString
	err = queryRow(
		"SELECT project, session_name, deleted_at, source_missing_at "+
			"FROM sessions WHERE id = ?", s.ID,
	).Scan(
		&previousProject, &previousSessionName, &deletedAt, &sourceMissingAt,
	)
	result := sessionUpsertResult{
		inserted:        errors.Is(err, sql.ErrNoRows),
		previousProject: previousProject,
		currentProject:  s.Project,
	}
	if err != nil && !result.inserted {
		return sessionUpsertResult{},
			fmt.Errorf("checking session %s: %w", s.ID, err)
	}
	if s.PreserveSessionName && !result.inserted {
		if previousSessionName.Valid {
			name := previousSessionName.String
			s.SessionName = &name
		} else {
			s.SessionName = nil
		}
	}
	if deletedAt.Valid {
		return sessionUpsertResult{}, ErrSessionTrashed
	}
	result.sourceMissing = sourceMissingAt.Valid

	// data_version is intentionally NOT advanced here. The
	// caller must call SetSessionDataVersion only after the
	// associated message rewrite succeeds, so a transient
	// failure to write messages doesn't mark the file as
	// up-to-date and starve the rewrite on the next sync.
	// New rows are seeded with 0 (the default) and bumped to
	// the current version once their messages land.
	query := upsertSessionBaseSQL
	if reviveSourceMissing {
		query = upsertSessionSQL
	}
	_, err = exec(
		query,
		upsertSessionArgs(s)...,
	)
	if err != nil {
		return sessionUpsertResult{},
			fmt.Errorf("upserting session %s: %w", s.ID, err)
	}
	return result, nil
}

// ClearSessionSourceMissing clears source-missing state after its replacement
// session row, messages, usage events, and data version have all been persisted
// successfully. User trash is never affected.
func (db *DB) ClearSessionSourceMissing(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(`
		UPDATE sessions
		SET source_missing_at = NULL,
		    local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ? AND source_missing_at IS NOT NULL`, id)
	if err != nil {
		return fmt.Errorf("clearing source-missing state for session %s: %w", id, err)
	}
	return nil
}

// insertSessionIfAbsent inserts a session only when no row with its id exists,
// leaving any existing row untouched (ON CONFLICT DO NOTHING). It is used for
// placeholder rows (e.g. recall import) that must never overwrite a real
// session synced concurrently. Permanently-excluded sessions are still
// rejected so a placeholder cannot resurrect them.
func (db *DB) insertSessionIfAbsent(ctx context.Context, s Session) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	var excluded int
	_ = db.getWriter().QueryRowContext(
		ctx, "SELECT 1 FROM excluded_sessions WHERE id = ?", s.ID,
	).Scan(&excluded)
	if excluded == 1 {
		return ErrSessionExcluded
	}
	// A soft-deleted (trashed) session would satisfy ON CONFLICT DO NOTHING and
	// silently leave the import attached to a hidden session. Reject it like
	// UpsertSession does, under the same lock to avoid a restore/delete race.
	var trashed int
	_ = db.getWriter().QueryRowContext(
		ctx, "SELECT 1 FROM sessions WHERE id = ? AND deleted_at IS NOT NULL", s.ID,
	).Scan(&trashed)
	if trashed == 1 {
		return ErrSessionTrashed
	}

	if _, err := db.getWriter().ExecContext(
		ctx, insertSessionIfAbsentSQL, upsertSessionArgs(s)...,
	); err != nil {
		return fmt.Errorf("inserting session %s if absent: %w", s.ID, err)
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

// subagentSpawnerExpr resolves the parent of the session aliased `s`
// from the authoritative tool_calls spawn edges (recorded by the parser
// from toolUseResult.agentId).
//
// A child is normally referenced by exactly one edge, but copied or
// forked history can leave several sessions claiming the same child.
// Resolution must then be a pure function of the stored edges and never
// of the order they arrived, because any single sync may observe only a
// subset of them: a link written from a partial view has to self-correct
// once the rest land, rather than being locked in.
//
// A fork derives from the session it was forked from, so it always
// starts later. Ordering candidates by start time therefore resolves to
// the original spawner from any subset, and converges on the next sync.
//
// started_at has to be NORMALIZED before it is ordered, not compared
// raw. It is TEXT written by timeutil.Format, i.e. time.RFC3339Nano,
// which STRIPS trailing zeros from the fractional second: a whole-second
// start is stored '...T00:00:00Z' while a later one is stored
// '...T00:00:00.1Z', and '.' (0x2E) sorts before 'Z' (0x5A). Raw lexical
// order is therefore not chronological in exactly the case that matters
// here — it ranks a whole-second spawner behind every fractional one and
// would hand the child to a copy. strftime re-renders each value as
// fixed-width '...T00:00:00.000Z', for which lexical order IS
// chronological (to the millisecond; anything closer than that falls
// through to the id key below).
//
// The remaining keys keep that total order well defined:
//   - strftime yields NULL for a started_at that is unset, empty or
//     malformed, and SQLite sorts NULL first, so an unknown start time
//     would otherwise outrank every real one — the leading IS NULL key
//     pushes those candidates last instead.
//   - among candidates whose normalized start times TIE (identical
//     timestamps — a copy shares its source's; sub-millisecond gaps
//     truncated away by %f; or all unknown), the parser-established
//     parent wins if it is one of them. parser_parent_session_id is
//     immutable linker provenance: unlike the effective parent, a
//     copied-only linker pass cannot overwrite it and then make its own
//     provisional choice sticky when the real edge arrives later.
//     Whenever start times DO differ, chronology still decides
//     unconditionally, so parser provenance never preserves a parent
//     that stronger evidence contradicts.
//   - the session id breaks any remaining tie (no parser-established
//     parent among the tied candidates), so resolution still never
//     depends on whichever edge SQLite visited first.
//
// Ranking unknown start times last is a deliberate trade-off: a real
// spawner with no usable started_at loses to a copied spawner that has
// one. Protecting it unconditionally would make the stored parent
// outrank fresher evidence, which is the ingestion-order dependence
// this resolution exists to remove. If the spawner's start time later
// becomes known, its row update re-enters linking and the child
// self-corrects.
//
// The LEFT JOIN keeps an edge whose spawner has no sessions row as a
// last-resort candidate (it sorts with the unknown start times) rather
// than discarding it.
//
// A self-referential edge (tc.session_id = tc.subagent_session_id, only
// reachable from a corrupt or crafted transcript) is never a candidate: a
// session cannot spawn itself, and treating the edge as evidence would
// make the row its own parent and drop it from the hierarchy roots. The
// same guard is applied wherever spawn edges are enumerated (the driver
// sets below, clearDanglingSubagentParentQuery, SubagentChildSessionIDs).
const subagentSpawnerExpr = `
		SELECT tc.session_id
		FROM tool_calls tc
		LEFT JOIN sessions ps ON ps.id = tc.session_id
		WHERE tc.subagent_session_id = s.id
		AND tc.session_id IS NOT s.id
		ORDER BY
			(strftime('%Y-%m-%dT%H:%M:%fZ', ps.started_at) IS NULL),
			strftime('%Y-%m-%dT%H:%M:%fZ', ps.started_at),
			(tc.session_id IS NOT s.parser_parent_session_id),
			tc.session_id
		LIMIT 1`

// linkSubagentSessionsQuery re-points every session that carries a spawn edge
// at the spawner subagentSpawnerExpr resolves for it.
//
// The statement is driven from the edges rather than from sessions: `s.id IN
// (SELECT tc.subagent_session_id ...)` lets SQLite seek the partial index
// idx_tool_calls_subagent and then look each child up by primary key, so a
// sync's linking cost scales with the number of spawn edges instead of with
// the size of the archive. The equivalent EXISTS(...) form reads the same but
// plans as a full scan of sessions with a correlated probe per row, which is
// what makes linking on a large archive expensive even when nothing changed.
// The IS NOT NULL filter keeps the candidate list free of NULLs, so the IN
// comparison cannot go three-valued (the partial index carries exactly those
// rows, so the filter is free).
const linkSubagentSessionsQuery = `
	UPDATE sessions AS s
	SET parent_session_id = (` + subagentSpawnerExpr + `
	),
	relationship_type = 'subagent',
	local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
	-- The tool_calls edge (from toolUseResult.agentId) records the actual
	-- spawn, authoritative over the path-derived parent set at parse time.
	-- Nested subagents (depth >= 2) live flat in <main>/subagents/, so path
	-- derivation pins them to the main session AND tags them 'subagent';
	-- the old relationship_type != 'subagent' guard skipped them, leaving
	-- the hierarchy flat.
	--
	-- Update when EITHER the row is not yet 'subagent' (upgrade
	-- continuation/fork/empty) OR the resolved spawner differs from the
	-- stored parent (null-safe IS NOT, so a subagent with a NULL parent is
	-- still linked). Because the resolved spawner depends only on the
	-- stored edges, a row already pointing at it matches neither branch:
	-- linking stays a no-op and does not churn local_modified_at.
	WHERE s.id IN (
		SELECT tc.subagent_session_id FROM tool_calls tc
		WHERE tc.subagent_session_id IS NOT NULL
		AND tc.session_id IS NOT tc.subagent_session_id
	)
	AND (
		relationship_type != 'subagent'
		OR parent_session_id IS NOT (` + subagentSpawnerExpr + `
		)
	)`

// LinkSubagentSessions sets parent_session_id and
// relationship_type on sessions referenced by
// tool_calls.subagent_session_id (the authoritative spawn edge).
// A session is updated when it is not yet tagged 'subagent' (e.g.
// a Zencoder session classified as "continuation" from a header
// parentId that is actually a spawned subagent) OR when its stored
// parent disagrees with the spawn edge. The latter re-parents
// nested subagents (depth >= 2), which the parser pins to the main
// session because Claude Code stores every subagent flat under
// <main>/subagents/. Already-correct subagents are left untouched.
//
// See subagentSpawnerExpr for how a child claimed by more than one
// spawner is resolved, and linkSubagentSessionsQuery for why the
// statement is driven from the spawn-edge index: every sync calls this,
// so its cost has to track the number of spawn edges rather than the
// size of the archive. Per-event paths (single-session watcher syncs)
// use LinkSubagentSessionsForSessions instead, which further bounds the
// pass to the changed batch.
func (db *DB) LinkSubagentSessions() error {
	return db.LinkSubagentSessionsContext(context.Background())
}

// LinkSubagentSessionsContext is LinkSubagentSessions with caller-controlled
// cancellation for bounded sync paths.
func (db *DB) LinkSubagentSessionsContext(ctx context.Context) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.repairLegacySelfParentedSessions(ctx); err != nil {
		return err
	}

	// local_modified_at is bumped so the sync_marker trigger fires and
	// push targets (PostgreSQL and the DuckDB mirror) re-select the linked
	// session: parent_session_id and relationship_type are mirrored
	// columns, but neither is a sync_marker signal, so linking an older
	// session after a mirror's cutoff would otherwise never re-push it
	// (see updateSessionSignalsTx and ReplaceSessionUsageEvents for the
	// same pattern).
	_, err := db.getWriter().ExecContext(ctx, linkSubagentSessionsQuery)
	if err != nil {
		return fmt.Errorf("linking subagent sessions: %w", err)
	}
	return nil
}

// selfParentRepairStateKey marks the archive as having cleared the
// self-parented rows that linking produced before self-referential spawn
// edges were ignored. See repairLegacySelfParentedSessions.
const selfParentRepairStateKey = "subagent_self_parent_repair_v1"

// clearSelfParentedSessionsSQL repairs any session that points at itself.
// The linker never writes parser_parent_session_id, so when it holds a
// different session that is the path-derived parent the legacy linker
// overwrote, and the row gets it back; otherwise (no parser parent, or the
// column was backfilled from an already self-parented row) the parent is
// cleared. Row-level ingest sanitization (SanitizeSession) and the
// self-edge guard in subagentSpawnerExpr keep new rows out of that state,
// so this only ever matches rows written by earlier builds. The
// archive-rebuild orphan copy applies the same repair to copied rows
// (clearCopiedSelfParents).
const clearSelfParentedSessionsSQL = `
	UPDATE sessions
	SET parent_session_id = NULLIF(parser_parent_session_id, id),
	local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
	WHERE parent_session_id IS id`

// repairLegacySelfParentedSessions runs clearSelfParentedSessionsSQL once
// per archive. Earlier builds let a self-referential spawn edge resolve to
// the session itself, and because linking now ignores those edges the
// affected rows would never re-enter the linker. The pass is a full scan
// of sessions (parent_session_id IS id cannot use idx_sessions_parent), so
// it is gated by a pg_sync_state marker rather than repeated on every sync.
// The marker and the clear commit together so a failed run retries.
func (db *DB) repairLegacySelfParentedSessions(ctx context.Context) error {
	writer := db.getWriter()
	var repaired int
	if err := writer.QueryRowContext(
		ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_sync_state WHERE key = ?)",
		selfParentRepairStateKey,
	).Scan(&repaired); err != nil {
		return fmt.Errorf("checking self-parent repair state: %w", err)
	}
	if repaired != 0 {
		return nil
	}
	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning self-parent repair: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, clearSelfParentedSessionsSQL); err != nil {
		return fmt.Errorf("clearing legacy self-parented sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pg_sync_state (key, value) VALUES (?, '1')
		ON CONFLICT(key) DO NOTHING`, selfParentRepairStateKey); err != nil {
		return fmt.Errorf("recording self-parent repair state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing self-parent repair: %w", err)
	}
	return nil
}

// linkSubagentSessionsForSessionsQuery is linkSubagentSessionsQuery
// restricted to the children a batch of changed sessions can affect. ph is
// an inPlaceholders list bound twice: once per branch of the UNION.
//
// A changed session can alter linking in exactly two ways, one per branch:
//   - tc.session_id IN ph: the session's transcript carries spawn edges, so
//     every child it claims is re-resolved (idx_tool_calls_session seek).
//     A conflicting edge always arrives through its spawner's transcript,
//     so this branch is what lets a provisional link self-correct — the
//     spawner whose edge just landed is in the batch by definition.
//   - tc.subagent_session_id IN ph: the session is itself a child whose row
//     was rewritten (e.g. re-parsed with a path-derived parent), so its own
//     link is re-resolved (idx_tool_calls_subagent seek).
//
// Sessions outside both branches have unchanged edges, and resolution is a
// pure function of the stored edges (see subagentSpawnerExpr), so their
// links cannot have changed — skipping them is what keeps a single-session
// watcher sync bounded by the batch instead of the archive.
func linkSubagentSessionsForSessionsQuery(ph string) string {
	return `
	UPDATE sessions AS s
	SET parent_session_id = (` + subagentSpawnerExpr + `
	),
	relationship_type = 'subagent',
	local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
	WHERE s.id IN (
		SELECT tc.subagent_session_id FROM tool_calls tc
		WHERE tc.session_id IN ` + ph + `
		AND tc.subagent_session_id IS NOT NULL
		AND tc.session_id IS NOT tc.subagent_session_id
		UNION
		SELECT tc.subagent_session_id FROM tool_calls tc
		WHERE tc.subagent_session_id IN ` + ph + `
		AND tc.session_id IS NOT tc.subagent_session_id
	)
	AND (
		relationship_type != 'subagent'
		OR parent_session_id IS NOT (` + subagentSpawnerExpr + `
		)
	)`
}

// clearDanglingSubagentParentQuery repairs a captured former child whose LAST spawn
// edge was removed together with its spawner: both UNION branches of the
// linking statement select from remaining tool_calls, so an edge-less child
// can never be re-resolved there, and its parent now points at a session
// that no longer exists. Clearing is deliberately restricted to that
// dangling case — when the stored parent still exists, only the edge is
// gone, and nothing distinguishes an edge-derived parent (stale) from a
// path-derived one (still valid, e.g. a Claude subagent whose directory
// proves membership), so the safer failure mode is to keep the historical
// claim. relationship_type stays 'subagent'; if an edge reappears, linking
// re-parents the NULL-parent row (see the null-safe IS NOT predicate).
func clearDanglingSubagentParentQuery(ph string) string {
	return `
	UPDATE sessions AS s
	SET parent_session_id = NULL,
	local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
	WHERE s.id IN ` + ph + `
	AND s.relationship_type = 'subagent'
	AND s.parent_session_id IS NOT NULL
	AND NOT EXISTS (
		SELECT 1 FROM tool_calls tc WHERE tc.subagent_session_id = s.id
		AND tc.session_id IS NOT tc.subagent_session_id
	)
	AND NOT EXISTS (
		SELECT 1 FROM sessions p WHERE p.id = s.parent_session_id
	)`
}

// LinkSubagentSessionsForSessions is LinkSubagentSessions scoped to the
// sessions written by one sync batch: only children reachable from a batch
// member's spawn edges (or batch members that are themselves children) are
// re-resolved. Generic changed-session IDs are deliberately ineligible for
// dangling-parent cleanup: a parser-derived parent may simply not have been
// ingested yet. Destructive cleanup is reserved for former children captured
// before a write that can remove their spawn edges and persisted through
// QueueSubagentParentCleanupRepairs.
// Per-event paths — the session watcher re-syncs a single file
// on every change — must use this form so their linking cost tracks the
// changed batch; bulk paths (full sync, reconciliation, resync) keep the
// global LinkSubagentSessions pass they already coalesce to.
func (db *DB) LinkSubagentSessionsForSessions(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()

	// Each id binds twice (once per UNION branch), so halve the chunk to
	// stay within SQLite's bind-variable limit.
	return queryChunkedSize(ids, maxSQLVars/2, func(chunk []string) error {
		ph, args := inPlaceholders(chunk)
		allArgs := append(append([]any{}, args...), args...)
		_, err := db.getWriter().Exec(
			linkSubagentSessionsForSessionsQuery(ph), allArgs...,
		)
		if err != nil {
			return fmt.Errorf(
				"linking subagent sessions for %d changed sessions: %w",
				len(chunk), err,
			)
		}
		return nil
	})
}

// QueueSubagentParentRepairs durably records sessions whose hierarchy must be
// re-evaluated from surviving spawn edges. These generic seeds are never used
// for destructive dangling-parent cleanup; callers that captured a former
// child before removing edges use QueueSubagentParentCleanupRepairs instead.
func (db *DB) QueueSubagentParentRepairs(ids []string) error {
	return db.queueSubagentParentRepairs(ids, false)
}

// QueueSubagentParentCleanupRepairs durably records former children captured
// before an exclusion or message replacement can remove their spawn edges.
// Cleanup intent is separate from ordinary relink work so a newly parsed child
// whose parent has not arrived yet never loses valid parser-derived parentage.
func (db *DB) QueueSubagentParentCleanupRepairs(ids []string) error {
	return db.queueSubagentParentRepairs(ids, true)
}

func (db *DB) queueSubagentParentRepairs(ids []string, cleanup bool) error {
	if len(ids) == 0 {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning subagent parent repair queue update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	repairStmt, err := tx.Prepare(`
		INSERT INTO subagent_parent_repair_queue (session_id) VALUES (?)
		ON CONFLICT(session_id) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("preparing subagent parent repair queue insert: %w", err)
	}
	defer repairStmt.Close()
	var cleanupStmt *sql.Stmt
	if cleanup {
		cleanupStmt, err = tx.Prepare(`
			INSERT INTO subagent_parent_cleanup_queue (session_id) VALUES (?)
			ON CONFLICT(session_id) DO NOTHING`)
		if err != nil {
			return fmt.Errorf(
				"preparing subagent parent cleanup queue insert: %w", err,
			)
		}
		defer cleanupStmt.Close()
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, err := repairStmt.Exec(id); err != nil {
			return fmt.Errorf("queueing subagent parent repair for %s: %w", id, err)
		}
		if cleanupStmt != nil {
			if _, err := cleanupStmt.Exec(id); err != nil {
				return fmt.Errorf(
					"queueing subagent parent cleanup for %s: %w", id, err,
				)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing subagent parent repair queue: %w", err)
	}
	return nil
}

// RepairQueuedSubagentParents re-evaluates every durably queued session and
// clears the queue in the same transaction. A failed link or cleanup rolls
// back both the hierarchy changes and queue deletion so a later sync retries
// the exact IDs even when their original spawn edges have disappeared.
func (db *DB) RepairQueuedSubagentParents() error {
	return db.RepairQueuedSubagentParentsContext(context.Background())
}

// RepairQueuedSubagentParentsContext is RepairQueuedSubagentParents with
// caller-controlled cancellation for bounded sync paths.
func (db *DB) RepairQueuedSubagentParentsContext(ctx context.Context) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	var pending int
	err := db.getWriter().QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM subagent_parent_repair_queue)
		    OR EXISTS(SELECT 1 FROM subagent_parent_cleanup_queue)
		    OR EXISTS(SELECT 1 FROM pg_sync_state WHERE key = ?)`,
		subagentParentRepairQueueStateKey,
	).Scan(&pending)
	if err != nil {
		return fmt.Errorf("checking subagent parent repair queue: %w", err)
	}
	if pending == 0 {
		return nil
	}
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning queued subagent parent repair: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := migrateLegacySubagentParentRepairQueueTx(ctx, tx); err != nil {
		return err
	}
	for {
		rows, err := tx.QueryContext(ctx, `
			SELECT session_id FROM subagent_parent_repair_queue
			UNION
			SELECT session_id FROM subagent_parent_cleanup_queue
			ORDER BY session_id LIMIT ?`, maxSQLVars/2)
		if err != nil {
			return fmt.Errorf("listing queued subagent parent repairs: %w", err)
		}
		ids := make([]string, 0, maxSQLVars/2)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("scanning queued subagent parent repair: %w", err)
			}
			ids = append(ids, id)
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			return fmt.Errorf("iterating queued subagent parent repairs: %w", rowsErr)
		}
		if len(ids) == 0 {
			break
		}

		chunk := ids
		ph, args := inPlaceholders(chunk)
		allArgs := append(append([]any{}, args...), args...)
		if _, err := tx.ExecContext(ctx,
			linkSubagentSessionsForSessionsQuery(ph), allArgs...,
		); err != nil {
			return fmt.Errorf(
				"linking queued subagent parents for %d sessions: %w",
				len(chunk), err,
			)
		}
		cleanupSeeds := `(SELECT session_id
			FROM subagent_parent_cleanup_queue WHERE session_id IN ` + ph + `)`
		if _, err := tx.ExecContext(ctx,
			clearDanglingSubagentParentQuery(cleanupSeeds), args...,
		); err != nil {
			return fmt.Errorf(
				"clearing queued dangling subagent parents for %d "+
					"sessions: %w",
				len(chunk), err,
			)
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM subagent_parent_cleanup_queue WHERE session_id IN "+ph,
			args...,
		); err != nil {
			return fmt.Errorf(
				"clearing %d queued subagent parent cleanups: %w",
				len(chunk), err,
			)
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM subagent_parent_repair_queue WHERE session_id IN "+ph,
			args...,
		); err != nil {
			return fmt.Errorf(
				"clearing %d queued subagent parent repairs: %w",
				len(chunk), err,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing queued subagent parent repair: %w", err)
	}
	return nil
}

func migrateLegacySubagentParentRepairQueueTx(
	ctx context.Context, tx *sql.Tx,
) error {
	var encoded string
	err := tx.QueryRowContext(ctx,
		"SELECT value FROM pg_sync_state WHERE key = ?",
		subagentParentRepairQueueStateKey,
	).Scan(&encoded)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading legacy subagent parent repair queue: %w", err)
	}
	var ids []string
	if err := json.Unmarshal([]byte(encoded), &ids); err != nil {
		return fmt.Errorf("decoding legacy subagent parent repair queue: %w", err)
	}
	repairStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO subagent_parent_repair_queue (session_id) VALUES (?)
		ON CONFLICT(session_id) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("preparing legacy subagent parent repair migration: %w", err)
	}
	defer repairStmt.Close()
	cleanupStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO subagent_parent_cleanup_queue (session_id) VALUES (?)
		ON CONFLICT(session_id) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("preparing legacy subagent parent cleanup migration: %w", err)
	}
	defer cleanupStmt.Close()
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, err := repairStmt.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("migrating legacy subagent parent repair for %s: %w", id, err)
		}
		// The JSON queue predates generic post-write and attempted-session
		// seeds; every legacy ID was captured before a destructive write and
		// therefore carries cleanup intent.
		if _, err := cleanupStmt.ExecContext(ctx, id); err != nil {
			return fmt.Errorf(
				"migrating legacy subagent parent cleanup for %s: %w", id, err,
			)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM pg_sync_state WHERE key = ?",
		subagentParentRepairQueueStateKey,
	); err != nil {
		return fmt.Errorf("clearing legacy subagent parent repair queue: %w", err)
	}
	return nil
}

// SubagentChildSessionIDs returns the distinct children the given sessions'
// spawn edges currently reference (tool_calls.subagent_session_id). Sync
// captures this BEFORE a full rewrite or parser-exclusion delete: those
// writes cascade tool_calls away, and LinkSubagentSessionsForSessions
// discovers children only through post-write edges, so a child whose edge is
// about to disappear must be carried into the scoped batch explicitly to be
// re-resolved against its remaining spawners.
func (db *DB) SubagentChildSessionIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var children []string
	err := queryChunked(ids, func(chunk []string) error {
		ph, args := inPlaceholders(chunk)
		rows, err := db.getReader().Query(`
			SELECT DISTINCT tc.subagent_session_id
			FROM tool_calls tc
			WHERE tc.session_id IN `+ph+`
			AND tc.subagent_session_id IS NOT NULL
			AND tc.session_id IS NOT tc.subagent_session_id`, args...)
		if err != nil {
			return fmt.Errorf(
				"listing subagent children of %d sessions: %w",
				len(chunk), err,
			)
		}
		defer rows.Close()
		for rows.Next() {
			var child string
			if err := rows.Scan(&child); err != nil {
				return fmt.Errorf("scanning subagent child: %w", err)
			}
			children = append(children, child)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return children, nil
}

// GetSessionFileInfo returns file_size and file_mtime for a session. Used for
// fast skip checks during sync. Missing sources are excluded so an identical
// source restoration cannot be mistaken for fresh.
func (db *DB) GetSessionFileInfo(
	id string,
) (size int64, mtime int64, ok bool) {
	var s, m sql.NullInt64
	err := db.getReader().QueryRow(
		"SELECT file_size, file_mtime FROM sessions WHERE id = ?"+
			" AND source_missing_at IS NULL",
		id,
	).Scan(&s, &m)
	if err != nil {
		return 0, 0, false
	}
	return s.Int64, m.Int64, true
}

// GetSessionFileHash returns file_hash for a non-source-missing session. The
// bool is false when no eligible session exists or the column is NULL.
func (db *DB) GetSessionFileHash(id string) (hash string, ok bool) {
	var h sql.NullString
	err := db.getReader().QueryRow(
		"SELECT file_hash FROM sessions WHERE id = ?"+
			" AND source_missing_at IS NULL",
		id,
	).Scan(&h)
	if err != nil || !h.Valid {
		return "", false
	}
	return h.String, true
}

// GetSessionFilePathNotSourceMissing returns the stored file_path for a
// session unless its source is missing. Freshness gates use
// it to decide whether a canonical primary row still vouches for its source: a
// source-missing row must not, or a rowless source could never be marked
// fresh, while a user-trashed row keeps its path so trash handling is
// unchanged. It returns "" when no eligible row exists or file_path is NULL.
func (db *DB) GetSessionFilePathNotSourceMissing(id string) string {
	var fp sql.NullString
	err := db.getReader().QueryRow(
		"SELECT file_path FROM sessions WHERE id = ?"+
			" AND source_missing_at IS NULL",
		id,
	).Scan(&fp)
	if err != nil || !fp.Valid {
		return ""
	}
	return fp.String
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

// FindSessionIDsByPartial returns up to limit session IDs that contain the
// given literal, case-sensitive substring. Used by CLI lookups so users can
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
		 WHERE instr(id, ?) > 0 AND deleted_at IS NULL
		 ORDER BY COALESCE(
		     NULLIF(ended_at, ''),
		     NULLIF(started_at, ''),
		     created_at
		 ) DESC
		 LIMIT ?`,
		partial, limit,
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

// SetSessionDataVersions atomically stamps the parser data_version on every
// listed session. This is used when several session rows represent one source:
// either every member becomes current, or all remain eligible for retry.
func (db *DB) SetSessionDataVersions(ids []string, version int) error {
	return db.SetSessionDataVersionsContext(
		context.Background(), ids, version,
	)
}

// SetSessionDataVersionsContext stamps parser versions within ctx.
func (db *DB) SetSessionDataVersionsContext(
	ctx context.Context, ids []string, version int,
) error {
	return db.setSessionDataVersions(ctx, ids, version, true)
}

// SetExistingSessionDataVersions atomically stamps every listed session that
// already exists, ignoring IDs that have not been inserted yet.
func (db *DB) SetExistingSessionDataVersions(ids []string, version int) error {
	return db.setSessionDataVersions(
		context.Background(), ids, version, false,
	)
}

func (db *DB) setSessionDataVersions(
	ctx context.Context, ids []string, version int, requireAll bool,
) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning data version update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		result, err := tx.ExecContext(
			ctx,
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
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf(
				"checking data_version update for %s: %w", id, err,
			)
		}
		if rows == 0 && !requireAll {
			continue
		}
		if rows != 1 {
			return fmt.Errorf(
				"setting data_version for %s: session not found", id,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing data version update: %w", err)
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
	Project              string
	SourceProject        string
	Machine              string
	Cwd                  string
	AgentLabel           string
	Entrypoint           string
	SessionKind          string
	FileSize             int64
	FileMtime            int64
	NextOrdinal          int
	LastEntryUUID        string
	ClaudeLinearParse    *bool
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
	EndedAt                 *string
	TerminationStatus       *string
	MsgCount                int
	UserMsgCount            int
	FileSize                int64
	FileMtime               int64
	FileHash                *string
	NextOrdinal             int
	LastEntryUUID           string
	TotalOutputTokens       int
	PeakContextTokens       int
	HasTotalOutputTokens    bool
	HasPeakContextTokens    bool
	SubagentLinks           []ToolCallSubagentLink
	BlockedResultCategories map[string]bool
}

type ToolCallSubagentLink struct {
	ToolUseID         string
	SubagentSessionID string
	ResultContent     string
	ResultContentLen  int
	HasResult         bool
}

// GetSessionForIncremental returns session state needed for incremental
// parsing, looked up by agent and file_path. Returns false when the scoped path
// is unknown or maps to multiple sessions (e.g. Claude DAG forks), since
// incremental parsing cannot update multiple sessions from a single append.
func (db *DB) GetSessionForIncremental(
	path, agent string,
) (*IncrementalInfo, bool) {
	// Bail out if the file maps to more than one session
	// (Claude fork/subagent splits).
	var count int
	err := db.getReader().QueryRow(
		`SELECT COUNT(*) FROM sessions
		 WHERE file_path = ?
		   AND agent = ?
		   AND deleted_at IS NULL
		   AND source_missing_at IS NULL`, path,
		agent,
	).Scan(&count)
	if err != nil || count != 1 {
		return nil, false
	}

	var info IncrementalInfo
	var fs, fm, fi, fd sql.NullInt64
	var firstMsg, lastEntryUUID sql.NullString
	var linearParse sql.NullBool
	err = db.getReader().QueryRow(
		`SELECT s.id, s.project, COALESCE(snap.project, ''),
			s.machine, s.cwd, s.agent_label, s.entrypoint, s.session_kind,
			file_size, file_mtime,
			next_ordinal, last_entry_uuid, claude_linear_parse,
			file_inode, file_device,
			message_count, user_message_count,
			first_message,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens
		 FROM sessions s
		 LEFT JOIN session_project_identity_snapshots snap
		   ON snap.session_id = s.id
		 WHERE s.file_path = ?
		   AND s.agent = ?
		   AND s.deleted_at IS NULL
		   AND s.source_missing_at IS NULL`,
		path, agent,
	).Scan(
		&info.ID, &info.Project, &info.SourceProject,
		&info.Machine, &info.Cwd,
		&info.AgentLabel, &info.Entrypoint, &info.SessionKind,
		&fs, &fm, &info.NextOrdinal, &lastEntryUUID, &linearParse,
		&fi, &fd,
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
	if linearParse.Valid {
		info.ClaudeLinearParse = &linearParse.Bool
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

// FileIdentityChanged reports whether any active session row for path has a
// known file identity that differs from the current file identity.
func (db *DB) FileIdentityChanged(path string, inode, device int64) bool {
	if path == "" || inode == 0 || device == 0 {
		return false
	}

	var count int
	err := db.getReader().QueryRow(
		`SELECT COUNT(*)
		 FROM sessions
		 WHERE file_path = ?
		   AND deleted_at IS NULL
		   AND source_missing_at IS NULL
		   AND file_inode IS NOT NULL
		   AND file_device IS NOT NULL
		   AND file_inode != 0
		   AND file_device != 0
		   AND (file_inode != ? OR file_device != ?)`,
		path, inode, device,
	).Scan(&count)
	return err == nil && count > 0
}

// UpdateSessionIncremental updates only the fields that change
// during an incremental append: ended_at, message_count,
// user_message_count, file_size, file_mtime, optional file_hash, token
// aggregates, and termination_status. All values are absolute (not deltas)
// so the update is idempotent on retry.
//
// is_automated is recomputed from the stored transcript's first
// user message (falling back to first_message for legacy rows)
// and the new user_message_count so that classifier additions
// reach rows that only ever take the incremental path. Without
// this, a row whose first parse predates a new pattern would stay
// is_automated=0 indefinitely (UpsertSession sets the flag once
// at insert; the incremental path never re-evaluates it).
//
// A non-nil termination_status is an authoritative incremental verdict and
// is stored as-is. Nil clears the status for parsers such as Claude whose
// incremental path only sees the new tail and needs the full message slice
// to classify termination reliably. Clearing prevents a stale prior verdict
// from remaining visible until the next full sync reclassifies the session.
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
			termination_status = ?,
			-- Mark the row as last written by the incremental-append path.
			-- The full-replace writer (upsertSessionArgs) resets this to
			-- false; parse-diff reads it to classify benign
			-- incremental-vs-full skew.
			last_write_incremental = 1
		WHERE id = ?`,
		update.EndedAt, update.MsgCount, update.UserMsgCount,
		update.FileSize, update.FileMtime,
		update.FileHash,
		update.NextOrdinal, lastEntryUUID,
		update.TotalOutputTokens, update.PeakContextTokens,
		update.HasTotalOutputTokens, update.HasPeakContextTokens,
		update.TerminationStatus, id,
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

// resetIncrementalMarkerTx clears last_write_incremental after a full
// message re-normalization. It is the counterpart to the marker set in
// updateSessionIncrementalTx: parse-diff reads the marker to suppress
// benign incremental-append skew, so only a path that actually rewrites
// every message row to the full-parse shape (ReplaceSessionContent,
// ReplaceSessionMessages, the batch ReplaceMessages branch) may clear it.
// A bare UpsertSession or an append-only write must not, or the
// suppression self-heals prematurely and still-present skew reappears as
// spurious drift.
func resetIncrementalMarkerTx(tx transactionQueries, sessionID string) error {
	if _, err := tx.Exec(
		`UPDATE sessions SET last_write_incremental = 0 WHERE id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf(
			"resetting incremental marker for %s: %w", sessionID, err,
		)
	}
	return nil
}

// GetFileInfoByPath returns file_size and file_mtime for a session identified
// by file_path. Missing sources are excluded so a source that returns with
// identical metadata cannot be skipped before revival.
func (db *DB) GetFileInfoByPath(
	path string,
) (size int64, mtime int64, ok bool) {
	var s, m sql.NullInt64
	err := db.getReader().QueryRow(
		"SELECT file_size, file_mtime FROM sessions"+
			" WHERE file_path = ?"+
			" AND source_missing_at IS NULL"+
			" ORDER BY file_mtime DESC LIMIT 1",
		path,
	).Scan(&s, &m)
	if err != nil {
		return 0, 0, false
	}
	return s.Int64, m.Int64, true
}

// GetFileInfoByAgentPath is GetFileInfoByPath scoped to the agent that owns
// the source path. Agent-scoped freshness queries force the path index because
// SQLite otherwise prefers idx_sessions_agent and scans every session for the
// agent once per discovered source, making archive reconciliation quadratic.
const getFileInfoByAgentPathQuery = "SELECT file_size, file_mtime FROM sessions" +
	" INDEXED BY idx_sessions_file_path" +
	" WHERE file_path = ? AND agent = ?" +
	" AND source_missing_at IS NULL" +
	" ORDER BY file_mtime DESC LIMIT 1"

func (db *DB) GetFileInfoByAgentPath(
	path, agent string,
) (size int64, mtime int64, ok bool) {
	var s, m sql.NullInt64
	err := db.getReader().QueryRow(getFileInfoByAgentPathQuery, path, agent).
		Scan(&s, &m)
	if err != nil {
		return 0, 0, false
	}
	return s.Int64, m.Int64, true
}

// GetCwdByAgentPath returns the stored Cwd for the source owned by agent. A
// source-missing row remains eligible because its positive Cwd is the
// preservation authority when the source is parsed again.
func (db *DB) GetCwdByAgentPath(path, agent string) (cwd string, ok bool) {
	err := db.getReader().QueryRow(
		"SELECT cwd FROM sessions"+
			" INDEXED BY idx_sessions_file_path"+
			" WHERE file_path = ? AND agent = ?"+
			" AND deleted_at IS NULL"+
			" ORDER BY file_mtime DESC LIMIT 1",
		path, agent,
	).Scan(&cwd)
	if err != nil {
		return "", false
	}
	return cwd, true
}

// UpdateSessionCwd updates only the durable workspace identity for an
// existing session. It is used when a parsed source is excluded by a cwd
// filter but its source-owned identity still has to be reconciled.
func (db *DB) UpdateSessionCwd(id, cwd string) error {
	_, err := db.updateSessionCwd(
		` WHERE id = ?`, []any{id}, cwd,
	)
	return err
}

// UpdateSessionCwdByIdentity updates one session only when its source path and
// agent still match the parsed source that requested the reconciliation.
func (db *DB) UpdateSessionCwdByIdentity(
	id, path, agent, cwd string,
) (bool, error) {
	return db.updateSessionCwd(
		` WHERE id = ? AND file_path = ? AND agent = ?`,
		[]any{id, path, agent}, cwd,
	)
}

func (db *DB) updateSessionCwd(
	identityWhere string, identityArgs []any, cwd string,
) (bool, error) {
	if err := db.requireWritable(); err != nil {
		return false, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	staleVersion := max(CurrentDataVersion()-1, 0)
	args := append([]any{cwd, staleVersion}, identityArgs...)
	result, err := db.getWriter().Exec(
		`UPDATE sessions SET
			cwd = ?,
			data_version = MIN(data_version, ?),
			local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		`+identityWhere+` AND cwd IS NOT ?
		 AND deleted_at IS NULL`,
		append(args, cwd)...,
	)
	if err != nil {
		return false, fmt.Errorf("updating session cwd: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting session cwd updates: %w", err)
	}
	return rows > 0, nil
}

// UpdateCwdByAgentPath updates the workspace identity for every active or
// source-missing row at one agent/path identity.
func (db *DB) UpdateCwdByAgentPath(path, agent, cwd string) error {
	_, err := db.UpdateCwdByAgentPathCount(path, agent, cwd)
	return err
}

// UpdateCwdByAgentPathCount returns the number of source rows changed and
// marks them stale so a later admitted parse can refresh project identity.
func (db *DB) UpdateCwdByAgentPathCount(path, agent, cwd string) (int, error) {
	if err := db.requireWritable(); err != nil {
		return 0, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	staleVersion := max(CurrentDataVersion()-1, 0)
	result, err := db.getWriter().Exec(
		`UPDATE sessions SET
			cwd = ?,
			data_version = MIN(data_version, ?),
			local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE file_path = ? AND agent = ?
		 AND cwd IS NOT ?
		 AND deleted_at IS NULL`,
		cwd, staleVersion, path, agent, cwd,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"updating cwd for %s/%s: %w", agent, path, err,
		)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf(
			"counting cwd updates for %s/%s: %w", agent, path, err,
		)
	}
	return int(rows), nil
}

// StaleDataVersionAgentPaths returns every (agent, file_path) source identity
// holding at least one row below version. Membership matches
// pathNeedsDataVersionReparse's per-path form -- GetFileInfoByAgentPath
// finding a qualifying row and GetDataVersionByAgentPath reporting a minimum
// below version -- because both are equivalent to a qualifying row existing
// with data_version < version. One scan replaces two point queries per
// discovered file on the sync mtime-cutoff path.
func (db *DB) StaleDataVersionAgentPaths(
	version int,
) ([]SessionSourcePath, error) {
	rows, err := db.getReader().Query(
		"SELECT DISTINCT agent, file_path FROM sessions"+
			" WHERE data_version < ?"+
			" AND file_path IS NOT NULL"+
			" AND source_missing_at IS NULL",
		version,
	)
	if err != nil {
		return nil, fmt.Errorf("listing stale data-version sources: %w", err)
	}
	defer rows.Close()
	var identities []SessionSourcePath
	for rows.Next() {
		var identity SessionSourcePath
		if err := rows.Scan(&identity.Agent, &identity.FilePath); err != nil {
			return nil, fmt.Errorf(
				"scanning stale data-version source: %w", err,
			)
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading stale data-version sources: %w", err)
	}
	return identities, nil
}

// VirtualContainerMemberFreshness is one stored virtual member's freshness
// signal: the newest stored file_mtime for its path, the minimum stored
// data version, and the newest row's fingerprint hash, mirroring
// GetFileInfoByPath, GetDataVersionByPath, and GetFileHashByPath.
type VirtualContainerMemberFreshness struct {
	MTimeNS     int64
	DataVersion int
	Hash        string
}

// VirtualContainerMemberFreshnessRow pairs one virtual member path with its
// folded freshness signal.
type VirtualContainerMemberFreshnessRow struct {
	Path string
	VirtualContainerMemberFreshness
}

// ListVirtualContainerMemberFreshnessPage returns the freshness signal for
// stored sessions whose file_path is a virtual member of the shared container
// at containerPath ("<containerPath>#<sessionID>"), excluding source-missing
// tombstones: at most limit member paths strictly after afterPath, in
// ascending path order, and whether the container's stored membership is
// exhausted. Changed-path classification merges a streamed watermark-only
// listing against these pages, so a one-session write flows one candidate
// into the sync pipeline while peak memory stays one page — never the
// container's full membership.
//
// Two queries per page keep each path's fold complete without materializing
// the container: a DISTINCT path page rides idx_sessions_file_path ('$' is
// the ASCII successor of '#', so the half-open range covers exactly the
// "<containerPath>#" prefix), and the row fetch is bounded to that page's
// [first, last] interval, so duplicate session rows for one path can never
// split across pages. Folded in Go rather than GROUP BY: the fold needs
// MAX(file_mtime), MIN(data_version), and the hash of the newest-mtime row,
// and SQLite's bare-column-from-the-extreme-row guarantee only holds with
// exactly one min/max aggregate in the query.
func (db *DB) ListVirtualContainerMemberFreshnessPage(
	ctx context.Context, containerPath, afterPath string, limit int,
) ([]VirtualContainerMemberFreshnessRow, bool, error) {
	if containerPath == "" || limit <= 0 {
		return nil, true, nil
	}
	notMissing := " AND source_missing_at IS NULL"
	lower := containerPath + "#"
	lowerOp := ">="
	if afterPath > lower {
		lower = afterPath
		lowerOp = ">"
	}
	pathRows, err := db.getReader().QueryContext(ctx,
		"SELECT DISTINCT file_path FROM sessions"+
			" WHERE file_path "+lowerOp+" ? AND file_path < ? || '$'"+
			notMissing+
			" ORDER BY file_path LIMIT ?",
		lower, containerPath, limit,
	)
	if err != nil {
		return nil, false, fmt.Errorf(
			"listing container member paths %s: %w", containerPath, err,
		)
	}
	defer pathRows.Close()
	var paths []string
	for pathRows.Next() {
		var path string
		if err := pathRows.Scan(&path); err != nil {
			return nil, false, fmt.Errorf(
				"scanning container member path %s: %w", containerPath, err,
			)
		}
		paths = append(paths, path)
	}
	if err := pathRows.Err(); err != nil {
		return nil, false, err
	}
	if len(paths) == 0 {
		return nil, true, nil
	}
	done := len(paths) < limit

	rows, err := db.getReader().QueryContext(ctx,
		"SELECT file_path, file_mtime, data_version, file_hash FROM sessions"+
			" WHERE file_path >= ? AND file_path <= ?"+notMissing,
		paths[0], paths[len(paths)-1],
	)
	if err != nil {
		return nil, false, fmt.Errorf(
			"listing container member freshness %s: %w", containerPath, err,
		)
	}
	defer rows.Close()
	members := make(map[string]VirtualContainerMemberFreshness, len(paths))
	for rows.Next() {
		var path string
		var mtime, version sql.NullInt64
		var hash sql.NullString
		if err := rows.Scan(&path, &mtime, &version, &hash); err != nil {
			return nil, false, fmt.Errorf(
				"scanning container member freshness %s: %w",
				containerPath, err,
			)
		}
		row := VirtualContainerMemberFreshness{
			MTimeNS:     mtime.Int64,
			DataVersion: int(version.Int64),
			Hash:        hash.String,
		}
		member, seen := members[path]
		if !seen {
			members[path] = row
			continue
		}
		if row.MTimeNS > member.MTimeNS {
			member.MTimeNS = row.MTimeNS
			member.Hash = row.Hash
		}
		if row.DataVersion < member.DataVersion {
			member.DataVersion = row.DataVersion
		}
		members[path] = member
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	page := make([]VirtualContainerMemberFreshnessRow, 0, len(paths))
	for _, path := range paths {
		page = append(page, VirtualContainerMemberFreshnessRow{
			Path:                            path,
			VirtualContainerMemberFreshness: members[path],
		})
	}
	return page, done, nil
}

// GetProjectByPath returns the stored project for the newest
// non-deleted session matching file_path.
func (db *DB) GetProjectByPath(path string) (project string, ok bool) {
	err := db.getReader().QueryRow(
		"SELECT project FROM sessions"+
			" WHERE file_path = ?"+
			" AND deleted_at IS NULL"+
			" AND source_missing_at IS NULL"+
			" ORDER BY file_mtime DESC LIMIT 1",
		path,
	).Scan(&project)
	if err != nil {
		return "", false
	}
	return project, true
}

// GetProjectByAgentPath is GetProjectByPath scoped to the agent that owns the
// source path.
func (db *DB) GetProjectByAgentPath(
	path, agent string,
) (project string, ok bool) {
	err := db.getReader().QueryRow(
		"SELECT project FROM sessions"+
			" WHERE file_path = ? AND agent = ?"+
			" AND deleted_at IS NULL"+
			" AND source_missing_at IS NULL"+
			" ORDER BY file_mtime DESC LIMIT 1",
		path, agent,
	).Scan(&project)
	if err != nil {
		return "", false
	}
	return project, true
}

// GetSourceRepairStateByPath returns the newest active session's project and
// file metadata plus the minimum active parser data version for one source
// path. It combines the lightweight self-healing checks used by hot sync paths
// into one query.
func (db *DB) GetSourceRepairStateByPath(
	path string,
) (
	project string,
	dataVersion int,
	fileSize int64,
	fileMtime int64,
	ok bool,
) {
	err := db.getReader().QueryRow(`
		SELECT project, file_size, file_mtime, (
			SELECT MIN(data_version)
			FROM sessions
			WHERE file_path = ? AND deleted_at IS NULL
			  AND source_missing_at IS NULL
		)
		FROM sessions
		WHERE file_path = ? AND deleted_at IS NULL
		  AND source_missing_at IS NULL
		ORDER BY file_mtime DESC
		LIMIT 1`, path, path,
	).Scan(&project, &fileSize, &fileMtime, &dataVersion)
	if err != nil {
		return "", 0, 0, 0, false
	}
	return project, dataVersion, fileSize, fileMtime, true
}

// GetSourceRepairStateByAgentPath is GetSourceRepairStateByPath scoped to the
// agent that owns the source path.
func (db *DB) GetSourceRepairStateByAgentPath(
	path, agent string,
) (
	project string,
	dataVersion int,
	fileSize int64,
	fileMtime int64,
	ok bool,
) {
	err := db.getReader().QueryRow(`
		SELECT project, file_size, file_mtime, (
			SELECT MIN(data_version)
			FROM sessions
			WHERE file_path = ? AND agent = ? AND deleted_at IS NULL
			  AND source_missing_at IS NULL
		)
		FROM sessions
		WHERE file_path = ? AND agent = ? AND deleted_at IS NULL
		  AND source_missing_at IS NULL
		ORDER BY file_mtime DESC
		LIMIT 1`, path, agent, path, agent,
	).Scan(&project, &fileSize, &fileMtime, &dataVersion)
	if err != nil {
		return "", 0, 0, 0, false
	}
	return project, dataVersion, fileSize, fileMtime, true
}

// GetFileHashByPath returns the stored file_hash for a non-source-missing
// session matching file_path, preferring the most recently modified row.
// The bool is false when no row exists or the column is NULL. Used
// by the Shelley skip to compare a per-conversation content
// fingerprint alongside file_mtime.
func (db *DB) GetFileHashByPath(path string) (hash string, ok bool) {
	var h sql.NullString
	err := db.getReader().QueryRow(
		"SELECT file_hash FROM sessions"+
			" WHERE file_path = ?"+
			" AND source_missing_at IS NULL"+
			" ORDER BY file_mtime DESC LIMIT 1",
		path,
	).Scan(&h)
	if err != nil {
		return "", false
	}
	return h.String, h.Valid
}

// GetFileHashByAgentPath is GetFileHashByPath scoped to the agent that owns
// the source path.
const getFileHashByAgentPathQuery = "SELECT file_hash FROM sessions" +
	" INDEXED BY idx_sessions_file_path" +
	" WHERE file_path = ? AND agent = ?" +
	" AND source_missing_at IS NULL" +
	" ORDER BY file_mtime DESC LIMIT 1"

func (db *DB) GetFileHashByAgentPath(
	path, agent string,
) (hash string, ok bool) {
	var h sql.NullString
	err := db.getReader().QueryRow(getFileHashByAgentPathQuery, path, agent).
		Scan(&h)
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
			" AND source_missing_at IS NULL"+
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

// ListStaleForkSessionOwnerships returns every active fork row written by an
// older parser data version for one agent, with its stored machine and source
// path. A rebuild loads this once from the write-barriered original archive so
// per-file legacy fork checks read memory instead of opening archive
// connections while the replacement database is being written.
func (db *DB) ListStaleForkSessionOwnerships(
	agent string,
) ([]SessionSourceOwnership, error) {
	rows, err := db.getReader().Query(
		"SELECT id, machine, file_path FROM sessions"+
			" WHERE agent = ? AND deleted_at IS NULL"+
			" AND source_missing_at IS NULL"+
			" AND relationship_type = 'fork' AND data_version < ?"+
			" AND file_path IS NOT NULL"+
			" ORDER BY file_path, id",
		agent, dataVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("listing stale fork session ownerships: %w", err)
	}
	defer rows.Close()

	var ownerships []SessionSourceOwnership
	for rows.Next() {
		var ownership SessionSourceOwnership
		if err := rows.Scan(
			&ownership.ID, &ownership.Machine, &ownership.FilePath,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning stale fork session ownership: %w", err,
			)
		}
		ownership.Agent = agent
		ownerships = append(ownerships, ownership)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating stale fork session ownerships: %w", err,
		)
	}
	return ownerships, nil
}

// ListStaleForkSessionIDsByFilePath returns active fork rows written by an
// older parser data version for one provider-owned source path. Both signals
// are required before a caller may treat a row omitted by a current complete
// parse as a legacy fork artifact rather than parser presence drift.
func (db *DB) ListStaleForkSessionIDsByFilePath(
	path, agent string,
) ([]string, error) {
	rows, err := db.getReader().Query(
		"SELECT id FROM sessions"+
			" WHERE file_path = ? AND agent = ? AND deleted_at IS NULL"+
			" AND source_missing_at IS NULL"+
			" AND relationship_type = 'fork' AND data_version < ?"+
			" ORDER BY id",
		path, agent, dataVersion,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"listing stale fork session IDs by file path: %w", err,
		)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf(
				"scanning stale fork session ID by file path: %w", err,
			)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating stale fork session IDs by file path: %w", err,
		)
	}
	return ids, nil
}

const sessionMachineBatchSize = 500

// SessionWriteIdentity is the stored state used to preserve immutable source
// attribution and parser-owned Codex titles across session writes and rebuilds.
type SessionWriteIdentity struct {
	Machine     string
	Agent       string
	FilePath    string
	FileHash    string
	SessionName *string
}

// ListSessionWriteIdentitiesByID returns stored source attribution and parser
// title state for each requested session, including tombstoned rows that may be
// revived by a later successful parse. Requests are chunked below SQLite's
// bind-variable limit.
func (db *DB) ListSessionWriteIdentitiesByID(
	ctx context.Context,
	ids []string,
) (map[string]SessionWriteIdentity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identities := make(map[string]SessionWriteIdentity, len(ids))
	unique := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	for start := 0; start < len(unique); start += sessionMachineBatchSize {
		end := min(start+sessionMachineBatchSize, len(unique))
		batch := unique[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := db.getReader().QueryContext(ctx, `
			SELECT id, machine, agent,
			       COALESCE(file_path, ''), COALESCE(file_hash, ''),
			       session_name
			FROM sessions
			WHERE id IN (`+placeholders+`)
			ORDER BY id`, args...)
		if err != nil {
			return nil, fmt.Errorf("listing session write identities by ID: %w", err)
		}
		for rows.Next() {
			var id string
			var identity SessionWriteIdentity
			if err := rows.Scan(
				&id, &identity.Machine, &identity.Agent,
				&identity.FilePath, &identity.FileHash, &identity.SessionName,
			); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf(
					"scanning session write identity by ID: %w", err,
				)
			}
			identities[id] = identity
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterating session write identities by ID: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("closing session write identities by ID: %w", err)
		}
	}
	return identities, nil
}

// ListSessionMachinesByID returns the stored machine attribution for each
// requested session, including tombstoned rows that may be revived by a later
// successful parse. Requests are chunked below SQLite's bind-variable limit.
func (db *DB) ListSessionMachinesByID(
	ctx context.Context,
	ids []string,
) (map[string]string, error) {
	identities, err := db.ListSessionWriteIdentitiesByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	machines := make(map[string]string, len(ids))
	for id, identity := range identities {
		machines[id] = identity.Machine
	}
	return machines, nil
}

const descendantSessionRootBatchSize = 100

// ListActiveDescendantSessionSourcePaths returns the source paths of active
// descendants already linked beneath parentIDs. Both the seed and recursive
// steps use idx_sessions_parent, so work scales with the affected subagent
// trees rather than every archived session.
func (db *DB) ListActiveDescendantSessionSourcePaths(
	ctx context.Context,
	machine, agent string,
	parentIDs []string,
) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var paths []string
	for start := 0; start < len(parentIDs); start += descendantSessionRootBatchSize {
		end := min(start+descendantSessionRootBatchSize, len(parentIDs))
		batch := parentIDs[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		query := `
			WITH RECURSIVE descendants(id, file_path) AS (
				SELECT id, file_path
				  FROM sessions INDEXED BY idx_sessions_parent
				 WHERE parent_session_id IS NOT NULL
				   AND parent_session_id IN (` + placeholders + `)
				   AND machine = ? AND agent = ? AND deleted_at IS NULL
				   AND source_missing_at IS NULL
				UNION
				SELECT s.id, s.file_path
				  FROM sessions AS s INDEXED BY idx_sessions_parent
				  JOIN descendants AS d ON s.parent_session_id = d.id
				 WHERE s.parent_session_id IS NOT NULL
				   AND s.machine = ? AND s.agent = ? AND s.deleted_at IS NULL
				   AND s.source_missing_at IS NULL
			)
			SELECT file_path
			  FROM descendants
			 WHERE file_path IS NOT NULL AND file_path <> ''
			 ORDER BY file_path`
		args := make([]any, 0, len(batch)+4)
		for _, id := range batch {
			args = append(args, id)
		}
		args = append(args, machine, agent, machine, agent)
		rows, err := db.getReader().QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf(
				"listing active descendant session sources: %w", err,
			)
		}
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf(
					"scanning active descendant session source: %w", err,
				)
			}
			if _, exists := seen[path]; exists {
				continue
			}
			seen[path] = struct{}{}
			paths = append(paths, path)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf(
				"iterating active descendant session sources: %w", err,
			)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf(
				"closing active descendant session sources: %w", err,
			)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

const storedSourcePathHintRootBatchSize = 100

// StoredSourcePathHintScope identifies one affected stored-source prefix.
// IncludeVirtualMembers is provider-declared ownership of path#member sources;
// it must not be inferred from filename shape.
type StoredSourcePathHintScope struct {
	Path                  string
	IncludeVirtualMembers bool
}

// WatchReconcileSourcePageSize bounds each watcher reconciliation ownership
// page. The engine releases every page before requesting the next one, so its
// live Go memory does not scale with the number of archived sessions.
const WatchReconcileSourcePageSize = 128

// watchReconcileOwnershipScopeBatchSize bounds the SQL expression and bind
// parameter count independently of the number of configured provider roots.
const watchReconcileOwnershipScopeBatchSize = 32

// SessionSourceCursor is the complete keyset cursor for source ownership
// pages. Agent is retained to prevent accidentally reusing a cursor across
// independently ordered agent scans.
type SessionSourceCursor struct {
	Agent    string
	FilePath string
	ID       string
}

// SessionSourceOwnership is the exact active row identity used by watcher
// reconciliation. Conditional tombstoning must match every field.
type SessionSourceOwnership struct {
	Machine  string
	Agent    string
	ID       string
	FilePath string
}

// SessionSourcePath identifies an exact source path observed during a
// successful local reconciliation. The baseline is deliberately path-exact:
// callers must pass virtual member paths without splitting their suffixes.
type SessionSourcePath struct {
	Agent    string
	FilePath string
}

// SessionSourceAttribution is one distinct immutable machine label represented
// by active sessions at an exact provider source.
type SessionSourceAttribution struct {
	Machine  string
	Agent    string
	FilePath string
}

func (o SessionSourceOwnership) Cursor() SessionSourceCursor {
	return SessionSourceCursor{Agent: o.Agent, FilePath: o.FilePath, ID: o.ID}
}

// ListActiveSessionSourceOwnershipScopesPage returns one stable keyset page
// across a provider's bounded physical scopes. Machine is an exact stored key,
// including the empty key retained by legacy sessions. Normalization
// deduplicates repeated declarations before building the query.
func (db *DB) ListActiveSessionSourceOwnershipScopesPage(
	ctx context.Context,
	machine string,
	agent string,
	scopes []StoredSourcePathHintScope,
	after SessionSourceCursor,
) ([]SessionSourceOwnership, error) {
	scopes = normalizeStoredSourcePathHintScopes(scopes)
	if agent == "" || len(scopes) == 0 {
		return nil, nil
	}
	if after.Agent != "" && after.Agent != agent {
		return nil, fmt.Errorf(
			"source ownership cursor agent %q does not match %q", after.Agent, agent,
		)
	}
	var ownership []SessionSourceOwnership
	for start := 0; start < len(scopes); start += watchReconcileOwnershipScopeBatchSize {
		end := min(start+watchReconcileOwnershipScopeBatchSize, len(scopes))
		page, err := db.listActiveSessionSourceOwnershipScopeBatch(
			ctx, machine, agent, scopes[start:end], after,
		)
		if err != nil {
			return nil, err
		}
		ownership = mergeSessionSourceOwnershipPages(
			ownership, page, WatchReconcileSourcePageSize,
		)
	}
	return ownership, nil
}

func (db *DB) listActiveSessionSourceOwnershipScopeBatch(
	ctx context.Context,
	machine string,
	agent string,
	scopes []StoredSourcePathHintScope,
	after SessionSourceCursor,
) ([]SessionSourceOwnership, error) {
	rootClauses := make([]string, 0, len(scopes))
	args := []any{machine, agent}
	for _, scope := range scopes {
		root := scope.Path
		likeRoot := sqliteLikeEscape(root)
		// A drive root or share root already ends in a separator, so the
		// child prefix must not add a second one: stored children carry one.
		childPrefix := likeRoot + string(filepath.Separator) + "%"
		if strings.HasSuffix(root, string(filepath.Separator)) {
			childPrefix = likeRoot + "%"
		}
		rootClause := `(b.file_path = ? OR b.file_path LIKE ? ESCAPE '!')`
		args = append(args, root, childPrefix)
		if scope.IncludeVirtualMembers {
			// Mirror storedSourcePathHintInRoot: a virtual member is the
			// container plus '#' and a nonempty single segment. Nested stored
			// paths such as root+"#backup/session.json" belong to other
			// sources and must not be claimed (and later tombstoned) here.
			rootClause = `(` + rootClause + ` OR (b.file_path > ? AND b.file_path < ?
				AND b.file_path NOT LIKE ? ESCAPE '!'
				AND b.file_path NOT LIKE ? ESCAPE '!'))`
			args = append(args, root+"#", root+"$",
				likeRoot+`#%/%`, likeRoot+`#%\%`)
		}
		rootClauses = append(rootClauses, rootClause)
	}
	rootClause := `(` + strings.Join(rootClauses, ` OR `) + `)`
	args = append(args,
		after.FilePath, after.FilePath, after.ID,
		WatchReconcileSourcePageSize,
	)
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT s.machine, s.agent, s.id, s.file_path
		FROM local_session_source_baselines AS b
		JOIN sessions AS s
		  ON s.id = b.session_id
		 AND s.machine = b.machine
		 AND s.agent = b.agent
		 AND s.file_path = b.file_path
		WHERE b.machine = ?
		  AND b.agent = ?
		  AND s.deleted_at IS NULL
		  AND s.source_missing_at IS NULL
		  AND `+rootClause+`
		  AND (b.file_path > ? OR (b.file_path = ? AND b.session_id > ?))
		ORDER BY b.file_path, b.session_id
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing active session source ownership: %w", err)
	}
	defer rows.Close()

	ownership := make([]SessionSourceOwnership, 0, WatchReconcileSourcePageSize)
	for rows.Next() {
		var item SessionSourceOwnership
		if err := rows.Scan(
			&item.Machine, &item.Agent, &item.ID, &item.FilePath,
		); err != nil {
			return nil, fmt.Errorf("scanning active session source ownership: %w", err)
		}
		ownership = append(ownership, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating active session source ownership: %w", err)
	}
	return ownership, nil
}

func mergeSessionSourceOwnershipPages(
	left, right []SessionSourceOwnership,
	limit int,
) []SessionSourceOwnership {
	// Every batch returns its first page after the same cursor. Keeping the
	// smallest unique limit across those prefixes therefore produces the first
	// global page without retaining work proportional to the number of scopes.
	combined := make([]SessionSourceOwnership, 0, min(len(left)+len(right), limit*2))
	combined = append(combined, left...)
	combined = append(combined, right...)
	sort.Slice(combined, func(i, j int) bool {
		if combined[i].FilePath != combined[j].FilePath {
			return combined[i].FilePath < combined[j].FilePath
		}
		return combined[i].ID < combined[j].ID
	})
	merged := combined[:0]
	for _, item := range combined {
		if len(merged) > 0 {
			previous := merged[len(merged)-1]
			if item.FilePath == previous.FilePath && item.ID == previous.ID {
				continue
			}
		}
		merged = append(merged, item)
		if len(merged) == limit {
			break
		}
	}
	return merged
}

// BaselineActiveSessionSourcePaths marks exact active local ownerships as
// observed. Machine is an exact stored key, including the empty key retained by
// legacy sessions. Callers pass one bounded discovery page or changed-path
// batch at a time so this update never scales its live memory with the archive.
func (db *DB) BaselineActiveSessionSourcePaths(
	ctx context.Context,
	machine string,
	sources []SessionSourcePath,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(sources) == 0 {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting source baseline transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := baselineActiveSessionSourcePathsTx(
		ctx, tx, machine, sources,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing source baseline transaction: %w", err)
	}
	return nil
}

// BaselineActiveSessionSourceOwnerships marks only the supplied exact active
// ownerships as observed. It is used when source-wide admission would include
// archived members that a per-session policy deliberately rejects.
func (db *DB) BaselineActiveSessionSourceOwnerships(
	ctx context.Context,
	ownerships []SessionSourceOwnership,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(ownerships) == 0 {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting exact source baseline transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := baselineActiveSessionSourceOwnershipsTx(
		ctx, tx, ownerships,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing exact source baselines: %w", err)
	}
	return nil
}

// CopySessionSourceOwnershipBaselinesFrom copies deletion proof from another
// archive only for the supplied exact active ownerships. Rebuilds use this for
// admitted archive-only members without granting proof to unrelated or
// policy-rejected orphaned sessions.
func (db *DB) CopySessionSourceOwnershipBaselinesFrom(
	ctx context.Context,
	sourcePath string,
	ownerships []SessionSourceOwnership,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(ownerships) == 0 {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring source baseline copy connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(
		ctx, "ATTACH DATABASE ? AS old_db", sourcePath,
	); err != nil {
		return fmt.Errorf("attaching source baseline archive: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(ctx, "DETACH DATABASE old_db")
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting source baseline copy transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if oldDBHasTable(ctx, tx, "local_session_source_baselines") {
		for _, ownership := range ownerships {
			if ownership.ID == "" || ownership.Agent == "" || ownership.FilePath == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO main.local_session_source_baselines
					(session_id, machine, agent, file_path)
				SELECT b.session_id, b.machine, b.agent, b.file_path
				FROM old_db.local_session_source_baselines AS b
				JOIN main.sessions AS s
				  ON s.id = b.session_id
				 AND s.machine = b.machine
				 AND s.agent = b.agent
				 AND s.file_path = b.file_path
				WHERE b.session_id = ? AND b.machine = ?
				  AND b.agent = ? AND b.file_path = ?
				  AND s.deleted_at IS NULL
				  AND s.source_missing_at IS NULL
				ON CONFLICT(session_id) DO UPDATE SET
					machine = excluded.machine,
					agent = excluded.agent,
					file_path = excluded.file_path`,
				ownership.ID, ownership.Machine,
				ownership.Agent, ownership.FilePath,
			); err != nil {
				return fmt.Errorf(
					"copying exact session source baseline: %w", err,
				)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing source baseline copy: %w", err)
	}
	return nil
}

// RemoveSessionSourceOwnershipBaselines revokes deletion proof only for the
// supplied exact ownerships. It preserves other active rows sharing the same
// source path.
func (db *DB) RemoveSessionSourceOwnershipBaselines(
	ctx context.Context,
	ownerships []SessionSourceOwnership,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(ownerships) == 0 {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting exact source baseline removal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := removeSessionSourceOwnershipBaselinesTx(
		ctx, tx, ownerships,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing exact source baseline removal: %w", err)
	}
	return nil
}

// RemoveSessionSourceBaselines revokes deletion proof for the supplied source
// paths across every stored machine attribution.
func (db *DB) RemoveSessionSourceBaselines(
	ctx context.Context,
	sources []SessionSourcePath,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(sources) == 0 {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting source baseline removal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteSessionSourceBaselinesAcrossMachinesTx(
		ctx, tx, sources, "rejected",
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing source baseline removal: %w", err)
	}
	return nil
}

// ReplaceActiveSessionSourceBaselines makes admitted the exact subset of a
// bounded candidate page that carries deletion proof. Machine is an exact
// stored key, including the empty key retained by legacy sessions. Existing
// proof for rejected candidates is removed in the same transaction that admits
// the successful candidates.
//
// The replacement is a diff, not a rewrite: warm no-op syncs replay every
// unchanged archived source as an admitted candidate each pass, so unchanged
// admitted rows must not be touched. Only rejected pairs, missing or changed
// admitted rows, and admitted-pair rows no longer backed by an active session
// with the same ownership (moved or tombstoned sessions) produce writes.
func (db *DB) ReplaceActiveSessionSourceBaselines(
	ctx context.Context,
	machine string,
	candidates []SessionSourcePath,
	admitted []SessionSourcePath,
) error {
	return db.ReplaceActiveSessionSourceBaselinesWithExceptions(
		ctx, machine, candidates, admitted, nil, nil,
	)
}

// ReplaceActiveSessionSourceBaselinesWithExceptions replaces source-wide
// deletion proof and applies per-session CWD exceptions in one transaction.
// This prevents a canceled or failed pass from committing broad proof before
// it revokes proof from rejected members of the same source.
func (db *DB) ReplaceActiveSessionSourceBaselinesWithExceptions(
	ctx context.Context,
	machine string,
	candidates []SessionSourcePath,
	admitted []SessionSourcePath,
	exactAdmitted []SessionSourceOwnership,
	exactRejected []SessionSourceOwnership,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(candidates) == 0 && len(exactAdmitted) == 0 &&
		len(exactRejected) == 0 {
		return nil
	}
	rejected := rejectedSourceCandidates(candidates, admitted)
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting source baseline replacement transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteSessionSourceBaselinesTx(
		ctx, tx, machine, rejected, "", "rejected",
	); err != nil {
		return err
	}
	if err := baselineActiveSessionSourcePathsTx(
		ctx, tx, machine, admitted,
	); err != nil {
		return err
	}
	if err := deleteSessionSourceBaselinesTx(
		ctx, tx, machine, admitted, `
			  AND NOT EXISTS (
				SELECT 1 FROM sessions AS s
				WHERE s.id = local_session_source_baselines.session_id
				  AND s.machine = local_session_source_baselines.machine
				  AND s.agent = local_session_source_baselines.agent
				  AND s.file_path = local_session_source_baselines.file_path
				  AND s.deleted_at IS NULL
				  AND s.source_missing_at IS NULL
			  )`, "stale admitted",
	); err != nil {
		return err
	}
	if err := removeSessionSourceOwnershipBaselinesTx(
		ctx, tx, exactRejected,
	); err != nil {
		return err
	}
	if err := baselineActiveSessionSourceOwnershipsTx(
		ctx, tx, exactAdmitted,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing source baseline replacement: %w", err)
	}
	return nil
}

func baselineActiveSessionSourceOwnershipsTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerships []SessionSourceOwnership,
) error {
	for _, ownership := range ownerships {
		if ownership.ID == "" || ownership.Agent == "" || ownership.FilePath == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO local_session_source_baselines
				(session_id, machine, agent, file_path)
			SELECT id, machine, agent, file_path
			FROM sessions
			WHERE id = ? AND machine = ? AND agent = ? AND file_path = ?
			  AND deleted_at IS NULL
			  AND source_missing_at IS NULL
			ON CONFLICT(session_id) DO UPDATE SET
				machine = excluded.machine,
				agent = excluded.agent,
				file_path = excluded.file_path
			WHERE local_session_source_baselines.machine IS NOT excluded.machine
			   OR local_session_source_baselines.agent IS NOT excluded.agent
			   OR local_session_source_baselines.file_path IS NOT excluded.file_path`,
			ownership.ID, ownership.Machine,
			ownership.Agent, ownership.FilePath,
		); err != nil {
			return fmt.Errorf("baselining exact active session ownership: %w", err)
		}
	}
	return nil
}

func removeSessionSourceOwnershipBaselinesTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerships []SessionSourceOwnership,
) error {
	for _, ownership := range ownerships {
		if ownership.ID == "" || ownership.Agent == "" || ownership.FilePath == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM local_session_source_baselines
			WHERE session_id = ? AND machine = ? AND agent = ? AND file_path = ?`,
			ownership.ID, ownership.Machine,
			ownership.Agent, ownership.FilePath,
		); err != nil {
			return fmt.Errorf("removing exact session source baseline: %w", err)
		}
	}
	return nil
}

// rejectedSourceCandidates returns the candidates not admitted, preserving
// candidate order. Admission is exact (agent, file_path) pair equality.
func rejectedSourceCandidates(
	candidates []SessionSourcePath, admitted []SessionSourcePath,
) []SessionSourcePath {
	admittedSet := make(map[SessionSourcePath]struct{}, len(admitted))
	for _, source := range admitted {
		admittedSet[source] = struct{}{}
	}
	rejected := make([]SessionSourcePath, 0, max(0, len(candidates)-len(admitted)))
	for _, source := range candidates {
		if _, ok := admittedSet[source]; ok {
			continue
		}
		rejected = append(rejected, source)
	}
	return rejected
}

// deleteSessionSourceBaselinesTx removes baseline rows matching the sources'
// (agent, file_path) pairs under machine, restricted by an optional extra
// condition, chunked to stay under SQLite's bind-variable limit.
func deleteSessionSourceBaselinesTx(
	ctx context.Context,
	tx *sql.Tx,
	machine string,
	sources []SessionSourcePath,
	condition string,
	what string,
) error {
	for start := 0; start < len(sources); start += baselinePairChunk {
		end := min(start+baselinePairChunk, len(sources))
		filter, args, ok := buildSourcePairFilter(sources[start:end])
		if !ok {
			continue
		}
		execArgs := append([]any{machine}, args...)
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM local_session_source_baselines
			WHERE machine = ? AND `+filter+condition, execArgs...,
		); err != nil {
			return fmt.Errorf(
				"removing %s session source baselines: %w", what, err,
			)
		}
	}
	return nil
}

func deleteSessionSourceBaselinesAcrossMachinesTx(
	ctx context.Context,
	tx *sql.Tx,
	sources []SessionSourcePath,
	what string,
) error {
	for start := 0; start < len(sources); start += baselinePairChunk {
		end := min(start+baselinePairChunk, len(sources))
		filter, args, ok := buildSourcePairFilter(sources[start:end])
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM local_session_source_baselines
			WHERE `+filter, args...,
		); err != nil {
			return fmt.Errorf(
				"removing %s session source baselines across machines: %w",
				what, err,
			)
		}
	}
	return nil
}

// baselinePairChunk bounds how many (agent, file_path) pairs bind into one
// set-based baseline statement. Warm no-op reconciliation replays a full
// discovery page (up to reconciliationPageSize) of unchanged sources every
// pass, so folding those per-source round trips into a handful of set-based
// statements keeps the unchanged path from allocating one prepared-statement
// exec per archived source. The chunk keeps the bind-variable count well under
// SQLite's default limit regardless of the caller's page size.
const baselinePairChunk = 200

// buildSourcePairFilter renders a row-value IN clause matching each non-empty
// (agent, file_path) pair and returns the SQL fragment plus its bind arguments
// in pair order. It returns ok=false when the batch holds no usable pair.
func buildSourcePairFilter(sources []SessionSourcePath) (string, []any, bool) {
	args := make([]any, 0, len(sources)*2)
	var sb strings.Builder
	sb.Grow(len("(agent, file_path) IN (VALUES )") + len(sources)*len("(?,?),"))
	sb.WriteString("(agent, file_path) IN (VALUES ")
	for _, source := range sources {
		if source.Agent == "" || source.FilePath == "" {
			continue
		}
		if len(args) > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?)")
		args = append(args, source.Agent, source.FilePath)
	}
	if len(args) == 0 {
		return "", nil, false
	}
	sb.WriteString(")")
	return sb.String(), args, true
}

// ListActiveSessionSourceAttributions returns every distinct machine label
// represented by active rows at the requested exact sources. A shared provider
// database may contain sessions admitted under multiple immutable labels, so
// the result is intentionally not collapsed to one machine per path.
func (db *DB) ListActiveSessionSourceAttributions(
	ctx context.Context,
	sources []SessionSourcePath,
) ([]SessionSourceAttribution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := make(map[SessionSourceAttribution]struct{})
	for start := 0; start < len(sources); start += baselinePairChunk {
		end := min(start+baselinePairChunk, len(sources))
		filter, args, ok := buildSourcePairFilter(sources[start:end])
		if !ok {
			continue
		}
		rows, err := db.getReader().QueryContext(ctx, `
			SELECT DISTINCT machine, agent, file_path
			FROM sessions
			WHERE `+filter+`
			  AND file_path IS NOT NULL
			  AND deleted_at IS NULL
			  AND source_missing_at IS NULL`, args...)
		if err != nil {
			return nil, fmt.Errorf("listing active session source attributions: %w", err)
		}
		for rows.Next() {
			var attribution SessionSourceAttribution
			if err := rows.Scan(
				&attribution.Machine,
				&attribution.Agent,
				&attribution.FilePath,
			); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf(
					"scanning active session source attribution: %w", err,
				)
			}
			seen[attribution] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf(
				"iterating active session source attributions: %w", err,
			)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf(
				"closing active session source attributions: %w", err,
			)
		}
	}
	attributions := make([]SessionSourceAttribution, 0, len(seen))
	for attribution := range seen {
		attributions = append(attributions, attribution)
	}
	sort.Slice(attributions, func(i, j int) bool {
		if attributions[i].Machine != attributions[j].Machine {
			return attributions[i].Machine < attributions[j].Machine
		}
		if attributions[i].Agent != attributions[j].Agent {
			return attributions[i].Agent < attributions[j].Agent
		}
		return attributions[i].FilePath < attributions[j].FilePath
	})
	return attributions, nil
}

func baselineActiveSessionSourcePathsTx(
	ctx context.Context,
	tx *sql.Tx,
	machine string,
	sources []SessionSourcePath,
) error {
	for start := 0; start < len(sources); start += baselinePairChunk {
		end := min(start+baselinePairChunk, len(sources))
		filter, args, ok := buildSourcePairFilter(sources[start:end])
		if !ok {
			continue
		}
		execArgs := append([]any{machine}, args...)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO local_session_source_baselines
				(session_id, machine, agent, file_path)
			SELECT id, machine, agent, file_path
			FROM sessions
			WHERE machine = ? AND `+filter+`
			  AND file_path IS NOT NULL AND deleted_at IS NULL
			  AND source_missing_at IS NULL
			ON CONFLICT(session_id) DO UPDATE SET
				machine = excluded.machine,
				agent = excluded.agent,
				file_path = excluded.file_path
			WHERE local_session_source_baselines.machine IS NOT excluded.machine
			   OR local_session_source_baselines.agent IS NOT excluded.agent
			   OR local_session_source_baselines.file_path IS NOT excluded.file_path`,
			execArgs...,
		); err != nil {
			return fmt.Errorf("baselining active session source paths: %w", err)
		}
	}
	return nil
}

// MarkSessionSourceMissing records unavailable source material only while the
// row is still owned by the exact agent and source observed by reconciliation.
// It deliberately does not change user-owned deletion state.
func (db *DB) MarkSessionSourceMissing(
	ctx context.Context,
	machine string,
	agent string,
	id string,
	filePath string,
) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	result, err := db.getWriter().ExecContext(ctx, `
		UPDATE sessions
		SET source_missing_at = COALESCE(
		        source_missing_at,
		        strftime('%Y-%m-%dT%H:%M:%fZ','now')
		    ),
		    local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE machine = ? AND agent = ? AND id = ? AND file_path = ?
		  AND source_missing_at IS NULL
		  AND EXISTS (
			SELECT 1 FROM local_session_source_baselines AS b
			WHERE b.session_id = sessions.id
			  AND b.machine = sessions.machine
			  AND b.agent = sessions.agent
			  AND b.file_path = sessions.file_path
		  )`,
		machine, agent, id, filePath,
	)
	if err != nil {
		return false, fmt.Errorf("marking exact session source missing: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting exact missing session source: %w", err)
	}
	return count > 0, nil
}

// ListStoredSourcePathHints returns active source paths for agent whose stored
// file_path falls under any affected source prefix. It is used by provider
// changed-path comparison to avoid losing sessions when the changed path is a
// sidecar or database event rather than the exact persisted source path.
func (db *DB) ListStoredSourcePathHints(
	agent string,
	scopes []StoredSourcePathHintScope,
) ([]string, error) {
	return db.ListStoredSourcePathHintsContext(
		context.Background(), agent, scopes,
	)
}

// ListStoredSourcePathHintsContext is ListStoredSourcePathHints with
// caller-controlled cancellation for watcher classification.
func (db *DB) ListStoredSourcePathHintsContext(
	ctx context.Context,
	agent string,
	scopes []StoredSourcePathHintScope,
) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if agent == "" {
		return nil, nil
	}
	scopes = normalizeStoredSourcePathHintScopes(scopes)
	if len(scopes) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{})
	var hints []string
	for start := 0; start < len(scopes); start += storedSourcePathHintRootBatchSize {
		end := min(start+storedSourcePathHintRootBatchSize, len(scopes))
		batch := scopes[start:end]
		query, args := storedSourcePathHintQuery(agent, batch)
		rows, err := db.getReader().QueryContext(ctx, query, args...)
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

func normalizeStoredSourcePathHintScopes(
	scopes []StoredSourcePathHintScope,
) []StoredSourcePathHintScope {
	byPath := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		path := cleanStoredSourcePathHint(scope.Path)
		if path == "" || path == "." {
			continue
		}
		byPath[path] = byPath[path] || scope.IncludeVirtualMembers
	}
	out := make([]StoredSourcePathHintScope, 0, len(byPath))
	for path, includeVirtualMembers := range byPath {
		out = append(out, StoredSourcePathHintScope{
			Path: path, IncludeVirtualMembers: includeVirtualMembers,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

func storedSourcePathHintQuery(
	agent string,
	scopes []StoredSourcePathHintScope,
) (string, []any) {
	selects := make([]string, 0, len(scopes)*3)
	var args []any
	appendSelect := func(predicate string, values ...any) {
		selects = append(selects, `SELECT file_path
			FROM sessions
			WHERE agent = ?
			  AND file_path IS NOT NULL
			  AND deleted_at IS NULL
			  AND source_missing_at IS NULL
			  AND `+predicate)
		args = append(args, agent)
		args = append(args, values...)
	}
	for _, scope := range scopes {
		root := cleanStoredSourcePathHint(scope.Path)
		if root == "" || root == "." {
			continue
		}
		descendantPrefix, descendantEnd := activeSessionSourceBounds(root)
		appendSelect(`file_path = ?`, root)
		appendSelect(`file_path >= ? AND file_path < ?`, descendantPrefix, descendantEnd)
		if scope.IncludeVirtualMembers {
			appendSelect(`file_path >= ? AND file_path < ?`, root+"#", root+"$")
		}
	}
	if len(selects) == 0 {
		return `SELECT file_path FROM sessions WHERE 0`, nil
	}
	query := `SELECT file_path FROM (` + strings.Join(selects, " UNION ALL ") + `)
		ORDER BY file_path`
	return query, args
}

func cleanStoredSourcePathHint(path string) string {
	// Remote source paths are stored with forward slashes even when the
	// collector runs on Windows. Keep those paths in their stored syntax so a
	// collector never rewrites a remote host's separators before the indexed
	// lookup. Native Windows paths contain backslashes and still use filepath.
	if strings.Contains(path, "/") {
		return pathpkg.Clean(path)
	}
	return filepath.Clean(path)
}

func storedSourcePathHintInAnyRoot(
	path string,
	scopes []StoredSourcePathHintScope,
) bool {
	for _, scope := range scopes {
		if storedSourcePathHintInRoot(path, scope) {
			return true
		}
	}
	return false
}

// StoredSourcePathHintScopesContain reports whether one stored or discovered
// source path lies inside any of the bounded scopes, using the same
// platform-aware containment the SQLite queries in this file re-check. It is
// the single Go-side authority for path-to-scope membership: the SQL LIKE
// prefilters stay a superset of this predicate for ASCII paths, and every
// caller that pages or admits by scope must apply it rather than comparing
// prefixes itself.
func StoredSourcePathHintScopesContain(
	path string, scopes []StoredSourcePathHintScope,
) bool {
	return storedSourcePathHintInAnyRoot(path, scopes)
}

func storedSourcePathHintInRoot(path string, scope StoredSourcePathHintScope) bool {
	path = cleanStoredSourcePathHint(path)
	root := cleanStoredSourcePathHint(scope.Path)
	if storedSourcePathSameOrDescendant(path, root) {
		return true
	}
	if !scope.IncludeVirtualMembers || len(path) < len(root)+2 ||
		path[len(root)] != '#' {
		return false
	}
	// A virtual member is the container plus '#' and a nonempty single
	// segment. The container prefix folds with the same platform semantics as
	// the directory branch; the member segment rule stays byte-exact.
	if rel, err := filepath.Rel(root, path[:len(root)]); err != nil || rel != "." {
		return false
	}
	member := path[len(root)+1:]
	return !strings.ContainsAny(member, `/\`)
}

// storedSourcePathSameOrDescendant compares with filepath.Rel semantics so
// containment matches platform path equality: case-folded per element on
// Windows, byte-exact on Unix. This keeps the Go predicate aligned with the
// ASCII-case-insensitive SQL LIKE prefilter on Windows while staying exact on
// case-sensitive filesystems.
func storedSourcePathSameOrDescendant(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sqliteLikeEscape(value string) string {
	value = strings.ReplaceAll(value, `!`, `!!`)
	value = strings.ReplaceAll(value, `%`, `!%`)
	value = strings.ReplaceAll(value, `_`, `!_`)
	return value
}

// ListOwnedSessionIDsForExport returns the IDs of locally-owned, non-deleted
// sessions for artifact export, ordered by id. Unlike ListSessions it does not
// apply the sidebar visibility filter (message_count > 0), so zero-message
// usage-only sessions are still published.
func (db *DB) ListOwnedSessionIDsForExport(ctx context.Context) ([]string, error) {
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT id FROM sessions
		 WHERE (
			machine = 'local' OR machine = (
				SELECT value FROM pg_sync_state
				WHERE key = 'artifact_local_machine_name'
			)
		 ) AND deleted_at IS NULL
		 ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing sessions for artifact export: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning export session ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating export session IDs: %w", err)
	}
	return ids, nil
}

// GetDataVersionByPath returns the minimum data_version for non-source-missing
// sessions matching a file_path. Returns 0 when no eligible session exists.
func (db *DB) GetDataVersionByPath(path string) int {
	var v int
	err := db.getReader().QueryRow(
		"SELECT MIN(data_version) FROM sessions"+
			" WHERE file_path = ?"+
			" AND source_missing_at IS NULL", path,
	).Scan(&v)
	if err != nil {
		return 0
	}
	return v
}

// GetDataVersionByAgentPath is GetDataVersionByPath scoped to the agent that
// owns the source path.
const getDataVersionByAgentPathQuery = "SELECT MIN(data_version) FROM sessions" +
	" INDEXED BY idx_sessions_file_path" +
	" WHERE file_path = ? AND agent = ?" +
	" AND source_missing_at IS NULL"

func (db *DB) GetDataVersionByAgentPath(path, agent string) int {
	var v int
	err := db.getReader().QueryRow(getDataVersionByAgentPathQuery, path, agent).
		Scan(&v)
	if err != nil {
		return 0
	}
	return v
}

// ResetAllMtimes zeroes file_mtime for every session, forcing
// the next sync to re-process all files regardless of whether
// their size+mtime matches what was previously stored. It also
// clears the provider_freshness side-table so the per-component
// stat digest cannot defeat the forced re-sync: without this, a
// Codebuff/Freebuff session whose mtime was zeroed would still
// skip re-processing via the digest shortcut, preserving stale
// parsed data indefinitely.
func (db *DB) ResetAllMtimes() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	// Both statements run in one transaction: a failure between them
	// would otherwise leave mtimes zeroed with digests intact,
	// letting the stat-digest shortcut defeat the forced re-sync.
	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning mtime reset tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		"UPDATE sessions SET file_mtime = 0",
	); err != nil {
		return fmt.Errorf("resetting mtimes: %w", err)
	}
	// Clear provider_freshness so the stat-digest shortcut cannot
	// defeat the forced re-sync. A DELETE (not a walk) is safe
	// because the side-table is rebuilt on the next sync pass.
	if _, err := tx.Exec(
		"DELETE FROM provider_freshness",
	); err != nil {
		return fmt.Errorf("clearing provider_freshness: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing mtime reset: %w", err)
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

// GetActiveProjectLabels returns every project attached to a non-deleted
// session, including fork and subagent sessions whose unique usage is eligible
// for aggregation.
func (db *DB) GetActiveProjectLabels(ctx context.Context) ([]string, error) {
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT DISTINCT project
		 FROM sessions
		 WHERE deleted_at IS NULL
		 ORDER BY project`)
	if err != nil {
		return nil, fmt.Errorf("querying active project labels: %w", err)
	}
	defer rows.Close()

	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, fmt.Errorf("scanning active project label: %w", err)
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
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

// BranchInfo is a (project, branch) pair, keyed by project so same-named
// branches across repos stay distinct.
type BranchInfo struct {
	Project string `json:"project"`
	Branch  string `json:"branch"`
	Token   string `json:"token"`
}

// GetBranches returns distinct (project, git_branch) pairs, including the empty
// branch used for sessions with no recorded branch. Scoping matches
// GetProjects/GetAgents (root sessions with messages) so the dropdown reflects
// real work rather than subagents.
func (db *DB) GetBranches(
	ctx context.Context,
	excludeOneShot, excludeAutomated bool,
) ([]BranchInfo, error) {
	q := `SELECT DISTINCT project, git_branch
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
	q += " ORDER BY project, git_branch"
	rows, err := db.getReader().QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("querying branches: %w", err)
	}
	defer rows.Close()

	branches := []BranchInfo{}
	for rows.Next() {
		var bi BranchInfo
		if err := rows.Scan(&bi.Project, &bi.Branch); err != nil {
			return nil, fmt.Errorf("scanning branch: %w", err)
		}
		bi.Token = EncodeBranchFilterToken(bi.Project, bi.Branch)
		branches = append(branches, bi)
	}
	return branches, rows.Err()
}

// scanSessionRows iterates rows and scans each using
// scanSessionRow.
func scanSessionRows(rows *sql.Rows) ([]Session, error) {
	return scanSessionRowsWithSource(rows, false)
}

func scanSessionRowsWithSource(rows *sql.Rows, includeSource bool) ([]Session, error) {
	sessions := []Session{}
	for rows.Next() {
		s, err := scanSessionRowWithSource(rows, includeSource)
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
			&s.AgentLabel, &s.Entrypoint, &s.SessionKind,
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
			&s.TranscriptFidelity,
			&s.ParserMalformedLines, &s.IsTruncated,
			&s.DeletedAt, &s.TerminationStatus, &s.TranscriptRevision,
			&s.FilePath, &s.FileSize, &s.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning prune candidate: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// SoftDeleteSession moves an active session to user trash. Source availability
// is independent and is left unchanged.
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

// SoftDeleteSessions moves multiple sessions to user trash. Existing user
// deletions are skipped and source availability is left unchanged.
func (db *DB) SoftDeleteSessions(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return 0, fmt.Errorf("beginning soft-delete tx: %w", err)
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

		res, err := tx.Exec(
			`UPDATE sessions
			 SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'),
			     local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			 WHERE id IN (`+placeholders+`)
			   AND deleted_at IS NULL`,
			args...,
		)
		if err != nil {
			return 0, fmt.Errorf("soft-deleting batch: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing soft-delete tx: %w", err)
	}
	return total, nil
}

// RestoreSession clears deleted_at, makes the session visible again, and
// invalidates source freshness so changes made while it was trashed are parsed.
// Returns the number of rows affected (0 if session doesn't exist or is not in
// trash).
func (db *DB) RestoreSession(id string) (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(context.Background(), nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(
		`UPDATE sessions
		 SET deleted_at = NULL,
		     data_version = ?,
		     local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ? AND deleted_at IS NOT NULL`,
		max(CurrentDataVersion()-1, 0), id,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		if _, err := tx.Exec(
			"DELETE FROM local_session_source_baselines WHERE session_id = ?", id,
		); err != nil {
			return 0, err
		}
		// The source may have changed while this member was in trash. Force the
		// next sync to reparse it instead of trusting a digest persisted while
		// the member was intentionally skipped. Delete by path because a path can
		// have provider aliases in the freshness table.
		if _, err := tx.Exec(
			`DELETE FROM provider_freshness
			 WHERE file_path = (
				SELECT file_path FROM sessions WHERE id = ?
			 )`,
			id,
		); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
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

	aliasIDs, err := sessionAliasIDsTx(
		tx, "deleted_at IS NOT NULL",
	)
	if err != nil {
		return 0, err
	}
	ids, err := sessionIDsTx(
		tx, "deleted_at IS NOT NULL",
	)
	if err != nil {
		return 0, err
	}

	// Record all trashed session IDs before deleting.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO excluded_sessions (id)
		 SELECT id FROM sessions
		 WHERE deleted_at IS NOT NULL`,
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
			&s.AgentLabel, &s.Entrypoint, &s.SessionKind,
			&s.FirstMessage, &s.DisplayName, &s.SessionName, &s.StartedAt, &s.EndedAt,
			&s.MessageCount, &s.UserMessageCount,
			&s.ParentSessionID, &s.ParserParentSessionID, &s.RelationshipType,
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
			&s.TranscriptFidelity,
			&s.ParserMalformedLines, &s.IsTruncated,
			&s.LastWriteIncremental,
			&s.DeletedAt, &s.DeletionCause, &s.SourceMissingAt,
			&s.TerminationStatus, &s.FilePath, &s.FileSize,
			&s.FileMtime, &s.NextOrdinal, &s.LastEntryUUID,
			&s.FileInode, &s.FileDevice,
			&s.FileHash, &s.LocalModifiedAt,
			&s.TranscriptRevision, &s.CreatedAt, &s.ProjectAssigned,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning session: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// ListSessionsForMirrorWindow returns sessions whose sync_marker lies in
// [since, +inf); the lower bound is inclusive and an empty since is
// unbounded. The marker is the trigger-maintained max of the four sync
// signals, so "marker >= since" is equivalent to "any signal >= since".
// Inclusive selection is required for mirror pushes: a boundary-equal
// update must be re-selected (the caller dedupes with fingerprints).
//
// The window deliberately has no upper bound. The marker is a MAX over
// timestamp signals, so one future-dated signal (for example a
// clock-skewed file_mtime) pushes it past any wall-clock cutoff; an upper
// bound would then exclude the session from every incremental window
// until wall time caught up, leaving later real changes (content,
// local_modified_at) unmirrored. Without the bound such a session is
// merely a perpetual candidate whose unchanged fingerprint is cheaply
// skipped on each push.
func (db *DB) ListSessionsForMirrorWindow(
	ctx context.Context, since string,
	projects, excludeProjects []string,
) ([]Session, error) {
	query := "SELECT " + sessionFullCols + " FROM sessions"
	var (
		args  []any
		where []string
	)
	if since != "" {
		normalized, err := normalizeMirrorWindowBound(since)
		if err != nil {
			return nil, err
		}
		where = append(where, "sync_marker >= ?")
		args = append(args, normalized)
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
	query += " ORDER BY created_at"

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"listing sessions for mirror window since %s: %w", since, err,
		)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var s Session
		err := rows.Scan(
			&s.ID, &s.Project, &s.Machine, &s.Agent,
			&s.AgentLabel, &s.Entrypoint, &s.SessionKind,
			&s.FirstMessage, &s.DisplayName, &s.SessionName, &s.StartedAt, &s.EndedAt,
			&s.MessageCount, &s.UserMessageCount,
			&s.ParentSessionID, &s.ParserParentSessionID, &s.RelationshipType,
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
			&s.TranscriptFidelity,
			&s.ParserMalformedLines, &s.IsTruncated,
			&s.LastWriteIncremental,
			&s.DeletedAt, &s.DeletionCause, &s.SourceMissingAt,
			&s.TerminationStatus, &s.FilePath, &s.FileSize,
			&s.FileMtime, &s.NextOrdinal, &s.LastEntryUUID,
			&s.FileInode, &s.FileDevice,
			&s.FileHash, &s.LocalModifiedAt,
			&s.TranscriptRevision, &s.CreatedAt, &s.ProjectAssigned,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning session: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// CountSessionsForMirrorScope returns the number of sessions in the given
// project scope, matching ListSessionsForMirrorWindow's project filtering
// with no time bound. Mirror pushes use this for a cheap diagnostics count
// (Diagnostics.LocalSessionCount) without materializing every session.
func (db *DB) CountSessionsForMirrorScope(
	ctx context.Context, projects, excludeProjects []string,
) (int, error) {
	query := "SELECT COUNT(*) FROM sessions"
	var (
		args  []any
		where []string
	)
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
	var count int
	if err := db.getReader().QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting sessions for mirror scope: %w", err)
	}
	return count, nil
}

// normalizeMirrorWindowBound parses an RFC3339Nano timestamp and formats it
// as ms-precision UTC text matching the sync_marker column format, so the
// bound compares correctly against trigger-maintained markers.
func normalizeMirrorWindowBound(bound string) (string, error) {
	parsed, err := time.Parse(time.RFC3339Nano, bound)
	if err != nil {
		return "", fmt.Errorf("parsing mirror window bound %q: %w", bound, err)
	}
	return parsed.UTC().Format("2006-01-02T15:04:05.000Z"), nil
}

// SessionProjectsByIDs returns each session's current project keyed by session
// ID. IDs with no sessions row are absent from the result, so a caller can tell
// "unknown session" (missing key) from "empty project" (present, empty value).
// It is the live-project source for scoping a filtered push by each session's
// current project rather than by a possibly stale mirror.
func (db *DB) SessionProjectsByIDs(
	ctx context.Context, ids []string,
) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	err := queryChunked(ids, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		rows, err := db.getReader().QueryContext(ctx,
			"SELECT id, project FROM sessions WHERE id IN "+placeholders, args...)
		if err != nil {
			return fmt.Errorf("reading session projects: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, project string
			if err := rows.Scan(&id, &project); err != nil {
				return fmt.Errorf("scanning session project: %w", err)
			}
			out[id] = project
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
