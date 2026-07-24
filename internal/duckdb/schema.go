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
// createSchema. The mirror schema is create-only: there are no in-place
// migrations between versions. A version mismatch means the mirror file
// must be rebuilt with 'agentsview duckdb push --full'. v6 adds
// sessions.source_archive_id and the source_worktree_project_mappings
// mirror table on top of the v5 deletion-cause shape.
const SchemaVersion = 6

const schemaVersionMetadataKey = "agentsview_schema_version"

// Mirror metadata keys recorded by writeMirrorMetadata and read back by
// readMirrorMetadata / ProbeMirror.
const (
	dataVersionMetadataKey      = "agentsview_source_data_version"
	sourceDatabaseIDMetadataKey = "agentsview_source_database_id"
	pushScopeMetadataKey        = "agentsview_push_scope"
	lastPushAtMetadataKey       = "agentsview_last_push_at"
	lastPushMachineMetadataKey  = "agentsview_last_push_machine"
	lastPushCutoffMetadataKey   = "agentsview_last_push_cutoff"
	deletionRevisionMetadataKey = "agentsview_session_deletion_revision"
	identityRevisionMetadataKey = "agentsview_project_identity_revision"
	mappingRevisionMetadataKey  = "agentsview_worktree_mapping_revision"
)

// curationFingerprintMetadataKey stores a hash of the local in-scope
// curation state (starred session ids, pinned message ids) as of the last
// push that actually refreshed starred_sessions/pinned_messages. It is read
// and written directly via readMetadataKey/recordMetadataKey rather than
// through mirrorMetadata/writeMirrorMetadata: unlike the fields in that
// struct, it is not part of the rebuild-vs-incremental decision (see
// rebuildReason in probe.go), only of the incremental curation-refresh
// skip (see refreshCurationIfChanged in push.go).
const curationFingerprintMetadataKey = "agentsview_curation_fingerprint"

// cursorUsageMaxIDMetadataKey stores the largest local cursor_usage_events
// id the mirror has consumed. The local table is append-only with a
// monotonic integer primary key, so this high-water mark lets every push
// load only appended rows instead of the full history (see
// syncCursorUsageEvents in push.go). Like the curation fingerprint, it is
// read and written directly and plays no part in the rebuild-vs-incremental
// decision; a fresh rebuild file has no metadata, so its first sync loads
// the full history.
const cursorUsageMaxIDMetadataKey = "agentsview_cursor_usage_max_id"

// DuckDB schema notes:
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
		name: "source_archives",
		create: `CREATE TABLE IF NOT EXISTS source_archives (
			source_archive_id TEXT PRIMARY KEY,
			source_archive_salt TEXT NOT NULL
		)`,
		columns: []columnSpec{
			{"source_archive_id", "source_archive_id TEXT"},
			{"source_archive_salt", "source_archive_salt TEXT NOT NULL DEFAULT ''"},
		},
	},
	{
		name: "sessions",
		create: `CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			project TEXT NOT NULL,
			machine TEXT NOT NULL DEFAULT 'local',
			agent TEXT NOT NULL DEFAULT 'claude',
			agent_label TEXT NOT NULL DEFAULT '',
			entrypoint TEXT NOT NULL DEFAULT '',
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
			transcript_revision TEXT NOT NULL DEFAULT '0',
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
			quality_signal_version INTEGER NOT NULL DEFAULT 0,
			short_prompt_count INTEGER NOT NULL DEFAULT 0,
			unstructured_start BOOLEAN NOT NULL DEFAULT FALSE,
			missing_success_criteria_count INTEGER NOT NULL DEFAULT 0,
			missing_verification_count INTEGER NOT NULL DEFAULT 0,
			duplicate_prompt_count INTEGER NOT NULL DEFAULT 0,
			no_code_context_count INTEGER NOT NULL DEFAULT 0,
			runaway_tool_loop_count INTEGER NOT NULL DEFAULT 0,
			data_version INTEGER NOT NULL DEFAULT 0,
			cwd TEXT NOT NULL DEFAULT '',
			git_branch TEXT NOT NULL DEFAULT '',
			source_session_id TEXT NOT NULL DEFAULT '',
			source_version TEXT NOT NULL DEFAULT '',
			transcript_fidelity TEXT NOT NULL DEFAULT '',
			parser_malformed_lines INTEGER NOT NULL DEFAULT 0,
			is_truncated BOOLEAN NOT NULL DEFAULT FALSE,
			deleted_at TIMESTAMP,
			deletion_cause TEXT,
			created_at TIMESTAMP,
			termination_status TEXT,
			secret_leak_count INTEGER NOT NULL DEFAULT 0,
			secrets_rules_version TEXT NOT NULL DEFAULT '',
			agentsview_push_fingerprint TEXT,
			source_archive_id TEXT NOT NULL DEFAULT ''
		)`,
		columns: []columnSpec{
			{"id", "id TEXT"},
			{"project", "project TEXT NOT NULL DEFAULT ''"},
			{"machine", "machine TEXT NOT NULL DEFAULT 'local'"},
			{"agent", "agent TEXT NOT NULL DEFAULT 'claude'"},
			{"agent_label", "agent_label TEXT NOT NULL DEFAULT ''"},
			{"entrypoint", "entrypoint TEXT NOT NULL DEFAULT ''"},
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
			{"transcript_revision", "transcript_revision TEXT NOT NULL DEFAULT '0'"},
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
			{"quality_signal_version", "quality_signal_version INTEGER NOT NULL DEFAULT 0"},
			{"short_prompt_count", "short_prompt_count INTEGER NOT NULL DEFAULT 0"},
			{"unstructured_start", "unstructured_start BOOLEAN NOT NULL DEFAULT FALSE"},
			{"missing_success_criteria_count", "missing_success_criteria_count INTEGER NOT NULL DEFAULT 0"},
			{"missing_verification_count", "missing_verification_count INTEGER NOT NULL DEFAULT 0"},
			{"duplicate_prompt_count", "duplicate_prompt_count INTEGER NOT NULL DEFAULT 0"},
			{"no_code_context_count", "no_code_context_count INTEGER NOT NULL DEFAULT 0"},
			{"runaway_tool_loop_count", "runaway_tool_loop_count INTEGER NOT NULL DEFAULT 0"},
			{"data_version", "data_version INTEGER NOT NULL DEFAULT 0"},
			{"cwd", "cwd TEXT NOT NULL DEFAULT ''"},
			{"git_branch", "git_branch TEXT NOT NULL DEFAULT ''"},
			{"source_session_id", "source_session_id TEXT NOT NULL DEFAULT ''"},
			{"source_version", "source_version TEXT NOT NULL DEFAULT ''"},
			{"transcript_fidelity", "transcript_fidelity TEXT NOT NULL DEFAULT ''"},
			{"parser_malformed_lines", "parser_malformed_lines INTEGER NOT NULL DEFAULT 0"},
			{"is_truncated", "is_truncated BOOLEAN NOT NULL DEFAULT FALSE"},
			{"deleted_at", "deleted_at TIMESTAMP"},
			{"deletion_cause", "deletion_cause TEXT"},
			{"created_at", "created_at TIMESTAMP"},
			{"termination_status", "termination_status TEXT"},
			{"secret_leak_count", "secret_leak_count INTEGER NOT NULL DEFAULT 0"},
			{"secrets_rules_version", "secrets_rules_version TEXT NOT NULL DEFAULT ''"},
			{"agentsview_push_fingerprint", "agentsview_push_fingerprint TEXT"},
			{"source_archive_id", "source_archive_id TEXT NOT NULL DEFAULT ''"},
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
		name: "cursor_usage_events",
		create: `CREATE TABLE IF NOT EXISTS cursor_usage_events (
			id BIGINT,
			occurred_at TIMESTAMP NOT NULL,
			model TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			charged_cents DOUBLE NOT NULL DEFAULT 0,
			cursor_token_fee DOUBLE NOT NULL DEFAULT 0,
			user_id TEXT NOT NULL DEFAULT '',
			user_email TEXT NOT NULL DEFAULT '',
			is_headless BOOLEAN NOT NULL DEFAULT FALSE,
			dedup_key TEXT NOT NULL DEFAULT ''
		)`,
		columns: []columnSpec{
			{"id", "id BIGINT"},
			{"occurred_at", "occurred_at TIMESTAMP NOT NULL"},
			{"model", "model TEXT NOT NULL DEFAULT ''"},
			{"kind", "kind TEXT NOT NULL DEFAULT ''"},
			{"input_tokens", "input_tokens INTEGER NOT NULL DEFAULT 0"},
			{"output_tokens", "output_tokens INTEGER NOT NULL DEFAULT 0"},
			{"cache_write_tokens", "cache_write_tokens INTEGER NOT NULL DEFAULT 0"},
			{"cache_read_tokens", "cache_read_tokens INTEGER NOT NULL DEFAULT 0"},
			{"charged_cents", "charged_cents DOUBLE NOT NULL DEFAULT 0"},
			{"cursor_token_fee", "cursor_token_fee DOUBLE NOT NULL DEFAULT 0"},
			{"user_id", "user_id TEXT NOT NULL DEFAULT ''"},
			{"user_email", "user_email TEXT NOT NULL DEFAULT ''"},
			{"is_headless", "is_headless BOOLEAN NOT NULL DEFAULT FALSE"},
			{"dedup_key", "dedup_key TEXT NOT NULL DEFAULT ''"},
		},
		indexes: []string{
			"CREATE UNIQUE INDEX IF NOT EXISTS idx_cursor_usage_events_dedup ON cursor_usage_events(dedup_key)",
			"CREATE INDEX IF NOT EXISTS idx_cursor_usage_events_occurred ON cursor_usage_events(occurred_at)",
			"CREATE INDEX IF NOT EXISTS idx_cursor_usage_events_model ON cursor_usage_events(model)",
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
		name: "source_project_identity_observations",
		create: `CREATE TABLE IF NOT EXISTS source_project_identity_observations (
			source_archive_id TEXT NOT NULL DEFAULT '',
			source_archive_salt TEXT NOT NULL DEFAULT '',
			project TEXT NOT NULL,
			machine TEXT NOT NULL,
			root_path TEXT NOT NULL DEFAULT '',
			git_remote TEXT NOT NULL DEFAULT '',
			git_remote_name TEXT NOT NULL DEFAULT '',
			repository_path TEXT NOT NULL DEFAULT '',
			worktree_name TEXT NOT NULL DEFAULT '',
			worktree_root_path TEXT NOT NULL DEFAULT '',
			worktree_relationship TEXT NOT NULL DEFAULT 'unknown',
			checkout_state TEXT NOT NULL DEFAULT 'unknown',
			git_branch TEXT NOT NULL DEFAULT '',
			remote_resolution TEXT NOT NULL DEFAULT 'unknown',
			remote_candidate_count INTEGER NOT NULL DEFAULT 0,
			observed_at TIMESTAMP NOT NULL,
			normalized_remote TEXT NOT NULL DEFAULT '',
			key_source TEXT NOT NULL DEFAULT '',
			key TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (source_archive_id, project, machine, root_path, git_remote)
		)`,
		columns: []columnSpec{
			{"source_archive_id", "source_archive_id TEXT NOT NULL DEFAULT ''"},
			{"source_archive_salt", "source_archive_salt TEXT NOT NULL DEFAULT ''"},
			{"project", "project TEXT NOT NULL DEFAULT ''"},
			{"machine", "machine TEXT NOT NULL DEFAULT ''"},
			{"root_path", "root_path TEXT NOT NULL DEFAULT ''"},
			{"git_remote", "git_remote TEXT NOT NULL DEFAULT ''"},
			{"git_remote_name", "git_remote_name TEXT NOT NULL DEFAULT ''"},
			{"repository_path", "repository_path TEXT NOT NULL DEFAULT ''"},
			{"worktree_name", "worktree_name TEXT NOT NULL DEFAULT ''"},
			{"worktree_root_path", "worktree_root_path TEXT NOT NULL DEFAULT ''"},
			{"worktree_relationship", "worktree_relationship TEXT NOT NULL DEFAULT 'unknown'"},
			{"checkout_state", "checkout_state TEXT NOT NULL DEFAULT 'unknown'"},
			{"git_branch", "git_branch TEXT NOT NULL DEFAULT ''"},
			{"remote_resolution", "remote_resolution TEXT NOT NULL DEFAULT 'unknown'"},
			{"remote_candidate_count", "remote_candidate_count INTEGER NOT NULL DEFAULT 0"},
			{"observed_at", "observed_at TIMESTAMP"},
			{"normalized_remote", "normalized_remote TEXT NOT NULL DEFAULT ''"},
			{"key_source", "key_source TEXT NOT NULL DEFAULT ''"},
			{"key", "key TEXT NOT NULL DEFAULT ''"},
		},
		indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_source_project_identity_observations_project ON source_project_identity_observations(project)",
		},
	},
	{
		name: "source_session_project_identity_snapshots",
		create: `CREATE TABLE IF NOT EXISTS source_session_project_identity_snapshots (
			source_archive_id TEXT NOT NULL,
			source_database_generation TEXT NOT NULL,
			source_session_id TEXT NOT NULL,
			project TEXT NOT NULL,
			machine TEXT NOT NULL,
			root_path TEXT NOT NULL DEFAULT '',
			git_remote TEXT NOT NULL DEFAULT '',
			git_remote_name TEXT NOT NULL DEFAULT '',
			repository_path TEXT NOT NULL DEFAULT '',
			worktree_name TEXT NOT NULL DEFAULT '',
			worktree_root_path TEXT NOT NULL DEFAULT '',
			worktree_relationship TEXT NOT NULL DEFAULT 'unknown',
			checkout_state TEXT NOT NULL DEFAULT 'unknown',
			git_branch TEXT NOT NULL DEFAULT '',
			remote_resolution TEXT NOT NULL DEFAULT 'unknown',
			remote_candidate_count INTEGER NOT NULL DEFAULT 0,
			observed_at TIMESTAMP NOT NULL,
			normalized_remote TEXT NOT NULL DEFAULT '',
			key_source TEXT NOT NULL DEFAULT '',
			key TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (
				source_archive_id, source_database_generation, source_session_id
			)
		)`,
		columns: []columnSpec{
			{"source_archive_id", "source_archive_id TEXT NOT NULL DEFAULT ''"},
			{"source_database_generation", "source_database_generation TEXT NOT NULL DEFAULT ''"},
			{"source_session_id", "source_session_id TEXT NOT NULL DEFAULT ''"},
			{"project", "project TEXT NOT NULL DEFAULT ''"},
			{"machine", "machine TEXT NOT NULL DEFAULT ''"},
			{"root_path", "root_path TEXT NOT NULL DEFAULT ''"},
			{"git_remote", "git_remote TEXT NOT NULL DEFAULT ''"},
			{"git_remote_name", "git_remote_name TEXT NOT NULL DEFAULT ''"},
			{"repository_path", "repository_path TEXT NOT NULL DEFAULT ''"},
			{"worktree_name", "worktree_name TEXT NOT NULL DEFAULT ''"},
			{"worktree_root_path", "worktree_root_path TEXT NOT NULL DEFAULT ''"},
			{"worktree_relationship", "worktree_relationship TEXT NOT NULL DEFAULT 'unknown'"},
			{"checkout_state", "checkout_state TEXT NOT NULL DEFAULT 'unknown'"},
			{"git_branch", "git_branch TEXT NOT NULL DEFAULT ''"},
			{"remote_resolution", "remote_resolution TEXT NOT NULL DEFAULT 'unknown'"},
			{"remote_candidate_count", "remote_candidate_count INTEGER NOT NULL DEFAULT 0"},
			{"observed_at", "observed_at TIMESTAMP"},
			{"normalized_remote", "normalized_remote TEXT NOT NULL DEFAULT ''"},
			{"key_source", "key_source TEXT NOT NULL DEFAULT ''"},
			{"key", "key TEXT NOT NULL DEFAULT ''"},
		},
		indexes: []string{
			"CREATE INDEX IF NOT EXISTS idx_source_session_project_identity_snapshots_project ON source_session_project_identity_snapshots(source_archive_id, project)",
		},
	},
	{
		name: "source_worktree_project_mappings",
		create: `CREATE TABLE IF NOT EXISTS source_worktree_project_mappings (
			source_archive_id TEXT NOT NULL,
			machine TEXT NOT NULL,
			path_prefix TEXT NOT NULL,
			layout TEXT NOT NULL DEFAULT 'explicit',
			project TEXT NOT NULL DEFAULT '',
			original_project TEXT NOT NULL DEFAULT '',
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			updated_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (source_archive_id, machine, path_prefix)
		)`,
		columns: []columnSpec{
			{"source_archive_id", "source_archive_id TEXT NOT NULL"},
			{"machine", "machine TEXT NOT NULL"},
			{"path_prefix", "path_prefix TEXT NOT NULL"},
			{"layout", "layout TEXT NOT NULL DEFAULT 'explicit'"},
			{"project", "project TEXT NOT NULL DEFAULT ''"},
			{"original_project", "original_project TEXT NOT NULL DEFAULT ''"},
			{"enabled", "enabled BOOLEAN NOT NULL DEFAULT TRUE"},
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
			subagent_session_id TEXT,
			file_path TEXT
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
			{"file_path", "file_path TEXT"},
		},
		indexes: []string{
			"CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_calls_dedup ON tool_calls(session_id, message_id, call_index)",
			"CREATE INDEX IF NOT EXISTS idx_tool_calls_session ON tool_calls(session_id)",
			"CREATE INDEX IF NOT EXISTS idx_tool_calls_message ON tool_calls(message_id)",
			"CREATE INDEX IF NOT EXISTS idx_tool_calls_category ON tool_calls(category)",
			// DuckDB has no partial indexes, so this mirrors SQLite's
			// idx_tool_calls_file_path without the WHERE file_path IS NOT NULL
			// clause; it backs the cross-session Recent Edits feed.
			"CREATE INDEX IF NOT EXISTS idx_tool_calls_file_path ON tool_calls(file_path)",
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

// EnsureSchema creates the DuckDB mirror schema. It has no production
// callers: Sync.Push always goes through ProbeMirror to pick rebuildMirror
// (create-only, via createSchema) or incrementalPush against an
// already-valid mirror, and 'duckdb serve'/'duckdb quack serve' probe
// instead of migrating (see ProbeMirror, WatchMirrorReplacement). It is
// kept exported as a convenient fixture builder for tests that need a
// fresh, schema-compatible, empty mirror file to seed with raw INSERTs.
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	return createSchema(ctx, db)
}

// createSchema creates the DuckDB mirror schema on a fresh file. Mirror
// schema v4 has no in-place migrations: an existing file whose shape or
// version does not match is rejected by CheckSchemaCompat and must be
// rebuilt with 'agentsview duckdb push --full' rather than patched here.
func createSchema(ctx context.Context, db *sql.DB) error {
	for _, table := range mirrorTables {
		if _, err := db.ExecContext(ctx, table.create); err != nil {
			return fmt.Errorf("creating duckdb table %s: %w", table.name, err)
		}
	}
	for _, table := range mirrorTables {
		for _, stmt := range table.indexes {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("creating duckdb index for %s: %w", table.name, err)
			}
		}
	}
	if err := recordMetadataKey(
		ctx, db, schemaVersionMetadataKey, strconv.Itoa(SchemaVersion),
	); err != nil {
		return fmt.Errorf("recording duckdb schema version: %w", err)
	}
	return nil
}

func recordMetadataKey(
	ctx context.Context, db *sql.DB, key string, value string,
) error {
	var existing string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		key,
	).Scan(&existing)
	if err == nil && existing == value {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("checking duckdb metadata key %s: %w", key, err)
	}
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

// mirrorMetadata captures the push-scope bookkeeping written to
// sync_metadata by writeMirrorMetadata and read back by readMirrorMetadata /
// ProbeMirror. It records what a mirror file contains (schema/data version,
// push scope) and how it got that way (cutoff, last push time/machine) plus
// the source revisions needed to detect deletions and identity changes that
// happened after the mirror was built.
type mirrorMetadata struct {
	SchemaVersion int
	DataVersion   int
	// SourceDatabaseID is the archive_metadata database_id of the SQLite
	// archive the mirror was built from. It identifies the archive
	// GENERATION, not just the path: a resync builds a fresh archive with a
	// new database_id (see internal/db/orphaned.go, which deliberately does
	// not copy the old id), so a recorded id that no longer matches the
	// local archive means the mirror's cutoff and journal cursors describe a
	// different archive's history and only a full rebuild is sound (see
	// rebuildReason).
	SourceDatabaseID string
	Scope            string
	LastPushCutoff   string
	LastPushAt       string
	LastPushMachine  string
	DeletionRevision int64
	IdentityRevision int64
	MappingRevision  int64
}

// writeMirrorMetadata upserts every mirrorMetadata field into sync_metadata.
func writeMirrorMetadata(ctx context.Context, db *sql.DB, meta mirrorMetadata) error {
	fields := []struct {
		key   string
		value string
	}{
		{schemaVersionMetadataKey, strconv.Itoa(meta.SchemaVersion)},
		{dataVersionMetadataKey, strconv.Itoa(meta.DataVersion)},
		{sourceDatabaseIDMetadataKey, meta.SourceDatabaseID},
		{pushScopeMetadataKey, meta.Scope},
		{lastPushCutoffMetadataKey, meta.LastPushCutoff},
		{lastPushAtMetadataKey, meta.LastPushAt},
		{lastPushMachineMetadataKey, meta.LastPushMachine},
		{deletionRevisionMetadataKey, strconv.FormatInt(meta.DeletionRevision, 10)},
		{identityRevisionMetadataKey, strconv.FormatInt(meta.IdentityRevision, 10)},
		{mappingRevisionMetadataKey, strconv.FormatInt(meta.MappingRevision, 10)},
	}
	for _, field := range fields {
		if err := recordMetadataKey(ctx, db, field.key, field.value); err != nil {
			return err
		}
	}
	return nil
}

// readMirrorMetadata reads mirrorMetadata back from sync_metadata. Missing
// keys read back as zero values; malformed integer fields are reported as
// errors so callers (ProbeMirror) can surface them as shape issues rather
// than silently treating a corrupt mirror as version 0.
func readMirrorMetadata(ctx context.Context, db *sql.DB) (mirrorMetadata, error) {
	raw := make(map[string]string, 8)
	for _, key := range []string{
		schemaVersionMetadataKey, dataVersionMetadataKey,
		sourceDatabaseIDMetadataKey, pushScopeMetadataKey,
		lastPushCutoffMetadataKey, lastPushAtMetadataKey, lastPushMachineMetadataKey,
		deletionRevisionMetadataKey, identityRevisionMetadataKey,
		mappingRevisionMetadataKey,
	} {
		value, err := readMetadataKey(ctx, db, key)
		if err != nil {
			return mirrorMetadata{}, err
		}
		raw[key] = value
	}
	meta := mirrorMetadata{
		SourceDatabaseID: raw[sourceDatabaseIDMetadataKey],
		Scope:            raw[pushScopeMetadataKey],
		LastPushCutoff:   raw[lastPushCutoffMetadataKey],
		LastPushAt:       raw[lastPushAtMetadataKey],
		LastPushMachine:  raw[lastPushMachineMetadataKey],
	}
	var err error
	if meta.SchemaVersion, err = parseMirrorMetadataInt(
		schemaVersionMetadataKey, raw[schemaVersionMetadataKey],
	); err != nil {
		return mirrorMetadata{}, err
	}
	if meta.DataVersion, err = parseMirrorMetadataInt(
		dataVersionMetadataKey, raw[dataVersionMetadataKey],
	); err != nil {
		return mirrorMetadata{}, err
	}
	if meta.DeletionRevision, err = parseMirrorMetadataInt64(
		deletionRevisionMetadataKey, raw[deletionRevisionMetadataKey],
	); err != nil {
		return mirrorMetadata{}, err
	}
	if meta.IdentityRevision, err = parseMirrorMetadataInt64(
		identityRevisionMetadataKey, raw[identityRevisionMetadataKey],
	); err != nil {
		return mirrorMetadata{}, err
	}
	if meta.MappingRevision, err = parseMirrorMetadataInt64(
		mappingRevisionMetadataKey, raw[mappingRevisionMetadataKey],
	); err != nil {
		return mirrorMetadata{}, err
	}
	return meta, nil
}

func readMetadataKey(ctx context.Context, db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`, key,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading duckdb metadata key %s: %w", key, err)
	}
	return value, nil
}

func parseMirrorMetadataInt(key, value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parsing duckdb metadata key %s value %q: %w", key, value, err)
	}
	return parsed, nil
}

func parseMirrorMetadataInt64(key, value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing duckdb metadata key %s value %q: %w", key, value, err)
	}
	return parsed, nil
}

// CheckSchemaCompat verifies that the local DuckDB mirror file has the
// required tables, columns, and schema version. It does not mutate the
// database. The mirror schema is create-only, so a mismatch of any kind
// means the mirror must be rebuilt rather than migrated in place.
func CheckSchemaCompat(ctx context.Context, db *sql.DB) error {
	return checkSchemaShapeCompat(ctx, db, localSchema)
}

// CheckSchemaCompatViaQuack verifies schema compatibility of a remote Quack
// server's underlying mirror file.
func CheckSchemaCompatViaQuack(ctx context.Context, db *sql.DB) error {
	return checkSchemaShapeCompat(ctx, db, remoteSchema)
}

// schemaLocation says whether a compat failure is against the local mirror
// file or a remote Quack server, which changes only the missing-table/column
// hint: a remote server's shape is fixed by upgrading and restarting it, but
// its schema *version* is a property of the mirror file it serves, which
// only 'agentsview duckdb push --full' on the owning machine can fix.
type schemaLocation bool

const (
	localSchema  schemaLocation = false
	remoteSchema schemaLocation = true
)

func checkSchemaShapeCompat(
	ctx context.Context, db *sql.DB, location schemaLocation,
) error {
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
		if location == remoteSchema {
			return fmt.Errorf(
				"duckdb schema incompatible; the DuckDB server is on an "+
					"older AgentsView build; upgrade and restart the DuckDB "+
					"server so it migrates its schema at startup; missing: %s",
				strings.Join(missing, ", "),
			)
		}
		return fmt.Errorf(
			"duckdb schema incompatible; rebuild with 'agentsview duckdb push --full'; missing: %s",
			strings.Join(missing, ", "),
		)
	}

	var version string
	err = db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		schemaVersionMetadataKey,
	).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		if location == remoteSchema {
			return fmt.Errorf(
				"duckdb schema incompatible; missing %s in sync_metadata; "+
					"upgrade and restart the DuckDB server so it migrates "+
					"its schema at startup",
				schemaVersionMetadataKey,
			)
		}
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
	if got != SchemaVersion {
		return fmt.Errorf(
			"mirror schema version %d does not match this build's %d; "+
				"rebuild with 'agentsview duckdb push --full'",
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
