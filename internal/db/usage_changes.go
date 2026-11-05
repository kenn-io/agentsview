package db

import (
	"context"
	"fmt"
	"strings"
)

const archiveMetadataUsageChangeRevisionKey = "usage_change_revision"

// Replace triggers atomically with column migrations.
var usageChangeJournalTriggerDropsSQL, usageChangeJournalTriggerSQL = usageChangeJournalSQL()

func usageChangeJournalSQL() (string, string) {
	const counterBody = `
 INSERT INTO archive_metadata (key, value)
  VALUES ('usage_change_revision', '1')
 ON CONFLICT(key) DO UPDATE SET
  value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
  updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');`
	const sessionBody = counterBody + `
 INSERT INTO usage_session_changes (session_id, revision)
  SELECT {{session_id}}, CAST(value AS INTEGER)
  FROM archive_metadata WHERE key = 'usage_change_revision'
 ON CONFLICT(session_id) DO UPDATE SET revision = excluded.revision;`
	var drops, creates strings.Builder
	add := func(name, event, table, condition, sessionID string) {
		fmt.Fprintf(&drops, "DROP TRIGGER IF EXISTS %s;\n", name)
		body := counterBody
		if sessionID != "" {
			body = strings.ReplaceAll(sessionBody, "{{session_id}}", sessionID)
		}
		if condition != "" {
			condition = "WHEN " + condition
		}
		fmt.Fprintf(&creates, "CREATE TRIGGER %s AFTER %s ON %s\n%s\nBEGIN%s\nEND;\n", name, event, table, condition, body)
	}
	add("trg_sessions_usage_change_insert", "INSERT", "sessions", "", "NEW.id")
	columns := []string{"transcript_revision", "started_at", "deleted_at", "project", "agent", "machine", "git_branch", "group_key", "is_automated", "user_message_count"}
	changes := make([]string, len(columns))
	for i, column := range columns {
		changes[i] = "OLD." + column + " IS NOT NEW." + column
	}
	add("trg_sessions_usage_change_update", "UPDATE OF "+strings.Join(columns, ", "), "sessions", strings.Join(changes, " OR "), "NEW.id")
	add("trg_sessions_usage_change_delete", "DELETE", "sessions", "", "OLD.id")
	add("trg_usage_events_usage_change_insert", "INSERT", "usage_events", "", "NEW.session_id")
	add("trg_usage_events_usage_change_delete", "DELETE", "usage_events", "", "OLD.session_id")
	for _, table := range []struct {
		name    string
		columns []string
	}{
		{"model_pricing", []string{"model_pattern", "input_microdollars_per_mtok", "output_microdollars_per_mtok", "cache_creation_microdollars_per_mtok", "cache_creation_1h_microdollars_per_mtok", "cache_read_microdollars_per_mtok", "updated_at"}},
		{"model_pricing_bands", []string{"model_pattern", "above_input_tokens", "input_microdollars_per_mtok", "output_microdollars_per_mtok", "cache_creation_microdollars_per_mtok", "cache_creation_1h_microdollars_per_mtok", "cache_read_microdollars_per_mtok", "updated_at"}},
		{"genai_pricing", []string{"singleton", "version", "source_ref", "source", "data_json", "updated_at"}},
	} {
		changes := make([]string, len(table.columns))
		for i, column := range table.columns {
			changes[i] = "OLD." + column + " IS NOT NEW." + column
		}
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			condition := ""
			if table.name != "genai_pricing" {
				// Match isPricingMetaPattern and the pricing queries' literal underscore prefix.
				const oldModel = `OLD.model_pattern NOT LIKE '\_%' ESCAPE '\'`
				const newModel = `NEW.model_pattern NOT LIKE '\_%' ESCAPE '\'`
				switch event {
				case "INSERT":
					condition = newModel
				case "DELETE":
					condition = oldModel
				case "UPDATE":
					condition = "(" + oldModel + " OR " + newModel + ") AND "
				}
			}
			if event == "UPDATE" {
				condition += "(" + strings.Join(changes, " OR ") + ")"
			}
			add("trg_"+table.name+"_usage_change_"+strings.ToLower(event), event, table.name, condition, "")
		}
	}
	return drops.String(), creates.String()
}

// UsageChangeRevision returns the trigger-maintained revision for usage facts,
// session filter dimensions, and pricing catalog writes. Revision 0 means no
// history. Readers must pair it with database_id because rebuilt archives
// restart the counter.
func (db *DB) UsageChangeRevision(ctx context.Context) (int64, error) {
	return db.archiveJournalRevision(ctx, archiveMetadataUsageChangeRevisionKey, "usage change")
}

// LoadUsageChanges returns the IDs with a latest usage change in
// (afterRevision, throughRevision], including hard-deleted sessions.
// Pricing writes advance the revision without journaling a session.
// A window with afterRevision greater than throughRevision is rejected.
func (db *DB) LoadUsageChanges(ctx context.Context, afterRevision, throughRevision int64) ([]string, error) {
	return db.loadSessionJournalChanges(ctx, "usage_session_changes", "usage", afterRevision, throughRevision)
}
