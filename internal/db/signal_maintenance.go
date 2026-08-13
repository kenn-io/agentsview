package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/secrets"
)

// ToolCallSignalFact is the bounded per-call fact set the incremental signal
// maintainer reads inside the incremental write transaction. It mirrors the
// columns extractToolCallRows reads from stored rows, so facts the maintainer
// sees match a full recompute over GetAllMessages.
type ToolCallSignalFact struct {
	MessageOrdinal int
	CallIndex      int
	ToolName       string
	Category       string
	InputJSON      string
	ResultContent  string
	EventStatus    string
	ToolUseID      string
}

// FindingDeleteKey addresses secret findings by their natural coordinates:
// every finding produced from a call's summarized result_content is removed
// when that call gains its first result event.
type FindingDeleteKey struct {
	MessageOrdinal int
	CallIndex      int
	LocationKind   string
}

// SignalDelta is the incremental signal/findings/state change applied inside
// the incremental write transaction, atomically with the message rows and
// the parser checkpoint.
type SignalDelta struct {
	Update            SessionSignalUpdate
	InsertFindings    []SecretFinding
	DeleteFindingKeys []FindingDeleteKey
	// State is the post-delta compact state row to persist (SQLite-only).
	State *SessionSignalState
}

// SessionSignalState is one session's persisted compact signal state row.
// TranscriptRevision and SignalVersion form the verification token: a state
// whose token disagrees with the stored rows must never be folded.
type SessionSignalState struct {
	SessionID          string
	State              []byte
	TranscriptRevision string
	SignalVersion      int
	UpdatedAt          string
}

// SignalMaintainer computes the incremental signal delta inside the
// incremental write transaction, after messages and result updates are
// applied but before the transaction commits. It may return a nil delta to
// decline maintenance: the write then invalidates the signal version as
// before and the caller debounces a full recompute.
type SignalMaintainer interface {
	MaintainTx(ctx context.Context, q SignalQuery) (*SignalDelta, error)
}

// SignalQuery is the read-only in-transaction view the maintainer uses.
// Every read reflects the transaction's uncommitted writes.
type SignalQuery interface {
	// Session returns the session row snapshot (signal columns plus the
	// outcome inputs). nil means the session row is gone.
	Session(ctx context.Context) (*Session, error)
	TranscriptRevision(ctx context.Context) (string, error)
	SignalState(ctx context.Context) (SessionSignalState, bool, error)
	// TrailingToolCalls returns the last n tool-call facts in
	// (message ordinal, call index) order, oldest first.
	TrailingToolCalls(ctx context.Context, n int) ([]ToolCallSignalFact, error)
	// ToolCallsByUseID returns the facts of the named calls after the
	// transaction's result updates, keyed by tool_use_id.
	ToolCallsByUseID(
		ctx context.Context, toolUseIDs []string,
	) ([]ToolCallSignalFact, error)
	// CallResultEvents returns the stored result events of one call in
	// event_index order, after the transaction's updates.
	CallResultEvents(
		ctx context.Context, messageOrdinal, callIndex int,
	) ([]ToolResultEvent, error)
}

// signalTxQuery implements SignalQuery over the incremental write
// transaction.
type signalTxQuery struct {
	tx        *sql.Tx
	sessionID string
}

func (q signalTxQuery) Session(
	ctx context.Context,
) (*Session, error) {
	var s Session
	var isAutomated, hasPeak, hasToolCalls, hasContextData int
	var unstructuredStart int
	var cpMax sql.NullFloat64
	var healthScore sql.NullInt64
	var healthGrade, endedAt, signalsPending sql.NullString
	err := q.tx.QueryRowContext(ctx, `
		SELECT message_count, is_automated, ended_at,
		       peak_context_tokens, has_peak_context_tokens,
		       tool_failure_signal_count, tool_retry_count,
		       edit_churn_count, consecutive_failure_max,
		       outcome, outcome_confidence, ended_with_role,
		       final_failure_streak, signals_pending_since,
		       compaction_count, mid_task_compaction_count,
		       context_pressure_max, health_score, health_grade,
		       has_tool_calls, has_context_data,
		       secret_leak_count, secrets_rules_version,
		       quality_signal_version, short_prompt_count,
		       unstructured_start, missing_success_criteria_count,
		       missing_verification_count, duplicate_prompt_count,
		       no_code_context_count, runaway_tool_loop_count
		 FROM sessions WHERE id = ?`,
		q.sessionID,
	).Scan(
		&s.MessageCount, &isAutomated, &endedAt,
		&s.PeakContextTokens, &hasPeak,
		&s.ToolFailureSignalCount, &s.ToolRetryCount,
		&s.EditChurnCount, &s.ConsecutiveFailureMax,
		&s.Outcome, &s.OutcomeConfidence, &s.EndedWithRole,
		&s.FinalFailureStreak, &signalsPending,
		&s.CompactionCount, &s.MidTaskCompactionCount,
		&cpMax, &healthScore, &healthGrade,
		&hasToolCalls, &hasContextData,
		&s.SecretLeakCount, &s.SecretsRulesVersion,
		&s.QualitySignalVersion, &s.ShortPromptCount,
		&unstructuredStart, &s.MissingSuccessCriteriaCount,
		&s.MissingVerificationCount, &s.DuplicatePromptCount,
		&s.NoCodeContextCount, &s.RunawayToolLoopCount,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(
			"loading session signal snapshot %s: %w", q.sessionID, err,
		)
	}
	s.ID = q.sessionID
	s.IsAutomated = isAutomated != 0
	s.HasPeakContextTokens = hasPeak != 0
	s.HasToolCalls = hasToolCalls != 0
	s.HasContextData = hasContextData != 0
	s.UnstructuredStart = unstructuredStart != 0
	if endedAt.Valid {
		s.EndedAt = &endedAt.String
	}
	if signalsPending.Valid {
		s.SignalsPendingSince = &signalsPending.String
	}
	if cpMax.Valid {
		v := cpMax.Float64
		s.ContextPressureMax = &v
	}
	if healthScore.Valid {
		v := int(healthScore.Int64)
		s.HealthScore = &v
	}
	if healthGrade.Valid {
		s.HealthGrade = &healthGrade.String
	}
	return &s, nil
}

func (q signalTxQuery) TranscriptRevision(
	ctx context.Context,
) (string, error) {
	var rev string
	if err := q.tx.QueryRowContext(ctx,
		`SELECT transcript_revision FROM sessions WHERE id = ?`,
		q.sessionID,
	).Scan(&rev); err != nil {
		return "", fmt.Errorf(
			"loading transcript revision %s: %w", q.sessionID, err,
		)
	}
	return rev, nil
}

func (q signalTxQuery) SignalState(
	ctx context.Context,
) (SessionSignalState, bool, error) {
	var st SessionSignalState
	err := q.tx.QueryRowContext(ctx, `
		SELECT state, transcript_revision, signal_version
		FROM session_signal_state WHERE session_id = ?`,
		q.sessionID,
	).Scan(&st.State, &st.TranscriptRevision, &st.SignalVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionSignalState{}, false, nil
	}
	if err != nil {
		return SessionSignalState{}, false, fmt.Errorf(
			"loading session signal state %s: %w", q.sessionID, err,
		)
	}
	st.SessionID = q.sessionID
	return st, true, nil
}

func (q signalTxQuery) TrailingToolCalls(
	ctx context.Context, n int,
) ([]ToolCallSignalFact, error) {
	rows, err := q.tx.QueryContext(ctx, `
		SELECT m.ordinal, COALESCE(tc.call_index, 0),
		       tc.tool_name, tc.category, COALESCE(tc.input_json, ''),
		       COALESCE(tc.result_content, ''),
		       COALESCE(tc.tool_use_id, ''),
		       COALESCE((
		           SELECT tre.status FROM tool_result_events tre
		           WHERE tre.session_id = tc.session_id
		             AND tre.tool_call_message_ordinal = m.ordinal
		             AND tre.call_index = COALESCE(tc.call_index, 0)
		           ORDER BY tre.event_index DESC, tre.id DESC
		           LIMIT 1
		       ), '')
		FROM tool_calls tc
		JOIN messages m ON m.id = tc.message_id
		WHERE tc.session_id = ?
		ORDER BY m.ordinal DESC, tc.call_index DESC
		LIMIT ?`,
		q.sessionID, n,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"loading trailing tool calls %s: %w", q.sessionID, err,
		)
	}
	defer rows.Close()
	var facts []ToolCallSignalFact
	for rows.Next() {
		var f ToolCallSignalFact
		if err := rows.Scan(
			&f.MessageOrdinal, &f.CallIndex, &f.ToolName, &f.Category,
			&f.InputJSON, &f.ResultContent, &f.ToolUseID, &f.EventStatus,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning trailing tool call %s: %w", q.sessionID, err,
			)
		}
		facts = append(facts, f)
	}
	// The query orders descending for the LIMIT; reverse to chronological.
	for i, j := 0, len(facts)-1; i < j; i, j = i+1, j-1 {
		facts[i], facts[j] = facts[j], facts[i]
	}
	return facts, rows.Err()
}

func (q signalTxQuery) ToolCallsByUseID(
	ctx context.Context, toolUseIDs []string,
) ([]ToolCallSignalFact, error) {
	if len(toolUseIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(toolUseIDs))
	args := make([]any, 0, len(toolUseIDs)+1)
	args = append(args, q.sessionID)
	for i, id := range toolUseIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := q.tx.QueryContext(ctx, `
		SELECT m.ordinal, COALESCE(tc.call_index, 0),
		       tc.tool_name, tc.category, COALESCE(tc.input_json, ''),
		       COALESCE(tc.result_content, ''),
		       COALESCE(tc.tool_use_id, ''),
		       COALESCE((
		           SELECT tre.status FROM tool_result_events tre
		           WHERE tre.session_id = tc.session_id
		             AND tre.tool_call_message_ordinal = m.ordinal
		             AND tre.call_index = COALESCE(tc.call_index, 0)
		           ORDER BY tre.event_index DESC, tre.id DESC
		           LIMIT 1
		       ), '')
		FROM tool_calls tc
		JOIN messages m ON m.id = tc.message_id
		WHERE tc.session_id = ? AND tc.tool_use_id IN (`+
		strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"loading tool call facts %s: %w", q.sessionID, err,
		)
	}
	defer rows.Close()
	var facts []ToolCallSignalFact
	for rows.Next() {
		var f ToolCallSignalFact
		if err := rows.Scan(
			&f.MessageOrdinal, &f.CallIndex, &f.ToolName, &f.Category,
			&f.InputJSON, &f.ResultContent, &f.ToolUseID, &f.EventStatus,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning tool call fact %s: %w", q.sessionID, err,
			)
		}
		facts = append(facts, f)
	}
	return facts, rows.Err()
}

func (q signalTxQuery) CallResultEvents(
	ctx context.Context, messageOrdinal, callIndex int,
) ([]ToolResultEvent, error) {
	rows, err := q.tx.QueryContext(ctx, `
		SELECT COALESCE(tool_use_id, ''), COALESCE(agent_id, ''),
		       COALESCE(subagent_session_id, ''), source, status,
		       content, content_length, COALESCE(timestamp, ''), event_index
		FROM tool_result_events
		WHERE session_id = ? AND tool_call_message_ordinal = ?
		  AND call_index = ?
		ORDER BY event_index, id`,
		q.sessionID, messageOrdinal, callIndex,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"loading result events %s/%d/%d: %w",
			q.sessionID, messageOrdinal, callIndex, err,
		)
	}
	defer rows.Close()
	var events []ToolResultEvent
	for rows.Next() {
		var ev ToolResultEvent
		if err := rows.Scan(
			&ev.ToolUseID, &ev.AgentID, &ev.SubagentSessionID,
			&ev.Source, &ev.Status, &ev.Content, &ev.ContentLength,
			&ev.Timestamp, &ev.EventIndex,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning result event %s: %w", q.sessionID, err,
			)
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

// TranscriptRevision returns a session's stored transcript revision. The
// incremental signal maintainer uses the pre-write value to verify the
// persisted state token before folding a delta.
func (db *DB) TranscriptRevision(sessionID string) (string, error) {
	var rev string
	if err := db.getReader().QueryRow(
		`SELECT transcript_revision FROM sessions WHERE id = ?`,
		sessionID,
	).Scan(&rev); err != nil {
		return "", fmt.Errorf(
			"loading transcript revision %s: %w", sessionID, err,
		)
	}
	return rev, nil
}

// GetSessionSignalState loads a session's compact signal state row.
// ok=false means no row exists.
func (db *DB) GetSessionSignalState(
	sessionID string,
) (SessionSignalState, bool, error) {
	var st SessionSignalState
	err := db.getReader().QueryRow(`
		SELECT state, transcript_revision, signal_version
		FROM session_signal_state WHERE session_id = ?`,
		sessionID,
	).Scan(&st.State, &st.TranscriptRevision, &st.SignalVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionSignalState{}, false, nil
	}
	if err != nil {
		return SessionSignalState{}, false, fmt.Errorf(
			"loading session signal state %s: %w", sessionID, err,
		)
	}
	st.SessionID = sessionID
	return st, true, nil
}

// UpsertSessionSignalState stores or replaces a session's compact signal
// state row. Full recompute paths call it after their signal columns
// commit so later incremental deltas can fold.
func (db *DB) UpsertSessionSignalState(st SessionSignalState) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning signal state tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertSessionSignalStateTx(tx, st); err != nil {
		return err
	}
	return tx.Commit()
}
func applySignalDeltaTx(
	tx *sql.Tx, sessionID string, d SignalDelta,
) error {
	// Remove stale findings by natural coordinates, counting definite
	// removals toward the leak-count adjustment.
	deletedDefinite := 0
	for _, key := range d.DeleteFindingKeys {
		var n int
		if err := tx.QueryRow(`
			SELECT COUNT(*) FROM secret_findings
			WHERE session_id = ? AND message_ordinal = ?
			  AND COALESCE(call_index, -1) = ?
			  AND location_kind = ? AND confidence = ?`,
			sessionID, key.MessageOrdinal, key.CallIndex,
			key.LocationKind, secrets.ConfidenceDefinite,
		).Scan(&n); err != nil {
			return fmt.Errorf(
				"counting removed findings %s: %w", sessionID, err,
			)
		}
		deletedDefinite += n
		if _, err := tx.Exec(`
			DELETE FROM secret_findings
			WHERE session_id = ? AND message_ordinal = ?
			  AND COALESCE(call_index, -1) = ?
			  AND location_kind = ?`,
			sessionID, key.MessageOrdinal, key.CallIndex,
			key.LocationKind,
		); err != nil {
			return fmt.Errorf(
				"deleting findings %s: %w", sessionID, err,
			)
		}
	}

	// Insert new findings, skipping rows an equivalent finding already
	// covers (idempotent replays of a result update). RowsAffected tells
	// which inserts actually landed, so the definite-leak adjustment
	// counts each finding once.
	addedDefinite := 0
	for i := range d.InsertFindings {
		f := &d.InsertFindings[i]
		res, err := tx.Exec(`
			INSERT INTO secret_findings (
				session_id, rule_name, confidence,
				location_kind, message_ordinal, call_index, event_index,
				match_start, match_end, match_index,
				redacted_match, rules_version
			)
			SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
			WHERE NOT EXISTS (
				SELECT 1 FROM secret_findings
				WHERE session_id = ? AND rule_name = ?
				  AND location_kind = ? AND message_ordinal = ?
				  AND COALESCE(call_index, -1) = ?
				  AND COALESCE(event_index, -1) = ?
				  AND match_start = ? AND match_end = ?
			)`,
			sessionID, f.RuleName, f.Confidence,
			f.LocationKind, f.MessageOrdinal, f.CallIndex, f.EventIndex,
			f.MatchStart, f.MatchEnd, f.MatchIndex,
			f.RedactedMatch, f.RulesVersion,
			sessionID, f.RuleName,
			f.LocationKind, f.MessageOrdinal,
			coalesceInt(f.CallIndex, -1),
			coalesceInt(f.EventIndex, -1),
			f.MatchStart, f.MatchEnd,
		)
		if err != nil {
			return fmt.Errorf("inserting secret finding: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting inserted finding: %w", err)
		}
		if n > 0 && f.Confidence == secrets.ConfidenceDefinite {
			addedDefinite++
		}
	}

	// Adjust the leak count from the stored value.
	var currentLeak int
	if err := tx.QueryRow(
		`SELECT secret_leak_count FROM sessions WHERE id = ?`,
		sessionID,
	).Scan(&currentLeak); err != nil {
		return fmt.Errorf(
			"reading leak count %s: %w", sessionID, err,
		)
	}
	newLeak := currentLeak - deletedDefinite + addedDefinite
	if newLeak < 0 {
		newLeak = 0
	}

	if err := updateSessionSignalsTx(tx, sessionID, d.Update); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE sessions
		SET secret_leak_count = ?, secrets_rules_version = ?
		WHERE id = ?`,
		newLeak, d.Update.SecretsRulesVersion, sessionID,
	); err != nil {
		return fmt.Errorf(
			"updating session secret columns %s: %w", sessionID, err,
		)
	}
	if d.State != nil {
		if err := upsertSessionSignalStateTx(tx, *d.State); err != nil {
			return err
		}
	}
	return nil
}

func coalesceInt(p *int, fallback int) int {
	if p == nil {
		return fallback
	}
	return *p
}

func upsertSessionSignalStateTx(
	tx *sql.Tx, st SessionSignalState,
) error {
	if st.UpdatedAt == "" {
		st.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if _, err := tx.Exec(`
		INSERT INTO session_signal_state (
			session_id, state, transcript_revision, signal_version,
			updated_at
		) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			state = excluded.state,
			transcript_revision = excluded.transcript_revision,
			signal_version = excluded.signal_version,
			updated_at = excluded.updated_at`,
		st.SessionID, st.State, st.TranscriptRevision,
		st.SignalVersion, st.UpdatedAt,
	); err != nil {
		return fmt.Errorf(
			"upserting session signal state %s: %w", st.SessionID, err,
		)
	}
	return nil
}
