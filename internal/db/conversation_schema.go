package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// This additive migration owns only local conversation export state. Historical
// rows become explicit prose gaps only when writable reparsing cannot supply
// evidence. A pending rebuild must not publish temporary message identities.
const conversationSchemaSQL = `
CREATE TRIGGER IF NOT EXISTS conversation_messages_insert AFTER INSERT ON conversation_messages
BEGIN
 INSERT INTO archive_metadata(key,value) VALUES ('conversation_publication_revision','1')
 ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT);
 UPDATE conversation_messages SET revision=(SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key='conversation_publication_revision')
 WHERE session_id=NEW.session_id AND message_id=NEW.message_id;
END;
CREATE TRIGGER IF NOT EXISTS conversation_messages_update
AFTER UPDATE OF ordinal,role,timestamp,source_id,body,digest,text_bytes,gap,deleted,removed ON conversation_messages
WHEN OLD.ordinal IS NOT NEW.ordinal OR OLD.role IS NOT NEW.role OR OLD.timestamp IS NOT NEW.timestamp
 OR OLD.source_id IS NOT NEW.source_id OR OLD.body IS NOT NEW.body OR OLD.digest IS NOT NEW.digest
 OR OLD.text_bytes IS NOT NEW.text_bytes OR OLD.gap IS NOT NEW.gap OR OLD.deleted IS NOT NEW.deleted
 OR OLD.removed IS NOT NEW.removed
BEGIN
 INSERT INTO archive_metadata(key,value) VALUES ('conversation_publication_revision','1')
 ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT);
 UPDATE conversation_messages SET revision=(SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key='conversation_publication_revision')
 WHERE session_id=NEW.session_id AND message_id=NEW.message_id;
END;
CREATE TRIGGER IF NOT EXISTS conversation_session_delete AFTER DELETE ON sessions
BEGIN
 UPDATE conversation_messages SET deleted=1,removed=1,body=NULL WHERE session_id=OLD.id AND removed=0;
 INSERT INTO archive_metadata(key,value) VALUES ('conversation_publication_revision','1') ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT);
 INSERT INTO conversation_session_changes(session_id,revision,deleted) SELECT OLD.id,CAST(value AS INTEGER),1 FROM archive_metadata WHERE key='conversation_publication_revision'
 ON CONFLICT(session_id) DO UPDATE SET revision=excluded.revision,deleted=1;
END;
CREATE TRIGGER IF NOT EXISTS conversation_session_trash AFTER UPDATE OF deleted_at ON sessions
WHEN OLD.deleted_at IS NOT NEW.deleted_at
BEGIN
 UPDATE conversation_messages SET deleted=(NEW.deleted_at IS NOT NULL) WHERE session_id=NEW.id AND removed=0;
 INSERT INTO archive_metadata(key,value) VALUES ('conversation_publication_revision','1') ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT);
 INSERT INTO conversation_session_changes(session_id,revision,deleted) SELECT NEW.id,CAST(value AS INTEGER),(NEW.deleted_at IS NOT NULL) FROM archive_metadata WHERE key='conversation_publication_revision'
 ON CONFLICT(session_id) DO UPDATE SET revision=excluded.revision,deleted=excluded.deleted;
END;
CREATE TRIGGER IF NOT EXISTS conversation_session_project AFTER UPDATE OF project,machine ON sessions
WHEN (OLD.project IS NOT NEW.project OR OLD.machine IS NOT NEW.machine) AND (EXISTS(SELECT 1 FROM conversation_messages WHERE session_id=NEW.id AND removed=0) OR EXISTS(SELECT 1 FROM conversation_session_changes WHERE session_id=NEW.id))
BEGIN
 INSERT INTO archive_metadata(key,value) VALUES ('conversation_publication_revision','1') ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT);
 INSERT INTO conversation_session_changes(session_id,revision,deleted) SELECT NEW.id,CAST(value AS INTEGER),(NEW.deleted_at IS NOT NULL) FROM archive_metadata WHERE key='conversation_publication_revision'
 ON CONFLICT(session_id) DO UPDATE SET revision=excluded.revision,deleted=excluded.deleted;
END;
CREATE TRIGGER IF NOT EXISTS conversation_project_insert AFTER INSERT ON session_project_identity_snapshots
WHEN EXISTS(SELECT 1 FROM conversation_messages WHERE session_id=NEW.session_id AND removed=0) OR EXISTS(SELECT 1 FROM conversation_session_changes WHERE session_id=NEW.session_id)
BEGIN
 INSERT INTO archive_metadata(key,value) VALUES ('conversation_publication_revision','1') ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT);
 INSERT INTO conversation_session_changes(session_id,revision,deleted) SELECT NEW.session_id,CAST(value AS INTEGER),COALESCE((SELECT deleted_at IS NOT NULL FROM sessions WHERE id=NEW.session_id),1) FROM archive_metadata WHERE key='conversation_publication_revision'
 ON CONFLICT(session_id) DO UPDATE SET revision=excluded.revision,deleted=excluded.deleted;
END;
CREATE TRIGGER IF NOT EXISTS conversation_project_update AFTER UPDATE ON session_project_identity_snapshots
WHEN (OLD.project IS NOT NEW.project OR OLD.machine IS NOT NEW.machine OR OLD.root_path IS NOT NEW.root_path
 OR OLD.git_remote IS NOT NEW.git_remote OR OLD.git_remote_name IS NOT NEW.git_remote_name
 OR OLD.repository_path IS NOT NEW.repository_path OR OLD.worktree_name IS NOT NEW.worktree_name
 OR OLD.worktree_root_path IS NOT NEW.worktree_root_path OR OLD.worktree_relationship IS NOT NEW.worktree_relationship
 OR OLD.checkout_state IS NOT NEW.checkout_state OR OLD.git_branch IS NOT NEW.git_branch
 OR OLD.remote_resolution IS NOT NEW.remote_resolution) AND (EXISTS(SELECT 1 FROM conversation_messages WHERE session_id=NEW.session_id AND removed=0) OR EXISTS(SELECT 1 FROM conversation_session_changes WHERE session_id=NEW.session_id))
BEGIN
 INSERT INTO archive_metadata(key,value) VALUES ('conversation_publication_revision','1') ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT);
 INSERT INTO conversation_session_changes(session_id,revision,deleted) SELECT NEW.session_id,CAST(value AS INTEGER),COALESCE((SELECT deleted_at IS NOT NULL FROM sessions WHERE id=NEW.session_id),1) FROM archive_metadata WHERE key='conversation_publication_revision'
 ON CONFLICT(session_id) DO UPDATE SET revision=excluded.revision,deleted=excluded.deleted;
END;
CREATE TRIGGER IF NOT EXISTS conversation_project_delete AFTER DELETE ON session_project_identity_snapshots
WHEN EXISTS(SELECT 1 FROM conversation_messages WHERE session_id=OLD.session_id AND removed=0) OR EXISTS(SELECT 1 FROM conversation_session_changes WHERE session_id=OLD.session_id)
BEGIN
 INSERT INTO archive_metadata(key,value) VALUES ('conversation_publication_revision','1') ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT);
 INSERT INTO conversation_session_changes(session_id,revision,deleted) SELECT OLD.session_id,CAST(value AS INTEGER),COALESCE((SELECT deleted_at IS NOT NULL FROM sessions WHERE id=OLD.session_id),1) FROM archive_metadata WHERE key='conversation_publication_revision'
 ON CONFLICT(session_id) DO UPDATE SET revision=excluded.revision,deleted=excluded.deleted;
END;`

func ensureConversationSchemaLocked(ctx context.Context, w *writerHandle, usageOnly bool) error {
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, conversationSchemaSQL); err != nil {
		return fmt.Errorf("creating conversation export state: %w", err)
	}
	var initialized bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM archive_metadata WHERE key='conversation_export_initialized')`).Scan(&initialized); err != nil {
		return err
	}
	if !initialized {
		if err := refreshConversationMessagesFromArchiveTx(ctx, tx, "1=1"); err != nil {
			return err
		}
		if usageOnly {
			rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				if err := clearUsageOnlyConversationTx(contextTransaction{ctx: ctx, tx: tx}, id); err != nil {
					return err
				}
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES ('conversation_export_initialized','1')`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const conversationCopyColumns = `session_id,message_id,ordinal,role,timestamp,source_id,body,digest,text_bytes,gap,deleted,removed`

// Initialize exports or refresh copied rows after archive policy has changed
// their stored content. These are the same archived messages, so keep their IDs.
func refreshConversationMessagesFromArchiveTx(ctx context.Context, tx *sql.Tx, where string) error {
	rows, err := tx.QueryContext(ctx, `SELECT m.session_id,m.ordinal,m.role,m.content,COALESCE(m.timestamp,''),m.source_uuid,
	 COALESCE(c.message_id,lower(hex(randomblob(16)))),COALESCE(c.gap,''),COUNT(*) OVER (PARTITION BY m.session_id,m.source_uuid)
	 FROM (SELECT * FROM messages WHERE role IN ('user','assistant') AND is_system=0 AND source_subtype!='tool_result' AND `+where+`) m
	 LEFT JOIN conversation_messages c ON c.session_id=m.session_id AND c.ordinal=m.ordinal AND c.removed=0
	 ORDER BY m.session_id,m.ordinal`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var msg Message
		var id, gap string
		var sourceCount int
		if err := rows.Scan(&msg.SessionID, &msg.Ordinal, &msg.Role, &msg.Content, &msg.Timestamp, &msg.SourceUUID, &id, &gap, &sourceCount); err != nil {
			return err
		}
		row, _ := conversationRowFromMessage(msg)
		row.MessageID = id
		if gap == "identity_ambiguous" || msg.SourceUUID != "" && sourceCount > 1 {
			row.Gap = "identity_ambiguous"
		}
		if gap == "archive_content_excluded" && row.body == nil {
			row.Gap = gap
		}
		if err := putConversationRowTx(contextTransaction{ctx: ctx, tx: tx}, row); err != nil {
			return err
		}
	}
	return rows.Err()
}

func copyConversationRowsTx(ctx context.Context, tx *sql.Tx, where string) error {
	var initialized bool
	if oldDBHasTable(ctx, tx, "conversation_messages") {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM old_db.archive_metadata WHERE key='conversation_export_initialized')`).Scan(&initialized); err != nil {
			return err
		}
	}
	if !initialized {
		return refreshConversationMessagesFromArchiveTx(ctx, tx, where)
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO main.conversation_messages (`+conversationCopyColumns+`) SELECT `+conversationCopyColumns+` FROM old_db.conversation_messages WHERE `+where)
	if err != nil {
		return err
	}
	return copyConversationSessionStatesTx(ctx, tx, where)
}

func retainConversationTombstonesTx(ctx context.Context, tx *sql.Tx) error {
	if !oldDBHasTable(ctx, tx, "conversation_messages") {
		return nil
	}
	if err := copyConversationRowsTx(ctx, tx, "session_id NOT IN (SELECT id FROM main.sessions)"); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE main.conversation_messages SET deleted=1,removed=1,body=NULL
	 WHERE session_id NOT IN (SELECT id FROM main.sessions) AND removed=0`)
	return err
}

func copyConversationSessionStatesTx(ctx context.Context, tx *sql.Tx, where string) error {
	rows, err := tx.QueryContext(ctx, `SELECT session_id,gap,
	 COALESCE((SELECT deleted_at IS NOT NULL FROM main.sessions WHERE id=s.session_id),1)
	 FROM old_db.conversation_session_changes s WHERE `+where)
	if err != nil {
		return err
	}
	defer rows.Close()
	type state struct {
		id, gap string
		deleted bool
	}
	var states []state
	for rows.Next() {
		var s state
		if err := rows.Scan(&s.id, &s.gap, &s.deleted); err != nil {
			return err
		}
		states = append(states, s)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, s := range states {
		if err := putConversationSessionChangeTx(contextTransaction{ctx: ctx, tx: tx}, s.id, s.gap, s.deleted); err != nil {
			return err
		}
	}
	return nil
}

// Rebuilt archives preserve opaque IDs from the old projection before matching
// freshly parsed messages. The new database keeps its own publication counter.
func reconcileConversationResyncTx(ctx context.Context, tx *sql.Tx, usageOnly bool) error {
	if !oldDBHasTable(ctx, tx, "conversation_messages") {
		return nil
	}
	// Trash was copied without reparsing, so keep its original projection and gaps.
	rows, err := tx.QueryContext(ctx, `SELECT id FROM main.sessions WHERE deleted_at IS NULL AND id IN (SELECT session_id FROM old_db.conversation_messages)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := conversationRowsTx(tx, id)
		if err != nil {
			return err
		}
		msgs := make([]Message, 0, len(current))
		for _, row := range current {
			msg := Message{SessionID: id, Ordinal: row.Ordinal, Role: row.Role, SourceUUID: row.sourceID}
			if row.body != nil {
				msg.Content = *row.body
			}
			if row.Timestamp != nil {
				msg.Timestamp = *row.Timestamp
			}
			msgs = append(msgs, msg)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM main.conversation_messages WHERE session_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO main.conversation_messages (`+conversationCopyColumns+`) SELECT `+conversationCopyColumns+` FROM old_db.conversation_messages WHERE session_id=?`, id); err != nil {
			return err
		}
		if err := reconcileConversationMessagesTx(tx, id, msgs, true, usageOnly); err != nil {
			return err
		}
	}
	return nil
}
