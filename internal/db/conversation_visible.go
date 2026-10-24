package db

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/agentsview/internal/activity"
)

// Upgrade the derived export index from archived dialogue without reparsing
// source files or changing opaque message identities.
func ensureConversationVisibleSchema(ctx context.Context, tx *sql.Tx) error {
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('conversation_messages') WHERE name='visible_digest')`).Scan(&present); err != nil {
		return err
	}
	if err := applyColumnMigrations([]schemaColumnMigration{
		{"conversation_messages", "visible_digest", "ALTER TABLE conversation_messages ADD COLUMN visible_digest TEXT NOT NULL DEFAULT ''"},
		{"conversation_messages", "visible_text_bytes", "ALTER TABLE conversation_messages ADD COLUMN visible_text_bytes INTEGER NOT NULL DEFAULT 0"},
		{"conversation_messages", "visible_gap", "ALTER TABLE conversation_messages ADD COLUMN visible_gap TEXT NOT NULL DEFAULT 'visible_text_unavailable'"},
	}, func(query string, args ...any) rowScanner { return tx.QueryRowContext(ctx, query, args...) },
		func(query string, args ...any) (sql.Result, error) { return tx.ExecContext(ctx, query, args...) }, nil); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, conversationVisibleRevisionSQL); err != nil {
		return err
	}
	if !present {
		active, err := conversationExportActiveTx(contextTransaction{ctx: ctx, tx: tx})
		if err != nil {
			return err
		}
		if active {
			if err := refreshConversationMessagesFromArchiveTx(ctx, tx, "1=1"); err != nil {
				return err
			}
			return applyConversationPolicyGapsTx(ctx, tx, "1=1")
		}
	}
	return nil
}

const conversationVisibleCopyColumns = `,visible_digest,visible_text_bytes,visible_gap`

func conversationCopySelect(ctx context.Context, tx *sql.Tx) string {
	if oldDBHasColumn(ctx, tx, "conversation_messages", "visible_digest") {
		return conversationCopyColumns + conversationVisibleCopyColumns
	}
	return conversationCopyColumns + `,'',0,'visible_text_unavailable'`
}

// Empty mode preserves the existing stored-content wire format and cursors.
func conversationTextMode(mode string) (string, error) {
	switch mode {
	case "", "stored":
		return "", nil
	case "visible":
		return mode, nil
	default:
		return "", errors.New("text mode must be stored or visible")
	}
}

func requireConversationTextSchema(ctx context.Context, tx *sql.Tx, mode string) error {
	if mode != "visible" {
		return nil
	}
	for _, column := range []string{"visible_digest", "visible_text_bytes", "visible_gap"} {
		var present bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('conversation_messages') WHERE name=?)`, column).Scan(&present); err != nil {
			return err
		}
		if !present {
			return &SchemaUpgradeRequiredError{Table: "conversation_messages", Column: column}
		}
	}
	return nil
}

func conversationColumnsForMode(mode string) string {
	if mode == "visible" {
		return `session_id,message_id,CAST(revision AS TEXT),ordinal,role,NULLIF(timestamp,''),deleted,
		 CASE WHEN gap='archive_content_excluded' THEN gap WHEN visible_gap!='' THEN visible_gap ELSE gap END,
		 CASE WHEN gap='archive_content_excluded' THEN '' ELSE visible_digest END,
		 CASE WHEN gap='archive_content_excluded' THEN 0 ELSE visible_text_bytes END`
	}
	return conversationChangeColumns
}

func conversationBodyColumn(mode string) string {
	if mode == "visible" {
		return "COALESCE(m.dialogue_text,'')"
	}
	return "m.content"
}

// Add a forward trigger for projection-only edits. The existing trigger owns
// changes to stored text, order, identity and deletion; do not publish twice
// when the same write changes both projections.
const conversationVisibleRevisionSQL = `
CREATE TRIGGER IF NOT EXISTS conversation_visible_revise
AFTER UPDATE OF visible_digest,visible_text_bytes,visible_gap ON conversation_messages
WHEN (OLD.visible_digest IS NOT NEW.visible_digest OR OLD.visible_text_bytes IS NOT NEW.visible_text_bytes OR OLD.visible_gap IS NOT NEW.visible_gap)
 AND OLD.ordinal IS NEW.ordinal AND OLD.role IS NEW.role AND OLD.timestamp IS NEW.timestamp
 AND OLD.source_id IS NEW.source_id AND OLD.digest IS NEW.digest AND OLD.text_bytes IS NEW.text_bytes
 AND OLD.gap IS NEW.gap AND OLD.deleted IS NEW.deleted AND OLD.removed IS NEW.removed
BEGIN
 INSERT INTO archive_metadata(key,value) VALUES ('conversation_publication_revision','1')
 ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT);
 UPDATE conversation_messages SET revision=(SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key='conversation_publication_revision')
 WHERE session_id=NEW.session_id AND message_id=NEW.message_id;
END;
CREATE TRIGGER IF NOT EXISTS conversation_session_classification
AFTER UPDATE OF agent,is_automated,relationship_type ON sessions
WHEN (OLD.agent IS NOT NEW.agent OR OLD.is_automated IS NOT NEW.is_automated OR OLD.relationship_type IS NOT NEW.relationship_type)
 AND (EXISTS(SELECT 1 FROM conversation_messages WHERE session_id=NEW.id AND removed=0) OR EXISTS(SELECT 1 FROM conversation_session_changes WHERE session_id=NEW.id))
BEGIN
 INSERT INTO archive_metadata(key,value) VALUES ('conversation_publication_revision','1')
 ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT);
 INSERT INTO conversation_session_changes(session_id,revision,deleted)
 SELECT NEW.id,CAST(value AS INTEGER),(NEW.deleted_at IS NOT NULL) FROM archive_metadata WHERE key='conversation_publication_revision'
 ON CONFLICT(session_id) DO UPDATE SET revision=excluded.revision,deleted=excluded.deleted;
END;`

func attachConversationSessions(ctx context.Context, tx *sql.Tx, changes []ConversationChange) error {
	sessions := make(map[string]*ConversationSession)
	for i := range changes {
		change := &changes[i]
		if change.Deleted {
			continue
		}
		session, ok := sessions[change.SessionID]
		if !ok {
			var meta activity.SessionMeta
			if err := tx.QueryRowContext(ctx, `SELECT agent,is_automated,relationship_type='subagent' FROM sessions WHERE id=?`, change.SessionID).Scan(&meta.Agent, &meta.IsAutomated, &meta.IsSubagent); err != nil {
				return err
			}
			session = &ConversationSession{Agent: meta.Agent, Automation: meta.ActivityCategory()}
			sessions[change.SessionID] = session
		}
		change.Session = session
	}
	return nil
}
