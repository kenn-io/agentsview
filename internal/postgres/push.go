package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/wesm/agentsview/internal/db"
)

const lastPushBoundaryStateKey = "last_push_boundary_state"

// syncStateStore abstracts sync state read/write operations on the
// local database. Used by push boundary state helpers.
type syncStateStore interface {
	GetSyncState(key string) (string, error)
	SetSyncState(key, value string) error
}

type pushBoundaryState struct {
	Cutoff       string            `json:"cutoff"`
	Fingerprints map[string]string `json:"fingerprints"`
}

// PushResult summarizes a push sync operation.
type PushResult struct {
	SessionsPushed int
	MessagesPushed int
	Errors         int
	Duration       time.Duration
}

// PushProgress is reported after each batch during Push.
type PushProgress struct {
	SessionsDone  int
	SessionsTotal int
	MessagesDone  int
	Errors        int
}

// Push syncs local sessions and messages to PostgreSQL.
// The onProgress callback, if non-nil, is called after each
// batch with current totals.
func (s *Sync) Push(
	ctx context.Context, full bool,
	onProgress func(PushProgress),
) (PushResult, error) {
	start := time.Now()
	var result PushResult

	if err := s.normalizeSyncTimestamps(ctx); err != nil {
		return result, err
	}

	lastPush, err := s.local.GetSyncState("last_push_at")
	if err != nil {
		return result, fmt.Errorf(
			"reading last_push_at: %w", err,
		)
	}
	if full {
		lastPush = ""
		// Caller requested a full push — the PG schema
		// may have been dropped since schemaDone was set.
		// Clear the memo so EnsureSchema re-runs.
		s.schemaMu.Lock()
		s.schemaDone = false
		s.schemaMu.Unlock()
		if err := s.normalizeSyncTimestamps(
			ctx,
		); err != nil {
			return result, err
		}
		// When a filtered full push runs, clear persisted
		// watermark and boundary state so the next
		// unfiltered push also starts from scratch.
		if s.isFiltered() {
			if err := clearPushState(s.local); err != nil {
				return result, err
			}
		}
	}

	// Coherence check: if the local watermark says we've
	// pushed before but PG has zero sessions for this
	// machine, the PG side was reset (schema dropped, DB
	// recreated, etc.). Force a full push so all sessions
	// are re-synced.
	if lastPush != "" {
		pgCount, cErr := s.pgSessionCount(ctx)
		if cErr != nil {
			return result, cErr
		}
		if pgCount == 0 {
			log.Printf(
				"pgsync: local watermark set but PG has "+
					"0 sessions for machine %q; "+
					"forcing full push",
				s.machine,
			)
			lastPush = ""
			full = true
			s.schemaMu.Lock()
			s.schemaDone = false
			s.schemaMu.Unlock()
			if err := s.normalizeSyncTimestamps(
				ctx,
			); err != nil {
				return result, err
			}
			// Filtered push against a reset PG: clear
			// watermark and boundary state so the next
			// unfiltered push also starts from scratch.
			if s.isFiltered() {
				if err := clearPushState(s.local); err != nil {
					return result, err
				}
			}
		}
	}
	if err := s.syncModelPricing(ctx); err != nil {
		return result, err
	}

	cutoff := time.Now().UTC().Format(LocalSyncTimestampLayout)

	allSessions, err := s.local.ListSessionsModifiedBetween(
		ctx, lastPush, cutoff, s.projects, s.excludeProjects,
	)
	if err != nil {
		return result, fmt.Errorf(
			"listing modified sessions: %w", err,
		)
	}

	sessionByID := make(
		map[string]db.Session, len(allSessions),
	)
	for _, sess := range allSessions {
		sessionByID[sess.ID] = sess
	}

	var priorFingerprints map[string]string
	var boundaryState map[string]string
	var boundaryOK bool
	if !full {
		var bErr error
		priorFingerprints, boundaryState, boundaryOK, bErr = readBoundaryAndFingerprints(
			s.local, lastPush,
		)
		if bErr != nil {
			return result, bErr
		}
	}

	if lastPush != "" {
		ok := boundaryOK
		windowStart, err := PreviousLocalSyncTimestamp(
			lastPush,
		)
		if err != nil {
			return result, fmt.Errorf(
				"computing push boundary window before %s: %w",
				lastPush, err,
			)
		}
		boundarySessions, err := s.local.ListSessionsModifiedBetween(
			ctx, windowStart, lastPush, s.projects, s.excludeProjects,
		)
		if err != nil {
			return result, fmt.Errorf(
				"listing push boundary sessions: %w", err,
			)
		}

		for _, sess := range boundarySessions {
			marker := localSessionSyncMarker(sess)
			if marker != lastPush {
				continue
			}
			if ok {
				fp := sessionPushFingerprint(sess)
				if boundaryState[sess.ID] == fp {
					continue
				}
			}
			if _, exists := sessionByID[sess.ID]; exists {
				continue
			}
			sessionByID[sess.ID] = sess
		}
	}

	if len(priorFingerprints) > 0 {
		for id, sess := range sessionByID {
			fp := sessionPushFingerprint(sess)
			if priorFingerprints[id] == fp {
				delete(sessionByID, id)
			}
		}
	}

	var sessions []db.Session
	for _, sess := range sessionByID {
		sessions = append(sessions, sess)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})

	if len(sessions) == 0 {
		if s.isFiltered() {
			// Filtered pushes must not advance the global
			// watermark but should still update fingerprints
			// so repeated filtered runs stay incremental.
			// Use cutoff as the boundary key when lastPush
			// is empty (--full/PG reset) so that the next
			// filtered run can match fingerprints.
			boundaryKey := lastPush
			if boundaryKey == "" {
				boundaryKey = cutoff
			}
			if err := writePushBoundaryState(
				s.local, boundaryKey, sessions,
				priorFingerprints,
			); err != nil {
				return result, err
			}
		} else {
			if err := finalizePushState(
				s.local, cutoff, sessions, nil,
			); err != nil {
				return result, err
			}
		}
		result.Duration = time.Since(start)
		return result, nil
	}

	var pushed []db.Session
	const batchSize = 50
	for i := 0; i < len(sessions); i += batchSize {
		end := min(i+batchSize, len(sessions))
		batch := sessions[i:end]

		batchResult, err := s.pushBatch(
			ctx, batch, full, &pushed,
		)
		if err != nil {
			return result, err
		}
		if batchResult.ok {
			result.SessionsPushed += batchResult.sessions
			result.MessagesPushed += batchResult.messages
		} else {
			// Batch failed — retry each session individually
			// so one bad session doesn't block the rest.
			for _, sess := range batch {
				sr, retryErr := s.pushBatch(
					ctx, []db.Session{sess},
					full, &pushed,
				)
				if retryErr != nil {
					return result, retryErr
				}
				if sr.ok {
					result.SessionsPushed += sr.sessions
					result.MessagesPushed += sr.messages
				} else {
					result.Errors++
				}
			}
		}
		if onProgress != nil {
			onProgress(PushProgress{
				SessionsDone:  end,
				SessionsTotal: len(sessions),
				MessagesDone:  result.MessagesPushed,
				Errors:        result.Errors,
			})
		}
	}

	if s.isFiltered() {
		// Filtered pushes update fingerprints for pushed
		// sessions so subsequent filtered runs stay
		// incremental, but do not advance the global
		// watermark past sessions from other projects.
		// Use cutoff as the boundary key when lastPush is
		// empty (--full/PG reset) so the next filtered
		// run can match fingerprints instead of
		// re-pushing everything.
		boundaryKey := lastPush
		if boundaryKey == "" {
			boundaryKey = cutoff
		}
		if err := writePushBoundaryState(
			s.local, boundaryKey, pushed,
			priorFingerprints,
		); err != nil {
			return result, err
		}
	} else {
		// When all sessions succeeded, advance the watermark
		// to cutoff. When some failed, keep the watermark at
		// lastPush so the failed sessions (plus any
		// already-pushed ones) are re-evaluated next time.
		// Already-pushed sessions are fingerprint-matched and
		// skipped cheaply.
		finalizeCutoff := cutoff
		var mergedFingerprints map[string]string
		if result.Errors > 0 {
			finalizeCutoff = lastPush
			mergedFingerprints = priorFingerprints
		}
		if err := finalizePushState(
			s.local, finalizeCutoff, pushed,
			mergedFingerprints,
		); err != nil {
			return result, err
		}
	}

	result.Duration = time.Since(start)
	return result, nil
}

// pgSessionCount returns the number of sessions in PG for
// this machine. Used to detect schema resets.
func (s *Sync) pgSessionCount(
	ctx context.Context,
) (int, error) {
	var count int
	err := s.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sessions WHERE machine = $1",
		s.machine,
	).Scan(&count)
	if err != nil {
		if isUndefinedTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf(
			"counting pg sessions: %w", err,
		)
	}
	return count, nil
}

type batchResult struct {
	ok       bool
	sessions int
	messages int
}

// pushBatch pushes a slice of sessions within a single
// transaction. On success it appends to pushed and returns
// ok=true with session/message counts. On a session-level
// error it rolls back and returns ok=false so the caller
// can retry individually. Fatal errors (BeginTx failure)
// return a non-nil error.
func (s *Sync) pushBatch(
	ctx context.Context,
	batch []db.Session,
	full bool,
	pushed *[]db.Session,
) (batchResult, error) {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return batchResult{}, fmt.Errorf(
			"begin pg tx: %w", err,
		)
	}

	n := 0
	msgs := 0
	for _, sess := range batch {
		if err := s.pushSession(
			ctx, tx, sess,
		); err != nil {
			log.Printf(
				"pgsync: session %s: %v",
				sess.ID, err,
			)
			_ = tx.Rollback()
			*pushed = (*pushed)[:len(*pushed)-n]
			return batchResult{}, nil
		}

		msgCount, err := s.pushMessages(
			ctx, tx, sess.ID, full,
		)
		if err != nil {
			log.Printf(
				"pgsync: session %s: %v",
				sess.ID, err,
			)
			_ = tx.Rollback()
			*pushed = (*pushed)[:len(*pushed)-n]
			return batchResult{}, nil
		}

		// Bump updated_at when messages were rewritten
		// but pushSession was a metadata no-op (its
		// WHERE clause skips unchanged rows).
		if msgCount > 0 {
			if _, err := tx.ExecContext(ctx, `
				UPDATE sessions
				SET updated_at = NOW()
				WHERE id = $1`,
				sess.ID,
			); err != nil {
				log.Printf(
					"pgsync: bumping updated_at %s: %v",
					sess.ID, err,
				)
				_ = tx.Rollback()
				*pushed = (*pushed)[:len(*pushed)-n]
				return batchResult{}, nil
			}
		}

		*pushed = append(*pushed, sess)
		n++
		msgs += msgCount
	}

	if err := tx.Commit(); err != nil {
		log.Printf(
			"pgsync: batch commit failed: %v", err,
		)
		*pushed = (*pushed)[:len(*pushed)-n]
		return batchResult{}, nil
	}
	return batchResult{ok: true, sessions: n, messages: msgs}, nil
}

func finalizePushState(
	local syncStateStore,
	cutoff string,
	sessions []db.Session,
	priorFingerprints map[string]string,
) error {
	if err := local.SetSyncState(
		"last_push_at", cutoff,
	); err != nil {
		return fmt.Errorf("updating last_push_at: %w", err)
	}
	return writePushBoundaryState(
		local, cutoff, sessions, priorFingerprints,
	)
}

// clearPushState resets the watermark and boundary state so that
// the next push starts from scratch. Used when a filtered push
// runs --full or detects a PG reset, to avoid leaving stale
// state that would cause the next unfiltered push to skip
// sessions.
func clearPushState(local syncStateStore) error {
	if err := local.SetSyncState(
		lastPushBoundaryStateKey, "",
	); err != nil {
		return fmt.Errorf(
			"clearing boundary state: %w", err,
		)
	}
	if err := local.SetSyncState(
		"last_push_at", "",
	); err != nil {
		return fmt.Errorf(
			"clearing last_push_at: %w", err,
		)
	}
	return nil
}

func readBoundaryAndFingerprints(
	local syncStateStore,
	cutoff string,
) (
	fingerprints map[string]string,
	boundary map[string]string,
	boundaryOK bool,
	err error,
) {
	raw, err := local.GetSyncState(
		lastPushBoundaryStateKey,
	)
	if err != nil {
		return nil, nil, false, fmt.Errorf(
			"reading %s: %w",
			lastPushBoundaryStateKey, err,
		)
	}
	if raw == "" {
		return nil, nil, false, nil
	}
	var state pushBoundaryState
	if err := json.Unmarshal(
		[]byte(raw), &state,
	); err != nil {
		return nil, nil, false, nil
	}
	fingerprints = state.Fingerprints
	if cutoff != "" &&
		state.Cutoff == cutoff &&
		state.Fingerprints != nil {
		boundary = state.Fingerprints
		boundaryOK = true
	}
	return fingerprints, boundary, boundaryOK, nil
}

func writePushBoundaryState(
	local syncStateStore,
	cutoff string,
	sessions []db.Session,
	priorFingerprints map[string]string,
) error {
	state := pushBoundaryState{
		Cutoff: cutoff,
		Fingerprints: make(
			map[string]string,
			len(priorFingerprints)+len(sessions),
		),
	}
	maps.Copy(state.Fingerprints, priorFingerprints)
	for _, sess := range sessions {
		state.Fingerprints[sess.ID] = sessionPushFingerprint(sess)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf(
			"encoding %s: %w",
			lastPushBoundaryStateKey, err,
		)
	}
	if err := local.SetSyncState(
		lastPushBoundaryStateKey, string(data),
	); err != nil {
		return fmt.Errorf(
			"writing %s: %w",
			lastPushBoundaryStateKey, err,
		)
	}
	return nil
}

func localSessionSyncMarker(sess db.Session) string {
	marker, err := NormalizeLocalSyncTimestamp(sess.CreatedAt)
	if err != nil || marker == "" {
		if err != nil {
			log.Printf(
				"pgsync: normalizing CreatedAt %q for "+
					"session %s: %v (skipping non-RFC3339 "+
					"value)",
				sess.CreatedAt, sess.ID, err,
			)
		}
		marker = ""
	}
	for _, value := range []*string{
		sess.LocalModifiedAt,
		sess.EndedAt,
		sess.StartedAt,
	} {
		if value == nil {
			continue
		}
		normalized, err := NormalizeLocalSyncTimestamp(*value)
		if err != nil {
			continue
		}
		if normalized > marker {
			marker = normalized
		}
	}
	if sess.FileMtime != nil {
		fileMtime := time.Unix(
			0, *sess.FileMtime,
		).UTC().Format(LocalSyncTimestampLayout)
		if fileMtime > marker {
			marker = fileMtime
		}
	}
	if marker == "" {
		log.Printf(
			"pgsync: session %s: all timestamps failed "+
				"normalization, falling back to raw "+
				"CreatedAt %q",
			sess.ID, sess.CreatedAt,
		)
		marker = sess.CreatedAt
	}
	return marker
}

func sessionPushFingerprint(sess db.Session) string {
	fields := []string{
		sess.ID,
		sess.Project,
		sess.Machine,
		sess.Agent,
		stringValue(sess.FirstMessage),
		stringValue(sess.DisplayName),
		stringValue(sess.StartedAt),
		stringValue(sess.EndedAt),
		stringValue(sess.DeletedAt),
		fmt.Sprintf("%d", sess.MessageCount),
		fmt.Sprintf("%d", sess.UserMessageCount),
		fmt.Sprintf("%d", sess.TotalOutputTokens),
		fmt.Sprintf("%d", sess.PeakContextTokens),
		fmt.Sprintf("%t", sess.HasTotalOutputTokens),
		fmt.Sprintf("%t", sess.HasPeakContextTokens),
		stringValue(sess.ParentSessionID),
		sess.RelationshipType,
		stringValue(sess.FileHash),
		int64Value(sess.FileMtime),
		stringValue(sess.LocalModifiedAt),
		sess.CreatedAt,
		fmt.Sprintf("%d", sess.ToolFailureSignalCount),
		fmt.Sprintf("%d", sess.ToolRetryCount),
		fmt.Sprintf("%d", sess.EditChurnCount),
		fmt.Sprintf("%d", sess.ConsecutiveFailureMax),
		sess.Outcome,
		sess.OutcomeConfidence,
		sess.EndedWithRole,
		fmt.Sprintf("%d", sess.FinalFailureStreak),
		stringValue(sess.SignalsPendingSince),
		fmt.Sprintf("%d", sess.CompactionCount),
		fmt.Sprintf("%d", sess.MidTaskCompactionCount),
		float64Value(sess.ContextPressureMax),
		intPtrValue(sess.HealthScore),
		stringValue(sess.HealthGrade),
		fmt.Sprintf("%t", sess.HasToolCalls),
		fmt.Sprintf("%t", sess.HasContextData),
		fmt.Sprintf("%d", sess.DataVersion),
		sess.Cwd,
		sess.GitBranch,
		sess.SourceSessionID,
		sess.SourceVersion,
		fmt.Sprintf("%d", sess.ParserMalformedLines),
		fmt.Sprintf("%t", sess.IsTruncated),
		stringValue(sess.TerminationStatus),
	}
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, "%d:%s", len(f), f)
	}
	return b.String()
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func int64Value(value *int64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d", *value)
}

func float64Value(value *float64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%g", *value)
}

func intPtrValue(value *int) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d", *value)
}

// nilStr converts a nil or empty *string to SQL NULL.
// Sanitizes before checking emptiness so strings like "\x00"
// that reduce to "" are correctly returned as NULL.
func nilStr(s *string) any {
	if s == nil {
		return nil
	}
	v := sanitizePG(*s)
	if v == "" {
		return nil
	}
	return v
}

// nilStrTS converts a nil or empty *string timestamp to a
// *time.Time for PG TIMESTAMPTZ columns.
func nilStrTS(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	t, ok := ParseSQLiteTimestamp(*s)
	if !ok {
		return nil
	}
	return t
}

// pushSession upserts a single session into PG.
// File-level metadata (file_hash, file_path, file_size,
// file_mtime) is intentionally not synced to PG -- it is
// local-only and used solely by the sync engine to detect
// re-parsed sessions.
func (s *Sync) pushSession(
	ctx context.Context, tx *sql.Tx, sess db.Session,
) error {
	createdAt, _ := ParseSQLiteTimestamp(sess.CreatedAt)
	isAutomated := sess.IsAutomated
	_, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent,
			first_message, display_name,
			created_at, started_at, ended_at, deleted_at,
			message_count, user_message_count,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens,
			is_automated, data_version,
			cwd, git_branch, source_session_id,
			source_version, parser_malformed_lines,
			is_truncated, termination_status,
			parent_session_id, relationship_type,
			tool_failure_signal_count, tool_retry_count,
			edit_churn_count, consecutive_failure_max,
			outcome, outcome_confidence,
			ended_with_role, final_failure_streak,
			signals_pending_since,
			compaction_count, mid_task_compaction_count,
			context_pressure_max,
			health_score, health_grade,
			has_tool_calls, has_context_data,
			updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12, $13, $14,
			$15, $16, $17, $18,
			$19, $20, $21, $22, $23, $24, $25,
			$26, $27,
			$28, $29, $30, $31,
			$32, $33, $34, $35,
			$36,
			$37, $38,
			$39,
			$40, $41, $42, $43,
			NOW()
		)
		ON CONFLICT (id) DO UPDATE SET
			machine = EXCLUDED.machine,
			project = EXCLUDED.project,
			agent = EXCLUDED.agent,
			first_message = EXCLUDED.first_message,
			display_name = EXCLUDED.display_name,
			created_at = EXCLUDED.created_at,
			started_at = EXCLUDED.started_at,
			ended_at = EXCLUDED.ended_at,
			deleted_at = EXCLUDED.deleted_at,
			message_count = EXCLUDED.message_count,
			user_message_count = EXCLUDED.user_message_count,
			total_output_tokens = EXCLUDED.total_output_tokens,
			peak_context_tokens = EXCLUDED.peak_context_tokens,
			has_total_output_tokens = EXCLUDED.has_total_output_tokens,
			has_peak_context_tokens = EXCLUDED.has_peak_context_tokens,
			is_automated = EXCLUDED.is_automated,
			data_version = EXCLUDED.data_version,
			cwd = EXCLUDED.cwd,
			git_branch = EXCLUDED.git_branch,
			source_session_id = EXCLUDED.source_session_id,
			source_version = EXCLUDED.source_version,
			parser_malformed_lines = EXCLUDED.parser_malformed_lines,
			is_truncated = EXCLUDED.is_truncated,
			termination_status = EXCLUDED.termination_status,
			parent_session_id = EXCLUDED.parent_session_id,
			relationship_type = EXCLUDED.relationship_type,
			tool_failure_signal_count = EXCLUDED.tool_failure_signal_count,
			tool_retry_count = EXCLUDED.tool_retry_count,
			edit_churn_count = EXCLUDED.edit_churn_count,
			consecutive_failure_max = EXCLUDED.consecutive_failure_max,
			outcome = EXCLUDED.outcome,
			outcome_confidence = EXCLUDED.outcome_confidence,
			ended_with_role = EXCLUDED.ended_with_role,
			final_failure_streak = EXCLUDED.final_failure_streak,
			signals_pending_since = EXCLUDED.signals_pending_since,
			compaction_count = EXCLUDED.compaction_count,
			mid_task_compaction_count = EXCLUDED.mid_task_compaction_count,
			context_pressure_max = EXCLUDED.context_pressure_max,
			health_score = EXCLUDED.health_score,
			health_grade = EXCLUDED.health_grade,
			has_tool_calls = EXCLUDED.has_tool_calls,
			has_context_data = EXCLUDED.has_context_data,
			updated_at = NOW()
		WHERE sessions.machine IS DISTINCT FROM EXCLUDED.machine
			OR sessions.project IS DISTINCT FROM EXCLUDED.project
			OR sessions.agent IS DISTINCT FROM EXCLUDED.agent
			OR sessions.first_message IS DISTINCT FROM EXCLUDED.first_message
			OR sessions.display_name IS DISTINCT FROM EXCLUDED.display_name
			OR sessions.created_at IS DISTINCT FROM EXCLUDED.created_at
			OR sessions.started_at IS DISTINCT FROM EXCLUDED.started_at
			OR sessions.ended_at IS DISTINCT FROM EXCLUDED.ended_at
			OR sessions.deleted_at IS DISTINCT FROM EXCLUDED.deleted_at
			OR sessions.message_count IS DISTINCT FROM EXCLUDED.message_count
			OR sessions.user_message_count IS DISTINCT FROM EXCLUDED.user_message_count
			OR sessions.total_output_tokens IS DISTINCT FROM EXCLUDED.total_output_tokens
			OR sessions.peak_context_tokens IS DISTINCT FROM EXCLUDED.peak_context_tokens
			OR sessions.has_total_output_tokens IS DISTINCT FROM EXCLUDED.has_total_output_tokens
			OR sessions.has_peak_context_tokens IS DISTINCT FROM EXCLUDED.has_peak_context_tokens
			OR sessions.is_automated IS DISTINCT FROM EXCLUDED.is_automated
			OR sessions.data_version IS DISTINCT FROM EXCLUDED.data_version
			OR sessions.cwd IS DISTINCT FROM EXCLUDED.cwd
			OR sessions.git_branch IS DISTINCT FROM EXCLUDED.git_branch
			OR sessions.source_session_id IS DISTINCT FROM EXCLUDED.source_session_id
			OR sessions.source_version IS DISTINCT FROM EXCLUDED.source_version
			OR sessions.parser_malformed_lines IS DISTINCT FROM EXCLUDED.parser_malformed_lines
			OR sessions.is_truncated IS DISTINCT FROM EXCLUDED.is_truncated
			OR sessions.termination_status IS DISTINCT FROM EXCLUDED.termination_status
			OR sessions.parent_session_id IS DISTINCT FROM EXCLUDED.parent_session_id
			OR sessions.relationship_type IS DISTINCT FROM EXCLUDED.relationship_type
			OR sessions.tool_failure_signal_count IS DISTINCT FROM EXCLUDED.tool_failure_signal_count
			OR sessions.tool_retry_count IS DISTINCT FROM EXCLUDED.tool_retry_count
			OR sessions.edit_churn_count IS DISTINCT FROM EXCLUDED.edit_churn_count
			OR sessions.consecutive_failure_max IS DISTINCT FROM EXCLUDED.consecutive_failure_max
			OR sessions.outcome IS DISTINCT FROM EXCLUDED.outcome
			OR sessions.outcome_confidence IS DISTINCT FROM EXCLUDED.outcome_confidence
			OR sessions.ended_with_role IS DISTINCT FROM EXCLUDED.ended_with_role
			OR sessions.final_failure_streak IS DISTINCT FROM EXCLUDED.final_failure_streak
			OR sessions.signals_pending_since IS DISTINCT FROM EXCLUDED.signals_pending_since
			OR sessions.compaction_count IS DISTINCT FROM EXCLUDED.compaction_count
			OR sessions.mid_task_compaction_count IS DISTINCT FROM EXCLUDED.mid_task_compaction_count
			OR sessions.context_pressure_max IS DISTINCT FROM EXCLUDED.context_pressure_max
			OR sessions.health_score IS DISTINCT FROM EXCLUDED.health_score
			OR sessions.health_grade IS DISTINCT FROM EXCLUDED.health_grade
			OR sessions.has_tool_calls IS DISTINCT FROM EXCLUDED.has_tool_calls
			OR sessions.has_context_data IS DISTINCT FROM EXCLUDED.has_context_data`,
		sess.ID, s.machine,
		sanitizePG(sess.Project),
		sess.Agent,
		nilStr(sess.FirstMessage),
		nilStr(sess.DisplayName),
		createdAt,
		nilStrTS(sess.StartedAt),
		nilStrTS(sess.EndedAt),
		nilStrTS(sess.DeletedAt),
		sess.MessageCount, sess.UserMessageCount,
		sess.TotalOutputTokens, sess.PeakContextTokens,
		sess.HasTotalOutputTokens, sess.HasPeakContextTokens,
		isAutomated, sess.DataVersion,
		sess.Cwd, sess.GitBranch, sess.SourceSessionID,
		sess.SourceVersion, sess.ParserMalformedLines,
		sess.IsTruncated, nilStr(sess.TerminationStatus),
		nilStr(sess.ParentSessionID),
		sess.RelationshipType,
		sess.ToolFailureSignalCount, sess.ToolRetryCount,
		sess.EditChurnCount, sess.ConsecutiveFailureMax,
		sess.Outcome, sess.OutcomeConfidence,
		sess.EndedWithRole, sess.FinalFailureStreak,
		nilStr(sess.SignalsPendingSince),
		sess.CompactionCount, sess.MidTaskCompactionCount,
		sess.ContextPressureMax,
		sess.HealthScore, nilStr(sess.HealthGrade),
		sess.HasToolCalls, sess.HasContextData,
	)
	return err
}

// pushMessages replaces a session's messages and tool calls
// in PG. It skips the replacement when the PG message count
// already matches the local count, avoiding redundant work
// for metadata-only changes.
func (s *Sync) pushMessages(
	ctx context.Context,
	tx *sql.Tx,
	sessionID string,
	full bool,
) (int, error) {
	localCount, err := s.local.MessageCount(sessionID)
	if err != nil {
		return 0, fmt.Errorf(
			"counting local messages: %w", err,
		)
	}
	if localCount == 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM tool_result_events WHERE session_id = $1`,
			sessionID,
		); err != nil {
			return 0, fmt.Errorf(
				"deleting stale pg tool_result_events: %w", err,
			)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM tool_calls WHERE session_id = $1`,
			sessionID,
		); err != nil {
			return 0, fmt.Errorf(
				"deleting stale pg tool_calls: %w", err,
			)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM messages WHERE session_id = $1`,
			sessionID,
		); err != nil {
			return 0, fmt.Errorf(
				"deleting stale pg messages: %w", err,
			)
		}
		return 0, nil
	}

	var pgCount int
	var pgContentSum, pgContentMax, pgContentMin int64
	// Exact string fingerprint for the system-message ordinal set:
	// STRING_AGG produces e.g. "0,2,5" — impossible to collide for
	// distinct ordinal sets (unlike SUM or SUM+SUM-of-squares).
	var pgSystemFP sql.NullString
	var pgToolCallCount int
	var pgTCContentSum int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*),
			COALESCE(SUM(content_length), 0),
			COALESCE(MAX(content_length), 0),
			COALESCE(MIN(content_length), 0),
			STRING_AGG(ordinal::text, ',' ORDER BY ordinal)
				FILTER (WHERE is_system)
		 FROM messages
		 WHERE session_id = $1`,
		sessionID,
	).Scan(
		&pgCount, &pgContentSum,
		&pgContentMax, &pgContentMin,
		&pgSystemFP,
	); err != nil {
		return 0, fmt.Errorf(
			"counting pg messages: %w", err,
		)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*),
			COALESCE(SUM(result_content_length), 0)
		 FROM tool_calls
		 WHERE session_id = $1`,
		sessionID,
	).Scan(&pgToolCallCount, &pgTCContentSum); err != nil {
		return 0, fmt.Errorf(
			"counting pg tool_calls: %w", err,
		)
	}

	if !full && pgCount == localCount && pgCount > 0 {
		localSum, localMax, localMin, err := s.local.MessageContentFingerprint(sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local content fingerprint: %w",
				err,
			)
		}
		localSysFP, err := s.local.SystemMessageFingerprint(sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local system message fingerprint: %w", err,
			)
		}
		localTCCount, err := s.local.ToolCallCount(sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"counting local tool_calls: %w", err,
			)
		}
		localTCSum, err := s.local.ToolCallContentFingerprint(sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local tool_call content "+
					"fingerprint: %w", err,
			)
		}
		localTokenFP, err := s.local.MessageTokenFingerprint(sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local token fingerprint: %w", err,
			)
		}
		pgTokenFP, err := pgMessageTokenFingerprint(ctx, tx, sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"computing pg token fingerprint: %w", err,
			)
		}
		if localSum == pgContentSum &&
			localMax == pgContentMax &&
			localMin == pgContentMin &&
			localSysFP == pgSystemFP.String &&
			localTCCount == pgToolCallCount &&
			localTCSum == pgTCContentSum &&
			localTokenFP == pgTokenFP {
			return 0, nil
		}
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM tool_result_events
		WHERE session_id = $1
	`, sessionID); err != nil {
		return 0, fmt.Errorf(
			"deleting pg tool_result_events: %w", err,
		)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM tool_calls
		WHERE session_id = $1
	`, sessionID); err != nil {
		return 0, fmt.Errorf(
			"deleting pg tool_calls: %w", err,
		)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM messages
		WHERE session_id = $1
	`, sessionID); err != nil {
		return 0, fmt.Errorf(
			"deleting pg messages: %w", err,
		)
	}

	count := 0
	startOrdinal := 0
	for {
		msgs, err := s.local.GetMessages(
			ctx, sessionID, startOrdinal,
			db.MaxMessageLimit, true,
		)
		if err != nil {
			return count, fmt.Errorf(
				"reading local messages: %w", err,
			)
		}
		if len(msgs) == 0 {
			break
		}

		nextOrdinal := msgs[len(msgs)-1].Ordinal + 1
		if nextOrdinal <= startOrdinal {
			return count, fmt.Errorf(
				"pushMessages %s: ordinal did not "+
					"advance (start=%d, last=%d)",
				sessionID, startOrdinal,
				msgs[len(msgs)-1].Ordinal,
			)
		}

		if err := bulkInsertMessages(
			ctx, tx, sessionID, msgs,
		); err != nil {
			return count, err
		}
		if err := bulkInsertToolCalls(
			ctx, tx, sessionID, msgs,
		); err != nil {
			return count, err
		}
		if err := bulkInsertToolResultEvents(
			ctx, tx, sessionID, msgs,
		); err != nil {
			return count, err
		}
		count += len(msgs)
		startOrdinal = nextOrdinal
	}

	return count, nil
}

func pgMessageTokenFingerprint(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT ordinal, model, token_usage, context_tokens,
			output_tokens, has_context_tokens, has_output_tokens,
			claude_message_id, claude_request_id,
			source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain, is_compact_boundary
		 FROM messages
		 WHERE session_id = $1
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

const msgInsertBatch = 100

// bulkInsertMessages inserts messages using multi-row VALUES.
func bulkInsertMessages(
	ctx context.Context, tx *sql.Tx,
	sessionID string, msgs []db.Message,
) error {
	for i := 0; i < len(msgs); i += msgInsertBatch {
		end := min(i+msgInsertBatch, len(msgs))
		batch := msgs[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO messages (
			session_id, ordinal, role, content, thinking_text,
			timestamp, has_thinking, has_tool_use,
			content_length, is_system, model, token_usage,
			context_tokens, output_tokens,
			has_context_tokens, has_output_tokens,
			claude_message_id, claude_request_id,
			source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain,
			is_compact_boundary) VALUES `)
		args := make([]any, 0, len(batch)*24)
		for j, m := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			p := j*24 + 1
			fmt.Fprintf(&b,
				"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				p, p+1, p+2, p+3, p+4,
				p+5, p+6, p+7, p+8, p+9,
				p+10, p+11, p+12, p+13, p+14, p+15,
				p+16, p+17, p+18, p+19, p+20,
				p+21, p+22, p+23,
			)
			var ts any
			if m.Timestamp != "" {
				if t, ok := ParseSQLiteTimestamp(
					m.Timestamp,
				); ok {
					ts = t
				}
			}
			args = append(args,
				sessionID, m.Ordinal, m.Role,
				sanitizePG(m.Content),
				sanitizePG(m.ThinkingText), ts,
				m.HasThinking,
				m.HasToolUse, m.ContentLength, m.IsSystem,
				m.Model, string(m.TokenUsage),
				m.ContextTokens, m.OutputTokens,
				m.HasContextTokens, m.HasOutputTokens,
				m.ClaudeMessageID, m.ClaudeRequestID,
				m.SourceType, m.SourceSubtype, m.SourceUUID,
				m.SourceParentUUID, m.IsSidechain,
				m.IsCompactBoundary,
			)
		}
		if _, err := tx.ExecContext(
			ctx, b.String(), args...,
		); err != nil {
			return fmt.Errorf(
				"bulk inserting messages: %w", err,
			)
		}
	}
	return nil
}

// bulkInsertToolCalls inserts tool calls using multi-row VALUES.
func bulkInsertToolCalls(
	ctx context.Context, tx *sql.Tx,
	sessionID string, msgs []db.Message,
) error {
	// Collect all tool calls from messages.
	type tcRow struct {
		ordinal int
		index   int
		tc      db.ToolCall
	}
	var rows []tcRow
	for _, m := range msgs {
		for i, tc := range m.ToolCalls {
			rows = append(rows, tcRow{m.Ordinal, i, tc})
		}
	}
	if len(rows) == 0 {
		return nil
	}

	const tcBatch = 50
	for i := 0; i < len(rows); i += tcBatch {
		end := min(i+tcBatch, len(rows))
		batch := rows[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO tool_calls (
			session_id, tool_name, category,
			call_index, tool_use_id, input_json,
			skill_name, result_content_length,
			result_content, subagent_session_id,
			message_ordinal) VALUES `)
		args := make([]any, 0, len(batch)*11)
		for j, r := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			p := j*11 + 1
			fmt.Fprintf(&b,
				"($%d,$%d,$%d,$%d,$%d,$%d,"+
					"$%d,$%d,$%d,$%d,$%d)",
				p, p+1, p+2, p+3, p+4, p+5,
				p+6, p+7, p+8, p+9, p+10,
			)
			args = append(args,
				sessionID,
				sanitizePG(r.tc.ToolName),
				sanitizePG(r.tc.Category),
				r.index,
				sanitizePG(r.tc.ToolUseID),
				nilIfEmpty(r.tc.InputJSON),
				nilIfEmpty(r.tc.SkillName),
				nilIfZero(r.tc.ResultContentLength),
				nilIfEmpty(r.tc.ResultContent),
				nilIfEmpty(r.tc.SubagentSessionID),
				r.ordinal,
			)
		}
		if _, err := tx.ExecContext(
			ctx, b.String(), args...,
		); err != nil {
			return fmt.Errorf(
				"bulk inserting tool_calls: %w", err,
			)
		}
	}
	return nil
}

func bulkInsertToolResultEvents(
	ctx context.Context, tx *sql.Tx,
	sessionID string, msgs []db.Message,
) error {
	type evRow struct {
		ordinal int
		index   int
		ev      db.ToolResultEvent
	}
	var rows []evRow
	for _, m := range msgs {
		for i, tc := range m.ToolCalls {
			for _, ev := range tc.ResultEvents {
				rows = append(rows, evRow{m.Ordinal, i, ev})
			}
		}
	}
	if len(rows) == 0 {
		return nil
	}

	const evBatch = 100
	for i := 0; i < len(rows); i += evBatch {
		end := min(i+evBatch, len(rows))
		batch := rows[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO tool_result_events (
			session_id, tool_call_message_ordinal, call_index,
			tool_use_id, agent_id, subagent_session_id,
			source, status, content, content_length,
			timestamp, event_index) VALUES `)
		args := make([]any, 0, len(batch)*12)
		for j, r := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			p := j*12 + 1
			fmt.Fprintf(&b,
				"($%d,$%d,$%d,$%d,$%d,$%d,"+
					"$%d,$%d,$%d,$%d,$%d,$%d)",
				p, p+1, p+2, p+3, p+4, p+5,
				p+6, p+7, p+8, p+9, p+10, p+11,
			)
			var ts any
			if r.ev.Timestamp != "" {
				if t, ok := ParseSQLiteTimestamp(r.ev.Timestamp); ok {
					ts = t
				}
			}
			args = append(args,
				sessionID,
				r.ordinal,
				r.index,
				nilIfEmpty(r.ev.ToolUseID),
				nilIfEmpty(r.ev.AgentID),
				nilIfEmpty(r.ev.SubagentSessionID),
				sanitizePG(r.ev.Source),
				sanitizePG(r.ev.Status),
				sanitizePG(r.ev.Content),
				r.ev.ContentLength,
				ts,
				r.ev.EventIndex,
			)
		}
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			return fmt.Errorf("bulk inserting tool_result_events: %w", err)
		}
	}
	return nil
}

// normalizeSyncTimestamps ensures schema exists and normalizes
// local sync state timestamps.
func (s *Sync) normalizeSyncTimestamps(
	ctx context.Context,
) error {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if !s.schemaDone {
		if err := EnsureSchema(
			ctx, s.pg, s.schema,
		); err != nil {
			return err
		}
		s.schemaDone = true
	}
	return NormalizeLocalSyncStateTimestamps(s.local)
}

// sanitizePG strips null bytes and replaces invalid UTF-8
// sequences so text can be safely inserted into PostgreSQL,
// which enforces strict UTF-8 encoding.
func sanitizePG(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ToValidUTF8(s, "")
	return s
}

func nilIfEmpty(s string) any {
	s = sanitizePG(s)
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
