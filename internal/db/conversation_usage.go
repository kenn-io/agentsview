package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// conversationUsageRevisionKey counts message usage changes that publish no
// conversation revision, such as token usage a later sync adds to a message
// whose text is unchanged. Each marked message row takes a fresh value, so a
// change stream can resume between any two of them.
const conversationUsageRevisionKey = "conversation_usage_revision"

// conversationUsageKeyExpr fingerprints the stored columns that decide one
// message row's own usage: its tokens, how they are priced, and the identity
// fields that deduplicate them.
func conversationUsageKeyExpr(alias string) string {
	return strings.Join([]string{
		alias + ".token_usage", alias + ".model", alias + ".provider_id",
		"COALESCE(" + alias + ".timestamp,'')", alias + ".claude_message_id",
		alias + ".claude_request_id", alias + ".source_uuid",
	}, "||char(31)||")
}

// A live projection row whose fingerprint was never recorded only takes one;
// that happens when the projection row is written before its message row in
// the same transaction, which already publishes a conversation revision.
const conversationUsageMarkSQL = `
 INSERT INTO archive_metadata(key,value) SELECT 'conversation_usage_revision','1'
  WHERE EXISTS(SELECT 1 FROM conversation_messages WHERE session_id={{session}} AND ordinal={{ordinal}} AND removed=0
   AND usage_key IS NOT NULL AND usage_key IS NOT ({{key}}))
 ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+1;
 UPDATE conversation_messages SET usage_revision=CASE WHEN usage_key IS NULL THEN usage_revision
   ELSE (SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key='conversation_usage_revision') END,
  usage_key=({{key}})
  WHERE session_id={{session}} AND ordinal={{ordinal}} AND removed=0 AND usage_key IS NOT ({{key}});`

// Usage events keep the unchanged prefix of a session's events, so a write
// reaches only the events that changed and those after them.
const conversationUsageEventMarkSQL = `
 INSERT INTO archive_metadata(key,value) SELECT 'conversation_usage_revision','1'
  WHERE EXISTS(SELECT 1 FROM conversation_messages WHERE session_id={{session}} AND ordinal={{ordinal}} AND removed=0)
 ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+1;
 UPDATE conversation_messages SET usage_revision=(SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key='conversation_usage_revision')
  WHERE session_id={{session}} AND ordinal={{ordinal}} AND removed=0;`

func conversationUsageSchemaSQL() string {
	fill := func(body, row, ordinal string) string {
		return strings.NewReplacer("{{session}}", row+".session_id", "{{ordinal}}", row+"."+ordinal,
			"{{key}}", conversationUsageKeyExpr(row)).Replace(body)
	}
	var sb strings.Builder
	trigger := func(name, event, table, when, body string) {
		sb.WriteString("DROP TRIGGER IF EXISTS " + name + ";\nCREATE TRIGGER " + name + " AFTER " + event + " ON " + table)
		if when != "" {
			sb.WriteString(" WHEN " + when)
		}
		sb.WriteString("\nBEGIN" + body + "\nEND;\n")
	}
	trigger("conversation_usage_message_insert", "INSERT", "messages", "", fill(conversationUsageMarkSQL, "NEW", "ordinal"))
	trigger("conversation_usage_message_update",
		"UPDATE OF token_usage,model,provider_id,timestamp,claude_message_id,claude_request_id,source_uuid", "messages", "",
		fill(conversationUsageMarkSQL, "NEW", "ordinal"))
	trigger("conversation_usage_event_insert", "INSERT", "usage_events", "NEW.message_ordinal IS NOT NULL",
		fill(conversationUsageEventMarkSQL, "NEW", "message_ordinal"))
	trigger("conversation_usage_event_delete", "DELETE", "usage_events", "OLD.message_ordinal IS NOT NULL",
		fill(conversationUsageEventMarkSQL, "OLD", "message_ordinal"))
	trigger("conversation_usage_event_update_old", "UPDATE", "usage_events", "OLD.message_ordinal IS NOT NULL",
		fill(conversationUsageEventMarkSQL, "OLD", "message_ordinal"))
	trigger("conversation_usage_event_update_new", "UPDATE", "usage_events",
		"NEW.message_ordinal IS NOT NULL AND (OLD.session_id IS NOT NEW.session_id OR OLD.message_ordinal IS NOT NEW.message_ordinal)",
		fill(conversationUsageEventMarkSQL, "NEW", "message_ordinal"))
	// A projection row that starts at, or moves to, an ordinal takes that
	// message's fingerprint without a usage revision; the row itself
	// publishes a conversation revision.
	initKey := `
 UPDATE conversation_messages SET usage_key=(SELECT ` + conversationUsageKeyExpr("m") + ` FROM messages m
  WHERE m.session_id=NEW.session_id AND m.ordinal=NEW.ordinal)
  WHERE session_id=NEW.session_id AND message_id=NEW.message_id;`
	trigger("conversation_usage_key_insert", "INSERT", "conversation_messages", "NEW.removed=0", initKey)
	trigger("conversation_usage_key_move", "UPDATE OF ordinal,removed", "conversation_messages",
		"NEW.removed=0 AND (OLD.ordinal IS NOT NEW.ordinal OR OLD.removed IS NOT NEW.removed)", initKey)
	sb.WriteString(`CREATE INDEX IF NOT EXISTS idx_conversation_messages_usage_revision
 ON conversation_messages(usage_revision) WHERE usage_revision > 0;`)
	return sb.String()
}

var conversationUsageSchema = conversationUsageSchemaSQL()

// ensureConversationUsageSchema adds per-message usage revisions. Existing
// live rows take their current fingerprint once, so the first later usage
// change is published rather than silently recorded. It also journals sessions
// that predate the usage change journal, so a change stream replay sends them.
func ensureConversationUsageSchema(ctx context.Context, tx *sql.Tx) error {
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('conversation_messages') WHERE name='usage_key')`).Scan(&present); err != nil {
		return err
	}
	if err := applyColumnMigrations([]schemaColumnMigration{
		{"conversation_messages", "usage_key", "ALTER TABLE conversation_messages ADD COLUMN usage_key TEXT"},
		{"conversation_messages", "usage_revision", "ALTER TABLE conversation_messages ADD COLUMN usage_revision INTEGER NOT NULL DEFAULT 0"},
	}, func(query string, args ...any) rowScanner { return tx.QueryRowContext(ctx, query, args...) },
		func(query string, args ...any) (sql.Result, error) { return tx.ExecContext(ctx, query, args...) }, nil); err != nil {
		return err
	}
	if !present {
		if _, err := tx.ExecContext(ctx, `UPDATE conversation_messages SET usage_key=(SELECT `+conversationUsageKeyExpr("m")+`
		 FROM messages m WHERE m.session_id=conversation_messages.session_id AND m.ordinal=conversation_messages.ordinal)
		 WHERE removed=0`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, conversationUsageSchema); err != nil {
		return err
	}
	return backfillUsageSessionChangesTx(ctx, tx)
}

// backfillUsageSessionChangesTx journals each session the usage change journal
// has never recorded. The journal started in v0.47.0 without a backfill, so a
// session written earlier and untouched since has no row. A change stream
// replay would then find that session only through its projected messages, and
// a session without any would be missing. Each session takes its own revision
// so the stream can page between any two of them. Every later session write
// journals the session, so later opens find nothing to add.
func backfillUsageSessionChangesTx(ctx context.Context, tx *sql.Tx) error {
	var base int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key=?),0)`,
		archiveMetadataUsageChangeRevisionKey).Scan(&base); err != nil {
		return fmt.Errorf("reading usage change revision: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO usage_session_changes(session_id,revision)
	 SELECT id,?+ROW_NUMBER() OVER (ORDER BY id) FROM sessions s
	 WHERE NOT EXISTS(SELECT 1 FROM usage_session_changes u WHERE u.session_id=s.id)`, base)
	if err != nil {
		return fmt.Errorf("journaling unrecorded sessions: %w", err)
	}
	added, err := result.RowsAffected()
	if err != nil || added == 0 {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES(?,?)
	 ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		archiveMetadataUsageChangeRevisionKey, strconv.FormatInt(base+added, 10))
	if err != nil {
		return fmt.Errorf("advancing usage change revision: %w", err)
	}
	return nil
}
