package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ErrReplaceUnchanged reports a replacement whose messages already match the stored transcript.
var ErrReplaceUnchanged = errors.New("replacement matches the stored transcript")

// ReplaceSessionKeepingTrashedCopy replaces messages and keeps the previous row, messages, tools, name, and pins in Trash atomically; with KeepTrashedCopyOnlyOnPinLoss, it returns an empty ID when all pins survive.
func (db *DB) ReplaceSessionKeepingTrashedCopy(
	ctx context.Context, write SessionBatchWrite,
) (string, error) {
	if err := db.requireWritable(); err != nil {
		return "", err
	}
	write.ReplaceMessages = true
	write.RejectMessageCountDecrease = false
	write = db.storageSessionBatchWrite(sanitizeSessionBatchWrite(write))
	id := write.Session.ID

	// Writers hold db.mu, so the row read here matches the messages the transaction reads.
	db.mu.Lock()
	defer db.mu.Unlock()
	src, err := db.getSessionFullUncoalesced(ctx, id)
	if err != nil {
		return "", err
	}
	if src == nil {
		return "", fmt.Errorf("session %s not found", id)
	}
	if src.DeletedAt != nil {
		return "", ErrSessionTrashed
	}
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("beginning session replace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ctxTx := contextTransaction{ctx: ctx, tx: tx}
	var pending recallEvidenceRevocationEvents

	var active int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM sessions WHERE id = ? AND deleted_at IS NULL`, id,
	).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSessionTrashed
	}
	if err != nil {
		return "", fmt.Errorf("checking session %s: %w", id, err)
	}

	stored, err := sessionMessagesTx(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if transcriptMessagesEqual(stored, write.Messages) {
		return "", ErrReplaceUnchanged
	}

	events, err := usageEventsWithQuerier(ctx, tx, id, 0)
	if err != nil {
		return "", err
	}

	// The batch writer replaces a session's usage events, so keep the stored ones unless the import supplies its own.
	if len(write.UsageEvents) == 0 {
		write.UsageEvents = events
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.ordinal, p.note, p.created_at FROM pinned_messages p JOIN messages m ON m.id = p.message_id WHERE p.session_id = ?`, id)
	if err != nil {
		return "", err
	}
	var pins []savedPin
	for rows.Next() {
		var pin savedPin
		if err := rows.Scan(&pin.ordinal, &pin.note, &pin.createdAt); err != nil {
			_ = rows.Close()
			return "", fmt.Errorf("reading copy pins: %w", err)
		}
		pins = append(pins, pin)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return "", fmt.Errorf("reading copy pins: %w", err)
	}
	if _, err := writeOneSessionBatchTx(
		ctx, tx, ctxTx, write, &pending, db.usageOnlyStorage(),
	); err != nil {
		return "", err
	}
	var restored int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pinned_messages p JOIN messages m ON m.id = p.message_id WHERE p.session_id = ?`, id).Scan(&restored); err != nil {
		return "", fmt.Errorf("counting restored pins: %w", err)
	}
	copyID := ""
	if !write.KeepTrashedCopyOnlyOnPinLoss || restored < len(pins) {
		copyID = replacedSessionCopyID(id, time.Now())
		copyWrite := db.storageSessionBatchWrite(sessionCopyWrite(*src, copyID, stored))
		copyWrite.UsageEvents = make([]UsageEvent, len(events))
		for i, ev := range events {
			ev.ID, ev.SessionID = 0, copyID
			copyWrite.UsageEvents[i] = ev
		}
		if _, err := writeOneSessionBatchTx(
			ctx, tx, ctxTx, copyWrite, &pending, db.usageOnlyStorage(),
		); err != nil {
			return "", fmt.Errorf("writing replaced session copy: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET display_name = ?,
		    deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'),
		    local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`, src.DisplayName, copyID,
		); err != nil {
			return "", fmt.Errorf("trashing replaced session copy: %w", err)
		}
		if err := copySessionPinsTx(ctx, tx, pins, copyID); err != nil {
			return "", err
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("committing session replace: %w", err)
	}
	changed := []string{id}
	if copyID != "" {
		changed = append(changed, copyID)
	}
	db.notifyUsageSessions(changed)
	pending.flush()
	return copyID, nil
}

// replacedSessionCopyID names the trashed copy after its source and the replacement time.
func replacedSessionCopyID(id string, now time.Time) string {
	return id + ":replaced:" + now.UTC().Format("20060102T150405.000000000Z")
}

// sessionCopyWrite rekeys a stored session and its messages under copyID without the source file's identity.
func sessionCopyWrite(src Session, copyID string, msgs []Message) SessionBatchWrite {
	sess := src
	sess.ID = copyID
	sess.FilePath, sess.FileSize, sess.FileMtime = nil, nil, nil
	sess.FileInode, sess.FileDevice, sess.FileHash = nil, nil, nil
	rows := make([]Message, len(msgs))
	for i, m := range msgs {
		m.ID, m.SessionID = 0, copyID
		m.ToolCalls = slices.Clone(m.ToolCalls)
		for j := range m.ToolCalls {
			m.ToolCalls[j].SessionID, m.ToolCalls[j].MessageID = copyID, 0
		}
		rows[i] = m
	}
	return SessionBatchWrite{
		Session:           sess,
		Messages:          rows,
		SkipSignalUpdates: true,
	}
}

// copySessionPinsTx copies pins to the message at the same ordinal in another session.
func copySessionPinsTx(ctx context.Context, tx *sql.Tx, pins []savedPin, toID string) error {
	for _, pin := range pins {
		if _, err := tx.ExecContext(ctx, `
		INSERT INTO pinned_messages (session_id, message_id, ordinal, note, created_at)
		SELECT ?, cm.id, cm.ordinal, ?, ? FROM messages cm
		WHERE cm.session_id = ? AND cm.ordinal = ?`, toID, pin.note, pin.createdAt, toID, pin.ordinal,
		); err != nil {
			return fmt.Errorf("copying pins to %s: %w", toID, err)
		}
	}
	return nil
}
