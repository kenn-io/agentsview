package db

import (
	"context"
	"fmt"
	"strings"
)

const (
	archiveMetadataUsageChangeRevisionKey     = "usage_change_revision"
	archiveMetadataUsagePricingRevisionKey    = "usage_pricing_revision"
	archiveMetadataAnalyticsChangeRevisionKey = "analytics_change_revision"
	archiveMetadataSearchChangeRevisionKey    = "search_change_revision"
)

const searchTokenSQL = `
 INSERT INTO archive_metadata (key, value) VALUES ('search_history_token', lower(hex(randomblob(16))))
 ON CONFLICT(key) DO UPDATE SET value = excluded.value;`

const counterTemplate = `
 INSERT INTO archive_metadata (key, value)
  VALUES ('{{counter}}', '1')
 ON CONFLICT(key) DO UPDATE SET
  value = CAST(value AS INTEGER) + 1,
  updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');`

// Full replacements need this when only derived dialogue changes: transcript
// equality ignores dialogue, and the replacement does not fire UPDATE triggers.
func bumpSearchChangeRevisionTx(tx transactionQueries) error {
	if _, err := tx.Exec(strings.ReplaceAll(counterTemplate, "{{counter}}", archiveMetadataSearchChangeRevisionKey)); err != nil {
		return fmt.Errorf("advancing search change revision: %w", err)
	}
	return nil
}

// Replace triggers atomically with column migrations.
var usageChangeJournalTriggerDropsSQL, usageChangeJournalTriggerSQL = usageChangeJournalSQL()

func usageChangeJournalSQL() (string, string) {
	counterBody := strings.ReplaceAll(counterTemplate, "{{counter}}", archiveMetadataUsageChangeRevisionKey)
	analyticsCounterBody := strings.ReplaceAll(counterTemplate, "{{counter}}", archiveMetadataAnalyticsChangeRevisionKey)
	searchCounterBody := strings.ReplaceAll(counterTemplate, "{{counter}}", archiveMetadataSearchChangeRevisionKey)
	const pricingRevisionBody = `
 INSERT INTO archive_metadata (key, value)
  VALUES ('usage_pricing_revision', (
   SELECT value FROM archive_metadata WHERE key = 'usage_change_revision'))
 ON CONFLICT(key) DO UPDATE SET
  value = excluded.value,
  updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');`
	sessionBody := counterBody + `
 INSERT INTO usage_session_changes (session_id, revision)
  SELECT {{session_id}}, CAST(value AS INTEGER)
  FROM archive_metadata WHERE key = 'usage_change_revision'
 ON CONFLICT(session_id) DO UPDATE SET revision = excluded.revision;`
	var drops, creates strings.Builder
	creates.WriteString(`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES ('search_history_token', lower(hex(randomblob(16))));` + "\n")
	// Older writers replace journal triggers but keep these search-owned triggers.
	for _, event := range []string{"INSERT", "UPDATE"} {
		name := "trg_search_history_revision_" + strings.ToLower(event)
		fmt.Fprintf(&drops, "DROP TRIGGER IF EXISTS %s;\n", name)
		fmt.Fprintf(&creates, "CREATE TRIGGER %s AFTER %s ON archive_metadata WHEN NEW.key = 'search_change_revision' BEGIN%s\nEND;\n", name, event, searchTokenSQL)
	}
	add := func(name, event, table, condition, sessionID string, analytics bool) {
		fmt.Fprintf(&drops, "DROP TRIGGER IF EXISTS %s;\n", name)
		body := counterBody
		if analytics {
			body = analyticsCounterBody
		}
		if sessionID != "" {
			body = strings.ReplaceAll(sessionBody, "{{session_id}}", sessionID)
		}
		switch table {
		case "image_text_cache", "image_text_occurrences":
			body = searchCounterBody
		case "model_pricing", "model_pricing_bands", "genai_pricing":
			body += pricingRevisionBody
		}
		if strings.HasPrefix(name, "trg_messages_search_dialogue") || name == "trg_sessions_search_first_message" {
			body = searchCounterBody
		}
		searchToken := table == "sessions" && strings.HasPrefix(name, "trg_sessions_usage_change_") && name != "trg_sessions_usage_change_group_label"
		if condition != "" {
			condition = "WHEN " + condition
		}
		fmt.Fprintf(&creates, "CREATE TRIGGER %s AFTER %s ON %s\n%s\nBEGIN%s\nEND;\n", name, event, table, condition, body)
		if searchToken {
			tokenTrigger := "trg_search_history_" + name
			fmt.Fprintf(&drops, "DROP TRIGGER IF EXISTS %s;\n", tokenTrigger)
			fmt.Fprintf(&creates, "CREATE TRIGGER %s AFTER %s ON %s\n%s\nBEGIN%s\nEND;\n", tokenTrigger, event, table, condition, searchTokenSQL)
		}
	}
	add("trg_sessions_usage_change_insert", "INSERT", "sessions", "", "NEW.id", false)
	columns := []string{"transcript_revision", "started_at", "deleted_at", "project", "agent", "machine", "git_branch", "group_key", "is_automated", "user_message_count", "ended_at", "termination_status", "display_name", "session_name", "relationship_type", "message_count", "created_at"}
	changes := make([]string, len(columns))
	for i, column := range columns {
		changes[i] = "OLD." + column + " IS NOT NEW." + column
	}
	add("trg_sessions_usage_change_update", "UPDATE OF "+strings.Join(columns, ", "), "sessions", strings.Join(changes, " OR "), "NEW.id", false)
	// Installed group labels come from cron run names. Older writers recreate
	// only the triggers they name, so this one survives their opens.
	add("trg_sessions_usage_change_group_label", "UPDATE OF session_name", "sessions", "OLD.session_name IS NOT NEW.session_name AND (OLD.group_key!='' OR NEW.group_key!='')", "NEW.id", false)
	add("trg_sessions_usage_change_delete", "DELETE", "sessions", "", "OLD.id", false)
	// Session or transcript revisions cover tool inserts and message metadata.
	// Tool replacements without a transcript bump delete old calls first.
	columns = []string{"first_message", "health_score", "health_grade", "outcome", "outcome_confidence", "tool_failure_signal_count", "tool_retry_count", "edit_churn_count", "compaction_count", "mid_task_compaction_count", "context_pressure_max", "quality_signal_version", "short_prompt_count", "unstructured_start", "missing_success_criteria_count", "missing_verification_count", "duplicate_prompt_count", "no_code_context_count", "runaway_tool_loop_count"}
	changes = make([]string, len(columns))
	for i, column := range columns {
		changes[i] = "OLD." + column + " IS NOT NEW." + column
	}
	add("trg_sessions_analytics_change_update", "UPDATE OF "+strings.Join(columns, ", "), "sessions", strings.Join(changes, " OR "), "", true)
	add("trg_sessions_search_first_message", "UPDATE OF first_message", "sessions", "OLD.first_message IS NOT NEW.first_message", "", false)
	add("trg_image_text_cache_search_insert", "INSERT", "image_text_cache", "", "", true)
	add("trg_image_text_cache_search_delete", "DELETE", "image_text_cache", "", "", true)
	add("trg_image_text_cache_search_update", "UPDATE OF text, succeeded", "image_text_cache", "OLD.text IS NOT NEW.text OR OLD.succeeded IS NOT NEW.succeeded", "", true)
	add("trg_image_text_occurrences_search_insert", "INSERT", "image_text_occurrences", "", "", true)
	add("trg_image_text_occurrences_search_delete", "DELETE", "image_text_occurrences", "", "", true)
	add("trg_messages_search_dialogue", "UPDATE OF dialogue_text", "messages", "OLD.dialogue_text IS NOT NEW.dialogue_text", "", false)
	add("trg_messages_analytics_change_length", "UPDATE OF content_length", "messages", "OLD.content_length IS NOT NEW.content_length", "", true)
	add("trg_tool_calls_analytics_change_delete", "DELETE", "tool_calls", "", "", true)
	columns = []string{"session_id", "message_id", "category", "tool_name", "observed_outcome", "observed_repeat", "sequence_ending"}
	changes = make([]string, len(columns))
	for i, column := range columns {
		changes[i] = "OLD." + column + " IS NOT NEW." + column
	}
	add("trg_tool_calls_analytics_change_update", "UPDATE OF "+strings.Join(columns, ", "), "tool_calls", strings.Join(changes, " OR "), "", true)
	add("trg_usage_events_usage_change_insert", "INSERT", "usage_events", "", "NEW.session_id", false)
	columns = usageEventRecordColumns
	changes = make([]string, len(columns))
	for i, column := range columns {
		changes[i] = "OLD." + column + " IS NOT NEW." + column
	}
	add("trg_usage_events_usage_change_update", "UPDATE", "usage_events", strings.Join(changes, " OR "), "NEW.session_id", false)
	add("trg_usage_events_usage_change_update_old_session", "UPDATE", "usage_events", "OLD.session_id IS NOT NEW.session_id", "OLD.session_id", false)
	add("trg_usage_events_usage_change_delete", "DELETE", "usage_events", "", "OLD.session_id", false)
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
			if event == "UPDATE" {
				condition = "(" + strings.Join(changes, " OR ") + ")"
			}
			add("trg_"+table.name+"_usage_change_"+strings.ToLower(event), event, table.name, condition, "", false)
		}
	}
	return drops.String(), creates.String()
}

// Independent source triggers survive an older writer reinstalling its journal.
var dashboardGenerationTriggerSQL = dashboardTriggerSQL()

func dashboardTriggerSQL() string {
	var sql strings.Builder
	sql.WriteString("DROP TRIGGER IF EXISTS trg_dashboard_generation_insert; DROP TRIGGER IF EXISTS trg_dashboard_generation_update;\n")
	for _, name := range []string{"common", "tools", "velocity", "signals", "scope"} {
		fmt.Fprintf(&sql, "INSERT INTO archive_metadata(key,value) VALUES('dashboard_%s',lower(hex(randomblob(16)))) ON CONFLICT(key) DO NOTHING;\n", name)
	}
	tables := []struct {
		name   string
		groups map[string][]string
	}{
		{"sessions", map[string][]string{"common": DashboardSessionInputs, "tools": DashboardToolsSessionInputs, "signals": DashboardSignalsSessionInputs}},
		{"messages", map[string][]string{"tools": append([]string{"id"}, DashboardToolsMessageInputs...), "velocity": DashboardVelocityMessageInputs, "signals": DashboardSignalsMessageInputs, "scope": DashboardScopeMessageInputs}},
		{"tool_calls", map[string][]string{"tools": append([]string{"message_id"}, DashboardToolsCallInputs...), "velocity": append([]string{"message_id"}, DashboardVelocityCallInputs...)}},
	}
	for _, table := range tables {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			name := "trg_dashboard_" + table.name + "_" + strings.ToLower(event)
			fmt.Fprintf(&sql, "DROP TRIGGER IF EXISTS %s; CREATE TRIGGER %s AFTER %s ON %s BEGIN\n", name, name, event, table.name)
			for _, group := range []string{"common", "tools", "velocity", "signals", "scope"} {
				columns, ok := table.groups[group]
				if !ok {
					continue
				}
				condition := "1"
				if event == "UPDATE" {
					changes := make([]string, len(columns))
					for i, column := range columns {
						changes[i] = "OLD." + column + " IS NOT NEW." + column
					}
					condition = strings.Join(changes, " OR ")
				}
				fmt.Fprintf(&sql, "INSERT INTO archive_metadata(key,value) SELECT 'dashboard_%s',lower(hex(randomblob(16))) WHERE %s ON CONFLICT(key) DO UPDATE SET value=excluded.value;\n", group, condition)
			}
			sql.WriteString("END;\n")
		}
	}
	return sql.String()
}

// UsageChangeRevision returns the trigger-maintained revision for usage facts,
// session filter dimensions, Activity session columns, and pricing catalog
// writes. Revision 0 means no history. Readers must pair it with database_id
// because rebuilt archives restart the counter.
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
