package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SchemaVersion is the version of the DuckDB mirror schema created by
// EnsureSchema. Increment it when a non-optional DuckDB column/table is added.
const SchemaVersion = 1

const schemaVersionMetadataKey = "agentsview_schema_version"
const defaultRepairMetadataKey = "agentsview_default_repair_v1"
const usageDedupIndexMetadataKey = "agentsview_usage_dedup_index_v1"

// duckDBSchemaNotes documents intentional differences from the SQLite primary
// archive and the PostgreSQL remote mirror:
//
//   - DuckDB stores timestamps as TIMESTAMP for mirror tables; read queries
//     should cast/format them to text when scanning into db.Session/db.Message.
//   - DuckDB BOOLEAN columns scan into Go bools directly, unlike SQLite's
//     integer booleans.
//   - SQLite INTEGER PRIMARY KEY rowids are mirrored as BIGINT values because
//     DuckDB does not provide SQLite rowid/autoincrement semantics.
//   - DuckDB does not support SQLite FTS5 or PostgreSQL GIN indexes here; text
//     search optimization is handled separately from this compatibility schema.
//   - DuckDB Quack currently rejects catalogs with TIMESTAMP DEFAULT
//     current_timestamp columns, so mirror timestamp columns avoid dynamic
//     defaults and writers supply current_timestamp explicitly where needed.
const duckDBSchemaNotes = `
DuckDB stores timestamps as TIMESTAMP.
DuckDB BOOLEAN columns scan into Go bools.
SQLite INTEGER PRIMARY KEY rowids are mirrored as BIGINT values.
DuckDB does not support SQLite FTS5 or PostgreSQL GIN indexes here.
DuckDB Quack rejects TIMESTAMP DEFAULT current_timestamp columns.
`

type tableSpec struct {
	name    string
	create  string
	columns []columnSpec
	indexes []string
}

type columnSpec struct {
	name string
	def  string
}

type timestampDefaultSpec struct {
	table  string
	column string
}

var quackIncompatibleTimestampDefaults = []timestampDefaultSpec{
	{"sessions", "created_at"},
	{"secret_findings", "created_at"},
	{"starred_sessions", "created_at"},
	{"pinned_messages", "created_at"},
}

var mirrorTables = []tableSpec{
	{
		name: "sync_metadata",
		create: `CREATE TABLE IF NOT EXISTS sync_metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		columns: []columnSpec{
			{"key", "key TEXT"},
			{"value", "value TEXT NOT NULL DEFAULT ''"},
		},
	},
	{
		name: "sessions",
		create: `CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			project TEXT NOT NULL,
			machine TEXT NOT NULL DEFAULT 'local',
			agent TEXT NOT NULL DEFAULT 'claude',
			first_message TEXT,
			display_name TEXT,
			session_name TEXT,
			started_at TIMESTAMP,
			ended_at TIMESTAMP,
			message_count INTEGER NOT NULL DEFAULT 0,
			user_message_count INTEGER NOT NULL DEFAULT 0,
			file_path TEXT,
			file_size BIGINT,
			file_mtime BIGINT,
			file_inode BIGINT,
			file_device BIGINT,
			file_hash TEXT,
			local_modified_at TIMESTAMP,
			parent_session_id TEXT,
			relationship_type TEXT NOT NULL DEFAULT '',
			total_output_tokens INTEGER NOT NULL DEFAULT 0,
			peak_context_tokens INTEGER NOT NULL DEFAULT 0,
			has_total_output_tokens BOOLEAN NOT NULL DEFAULT FALSE,
			has_peak_context_tokens BOOLEAN NOT NULL DEFAULT FALSE,
			is_automated BOOLEAN NOT NULL DEFAULT FALSE,
			tool_failure_signal_count INTEGER NOT NULL DEFAULT 0,
			tool_retry_count INTEGER NOT NULL DEFAULT 0,
			edit_churn_count INTEGER NOT NULL DEFAULT 0,
			consecutive_failure_max INTEGER NOT NULL DEFAULT 0,
			outcome TEXT NOT NULL DEFAULT 'unknown',
			outcome_confidence TEXT NOT NULL DEFAULT 'low',
			ended_with_role TEXT NOT NULL DEFAULT '',
			final_failure_streak INTEGER NOT NULL DEFAULT 0,
			signals_pending_since TEXT,
			compaction_count INTEGER NOT NULL DEFAULT 0,
			mid_task_compaction_count INTEGER NOT NULL DEFAULT 0,
			context_pressure_max DOUBLE,
			health_score INTEGER,
			health_grade TEXT,
			has_tool_calls BOOLEAN NOT NULL DEFAULT FALSE,
			has_context_data BOOLEAN NOT NULL DEFAULT FALSE,
			data_version INTEGER NOT NULL DEFAULT 0,
			cwd TEXT NOT NULL DEFAULT '',
			git_branch TEXT NOT NULL DEFAULT '',
			source_session_id TEXT NOT NULL DEFAULT '',
			source_version TEXT NOT NULL DEFAULT '',
			parser_malformed_lines INTEGER NOT NULL DEFAULT 0,
			is_truncated BOOLEAN NOT NULL DEFAULT FALSE,
			deleted_at TIMESTAMP,
			created_at TIMESTAMP,
			termination_status TEXT,
			secret_leak_count INTEGER NOT NULL DEFAULT 0,
			secrets_rules_version TEXT NOT NULL DEFAULT ''
		)`,
		columns: []columnSpec{
			{"id", "id TEXT"},
			{"project", "project TEXT NOT NULL DEFAULT ''"},
			{"machine", "machine TEXT NOT NULL DEFAULT 'local'"},
			{"agent", "agent TEXT NOT NULL DEFAULT 'claude'"},
			{"first_message", "first_message TEXT"},
			{"display_name", "display_name TEXT"},
			{"session_name", "session_name TEXT"},
			{"started_at", "started_at TIMESTAMP"},
			{"ended_at", "ended_at TIMESTAMP"},
			{"message_count", "message_count INTEGER NOT NULL DEFAULT 0"},
			{"user_message_count", "user_message_count INTEGER NOT NULL DEFAULT 0"},
			{"file_path", "file_path TEXT"},
			{"file_size", "file_size BIGINT"},
			{"file_mtime", "file_mtime BIGINT"},
			{"file_inode", "file_inode BIGINT"},
			{"file_device", "file_device BIGINT"},
			{"file_hash", "file_hash TEXT"},
			{"local_modified_at", "local_modified_at TIMESTAMP"},
			{"parent_session_id", "parent_session_id TEXT"},
			{"relationship_type", "relationship_type TEXT NOT NULL DEFAULT ''"},
			{"total_output_tokens", "total_output_tokens INTEGER NOT NULL DEFAULT 0"},
			{"peak_context_tokens", "peak_context_tokens INTEGER NOT NULL DEFAULT 0"},
			{"has_total_output_tokens", "has_total_output_tokens BOOLEAN NOT NULL DEFAULT FALSE"},
			{"has_peak_context_tokens", "has_peak_context_tokens BOOLEAN NOT NULL DEFAULT FALSE"},
			{"is_automated", "is_automated BOOLEAN NOT NULL DEFAULT FALSE"},
			{"tool_failure_signal_count", "tool_failure_signal_count INTEGER NOT NULL DEFAULT 0"},
			{"tool_retry_count", "tool_retry_count INTEGER NOT NULL DEFAULT 0"},
			{"edit_churn_count", "edit_churn_count INTEGER NOT NULL DEFAULT 0"},
			{"consecutive_failure_max", "consecutive_failure_max INTEGER NOT NULL DEFAULT 0"},
			{"outcome", "outcome TEXT NOT NULL DEFAULT 'unknown'"},
			{"outcome_confidence", "outcome_confidence TEXT NOT NULL DEFAULT 'low'"},
			{"ended_with_role", "ended_with_role TEXT NOT NULL DEFAULT ''"},
			{"final_failure_streak", "final_failure_streak INTEGER NOT NULL DEFAULT 0"},
			{"signals_pending_since", "signals_pending_since TEXT"},
			{"compaction_count", "compaction_count INTEGER NOT NULL DEFAULT 0"},
			{"mid_task_compaction_count", "mid_task_compaction_count INTEGER NOT NULL DEFAULT 0"},
			{"context_pressure_max", "context_pressure_max DOUBLE"},
			{"health_score", "health_score INTEGER"},
			{"health_grade", "health_grade TEXT"},
			{"has_tool_calls", "has_tool_calls BOOLEAN NOT NULL DEFAULT FALSE"},
			{"has_context_data", "has_context_data BOOLEAN NOT NULL DEFAULT FALSE"},
			{"data_version", "data_version INTEGER NOT NULL DEFAULT 0"},
			{"cwd", "cwd TEXT NOT NULL DEFAULT ''"},
			{"git_branch", "git_branch TEXT NOT NULL DEFAULT ''"},
			{"source_session_id", "source_session_id TEXT NOT NULL DEFAULT ''"},
			{"source_version", "source_version TEXT NOT NULL DEFAULT ''"},
			{"parser_malformed_lines", "parser_malformed_lines INTEGER NOT NULL DEFAULT 0"},
			{"is_truncated", "is_truncated BOOLEAN NOT NULL DEFAULT FALSE"},
			{"deleted_at", "deleted_at TIMESTAMP"},
			{"created_at", "created_at TIMESTAMP"},
			{"termination_status", "termination_status TEXT"},
			{"secret_leak_count", "secret_leak_count INTEGER NOT NULL DEFAULT 0"},
			{"secrets_rules_version", "secrets_rules_version TEXT NOT NULL DEFAULT ''"},
		},
		indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_sessions_ended ON sessions(ended_at, id)",
			"CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project)",
			"CREATE INDEX IF NOT EXISTS idx_sessions_machine ON sessions(machine)",
			"CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_session_id)",
			"CREATE INDEX IF NOT EXISTS idx_sessions_started ON sessions(started_at)",
			"CREATE INDEX IF NOT EXISTS idx_sessions_agent ON sessions(agent)",
			"CREATE INDEX IF NOT EXISTS idx_sessions_termination_status ON sessions(termination_status)",
		},
	},
	{
		name: "messages",
		create: `CREATE TABLE IF NOT EXISTS messages (
			id BIGINT,
			session_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			thinking_text TEXT NOT NULL DEFAULT '',
			timestamp TIMESTAMP,
			has_thinking BOOLEAN NOT NULL DEFAULT FALSE,
			has_tool_use BOOLEAN NOT NULL DEFAULT FALSE,
			content_length INTEGER NOT NULL DEFAULT 0,
			is_system BOOLEAN NOT NULL DEFAULT FALSE,
			model TEXT NOT NULL DEFAULT '',
			token_usage TEXT NOT NULL DEFAULT '',
			context_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			has_context_tokens BOOLEAN NOT NULL DEFAULT FALSE,
			has_output_tokens BOOLEAN NOT NULL DEFAULT FALSE,
			claude_message_id TEXT NOT NULL DEFAULT '',
			claude_request_id TEXT NOT NULL DEFAULT '',
			source_type TEXT NOT NULL DEFAULT '',
			source_subtype TEXT NOT NULL DEFAULT '',
			source_uuid TEXT NOT NULL DEFAULT '',
			source_parent_uuid TEXT NOT NULL DEFAULT '',
			is_sidechain BOOLEAN NOT NULL DEFAULT FALSE,
			is_compact_boundary BOOLEAN NOT NULL DEFAULT FALSE,
			UNIQUE(session_id, ordinal)
		)`,
		columns: []columnSpec{
			{"id", "id BIGINT"},
			{"session_id", "session_id TEXT NOT NULL DEFAULT ''"},
			{"ordinal", "ordinal INTEGER NOT NULL DEFAULT 0"},
			{"role", "role TEXT NOT NULL DEFAULT ''"},
			{"content", "content TEXT NOT NULL DEFAULT ''"},
			{"thinking_text", "thinking_text TEXT NOT NULL DEFAULT ''"},
			{"timestamp", "timestamp TIMESTAMP"},
			{"has_thinking", "has_thinking BOOLEAN NOT NULL DEFAULT FALSE"},
			{"has_tool_use", "has_tool_use BOOLEAN NOT NULL DEFAULT FALSE"},
			{"content_length", "content_length INTEGER NOT NULL DEFAULT 0"},
			{"is_system", "is_system BOOLEAN NOT NULL DEFAULT FALSE"},
			{"model", "model TEXT NOT NULL DEFAULT ''"},
			{"token_usage", "token_usage TEXT NOT NULL DEFAULT ''"},
			{"context_tokens", "context_tokens INTEGER NOT NULL DEFAULT 0"},
			{"output_tokens", "output_tokens INTEGER NOT NULL DEFAULT 0"},
			{"has_context_tokens", "has_context_tokens BOOLEAN NOT NULL DEFAULT FALSE"},
			{"has_output_tokens", "has_output_tokens BOOLEAN NOT NULL DEFAULT FALSE"},
			{"claude_message_id", "claude_message_id TEXT NOT NULL DEFAULT ''"},
			{"claude_request_id", "claude_request_id TEXT NOT NULL DEFAULT ''"},
			{"source_type", "source_type TEXT NOT NULL DEFAULT ''"},
			{"source_subtype", "source_subtype TEXT NOT NULL DEFAULT ''"},
			{"source_uuid", "source_uuid TEXT NOT NULL DEFAULT ''"},
			{"source_parent_uuid", "source_parent_uuid TEXT NOT NULL DEFAULT ''"},
			{"is_sidechain", "is_sidechain BOOLEAN NOT NULL DEFAULT FALSE"},
			{"is_compact_boundary", "is_compact_boundary BOOLEAN NOT NULL DEFAULT FALSE"},
		},
		indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_messages_session_ordinal ON messages(session_id, ordinal)",
			"CREATE INDEX IF NOT EXISTS idx_messages_session_role ON messages(session_id, role)",
			"CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages(timestamp)",
		},
	},
	{
		name: "usage_events",
		create: `CREATE TABLE IF NOT EXISTS usage_events (
			id BIGINT,
			session_id TEXT NOT NULL,
			message_ordinal INTEGER,
			source TEXT NOT NULL,
			model TEXT NOT NULL,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			cost_usd DOUBLE,
			cost_status TEXT NOT NULL DEFAULT '',
			cost_source TEXT NOT NULL DEFAULT '',
			occurred_at TIMESTAMP,
			dedup_key TEXT NOT NULL DEFAULT ''
		)`,
		columns: []columnSpec{
			{"id", "id BIGINT"},
			{"session_id", "session_id TEXT NOT NULL DEFAULT ''"},
			{"message_ordinal", "message_ordinal INTEGER"},
			{"source", "source TEXT NOT NULL DEFAULT ''"},
			{"model", "model TEXT NOT NULL DEFAULT ''"},
			{"input_tokens", "input_tokens INTEGER NOT NULL DEFAULT 0"},
			{"output_tokens", "output_tokens INTEGER NOT NULL DEFAULT 0"},
			{"cache_creation_input_tokens", "cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0"},
			{"cache_read_input_tokens", "cache_read_input_tokens INTEGER NOT NULL DEFAULT 0"},
			{"reasoning_tokens", "reasoning_tokens INTEGER NOT NULL DEFAULT 0"},
			{"cost_usd", "cost_usd DOUBLE"},
			{"cost_status", "cost_status TEXT NOT NULL DEFAULT ''"},
			{"cost_source", "cost_source TEXT NOT NULL DEFAULT ''"},
			{"occurred_at", "occurred_at TIMESTAMP"},
			{"dedup_key", "dedup_key TEXT NOT NULL DEFAULT ''"},
		},
		indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_usage_events_session ON usage_events(session_id)",
			"CREATE INDEX IF NOT EXISTS idx_usage_events_occurred ON usage_events(occurred_at)",
			"CREATE INDEX IF NOT EXISTS idx_usage_events_dedup ON usage_events(session_id, source, dedup_key)",
		},
	},
	{
		name: "model_pricing",
		create: `CREATE TABLE IF NOT EXISTS model_pricing (
			model_pattern TEXT PRIMARY KEY,
			input_per_mtok DOUBLE NOT NULL DEFAULT 0,
			output_per_mtok DOUBLE NOT NULL DEFAULT 0,
			cache_creation_per_mtok DOUBLE NOT NULL DEFAULT 0,
			cache_read_per_mtok DOUBLE NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT ''
		)`,
		columns: []columnSpec{
			{"model_pattern", "model_pattern TEXT"},
			{"input_per_mtok", "input_per_mtok DOUBLE NOT NULL DEFAULT 0"},
			{"output_per_mtok", "output_per_mtok DOUBLE NOT NULL DEFAULT 0"},
			{"cache_creation_per_mtok", "cache_creation_per_mtok DOUBLE NOT NULL DEFAULT 0"},
			{"cache_read_per_mtok", "cache_read_per_mtok DOUBLE NOT NULL DEFAULT 0"},
			{"updated_at", "updated_at TEXT NOT NULL DEFAULT ''"},
		},
	},
	{
		name: "tool_calls",
		create: `CREATE TABLE IF NOT EXISTS tool_calls (
			id BIGINT,
			message_id BIGINT,
			session_id TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			category TEXT NOT NULL,
			call_index INTEGER NOT NULL DEFAULT 0,
			tool_use_id TEXT NOT NULL DEFAULT '',
			input_json TEXT,
			skill_name TEXT,
			result_content_length INTEGER,
			result_content TEXT,
			subagent_session_id TEXT
		)`,
		columns: []columnSpec{
			{"id", "id BIGINT"},
			{"message_id", "message_id BIGINT"},
			{"session_id", "session_id TEXT NOT NULL DEFAULT ''"},
			{"tool_name", "tool_name TEXT NOT NULL DEFAULT ''"},
			{"category", "category TEXT NOT NULL DEFAULT ''"},
			{"call_index", "call_index INTEGER NOT NULL DEFAULT 0"},
			{"tool_use_id", "tool_use_id TEXT NOT NULL DEFAULT ''"},
			{"input_json", "input_json TEXT"},
			{"skill_name", "skill_name TEXT"},
			{"result_content_length", "result_content_length INTEGER"},
			{"result_content", "result_content TEXT"},
			{"subagent_session_id", "subagent_session_id TEXT"},
		},
		indexes: []string{
			"CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_calls_dedup ON tool_calls(session_id, message_id, call_index)",
			"CREATE INDEX IF NOT EXISTS idx_tool_calls_session ON tool_calls(session_id)",
			"CREATE INDEX IF NOT EXISTS idx_tool_calls_message ON tool_calls(message_id)",
			"CREATE INDEX IF NOT EXISTS idx_tool_calls_category ON tool_calls(category)",
		},
	},
	{
		name: "tool_result_events",
		create: `CREATE TABLE IF NOT EXISTS tool_result_events (
			id BIGINT,
			session_id TEXT NOT NULL,
			tool_call_message_ordinal INTEGER NOT NULL,
			call_index INTEGER NOT NULL DEFAULT 0,
			tool_use_id TEXT,
			agent_id TEXT,
			subagent_session_id TEXT,
			source TEXT NOT NULL,
			status TEXT NOT NULL,
			content TEXT NOT NULL,
			content_length INTEGER NOT NULL DEFAULT 0,
			timestamp TIMESTAMP,
			event_index INTEGER NOT NULL DEFAULT 0
		)`,
		columns: []columnSpec{
			{"id", "id BIGINT"},
			{"session_id", "session_id TEXT NOT NULL DEFAULT ''"},
			{"tool_call_message_ordinal", "tool_call_message_ordinal INTEGER NOT NULL DEFAULT 0"},
			{"call_index", "call_index INTEGER NOT NULL DEFAULT 0"},
			{"tool_use_id", "tool_use_id TEXT"},
			{"agent_id", "agent_id TEXT"},
			{"subagent_session_id", "subagent_session_id TEXT"},
			{"source", "source TEXT NOT NULL DEFAULT ''"},
			{"status", "status TEXT NOT NULL DEFAULT ''"},
			{"content", "content TEXT NOT NULL DEFAULT ''"},
			{"content_length", "content_length INTEGER NOT NULL DEFAULT 0"},
			{"timestamp", "timestamp TIMESTAMP"},
			{"event_index", "event_index INTEGER NOT NULL DEFAULT 0"},
		},
		indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_tool_result_events_session ON tool_result_events(session_id)",
			"CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_result_events_dedup ON tool_result_events(session_id, tool_call_message_ordinal, call_index, event_index)",
		},
	},
	{
		name: "secret_findings",
		create: `CREATE TABLE IF NOT EXISTS secret_findings (
			id BIGINT,
			session_id TEXT NOT NULL,
			rule_name TEXT NOT NULL,
			confidence TEXT NOT NULL,
			location_kind TEXT NOT NULL,
			message_ordinal INTEGER NOT NULL,
			call_index INTEGER,
			event_index INTEGER,
			match_start INTEGER NOT NULL,
			match_end INTEGER NOT NULL,
			match_index INTEGER NOT NULL,
			redacted_match TEXT NOT NULL,
			rules_version TEXT NOT NULL,
			created_at TIMESTAMP
		)`,
		columns: []columnSpec{
			{"id", "id BIGINT"},
			{"session_id", "session_id TEXT NOT NULL DEFAULT ''"},
			{"rule_name", "rule_name TEXT NOT NULL DEFAULT ''"},
			{"confidence", "confidence TEXT NOT NULL DEFAULT ''"},
			{"location_kind", "location_kind TEXT NOT NULL DEFAULT ''"},
			{"message_ordinal", "message_ordinal INTEGER NOT NULL DEFAULT 0"},
			{"call_index", "call_index INTEGER"},
			{"event_index", "event_index INTEGER"},
			{"match_start", "match_start INTEGER NOT NULL DEFAULT 0"},
			{"match_end", "match_end INTEGER NOT NULL DEFAULT 0"},
			{"match_index", "match_index INTEGER NOT NULL DEFAULT 0"},
			{"redacted_match", "redacted_match TEXT NOT NULL DEFAULT ''"},
			{"rules_version", "rules_version TEXT NOT NULL DEFAULT ''"},
			{"created_at", "created_at TIMESTAMP"},
		},
		indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_secret_findings_session ON secret_findings(session_id)",
			"CREATE INDEX IF NOT EXISTS idx_secret_findings_rule ON secret_findings(rule_name)",
		},
	},
	{
		name: "starred_sessions",
		create: `CREATE TABLE IF NOT EXISTS starred_sessions (
			session_id TEXT PRIMARY KEY,
			created_at TIMESTAMP
		)`,
		columns: []columnSpec{
			{"session_id", "session_id TEXT"},
			{"created_at", "created_at TIMESTAMP"},
		},
	},
	{
		name: "pinned_messages",
		create: `CREATE TABLE IF NOT EXISTS pinned_messages (
			id BIGINT,
			session_id TEXT NOT NULL,
			message_id BIGINT NOT NULL,
			ordinal INTEGER NOT NULL,
			source_uuid TEXT NOT NULL DEFAULT '',
			note TEXT,
			created_at TIMESTAMP,
			UNIQUE(session_id, message_id)
		)`,
		columns: []columnSpec{
			{"id", "id BIGINT"},
			{"session_id", "session_id TEXT NOT NULL DEFAULT ''"},
			{"message_id", "message_id BIGINT NOT NULL DEFAULT 0"},
			{"ordinal", "ordinal INTEGER NOT NULL DEFAULT 0"},
			{"source_uuid", "source_uuid TEXT NOT NULL DEFAULT ''"},
			{"note", "note TEXT"},
			{"created_at", "created_at TIMESTAMP"},
		},
		indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_pinned_session ON pinned_messages(session_id)",
			"CREATE INDEX IF NOT EXISTS idx_pinned_message ON pinned_messages(message_id)",
			"CREATE INDEX IF NOT EXISTS idx_pinned_created ON pinned_messages(created_at)",
		},
	},
}

// EnsureSchema creates and additively migrates the DuckDB mirror schema.
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	for _, table := range mirrorTables {
		if _, err := db.ExecContext(ctx, table.create); err != nil {
			return fmt.Errorf("creating duckdb table %s: %w", table.name, err)
		}
	}

	existing, err := loadColumns(ctx, db)
	if err != nil {
		return err
	}
	defaultRepairDone, err := metadataKeyExists(ctx, db, defaultRepairMetadataKey)
	if err != nil {
		return err
	}
	for _, table := range mirrorTables {
		have := existing[table.name]
		if have == nil {
			have = make(map[string]bool)
			existing[table.name] = have
		}
		for _, column := range table.columns {
			added := false
			if !have[column.name] {
				stmt := fmt.Sprintf(
					"ALTER TABLE %s ADD COLUMN %s",
					table.name, relaxedColumnDef(column.def),
				)
				if _, err := db.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf(
						"adding duckdb column %s.%s: %w",
						table.name, column.name, err,
					)
				}
				have[column.name] = true
				added = true
			}
			if added || !defaultRepairDone {
				if err := backfillAddedColumnDefault(ctx, db, table.name, column); err != nil {
					return err
				}
			}
		}
	}

	if err := migrateMessagesIDPrimaryKey(ctx, db); err != nil {
		return err
	}

	if err := dropQuackIncompatibleTimestampDefaults(ctx, db); err != nil {
		return err
	}
	recordUsageDedupIndexMigration, err := migrateUsageEventsDedupIndex(ctx, db)
	if err != nil {
		return err
	}

	for _, table := range mirrorTables {
		for _, stmt := range table.indexes {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("creating duckdb index for %s: %w", table.name, err)
			}
		}
	}

	if !defaultRepairDone {
		if err := recordMetadataKey(ctx, db, defaultRepairMetadataKey, "1"); err != nil {
			return err
		}
	}
	if recordUsageDedupIndexMigration {
		if err := recordMetadataKey(ctx, db, usageDedupIndexMetadataKey, "1"); err != nil {
			return err
		}
	}
	if err := recordMetadataKey(
		ctx, db, schemaVersionMetadataKey, strconv.Itoa(SchemaVersion),
	); err != nil {
		return fmt.Errorf("recording duckdb schema version: %w", err)
	}
	return nil
}

func migrateUsageEventsDedupIndex(ctx context.Context, db *sql.DB) (bool, error) {
	done, err := metadataKeyExists(ctx, db, usageDedupIndexMetadataKey)
	if err != nil {
		return false, err
	}
	if done {
		return false, nil
	}
	if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_usage_events_dedup`); err != nil {
		return false, fmt.Errorf("dropping duckdb usage_events dedup index: %w", err)
	}
	return true, nil
}

func migrateMessagesIDPrimaryKey(ctx context.Context, db *sql.DB) error {
	hasPrimary, err := tableHasPrimaryKey(ctx, db, "messages")
	if err != nil {
		return err
	}
	if !hasPrimary {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE messages RENAME TO messages_with_id_pk`); err != nil {
		return fmt.Errorf("renaming duckdb messages table for rekey: %w", err)
	}
	create := mirrorTableCreate("messages")
	cols := mirrorTableColumns("messages")
	if create == "" || len(cols) == 0 {
		return fmt.Errorf("missing duckdb messages table spec")
	}
	if _, err := db.ExecContext(ctx, create); err != nil {
		return fmt.Errorf("creating rekeyed duckdb messages table: %w", err)
	}
	colList := strings.Join(cols, ", ")
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO messages (%[1]s) SELECT %[1]s FROM messages_with_id_pk`,
		colList,
	)); err != nil {
		return fmt.Errorf("copying rekeyed duckdb messages: %w", err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE messages_with_id_pk`); err != nil {
		return fmt.Errorf("dropping old duckdb messages table: %w", err)
	}
	return nil
}

func tableHasPrimaryKey(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM information_schema.table_constraints
		WHERE table_schema = current_schema()
		  AND table_name = ?
		  AND constraint_type = 'PRIMARY KEY'`,
		strings.ToLower(table),
	).Scan(&count); err != nil {
		return false, fmt.Errorf("checking duckdb primary key for %s: %w", table, err)
	}
	return count > 0, nil
}

func mirrorTableCreate(name string) string {
	for _, table := range mirrorTables {
		if table.name == name {
			return table.create
		}
	}
	return ""
}

func mirrorTableColumns(name string) []string {
	for _, table := range mirrorTables {
		if table.name != name {
			continue
		}
		cols := make([]string, len(table.columns))
		for i, column := range table.columns {
			cols[i] = column.name
		}
		return cols
	}
	return nil
}

func metadataKeyExists(ctx context.Context, db *sql.DB, key string) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) > 0 FROM sync_metadata WHERE key = ?`,
		key,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("checking duckdb metadata key %s: %w", key, err)
	}
	return exists, nil
}

func recordMetadataKey(
	ctx context.Context, db *sql.DB, key string, value string,
) error {
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sync_metadata (key, value)
		VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	); err != nil {
		return fmt.Errorf("recording duckdb metadata key %s: %w", key, err)
	}
	return nil
}

func dropQuackIncompatibleTimestampDefaults(ctx context.Context, db *sql.DB) error {
	for _, spec := range quackIncompatibleTimestampDefaults {
		defaultValue, err := columnDefault(ctx, db, spec.table, spec.column)
		if err != nil {
			return err
		}
		if !strings.Contains(strings.ToLower(defaultValue), "current_timestamp") {
			continue
		}
		stmt := fmt.Sprintf(
			"ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT",
			spec.table,
			spec.column,
		)
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf(
				"dropping quack-incompatible duckdb default %s.%s: %w",
				spec.table,
				spec.column,
				err,
			)
		}
	}
	return nil
}

func columnDefault(
	ctx context.Context, db *sql.DB, table, column string,
) (string, error) {
	var defaultValue sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT column_default
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND lower(table_name) = ?
		  AND lower(column_name) = ?`,
		strings.ToLower(table),
		strings.ToLower(column),
	).Scan(&defaultValue)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf(
			"checking duckdb column default %s.%s: %w",
			table,
			column,
			err,
		)
	}
	if !defaultValue.Valid {
		return "", nil
	}
	return defaultValue.String, nil
}

func relaxedColumnDef(def string) string {
	def = strings.ReplaceAll(def, " NOT NULL", "")
	if i := strings.Index(def, " DEFAULT "); i >= 0 {
		def = def[:i]
	}
	return def
}

func backfillAddedColumnDefault(
	ctx context.Context, db *sql.DB, table string, column columnSpec,
) error {
	defaultValue, ok := columnDefaultLiteral(column.def)
	if !ok {
		return nil
	}
	stmt := fmt.Sprintf(
		"UPDATE %s SET %s = %s WHERE %s IS NULL",
		table, column.name, defaultValue, column.name,
	)
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf(
			"backfilling duckdb column %s.%s default: %w",
			table, column.name, err,
		)
	}
	return nil
}

func columnDefaultLiteral(def string) (string, bool) {
	idx := strings.Index(strings.ToUpper(def), " DEFAULT ")
	if idx < 0 {
		return "", false
	}
	value := strings.TrimSpace(def[idx+len(" DEFAULT "):])
	if value == "" || strings.Contains(strings.ToLower(value), "current_timestamp") {
		return "", false
	}
	return value, true
}

// CheckSchemaCompat verifies that the DuckDB mirror has the required
// read/push tables and columns. It does not mutate the database.
func CheckSchemaCompat(ctx context.Context, db *sql.DB) error {
	existing, err := loadColumns(ctx, db)
	if err != nil {
		return err
	}
	var missing []string
	for _, table := range mirrorTables {
		have, ok := existing[table.name]
		if !ok || len(have) == 0 {
			missing = append(missing, "missing table "+table.name)
			continue
		}
		for _, column := range table.columns {
			if !have[column.name] {
				missing = append(missing, table.name+"."+column.name)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf(
			"duckdb schema incompatible; run agentsview duckdb push to migrate; missing: %s",
			strings.Join(missing, ", "),
		)
	}

	var version string
	err = db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		schemaVersionMetadataKey,
	).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(
			"duckdb schema incompatible; missing %s in sync_metadata",
			schemaVersionMetadataKey,
		)
	}
	if err != nil {
		return fmt.Errorf("checking duckdb schema version: %w", err)
	}
	got, err := strconv.Atoi(version)
	if err != nil {
		return fmt.Errorf(
			"duckdb schema incompatible; invalid schema version %q",
			version,
		)
	}
	if got < SchemaVersion {
		return fmt.Errorf(
			"duckdb schema incompatible; version %d is older than required %d",
			got, SchemaVersion,
		)
	}
	return nil
}

func loadColumns(ctx context.Context, db *sql.DB) (map[string]map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT lower(table_name), lower(column_name)
		FROM information_schema.columns
		WHERE table_schema = current_schema()`)
	if err != nil {
		return nil, fmt.Errorf("loading duckdb schema columns: %w", err)
	}
	defer rows.Close()

	columns := make(map[string]map[string]bool)
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return nil, fmt.Errorf("scanning duckdb schema columns: %w", err)
		}
		if columns[table] == nil {
			columns[table] = make(map[string]bool)
		}
		columns[table][column] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb schema columns: %w", err)
	}
	return columns, nil
}
