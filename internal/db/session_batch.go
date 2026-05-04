package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// SessionBatchWrite is one full session rewrite for a bulk
// rebuild. Callers must provide a complete session row, the
// complete message set to store, the computed signal values,
// and the data version to stamp after messages are written.
type SessionBatchWrite struct {
	Session         Session
	Messages        []Message
	Signals         SessionSignalUpdate
	DataVersion     int
	ReplaceMessages bool
}

// SessionBatchResult summarizes a WriteSessionBatch call.
type SessionBatchResult struct {
	WrittenSessions  int
	WrittenMessages  int
	ExcludedSessions int
	ExcludedIDs      []string
	FailedSessions   int
	Errors           []error
}

// WriteSessionBatch writes multiple complete sessions inside
// one transaction. Each session is wrapped in a savepoint so a
// single bad row rolls back only that session and does not
// poison the rest of the batch.
//
// This is intended for full-resync temp databases, where there
// are no user pins to preserve yet. Use ReplaceSessionMessages
// for ordinary single-session replacement on a live database.
func (db *DB) WriteSessionBatch(
	writes []SessionBatchWrite,
) (SessionBatchResult, error) {
	var result SessionBatchResult
	if len(writes) == 0 {
		return result, nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return result, fmt.Errorf("beginning batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, write := range writes {
		savepoint := fmt.Sprintf("session_batch_%d", i)
		if _, err := tx.Exec("SAVEPOINT " + savepoint); err != nil {
			return result, fmt.Errorf(
				"creating savepoint %s: %w", savepoint, err,
			)
		}

		messagesWritten, err := writeOneSessionBatchTx(tx, write)
		switch {
		case err == nil:
			if _, err := tx.Exec("RELEASE SAVEPOINT " + savepoint); err != nil {
				return result, fmt.Errorf(
					"releasing savepoint %s: %w",
					savepoint, err,
				)
			}
			result.WrittenSessions++
			result.WrittenMessages += messagesWritten
		case errors.Is(err, ErrSessionExcluded),
			errors.Is(err, ErrSessionTrashed):
			if rerr := rollbackSavepoint(tx, savepoint); rerr != nil {
				return result, rerr
			}
			result.ExcludedSessions++
			result.ExcludedIDs = append(
				result.ExcludedIDs,
				write.Session.ID,
			)
		default:
			if rerr := rollbackSavepoint(tx, savepoint); rerr != nil {
				return result, rerr
			}
			result.FailedSessions++
			result.Errors = append(result.Errors, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("committing batch tx: %w", err)
	}
	return result, nil
}

// WriteSessionBatchAtomic writes all sessions in one
// transaction. Any rejected or failed row rolls back the whole
// batch.
func (db *DB) WriteSessionBatchAtomic(
	writes []SessionBatchWrite,
	beforeCommit ...func() error,
) (SessionBatchResult, error) {
	var result SessionBatchResult
	if len(writes) == 0 {
		return result, nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return result, fmt.Errorf("beginning batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, write := range writes {
		messagesWritten, err := writeOneSessionBatchTx(tx, write)
		if err != nil {
			result.WrittenSessions = 0
			result.WrittenMessages = 0
			switch {
			case errors.Is(err, ErrSessionExcluded),
				errors.Is(err, ErrSessionTrashed):
				result.ExcludedSessions++
				result.ExcludedIDs = append(
					result.ExcludedIDs,
					write.Session.ID,
				)
			default:
				result.FailedSessions++
				result.Errors = append(result.Errors, err)
			}
			return result, err
		}
		result.WrittenSessions++
		result.WrittenMessages += messagesWritten
	}

	if len(beforeCommit) > 0 && beforeCommit[0] != nil {
		if err := beforeCommit[0](); err != nil {
			result.WrittenSessions = 0
			result.WrittenMessages = 0
			return result, err
		}
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("committing batch tx: %w", err)
	}
	return result, nil
}

func rollbackSavepoint(tx *sql.Tx, savepoint string) error {
	if _, err := tx.Exec("ROLLBACK TO SAVEPOINT " + savepoint); err != nil {
		return fmt.Errorf(
			"rolling back savepoint %s: %w", savepoint, err,
		)
	}
	if _, err := tx.Exec("RELEASE SAVEPOINT " + savepoint); err != nil {
		return fmt.Errorf(
			"releasing rolled back savepoint %s: %w",
			savepoint, err,
		)
	}
	return nil
}

func writeOneSessionBatchTx(
	tx *sql.Tx, write SessionBatchWrite,
) (int, error) {
	var excluded int
	err := tx.QueryRow(
		"SELECT 1 FROM excluded_sessions WHERE id = ?",
		write.Session.ID,
	).Scan(&excluded)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf(
			"checking exclusion for %s: %w",
			write.Session.ID, err,
		)
	}
	if excluded == 1 {
		return 0, ErrSessionExcluded
	}
	var trashed int
	err = tx.QueryRow(
		"SELECT 1 FROM sessions WHERE id = ? AND deleted_at IS NOT NULL",
		write.Session.ID,
	).Scan(&trashed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf(
			"checking trash for %s: %w",
			write.Session.ID, err,
		)
	}
	if trashed == 1 {
		return 0, ErrSessionTrashed
	}

	if _, err := tx.Exec(
		upsertSessionSQL,
		upsertSessionArgs(write.Session)...,
	); err != nil {
		return 0, fmt.Errorf(
			"upserting session %s: %w",
			write.Session.ID, err,
		)
	}

	msgs := write.Messages
	var pins []savedPin
	if write.ReplaceMessages {
		pins, err = savePinsTx(tx, write.Session.ID)
		if err != nil {
			return 0, err
		}
		if err := deleteSessionMessagesTx(tx, write.Session.ID); err != nil {
			return 0, err
		}
	} else {
		maxOrd, err := maxOrdinalTx(tx, write.Session.ID)
		if err != nil {
			return 0, err
		}
		msgs = messagesAfterOrdinal(msgs, maxOrd)
	}

	if len(msgs) > 0 {
		ids, err := insertMessagesTx(tx, msgs)
		if err != nil {
			return 0, err
		}
		toolCalls := resolveToolCalls(msgs, ids)
		if err := insertToolCallsTx(tx, toolCalls); err != nil {
			return 0, err
		}
		events := resolveToolResultEvents(msgs)
		if err := insertToolResultEventsTx(tx, events); err != nil {
			return 0, err
		}
	}
	if write.ReplaceMessages {
		if err := restorePinsTx(tx, write.Session.ID, pins); err != nil {
			return 0, err
		}
	}

	if write.DataVersion > 0 {
		if _, err := tx.Exec(
			`UPDATE sessions SET
				data_version = ?,
				local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			 WHERE id = ?`,
			write.DataVersion, write.Session.ID,
		); err != nil {
			return 0, fmt.Errorf(
				"setting data_version for %s: %w",
				write.Session.ID, err,
			)
		}
	}

	if _, err := tx.Exec(`
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
			local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`,
		write.Signals.ToolFailureSignalCount,
		write.Signals.ToolRetryCount,
		write.Signals.EditChurnCount,
		write.Signals.ConsecutiveFailureMax,
		write.Signals.Outcome,
		write.Signals.OutcomeConfidence,
		write.Signals.EndedWithRole,
		write.Signals.FinalFailureStreak,
		write.Signals.SignalsPendingSince,
		write.Signals.CompactionCount,
		write.Signals.MidTaskCompactionCount,
		write.Signals.ContextPressureMax,
		write.Signals.HealthScore,
		write.Signals.HealthGrade,
		write.Signals.HasToolCalls,
		write.Signals.HasContextData,
		write.Session.ID,
	); err != nil {
		return 0, fmt.Errorf(
			"updating session signals for %s: %w",
			write.Session.ID, err,
		)
	}

	return len(msgs), nil
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
	if _, err := tx.Exec(
		"DELETE FROM messages WHERE session_id = ?",
		sessionID,
	); err != nil {
		return fmt.Errorf("deleting old messages: %w", err)
	}
	return nil
}

func maxOrdinalTx(tx *sql.Tx, sessionID string) (int, error) {
	var n sql.NullInt64
	err := tx.QueryRow(
		"SELECT MAX(ordinal) FROM messages WHERE session_id = ?",
		sessionID,
	).Scan(&n)
	if err != nil {
		return -1, fmt.Errorf(
			"reading max ordinal for %s: %w", sessionID, err,
		)
	}
	if !n.Valid {
		return -1, nil
	}
	return int(n.Int64), nil
}

func messagesAfterOrdinal(msgs []Message, maxOrd int) []Message {
	if maxOrd < 0 {
		return msgs
	}
	for i, m := range msgs {
		if m.Ordinal > maxOrd {
			return msgs[i:]
		}
	}
	return nil
}
