package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
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
	keepCopy := true
	if write.KeepTrashedCopyOnlyOnPinLoss {
		dropped, err := countReplacementPinLossTx(ctx, tx, id, src.Agent, stored, write.Messages)
		if err != nil {
			return "", err
		}
		keepCopy = dropped > 0
	}
	copyID := ""
	if keepCopy {
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
		if err := copySessionPinsTx(ctx, tx, id, copyID); err != nil {
			return "", err
		}
	}

	if _, err := writeOneSessionBatchTx(
		ctx, tx, ctxTx, write, &pending, db.usageOnlyStorage(),
	); err != nil {
		return "", err
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

// countReplacementPinLossTx applies the pin identity rules to projected rows before writing.
func countReplacementPinLossTx(ctx context.Context, tx *sql.Tx, id, agent string, stored, incoming []Message) (int, error) {
	type identity struct{ uuid, role, content string }
	legacy := agent == "claude-ai"
	for _, m := range incoming {
		if m.SourceUUID != "" {
			legacy = false
			break
		}
	}
	groups := func(msgs []Message) (map[identity][]int, map[string][]int) {
		identities := make(map[identity][]int)
		uuids := make(map[string][]int)
		for _, m := range msgs {
			uuids[m.SourceUUID] = append(uuids[m.SourceUUID], m.Ordinal)
			if m.SourceUUID != "" && !legacy {
				key := identity{m.SourceUUID, m.Role, m.Content}
				identities[key] = append(identities[key], m.Ordinal)
			}
			if !m.IsSystem {
				key := identity{"", m.Role, m.Content}
				identities[key] = append(identities[key], m.Ordinal)
			}
		}
		for _, ordinals := range identities {
			slices.Sort(ordinals)
		}
		return identities, uuids
	}
	oldGroups, oldUUIDs := groups(stored)
	newGroups, newUUIDs := groups(incoming)
	var targets [][2]int64
	for _, m := range stored {
		target := -1
		key := identity{"", m.Role, m.Content}
		if m.SourceUUID != "" && !legacy {
			key.uuid = m.SourceUUID
			if len(oldUUIDs[m.SourceUUID]) == 1 && len(newUUIDs[m.SourceUUID]) == 1 {
				target = newUUIDs[m.SourceUUID][0]
			}
		} else if m.IsSystem {
			continue
		}
		if target < 0 && len(oldGroups[key]) == len(newGroups[key]) {
			rank, found := slices.BinarySearch(oldGroups[key], m.Ordinal)
			if found {
				target = newGroups[key][rank]
			}
		}
		if target >= 0 {
			targets = append(targets, [2]int64{m.ID, int64(target)})
		}
	}
	raw, err := json.Marshal(targets)
	if err != nil {
		return 0, err
	}
	var dropped int
	err = tx.QueryRowContext(ctx, `
		WITH targets AS (
			SELECT json_extract(value, '$[0]') AS message_id,
				   json_extract(value, '$[1]') AS ordinal
			FROM json_each(?)
		)
		SELECT COUNT(*) - COUNT(DISTINCT targets.ordinal)
		FROM pinned_messages p
		LEFT JOIN targets ON targets.message_id = p.message_id
		WHERE p.session_id = ?`, string(raw), id).Scan(&dropped)
	if err != nil {
		return 0, fmt.Errorf("counting replacement pin loss: %w", err)
	}
	return dropped, nil
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
		WHERE p.session_id = ?`, toID, toID, fromID,
	); err != nil {
		return fmt.Errorf("copying pins to %s: %w", toID, err)
	}
	return nil
}
