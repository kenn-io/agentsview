package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

const signalsBackfillMarker = "session_quality_signals_v1"

// SessionSignalUpdate holds computed signal values to persist
// on the sessions table.
type SessionSignalUpdate struct {
	ToolFailureSignalCount int
	ToolRetryCount         int
	EditChurnCount         int
	ConsecutiveFailureMax  int
	Outcome                string
	OutcomeConfidence      string
	EndedWithRole          string
	FinalFailureStreak     int
	SignalsPendingSince    *string
	CompactionCount        int
	MidTaskCompactionCount int
	ContextPressureMax     *float64
	HealthScore            *int
	HealthGrade            *string
	HasToolCalls           bool
	HasContextData         bool
	SecretLeakCount        int
	SecretsRulesVersion    string
	QualitySignals         QualitySignals
}

// UpdateSessionSignals persists computed signal values on the
// sessions table. Bumps local_modified_at so the session is
// re-selected by the next pg push -- a recomputed signal column
// is a change to the row from PG's perspective, even when the
// inline write path didn't touch anything else (e.g. a one-time
// BackfillSignals run after a schema migration).
func (db *DB) UpdateSessionSignals(
	sessionID string, u SessionSignalUpdate,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := updateSessionSignalsTx(tx, sessionID, u); err != nil {
		return err
	}
	return tx.Commit()
}

// updateSessionSignalsTx writes signal columns within an existing transaction.
// Caller owns the lock and transaction lifecycle. It deliberately does NOT
// write secret_leak_count/secrets_rules_version: those are owned solely by the
// secret-finding replacement path (replaceSecretFindingsTx), so a signals-only
// recompute cannot reset them while findings still exist. The two secret fields
// on SessionSignalUpdate are carried here only so callers can forward them to
// replaceSecretFindingsTx alongside the findings.
func updateSessionSignalsTx(
	tx *sql.Tx, sessionID string, u SessionSignalUpdate,
) error {
	_, err := tx.Exec(`
		UPDATE sessions SET
			tool_failure_signal_count = ?,
			tool_retry_count = ?,
			edit_churn_count = ?,
			consecutive_failure_max = ?,
			outcome = ?,
			outcome_confidence = ?,
			ended_with_role = ?,
			final_failure_streak = ?,
			signals_pending_since = ?,
			compaction_count = ?,
			mid_task_compaction_count = ?,
			context_pressure_max = ?,
			health_score = ?,
			health_grade = ?,
			has_tool_calls = ?,
			has_context_data = ?,
			quality_signal_version = ?,
			short_prompt_count = ?,
			unstructured_start = ?,
			missing_success_criteria_count = ?,
			missing_verification_count = ?,
			duplicate_prompt_count = ?,
			no_code_context_count = ?,
			runaway_tool_loop_count = ?,
			local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`,
		u.ToolFailureSignalCount,
		u.ToolRetryCount,
		u.EditChurnCount,
		u.ConsecutiveFailureMax,
		u.Outcome,
		u.OutcomeConfidence,
		u.EndedWithRole,
		u.FinalFailureStreak,
		u.SignalsPendingSince,
		u.CompactionCount,
		u.MidTaskCompactionCount,
		u.ContextPressureMax,
		u.HealthScore,
		u.HealthGrade,
		u.HasToolCalls,
		u.HasContextData,
		u.QualitySignals.Version,
		u.QualitySignals.ShortPromptCount,
		u.QualitySignals.UnstructuredStart,
		u.QualitySignals.MissingSuccessCriteriaCount,
		u.QualitySignals.MissingVerificationCount,
		u.QualitySignals.DuplicatePromptCount,
		u.QualitySignals.NoCodeContextCount,
		u.QualitySignals.RunawayToolLoopCount,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf(
			"updating session signals for %s: %w",
			sessionID, err,
		)
	}
	return nil
}

// PendingSignalSessions returns session IDs whose
// signals_pending_since is non-NULL and older than cutoff.
func (db *DB) PendingSignalSessions(
	ctx context.Context, cutoff string,
) ([]string, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT id FROM sessions
		WHERE signals_pending_since IS NOT NULL
		  AND signals_pending_since < ?`, cutoff)
	if err != nil {
		return nil, fmt.Errorf(
			"querying pending signal sessions: %w", err,
		)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf(
				"scanning pending signal session: %w", err,
			)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// BackfillSignals runs a one-time computation of session
// signals for all sessions. Guarded by a stats marker so it
// only runs once. computeFn returns nil on success or an
// error to signal that the per-session recompute could not
// be completed (e.g. the DB connection went away during a
// concurrent resync swap). The completion marker is only set
// when every session was processed successfully -- partial
// runs leave the marker unset so the next startup retries.
func (db *DB) BackfillSignals(
	ctx context.Context,
	computeFn func(ctx context.Context, sessionID string) error,
) error {
	db.mu.Lock()
	var done int
	if err := db.getWriter().QueryRow(
		`SELECT count(*)
		 FROM stats
		 WHERE key = ? AND value != 0`,
		signalsBackfillMarker,
	).Scan(&done); err != nil {
		db.mu.Unlock()
		return fmt.Errorf(
			"probing signals backfill marker: %w", err,
		)
	}
	db.mu.Unlock()

	query := `SELECT id FROM sessions WHERE message_count > 0`
	args := []any{}
	if done > 0 {
		query += ` AND quality_signal_version < ?`
		args = append(args, CurrentQualitySignalVersion)
	}
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf(
			"querying backfill candidates: %w", err,
		)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf(
				"scanning backfill candidate: %w", err,
			)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		if done == 0 {
			return db.MarkSignalsBackfillDone()
		}
		return nil
	}

	if done > 0 {
		log.Printf(
			"backfill: recomputing %d stale session signals...",
			len(ids),
		)
	} else {
		log.Println("backfill: computing session signals...")
	}

	var failed int
	for i, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := computeFn(ctx, id); err != nil {
			failed++
			log.Printf(
				"backfill: %s: %v", id, err,
			)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if (i+1)%100 == 0 {
			log.Printf(
				"backfill: %d/%d sessions", i+1, len(ids),
			)
		}
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if failed > 0 {
		return fmt.Errorf(
			"backfill incomplete: %d/%d sessions failed; "+
				"marker not set, next startup will retry",
			failed, len(ids),
		)
	}

	log.Printf(
		"backfill: completed %d sessions", len(ids),
	)

	return db.MarkSignalsBackfillDone()
}

// MarkSignalsBackfillDone records that legacy signal backfill is
// no longer needed for this database. Set after a fresh resync,
// where every session is rewritten through the inline signal
// path, so the post-resync BackfillSignals call is a no-op.
func (db *DB) MarkSignalsBackfillDone() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		`INSERT INTO stats (key, value) VALUES (?, 1)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		signalsBackfillMarker,
	)
	if err != nil {
		return fmt.Errorf(
			"storing signals backfill marker: %w", err,
		)
	}
	return nil
}
