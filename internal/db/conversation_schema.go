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

func ensureConversationSchemaLocked(ctx context.Context, w *writerHandle, rebuildPending bool) error {
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
	if !initialized && !rebuildPending {
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_messages(session_id,message_id,ordinal,role,timestamp,gap,deleted)
		 SELECT m.session_id,lower(hex(randomblob(16))),m.ordinal,m.role,COALESCE(m.timestamp,''),'visible_text_unavailable',s.deleted_at IS NOT NULL
		 FROM messages m JOIN sessions s ON s.id=m.session_id WHERE m.role IN ('user','assistant') AND m.is_system=0 AND COALESCE(m.source_subtype,'')!='tool_result'`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES ('conversation_export_initialized','1')`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const conversationCopyColumns = `session_id,message_id,ordinal,role,timestamp,source_id,body,digest,text_bytes,gap,deleted,removed`

func copyConversationRowsTx(ctx context.Context, tx *sql.Tx, where string) error {
	var initialized bool
	if oldDBHasTable(ctx, tx, "conversation_messages") {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM old_db.archive_metadata WHERE key='conversation_export_initialized')`).Scan(&initialized); err != nil {
			return err
		}
	}
	if !initialized {
		_, err := tx.ExecContext(ctx, `INSERT INTO main.conversation_messages(session_id,message_id,ordinal,role,timestamp,gap,deleted)
		 SELECT session_id,lower(hex(randomblob(16))),ordinal,role,COALESCE(timestamp,''),'visible_text_unavailable',
		 COALESCE((SELECT deleted_at IS NOT NULL FROM main.sessions WHERE id=m.session_id),0)
		 FROM main.messages m WHERE role IN ('user','assistant') AND is_system=0 AND COALESCE(source_subtype,'')!='tool_result' AND `+where)
		return err
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
	type state struct {
		id, gap string
		deleted bool
	}
	var states []state
	for rows.Next() {
		var s state
		if err := rows.Scan(&s.id, &s.gap, &s.deleted); err != nil {
			_ = rows.Close()
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
func reconcileConversationResyncTx(ctx context.Context, tx *sql.Tx) error {
	if !oldDBHasTable(ctx, tx, "conversation_messages") {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM main.sessions WHERE id IN (SELECT session_id FROM old_db.conversation_messages)`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
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
			msg := Message{SessionID: id, Ordinal: row.Ordinal, Role: row.Role, VisibleText: row.body, ConversationSourceID: row.sourceID}
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
		if err := reconcileConversationMessagesTx(tx, id, msgs, true); err != nil {
			return err
		}
	}
	return nil
}
