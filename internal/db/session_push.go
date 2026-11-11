package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// SessionPushReceipt pins the first snapshot accepted for an exact session ID.
type SessionPushReceipt struct {
	SessionID               string
	SourceOrigin            string
	PayloadHash             string
	ExternalParentSessionID *string
}

// ErrSessionPushConflict reports an ID whose first snapshot cannot be accepted
// or retried without changing existing ownership or deletion decisions.
var ErrSessionPushConflict = errors.New("session push conflicts with archived session")

// ErrSessionPushNeedsUpgrade means the archive cannot yet protect receipts.
var ErrSessionPushNeedsUpgrade = errors.New("session push requires archive rebuild")

const SessionPushDataVersion = 176

// ApplySessionPush atomically publishes a first snapshot and its receipt.
// Matching receipts are no-ops only while their session still exists outside
// Trash. Receipts outlive permanent deletion to prevent later resurrection.
func (db *DB) ApplySessionPush(
	ctx context.Context, receipt SessionPushReceipt, write SessionBatchWrite,
) (bool, error) {
	if err := db.requireWritable(); err != nil {
		return false, err
	}
	if receipt.SessionID == "" || receipt.SourceOrigin == "" || receipt.PayloadHash == "" {
		return false, errors.New("session push receipt fields are required")
	}
	if receipt.SessionID != write.Session.ID {
		return false, errors.New("session push receipt does not match session id")
	}
	write.Session.Machine = receipt.SourceOrigin
	write, err := sanitizeSessionBatchWriteContext(ctx, write)
	if err != nil {
		return false, err
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning session push: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	version, _, err := readUserVersion(ctx, tx)
	if err != nil {
		return false, err
	}
	if version < SessionPushDataVersion {
		return false, ErrSessionPushNeedsUpgrade
	}
	// Retention includes inherited exclusions/trash for absent archive rows.
	// Resolve it in this transaction before any image policy can publish files.
	if err := validateRecallImportPlaceholderSessionStateWithQueryer(ctx, tx, receipt.SessionID); err != nil {
		if errors.Is(err, ErrSessionExcluded) || errors.Is(err, ErrSessionTrashed) {
			return false, fmt.Errorf("%w: %w", ErrSessionPushConflict, err)
		}
		return false, err
	}

	var origin, hash string
	err = tx.QueryRowContext(ctx, `
		SELECT source_origin, payload_hash FROM session_push_receipts
		WHERE session_id = ?`, receipt.SessionID).Scan(&origin, &hash)
	hasReceipt := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("checking session push receipt: %w", err)
	}
	var deletedAt sql.NullString
	var machine, agent string
	err = tx.QueryRowContext(ctx, `SELECT deleted_at, machine, agent FROM sessions WHERE id = ?`,
		receipt.SessionID).Scan(&deletedAt, &machine, &agent)
	hasSession := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("checking pushed session: %w", err)
	}
	if hasReceipt {
		if origin != receipt.SourceOrigin || hash != receipt.PayloadHash ||
			!hasSession || deletedAt.Valid || machine != receipt.SourceOrigin || agent != write.Session.Agent {
			return false, ErrSessionPushConflict
		}
		return false, nil
	}
	if hasSession {
		return false, ErrSessionPushConflict
	}
	var existingParent bool
	if receipt.ExternalParentSessionID != nil {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_external_parents WHERE session_id = ?)`, receipt.SessionID).Scan(&existingParent); err != nil {
			return false, err
		}
		if !existingParent {
			if err := rejectExternalParentCycle(ctx, tx, receipt.SessionID, *receipt.ExternalParentSessionID); err != nil {
				if errors.Is(err, ErrSessionExternalParentInvalid) {
					return false, fmt.Errorf("%w: %w", ErrSessionPushConflict, err)
				}
				return false, err
			}
		}
	}
	// Image offload publishes files. Decide conflicts/no-ops before any policy I/O.
	write = db.storageSessionBatchWrite(write)

	var pending recallEvidenceRevocationEvents
	queries := contextTransaction{ctx: ctx, tx: tx}
	if _, err := db.writeOneSessionBatchTx(ctx, tx, queries, write, &pending,
		db.usageOnlyStorage(), nil); err != nil {
		if errors.Is(err, ErrSessionExcluded) || errors.Is(err, ErrSessionTrashed) {
			return false, fmt.Errorf("%w: %w", ErrSessionPushConflict, err)
		}
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_push_receipts (session_id, source_origin, payload_hash)
		VALUES (?, ?, ?)`, receipt.SessionID, receipt.SourceOrigin, receipt.PayloadHash); err != nil {
		return false, fmt.Errorf("recording session push receipt: %w", err)
	}
	if receipt.ExternalParentSessionID != nil && !existingParent {
		if _, err := setSessionExternalParentTx(ctx, tx, receipt.SessionID, *receipt.ExternalParentSessionID); err != nil {
			if errors.Is(err, ErrSessionExternalParentInvalid) {
				return false, fmt.Errorf("%w: %w", ErrSessionPushConflict, err)
			}
			return false, err
		}
	}
	if _, err := applySessionExternalParentsFor(ctx, tx.ExecContext,
		func(ctx context.Context, query string, args ...any) rowScanner {
			return tx.QueryRowContext(ctx, query, args...)
		}, []string{receipt.SessionID}); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("committing session push: %w", err)
	}
	db.notifyUsageSessions([]string{receipt.SessionID})
	pending.flush()
	return true, nil
}

// SessionPushReservations returns the candidate IDs held by durable receipts.
func (db *DB) SessionPushReservations(ctx context.Context, ids []string) (map[string]bool, error) {
	reserved := make(map[string]bool)
	if len(ids) == 0 {
		return reserved, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := db.getReader().QueryContext(ctx, `SELECT session_id FROM session_push_receipts WHERE session_id IN (SELECT value FROM json_each(?))`, string(encoded))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		reserved[id] = true
	}
	return reserved, rows.Err()
}

// checkSessionPushReservationTx applies to ordinary ingestion, not curation or
// deletion. The first push records its receipt after writing the transcript.
func checkSessionPushReservationTx(ctx context.Context, tx *sql.Tx, id string) error {
	var reserved bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_push_receipts WHERE session_id=?)`, id).Scan(&reserved); err != nil {
		return err
	}
	if reserved {
		return ErrSessionPushConflict
	}
	return nil
}

// copySessionPushReceiptsTx reserves IDs before rebuild parsing and repeats
// safely during final state preservation. Different authorities never replace.
func copySessionPushReceiptsTx(ctx context.Context, tx *sql.Tx) error {
	if !oldDBHasTable(ctx, tx, "session_push_receipts") {
		return nil
	}
	var conflict bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM main.session_push_receipts current
		JOIN old_db.session_push_receipts old USING(session_id)
		WHERE current.source_origin != old.source_origin OR current.payload_hash != old.payload_hash
	)`).Scan(&conflict); err != nil {
		return err
	}
	if conflict {
		return ErrSessionPushConflict
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.session_push_receipts(session_id,source_origin,payload_hash)
		SELECT session_id,source_origin,payload_hash FROM old_db.session_push_receipts`)
	return err
}
