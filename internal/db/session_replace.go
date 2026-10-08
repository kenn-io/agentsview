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

// ReplaceSessionKeepingTrashedCopy replaces an active session's messages with write.Messages and, in the same transaction, keeps the previous version as a trashed copy under a new ID: its row, messages, tool calls, display name, and pins. It returns the copy's ID.
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

	copyID, err := matchingTrashedCopyTx(ctx, tx, *src, stored)
	if err != nil {
		return "", err
	}
	if copyID == "" {
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
		SET display_name = (SELECT display_name FROM sessions WHERE id = ?),
		    deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'),
		    local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`, id, copyID,
		); err != nil {
			return "", fmt.Errorf("trashing replaced session copy: %w", err)
		}
	}
	if err := copySessionPinsTx(ctx, tx, id, copyID); err != nil {
		return "", err
	}
	// The batch writer replaces a session's usage events, so keep the stored ones unless the import supplies its own.
	if len(write.UsageEvents) == 0 {
		write.UsageEvents = events
	}

	if _, err := writeOneSessionBatchTx(
		ctx, tx, ctxTx, write, &pending, db.usageOnlyStorage(),
	); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("committing session replace: %w", err)
	}
	db.notifyUsageSessions([]string{copyID, id})
	pending.flush()
	return copyID, nil
}

func matchingTrashedCopyTx(ctx context.Context, tx *sql.Tx, src Session, stored []Message) (string, error) {
	prefix := src.ID + ":replaced:"
	rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions WHERE deleted_at IS NOT NULL AND id >= ? AND id < ?`, prefix, src.ID+":replaced;")
	if err != nil {
		return "", err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return "", err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		messages, err := sessionMessagesTx(ctx, tx, id)
		if err != nil {
			return "", err
		}
		previous := sessionCopyWrite(src, id, stored).Messages
		// Claude.ai exports omit UUIDs that Sync later adds to the same turns.
		if src.Agent == "claude-ai" && len(messages) == len(previous) {
			for i := range messages {
				if messages[i].SourceUUID == "" || previous[i].SourceUUID == "" {
					messages[i].SourceUUID, previous[i].SourceUUID = "", ""
				}
			}
		}
		if transcriptMessagesEqual(messages, previous) {
			return id, nil
		}
	}
	return "", nil
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
func copySessionPinsTx(ctx context.Context, tx *sql.Tx, fromID, toID string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pinned_messages (session_id, message_id, ordinal, note, created_at)
		SELECT ?, cm.id, cm.ordinal, p.note, p.created_at
		FROM pinned_messages p
		JOIN messages sm ON sm.id = p.message_id
		JOIN messages cm ON cm.session_id = ? AND cm.ordinal = sm.ordinal
		WHERE p.session_id = ? AND NOT EXISTS (
			SELECT 1 FROM pinned_messages existing WHERE existing.session_id = ? AND existing.message_id = cm.id
		)`, toID, toID, fromID, toID,
	); err != nil {
		return fmt.Errorf("copying pins to %s: %w", toID, err)
	}
	return nil
}
