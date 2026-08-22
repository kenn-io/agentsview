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
	// IdentitySnapshotProject distinguishes legacy omission (nil, use the
	// aggregate project) from an explicit empty parser source (omit snapshot).
	IdentitySnapshotProject *string
	Signals                 SessionSignalUpdate
	Findings                []SecretFinding
	// SkipSignalUpdates omits automatic quality-signal and secret-finding
	// persistence for bounded ingestion callers that do not consume it.
	SkipSignalUpdates bool
	DataVersion       int
	ReplaceMessages   bool
	// RejectMessageCountDecrease prevents full replacement with fewer messages.
	RejectMessageCountDecrease bool
}

// SessionWouldShortenError reports a rejected message-count decrease.
type SessionWouldShortenError struct {
	SessionID        string
	ExistingMessages int
	IncomingMessages int
}

func (e *SessionWouldShortenError) Error() string {
	return fmt.Sprintf(
		"session %q would shrink from %d to %d messages",
		e.SessionID, e.ExistingMessages, e.IncomingMessages,
	)
}

// SessionBatchResult summarizes a WriteSessionBatch call.
type SessionBatchResult struct {
	WrittenSessions  int
	WrittenMessages  int
	WrittenIndexes   []int
	ExcludedSessions int
	ExcludedIDs      []string
	FailedSessions   int
	FailedIDs        []string
	Errors           []error
}

type contextTransaction struct {
	ctx context.Context
	tx  *sql.Tx
}

func (tx contextTransaction) Exec(
	query string, args ...any,
) (sql.Result, error) {
	return tx.tx.ExecContext(tx.ctx, query, args...)
}

func (tx contextTransaction) Query(
	query string, args ...any,
) (*sql.Rows, error) {
	return tx.tx.QueryContext(tx.ctx, query, args...)
}

func (tx contextTransaction) QueryRow(
	query string, args ...any,
) *sql.Row {
	return tx.tx.QueryRowContext(tx.ctx, query, args...)
}

type transactionQueries interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
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
	return db.WriteSessionBatchContext(context.Background(), writes)
}

// WriteSessionBatchContext writes a full-session batch while bounding every
// transaction query and write with ctx.
func (db *DB) WriteSessionBatchContext(
	ctx context.Context,
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

	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("beginning batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ctxTx := contextTransaction{ctx: ctx, tx: tx}
	var pendingRecallRevocations recallEvidenceRevocationEvents
	var writtenUsageIDs []string

	for i, write := range writes {
		write, err = sanitizeSessionBatchWriteContext(ctx, write)
		if err != nil {
			return result, err
		}
		savepoint := fmt.Sprintf("session_batch_%d", i)
		if _, err := ctxTx.Exec("SAVEPOINT " + savepoint); err != nil {
			return result, fmt.Errorf(
				"creating savepoint %s: %w", savepoint, err,
			)
		}

		var sessionRecallRevocations recallEvidenceRevocationEvents
		messagesWritten, err := writeOneSessionBatchTx(
			ctx, tx, ctxTx,
			write,
			&sessionRecallRevocations,
		)
		switch {
		case err == nil:
			if _, err := ctxTx.Exec("RELEASE SAVEPOINT " + savepoint); err != nil {
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
			result.WrittenIndexes = append(result.WrittenIndexes, i)
			writtenUsageIDs = append(writtenUsageIDs, write.Session.ID)
		case errors.Is(err, ErrSessionExcluded),
			errors.Is(err, ErrSessionTrashed):
			if rerr := rollbackSavepoint(ctxTx, savepoint); rerr != nil {
				return result, rerr
			}
			result.ExcludedSessions++
			result.ExcludedIDs = append(
				result.ExcludedIDs,
				write.Session.ID,
			)
		default:
			if rerr := rollbackSavepoint(ctxTx, savepoint); rerr != nil {
				return result, rerr
			}
			result.FailedSessions++
			result.FailedIDs = append(result.FailedIDs, write.Session.ID)
			result.Errors = append(result.Errors, err)
		}
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("committing batch tx: %w", err)
	}
	db.notifyUsageSessions(writtenUsageIDs)
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
	var writtenUsageIDs []string

	for i, write := range writes {
		write = sanitizeSessionBatchWrite(write)
		messagesWritten, err := writeOneSessionBatchTx(
			context.Background(), tx, tx,
			write,
			&pendingRecallRevocations,
		)
		if err != nil {
			result.WrittenSessions = 0
			result.WrittenMessages = 0
			result.WrittenIndexes = nil
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
		result.WrittenIndexes = append(result.WrittenIndexes, i)
		writtenUsageIDs = append(writtenUsageIDs, write.Session.ID)
	}

	if len(beforeCommit) > 0 && beforeCommit[0] != nil {
		if err := beforeCommit[0](); err != nil {
			result.WrittenSessions = 0
			result.WrittenMessages = 0
			result.WrittenIndexes = nil
			return result, err
		}
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("committing batch tx: %w", err)
	}
	db.notifyUsageSessions(writtenUsageIDs)
	pendingRecallRevocations.flush()
	return result, nil
}

func sanitizeSessionBatchWrite(write SessionBatchWrite) SessionBatchWrite {
	sanitized, _ := sanitizeSessionBatchWriteContext(context.Background(), write)
	return sanitized
}

func sanitizeSessionBatchWriteContext(
	ctx context.Context, write SessionBatchWrite,
) (SessionBatchWrite, error) {
	messages := write.Messages
	write.Messages = make([]Message, len(messages))
	for i := range write.Messages {
		if err := ctx.Err(); err != nil {
			return SessionBatchWrite{}, err
		}
		write.Messages[i] = messages[i]
	}
	usageEvents := write.UsageEvents
	write.UsageEvents = make([]UsageEvent, len(usageEvents))
	for i := range write.UsageEvents {
		if err := ctx.Err(); err != nil {
			return SessionBatchWrite{}, err
		}
		write.UsageEvents[i] = usageEvents[i]
	}

	msgTotal, msgHasOut, msgPeak, msgHasCtx, err :=
		batchMessageTokenTotalsContext(ctx, write.Messages)
	if err != nil {
		return SessionBatchWrite{}, err
	}
	evtTotal, evtHasOut, evtPeak, evtHasCtx, err :=
		batchUsageEventTokenTotalsContext(ctx, write.UsageEvents)
	if err != nil {
		return SessionBatchWrite{}, err
	}
	totalFromMsgs := write.Session.HasTotalOutputTokens == msgHasOut &&
		write.Session.TotalOutputTokens == msgTotal
	totalFromEvts := write.Session.HasTotalOutputTokens == evtHasOut &&
		write.Session.TotalOutputTokens == evtTotal
	peakFromMsgs := write.Session.HasPeakContextTokens == msgHasCtx &&
		write.Session.PeakContextTokens == msgPeak
	peakFromEvts := write.Session.HasPeakContextTokens == evtHasCtx &&
		write.Session.PeakContextTokens == evtPeak

	if _, err := ValidateAndSanitizeContext(
		ctx, &write.Session, write.Messages, write.UsageEvents,
	); err != nil {
		return SessionBatchWrite{}, err
	}

	if totalFromMsgs || peakFromMsgs {
		total, hasTotal, peak, hasPeak, err :=
			batchMessageTokenTotalsContext(ctx, write.Messages)
		if err != nil {
			return SessionBatchWrite{}, err
		}
		if totalFromMsgs {
			write.Session.TotalOutputTokens = total
			write.Session.HasTotalOutputTokens = hasTotal
		}
		if peakFromMsgs {
			write.Session.PeakContextTokens = peak
			write.Session.HasPeakContextTokens = hasPeak
		}
	}
	eventTotalNeeded := totalFromEvts && !totalFromMsgs
	eventPeakNeeded := peakFromEvts && !peakFromMsgs
	if eventTotalNeeded || eventPeakNeeded {
		total, hasTotal, peak, hasPeak, err :=
			batchUsageEventTokenTotalsContext(ctx, write.UsageEvents)
		if err != nil {
			return SessionBatchWrite{}, err
		}
		if eventTotalNeeded {
			write.Session.TotalOutputTokens = total
			write.Session.HasTotalOutputTokens = hasTotal
		}
		if eventPeakNeeded {
			write.Session.PeakContextTokens = peak
			write.Session.HasPeakContextTokens = hasPeak
		}
	}
	return write, nil
}

func batchMessageTokenTotalsContext(
	ctx context.Context, msgs []Message,
) (totalOut int, hasOut bool, peakCtx int, hasCtx bool, err error) {
	for _, msg := range msgs {
		if err := ctx.Err(); err != nil {
			return 0, false, 0, false, err
		}
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
	return totalOut, hasOut, peakCtx, hasCtx, ctx.Err()
}

func batchUsageEventTokenTotalsContext(
	ctx context.Context, events []UsageEvent,
) (totalOut int, hasOut bool, peakCtx int, hasCtx bool, err error) {
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return 0, false, 0, false, err
		}
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
	return totalOut, hasOut, peakCtx, hasCtx, ctx.Err()
}

func rollbackSavepoint(tx transactionQueries, savepoint string) error {
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
	ctx context.Context,
	tx *sql.Tx,
	queries transactionQueries,
	write SessionBatchWrite,
	pendingRecallRevocations *recallEvidenceRevocationEvents,
) (int, error) {
	if write.IdentityObservation.Project != "" {
		normalized, err := normalizeProjectIdentityObservation(
			write.IdentityObservation,
		)
		if err != nil {
			return 0, err
		}
		if normalized.SessionID == "" {
			normalized.SessionID = write.Session.ID
		}
		if normalized.SessionID != write.Session.ID {
			return 0, fmt.Errorf(
				"identity observation session id %q does not match session id %q",
				normalized.SessionID, write.Session.ID,
			)
		}
		write.IdentityObservation = normalized
	}

	upsertResult, err := upsertSessionExec(
		queries.Exec,
		func(query string, args ...any) rowScanner {
			return queries.QueryRow(query, args...)
		},
		write.Session,
		true,
	)
	if err != nil {
		return 0, err
	}
	replaceMessages := write.ReplaceMessages ||
		upsertResult.sourceMissing
	queueGenerationBefore, queueExistedBefore, err := artifactExportGenerationTx(
		queries, write.Session.ID,
	)
	if err != nil {
		return 0, err
	}
	sessionExists := !upsertResult.inserted
	replacementTranscriptChanged := false
	if replaceMessages && sessionExists {
		stored, err := sessionMessagesTx(
			ctx, tx, write.Session.ID,
		)
		if err != nil {
			return 0, err
		}
		if write.RejectMessageCountDecrease &&
			len(write.Messages) < len(stored) {
			return 0, &SessionWouldShortenError{
				SessionID:        write.Session.ID,
				ExistingMessages: len(stored),
				IncomingMessages: len(write.Messages),
			}
		}
		replacementTranscriptChanged = !transcriptMessagesEqual(
			stored, write.Messages,
		)
	}

	if write.IdentityObservation.Project != "" {
		var err error
		if write.IdentitySnapshotProject == nil {
			err = upsertProjectIdentityObservationTxContext(
				ctx, tx, write.IdentityObservation,
			)
		} else {
			err = upsertProjectIdentityObservationWithSnapshotProjectTxContext(
				ctx, tx, write.IdentityObservation,
				*write.IdentitySnapshotProject,
				upsertResult.inserted, true,
			)
		}
		if err != nil {
			return 0, err
		}
	}
	if !upsertResult.inserted &&
		upsertResult.previousProject != upsertResult.currentProject {
		if err := reconcileSessionProjectIdentityAggregatesTx(
			ctx, tx, write.Session.ID,
			[]string{
				upsertResult.previousProject,
				upsertResult.currentProject,
			},
		); err != nil {
			return 0, err
		}
	}
	if err := replaceSessionUsageEventsTx(
		queries, write.Session.ID, write.UsageEvents, false,
	); err != nil {
		return 0, err
	}

	msgs := write.Messages
	var pins []savedPin
	if replaceMessages && sessionExists {
		pins, err = savePinsTx(queries, write.Session.ID)
		if err != nil {
			return 0, err
		}
		if err := deleteSessionMessagesTx(queries, write.Session.ID); err != nil {
			return 0, err
		}
	} else {
		maxOrd, err := maxOrdinalTx(queries, write.Session.ID)
		if err != nil {
			return 0, err
		}
		msgs = messagesAfterOrdinal(msgs, maxOrd)
	}
	transcriptChanged := len(msgs) > 0
	if replaceMessages && sessionExists {
		transcriptChanged = replacementTranscriptChanged
	}

	if len(msgs) > 0 {
		ids, err := insertMessagesTx(queries, msgs)
		if err != nil {
			return 0, err
		}
		toolCalls := resolveToolCalls(msgs, ids)
		if err := insertToolCallsTx(queries, toolCalls); err != nil {
			return 0, err
		}
		events := resolveToolResultEvents(msgs)
		if err := insertToolResultEventsTx(queries, events); err != nil {
			return 0, err
		}
	}
	if transcriptChanged {
		bump := bumpTranscriptRevisionTx
		if !sessionExists {
			bump = bumpInsertedTranscriptRevisionTx
		}
		if err := bump(queries, write.Session.ID); err != nil {
			return 0, err
		}
	}
	if replaceMessages && sessionExists {
		if err := reconcileRecallEvidenceForSessionTx(
			ctx,
			tx,
			write.Session.ID,
			pendingRecallRevocations,
		); err != nil {
			return 0, err
		}
	}
	if replaceMessages {
		if err := restorePinsTx(
			queries, write.Session.ID, pins,
		); err != nil {
			return 0, err
		}
		// A full message replacement re-normalizes every row, so this row is
		// no longer incremental-append skew. The append-only branch
		// (ReplaceMessages=false) deliberately leaves the marker untouched so
		// earlier incrementally written rows stay flagged for parse-diff.
		if err := resetIncrementalMarkerTx(queries, write.Session.ID); err != nil {
			return 0, err
		}
	}
	if err := updateSessionAutomationFromMessagesTx(
		queries, write.Session.ID,
	); err != nil {
		return 0, err
	}

	if write.DataVersion > 0 {
		if _, err := queries.Exec(
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

	if !write.SkipSignalUpdates {
		if err := updateSessionSignalsTx(queries, write.Session.ID, write.Signals); err != nil {
			return 0, err
		}
		if err := replaceSecretFindingsTx(queries, write.Session.ID, write.Findings,
			write.Signals.SecretLeakCount, write.Signals.SecretsRulesVersion); err != nil {
			return 0, err
		}
	}
	if err := enqueueArtifactExportIfGenerationUnchangedTx(
		queries, write.Session.ID, queueGenerationBefore, queueExistedBefore,
	); err != nil {
		return 0, err
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

func maxOrdinalTx(tx transactionQueries, sessionID string) (int, error) {
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
