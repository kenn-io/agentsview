package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/export"
)

// SessionBatchWrite is one full session rewrite for a bulk
// rebuild. Callers must provide a complete session row, the
// complete message set to store, the computed signal values,
// and the data version to stamp after messages are written.
type SessionBatchWrite struct {
	Session             Session
	Messages            []Message
	UsageEvents         []UsageEvent
	IdentityObservation export.ProjectIdentityObservation
	Signals             SessionSignalUpdate
	Findings            []SecretFinding
	DataVersion         int
	ReplaceMessages     bool
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
	if err := db.requireWritable(); err != nil {
		return result, err
	}
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
	var pendingRecallRevocations recallEvidenceRevocationEvents

	for i, write := range writes {
		write = sanitizeSessionBatchWrite(write)
		savepoint := fmt.Sprintf("session_batch_%d", i)
		if _, err := tx.Exec("SAVEPOINT " + savepoint); err != nil {
			return result, fmt.Errorf(
				"creating savepoint %s: %w", savepoint, err,
			)
		}

		var sessionRecallRevocations recallEvidenceRevocationEvents
		messagesWritten, err := writeOneSessionBatchTx(
			tx,
			write,
			&sessionRecallRevocations,
		)
		switch {
		case err == nil:
			if _, err := tx.Exec("RELEASE SAVEPOINT " + savepoint); err != nil {
				return result, fmt.Errorf(
					"releasing savepoint %s: %w",
					savepoint, err,
				)
			}
			pendingRecallRevocations = append(
				pendingRecallRevocations,
				sessionRecallRevocations...,
			)
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
	pendingRecallRevocations.flush()
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
	if err := db.requireWritable(); err != nil {
		return result, err
	}
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
	var pendingRecallRevocations recallEvidenceRevocationEvents

	for _, write := range writes {
		write = sanitizeSessionBatchWrite(write)
		messagesWritten, err := writeOneSessionBatchTx(
			tx,
			write,
			&pendingRecallRevocations,
		)
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
	pendingRecallRevocations.flush()
	return result, nil
}

func sanitizeSessionBatchWrite(write SessionBatchWrite) SessionBatchWrite {
	write.Messages = append([]Message(nil), write.Messages...)
	write.UsageEvents = append([]UsageEvent(nil), write.UsageEvents...)

	msgTotal, msgHasOut, msgPeak, msgHasCtx :=
		batchMessageTokenTotals(write.Messages)
	evtTotal, evtHasOut, evtPeak, evtHasCtx :=
		batchUsageEventTokenTotals(write.UsageEvents)
	totalFromMsgs := write.Session.HasTotalOutputTokens == msgHasOut &&
		write.Session.TotalOutputTokens == msgTotal
	totalFromEvts := write.Session.HasTotalOutputTokens == evtHasOut &&
		write.Session.TotalOutputTokens == evtTotal
	peakFromMsgs := write.Session.HasPeakContextTokens == msgHasCtx &&
		write.Session.PeakContextTokens == msgPeak
	peakFromEvts := write.Session.HasPeakContextTokens == evtHasCtx &&
		write.Session.PeakContextTokens == evtPeak

	_ = ValidateAndSanitize(&write.Session, write.Messages, write.UsageEvents)

	if totalFromMsgs {
		t, h, _, _ := batchMessageTokenTotals(write.Messages)
		write.Session.TotalOutputTokens = t
		write.Session.HasTotalOutputTokens = h
	} else if totalFromEvts {
		t, h, _, _ := batchUsageEventTokenTotals(write.UsageEvents)
		write.Session.TotalOutputTokens = t
		write.Session.HasTotalOutputTokens = h
	}
	if peakFromMsgs {
		_, _, p, h := batchMessageTokenTotals(write.Messages)
		write.Session.PeakContextTokens = p
		write.Session.HasPeakContextTokens = h
	} else if peakFromEvts {
		_, _, p, h := batchUsageEventTokenTotals(write.UsageEvents)
		write.Session.PeakContextTokens = p
		write.Session.HasPeakContextTokens = h
	}
	return write
}

func batchMessageTokenTotals(
	msgs []Message,
) (totalOut int, hasOut bool, peakCtx int, hasCtx bool) {
	for _, msg := range msgs {
		if msg.HasOutputTokens {
			hasOut = true
			totalOut += msg.OutputTokens
		}
		if msg.HasContextTokens {
			hasCtx = true
			if msg.ContextTokens > peakCtx {
				peakCtx = msg.ContextTokens
			}
		}
	}
	return totalOut, hasOut, peakCtx, hasCtx
}

func batchUsageEventTokenTotals(
	events []UsageEvent,
) (totalOut int, hasOut bool, peakCtx int, hasCtx bool) {
	for _, ev := range events {
		if ev.Source == "session" {
			continue
		}
		if ev.OutputTokens > 0 {
			hasOut = true
			totalOut += ev.OutputTokens
		}
		context := ev.InputTokens +
			ev.CacheCreationInputTokens +
			ev.CacheReadInputTokens
		if context > 0 {
			hasCtx = true
			if context > peakCtx {
				peakCtx = context
			}
		}
	}
	return totalOut, hasOut, peakCtx, hasCtx
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
	tx *sql.Tx,
	write SessionBatchWrite,
	pendingRecallRevocations *recallEvidenceRevocationEvents,
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
	var deletedAt sql.NullString
	err = tx.QueryRow(
		"SELECT deleted_at FROM sessions WHERE id = ?",
		write.Session.ID,
	).Scan(&deletedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf(
			"checking trash for %s: %w",
			write.Session.ID, err,
		)
	}
	sessionExists := err == nil
	if deletedAt.Valid {
		return 0, ErrSessionTrashed
	}
	queueGenerationBefore, queueExistedBefore, err := artifactExportGenerationTx(
		tx, write.Session.ID,
	)
	if err != nil {
		return 0, err
	}
	replacementTranscriptChanged := false
	if write.ReplaceMessages && sessionExists {
		stored, err := sessionMessagesTx(
			context.Background(), tx, write.Session.ID,
		)
		if err != nil {
			return 0, err
		}
		replacementTranscriptChanged = !transcriptMessagesEqual(
			stored, write.Messages,
		)
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
	if write.IdentityObservation.Project != "" {
		if err := upsertProjectIdentityObservationTx(
			tx, write.IdentityObservation,
		); err != nil {
			return 0, err
		}
	}
	if err := replaceSessionUsageEventsTx(
		tx, write.Session.ID, write.UsageEvents, false,
	); err != nil {
		return 0, err
	}

	msgs := write.Messages
	var pins []savedPin
	if write.ReplaceMessages && sessionExists {
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
	transcriptChanged := len(msgs) > 0
	if write.ReplaceMessages && sessionExists {
		transcriptChanged = replacementTranscriptChanged
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
	if transcriptChanged {
		if err := bumpTranscriptRevisionTx(tx, write.Session.ID); err != nil {
			return 0, err
		}
	}
	if write.ReplaceMessages && sessionExists {
		if err := reconcileRecallEvidenceForSessionTx(
			context.Background(),
			tx,
			write.Session.ID,
			pendingRecallRevocations,
		); err != nil {
			return 0, err
		}
	}
	if write.ReplaceMessages {
		if err := restorePinsTx(tx, write.Session.ID, pins); err != nil {
			return 0, err
		}
		// A full message replacement re-normalizes every row, so this row is
		// no longer incremental-append skew. The append-only branch
		// (ReplaceMessages=false) deliberately leaves the marker untouched so
		// earlier incrementally written rows stay flagged for parse-diff.
		if err := resetIncrementalMarkerTx(tx, write.Session.ID); err != nil {
			return 0, err
		}
	}
	if err := updateSessionAutomationFromMessagesTx(
		tx, write.Session.ID,
	); err != nil {
		return 0, err
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

	if err := updateSessionSignalsTx(tx, write.Session.ID, write.Signals); err != nil {
		return 0, err
	}
	if err := replaceSecretFindingsTx(tx, write.Session.ID, write.Findings,
		write.Signals.SecretLeakCount, write.Signals.SecretsRulesVersion); err != nil {
		return 0, err
	}
	queueGenerationAfter, queueExistsAfter, err := artifactExportGenerationTx(
		tx, write.Session.ID,
	)
	if err != nil {
		return 0, err
	}
	if queueExistedBefore == queueExistsAfter &&
		queueGenerationBefore == queueGenerationAfter {
		if err := enqueueArtifactExportTx(tx, write.Session.ID); err != nil {
			return 0, err
		}
	}

	return len(msgs), nil
}

func sessionMessagesTx(
	ctx context.Context, tx *sql.Tx, sessionID string,
) ([]Message, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s
		FROM messages
		WHERE session_id = ?
		ORDER BY ordinal ASC`, selectMessageCols), sessionID)
	if err != nil {
		return nil, fmt.Errorf(
			"querying stored batch messages for %s: %w",
			sessionID, err,
		)
	}
	msgs, scanErr := scanMessages(rows)
	closeErr := rows.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := attachToolCallsWithQuerier(ctx, tx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
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
