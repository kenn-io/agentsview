package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
)

// SessionPushReceipt pins the first snapshot accepted for an exact session ID.
type SessionPushReceipt struct {
	SessionID    string
	SourceOrigin string
	PayloadHash  string
}

// ErrSessionPushConflict reports an ID whose first snapshot cannot be accepted
// or retried without changing existing ownership or deletion decisions.
var ErrSessionPushConflict = errors.New("session push conflicts with archived session")

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
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("committing session push: %w", err)
	}
	db.notifyUsageSessions([]string{receipt.SessionID})
	pending.flush()
	return true, nil
}

// checkSessionPushReservationTx applies to ordinary ingestion, not curation or
// deletion. The first push writes its receipt after its transcript transaction.
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

// ValidateSessionPushResultCollections bounds JSON collections that image
// projection parses inside otherwise opaque tool-result text. Ordinary text
// and small malformed JSON retain their usual rendering behavior.
func ValidateSessionPushResultCollections(content string) error {
	// Inspect the same text storage and downstream image/signal parsers see.
	content = SanitizeUTF8(content)
	if err := validateSessionPushResultJSON(content); err != nil {
		return err
	}
	var invalid error
	scanSummarySections(content, func(_, _ int, raw jsontext.Value) {
		if invalid == nil {
			invalid = validateSessionPushResultJSON(string(raw))
		}
	})
	return invalid
}

func validateSessionPushResultJSON(content string) error {
	dec := jsontext.NewDecoder(strings.NewReader(content))
	first, err := dec.ReadToken()
	if err != nil || (first.Kind() != jsontext.KindBeginArray && first.Kind() != jsontext.KindBeginObject) {
		return nil
	}
	type collection struct {
		kind  jsontext.Kind
		count int
	}
	stack := []collection{{kind: first.Kind()}}
	for len(stack) > 0 {
		token, err := dec.ReadToken()
		if err != nil {
			return nil
		}
		kind := token.Kind()
		if kind == jsontext.KindEndArray || kind == jsontext.KindEndObject {
			stack = stack[:len(stack)-1]
			continue
		}
		current := &stack[len(stack)-1]
		current.count++
		limit := 4096
		if current.kind == jsontext.KindBeginObject {
			limit = 512
		} // 256 name/value pairs
		if current.count > limit {
			return errors.New("session push result collection exceeds limit")
		}
		if kind == jsontext.KindBeginArray || kind == jsontext.KindBeginObject {
			stack = append(stack, collection{kind: kind})
		}
	}
	return nil
}
