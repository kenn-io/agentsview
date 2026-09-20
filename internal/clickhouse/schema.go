package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// SchemaVersion is recorded in sync_metadata after EnsureSchema. Columns
// added later ship as ADD COLUMN IF NOT EXISTS entries in the table specs,
// so an older mirror upgrades in place; the version tells operators and
// status output which shape a mirror has.
const SchemaVersion = 2

// Metadata keys shared by every archive that pushes into the mirror.
const (
	schemaVersionKey     = "agentsview_schema_version"
	sourceDataVersionKey = "agentsview_source_data_version"
)

// Per-archive metadata key bases; see archiveMetadataKey.
const (
	lastPushCutoffKeyBase      = "agentsview_last_push_cutoff"
	lastPushAtKeyBase          = "agentsview_last_push_at"
	lastPushMachineKeyBase     = "agentsview_last_push_machine"
	pushScopeKeyBase           = "agentsview_push_scope"
	deletionRevisionKeyBase    = "agentsview_deletion_revision"
	identityRevisionKeyBase    = "agentsview_identity_revision"
	mappingRevisionKeyBase     = "agentsview_mapping_revision"
	curationFingerprintKeyBase = "agentsview_curation_fingerprint"
	cursorUsageMaxIDKeyBase    = "agentsview_cursor_usage_max_id"
)

// archiveMetadataKey scopes a sync_metadata key to one source archive so
// several machines can push into one database without overwriting each
// other's cursors.
func archiveMetadataKey(base, archiveID string) string {
	return base + ":" + archiveID
}

const (
	tString        = "String"
	tNullString    = "Nullable(String)"
	tInt           = "Int64"
	tNullInt       = "Nullable(Int64)"
	tFloat         = "Float64"
	tNullFloat     = "Nullable(Float64)"
	tBool          = "Bool"
	tTime          = "DateTime64(6, 'UTC')"
	tNullTime      = "Nullable(DateTime64(6, 'UTC'))"
	tVersion       = "UInt64"
	tFloatArray    = "Array(Float32)"
	pushVersionCol = "push_version"
)

type columnSpec struct {
	name string
	typ  string
	// def is an optional DEFAULT expression. Empty uses the type default.
	def string
}

func (c columnSpec) ddl() string {
	if c.def == "" {
		return c.name + " " + c.typ
	}
	return c.name + " " + c.typ + " DEFAULT " + c.def
}

type tableSpec struct {
	name    string
	columns []columnSpec
	orderBy []string
}

func (t tableSpec) createSQL() string {
	cols := make([]string, 0, len(t.columns)+1)
	for _, c := range t.columns {
		cols = append(cols, c.ddl())
	}
	cols = append(cols, pushVersionCol+" "+tVersion)
	return "CREATE TABLE IF NOT EXISTS " + t.name + " (\n\t" +
		strings.Join(cols, ",\n\t") + "\n) ENGINE = ReplacingMergeTree(" +
		pushVersionCol + ") ORDER BY (" + strings.Join(t.orderBy, ", ") + ")"
}

func (t tableSpec) addColumnSQL(c columnSpec) string {
	return "ALTER TABLE " + t.name + " ADD COLUMN IF NOT EXISTS " + c.ddl()
}

func col(name, typ string) columnSpec { return columnSpec{name: name, typ: typ} }

func colDefault(name, typ, def string) columnSpec {
	return columnSpec{name: name, typ: typ, def: def}
}

var pricingRateColumns = []columnSpec{
	col("input_microdollars_per_mtok", tInt),
	col("output_microdollars_per_mtok", tInt),
	col("cache_creation_microdollars_per_mtok", tInt),
	col("cache_creation_1h_microdollars_per_mtok", tInt),
	col("cache_read_microdollars_per_mtok", tInt),
	col("updated_at", tString),
}

var projectIdentityColumns = []columnSpec{
	col("project", tString),
	col("machine", tString),
	col("root_path", tString),
	col("git_remote", tString),
	col("git_remote_name", tString),
	col("repository_path", tString),
	col("worktree_name", tString),
	col("worktree_root_path", tString),
	colDefault("worktree_relationship", tString, "'unknown'"),
	colDefault("checkout_state", tString, "'unknown'"),
	col("git_branch", tString),
	colDefault("remote_resolution", tString, "'unknown'"),
	col("remote_candidate_count", tInt),
	col("observed_at", tNullTime),
	col("normalized_remote", tString),
	col("key_source", tString),
	col("key", tString),
}

func concatColumns(groups ...[]columnSpec) []columnSpec {
	var out []columnSpec
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// mirrorTables lists every mirrored table. Order matters only for
// readability; every table is created independently.
var mirrorTables = []tableSpec{
	{
		name:    "sync_metadata",
		columns: []columnSpec{col("key", tString), col("value", tString)},
		orderBy: []string{"key"},
	},
	{
		name:    "source_archives",
		columns: []columnSpec{col("source_archive_id", tString), col("source_archive_salt", tString)},
		orderBy: []string{"source_archive_id"},
	},
	{
		name: "sessions",
		columns: []columnSpec{
			col("id", tString),
			col("project", tString),
			col("project_assigned", tBool),
			colDefault("machine", tString, "'local'"),
			colDefault("agent", tString, "'claude'"),
			col("agent_label", tString),
			col("entrypoint", tString),
			col("session_kind", tString),
			col("first_message", tNullString),
			col("display_name", tNullString),
			col("session_name", tNullString),
			col("started_at", tNullTime),
			col("ended_at", tNullTime),
			col("message_count", tInt),
			col("user_message_count", tInt),
			col("file_path", tNullString),
			col("file_size", tNullInt),
			col("file_mtime", tNullInt),
			col("file_inode", tNullInt),
			col("file_device", tNullInt),
			col("file_hash", tNullString),
			col("local_modified_at", tNullTime),
			colDefault("transcript_revision", tString, "'0'"),
			col("parent_session_id", tNullString),
			col("relationship_type", tString),
			col("total_output_tokens", tInt),
			col("peak_context_tokens", tInt),
			col("has_total_output_tokens", tBool),
			col("has_peak_context_tokens", tBool),
			col("is_automated", tBool),
			col("tool_failure_signal_count", tInt),
			col("tool_retry_count", tInt),
			col("edit_churn_count", tInt),
			col("consecutive_failure_max", tInt),
			colDefault("outcome", tString, "'unknown'"),
			colDefault("outcome_confidence", tString, "'low'"),
			col("ended_with_role", tString),
			col("final_failure_streak", tInt),
			col("signals_pending_since", tNullString),
			col("compaction_count", tInt),
			col("mid_task_compaction_count", tInt),
			col("context_pressure_max", tNullFloat),
			col("health_score", tNullInt),
			col("health_grade", tNullString),
			col("has_tool_calls", tBool),
			col("has_context_data", tBool),
			col("quality_signal_version", tInt),
			col("short_prompt_count", tInt),
			col("unstructured_start", tBool),
			col("missing_success_criteria_count", tInt),
			col("missing_verification_count", tInt),
			col("duplicate_prompt_count", tInt),
			col("no_code_context_count", tInt),
			col("runaway_tool_loop_count", tInt),
			col("data_version", tInt),
			col("cwd", tString),
			col("git_branch", tString),
			col("source_session_id", tString),
			col("source_version", tString),
			col("transcript_fidelity", tString),
			col("parser_malformed_lines", tInt),
			col("is_truncated", tBool),
			col("deleted_at", tNullTime),
			col("deletion_cause", tNullString),
			col("created_at", tNullTime),
			col("termination_status", tNullString),
			col("secret_leak_count", tInt),
			col("secrets_rules_version", tString),
			col("last_message_at", tNullTime),
			col("agentsview_push_fingerprint", tString),
			col("source_archive_id", tString),
		},
		orderBy: []string{"id"},
	},
	{
		name: "messages",
		columns: []columnSpec{
			col("id", tInt),
			col("session_id", tString),
			col("ordinal", tInt),
			col("role", tString),
			col("content", tString),
			col("thinking_text", tString),
			col("timestamp", tNullTime),
			col("has_thinking", tBool),
			col("has_tool_use", tBool),
			col("content_length", tInt),
			col("is_system", tBool),
			col("model", tString),
			col("reasoning_effort", tString),
			col("token_usage", tString),
			col("context_tokens", tInt),
			col("output_tokens", tInt),
			col("provider_id", tString),
			col("has_context_tokens", tBool),
			col("has_output_tokens", tBool),
			col("claude_message_id", tString),
			col("claude_request_id", tString),
			col("source_type", tString),
			col("source_subtype", tString),
			col("prompt_source", tString),
			col("source_uuid", tString),
			col("source_parent_uuid", tString),
			col("is_sidechain", tBool),
			col("is_compact_boundary", tBool),
		},
		orderBy: []string{"session_id", "ordinal"},
	},
	{
		name: "usage_events",
		columns: []columnSpec{
			col("id", tInt),
			col("session_id", tString),
			col("message_ordinal", tNullInt),
			col("source", tString),
			col("model", tString),
			col("provider_id", tString),
			col("input_tokens", tInt),
			col("output_tokens", tInt),
			col("cache_creation_input_tokens", tInt),
			col("cache_read_input_tokens", tInt),
			col("reasoning_tokens", tInt),
			col("cost_microdollars", tNullInt),
			col("cost_status", tString),
			col("cost_source", tString),
			col("occurred_at", tNullTime),
			col("dedup_key", tString),
		},
		orderBy: []string{"session_id", "id"},
	},
	{
		name: "cursor_usage_events",
		columns: []columnSpec{
			col("id", tInt),
			col("occurred_at", tNullTime),
			col("model", tString),
			col("kind", tString),
			col("input_tokens", tInt),
			col("output_tokens", tInt),
			col("cache_write_tokens", tInt),
			col("cache_read_tokens", tInt),
			col("charged_microdollars", tInt),
			col("cursor_token_fee_microdollars", tInt),
			col("user_id", tString),
			col("user_email", tString),
			col("is_headless", tBool),
			col("dedup_key", tString),
		},
		orderBy: []string{"dedup_key", "id"},
	},
	{
		name:    "model_pricing",
		columns: concatColumns([]columnSpec{col("model_pattern", tString)}, pricingRateColumns),
		orderBy: []string{"model_pattern"},
	},
	{
		name: "model_pricing_bands",
		columns: concatColumns(
			[]columnSpec{col("model_pattern", tString), col("above_input_tokens", tInt)},
			pricingRateColumns,
		),
		orderBy: []string{"model_pattern", "above_input_tokens"},
	},
	{
		name: "genai_pricing",
		columns: []columnSpec{
			colDefault("singleton", tInt, "1"),
			col("version", tString),
			col("source_ref", tString),
			col("source", tString),
			col("data_json", tString),
			col("updated_at", tString),
		},
		orderBy: []string{"singleton"},
	},
	{
		// usage_event_prices holds what Go priced for one distinct set of
		// normalized usage inputs under one catalog digest. price_key is
		// computed by chUsagePriceKeySQL, never in Go, so the usage reader's
		// join matches by construction.
		name: "usage_event_prices",
		columns: []columnSpec{
			col("pricing_digest", tString),
			col("price_key", tString),
			col("token_cost_microdollars", tInt),
			col("cache_savings_microdollars", tInt),
			col("billed_context_id", tString),
			col("unbilled_context_id", tString),
			col("request_scoped", tBool),
			col("band_above_input_tokens", tInt),
			col("price_error", tString),
			col("priced", tInt),
		},
		orderBy: []string{"pricing_digest", "price_key"},
	},
	{
		name: "usage_price_contexts",
		columns: []columnSpec{
			col("pricing_digest", tString),
			col("context_id", tString),
			col("context_json", tString),
		},
		orderBy: []string{"pricing_digest", "context_id"},
	},
	{
		name: "source_project_identity_observations",
		columns: concatColumns(
			[]columnSpec{col("source_archive_id", tString), col("source_archive_salt", tString)},
			projectIdentityColumns,
		),
		orderBy: []string{"source_archive_id", "project", "machine", "root_path", "git_remote"},
	},
	{
		name: "source_session_project_identity_snapshots",
		columns: concatColumns(
			[]columnSpec{
				col("source_archive_id", tString),
				col("source_database_generation", tString),
				col("source_session_id", tString),
			},
			projectIdentityColumns,
		),
		orderBy: []string{"source_archive_id", "source_database_generation", "source_session_id"},
	},
	{
		name: "source_worktree_project_mappings",
		columns: []columnSpec{
			col("source_archive_id", tString),
			col("machine", tString),
			col("path_prefix", tString),
			colDefault("layout", tString, "'explicit'"),
			col("project", tString),
			col("original_project", tString),
			colDefault("enabled", tBool, "true"),
			col("updated_at", tString),
		},
		orderBy: []string{"source_archive_id", "machine", "path_prefix"},
	},
	{
		name: "tool_calls",
		columns: []columnSpec{
			col("message_id", tInt),
			col("message_ordinal", tInt),
			col("session_id", tString),
			col("tool_name", tString),
			col("category", tString),
			col("call_index", tInt),
			col("tool_use_id", tString),
			col("input_json", tString),
			col("skill_name", tString),
			col("result_content_length", tInt),
			col("result_content", tString),
			col("subagent_session_id", tString),
			col("file_path", tString),
		},
		orderBy: []string{"session_id", "message_ordinal", "call_index"},
	},
	{
		name: "tool_result_events",
		columns: []columnSpec{
			col("session_id", tString),
			col("tool_call_message_ordinal", tInt),
			col("call_index", tInt),
			col("tool_use_id", tString),
			col("agent_id", tString),
			col("subagent_session_id", tString),
			col("source", tString),
			col("status", tString),
			col("content", tString),
			col("content_length", tInt),
			col("timestamp", tNullTime),
			col("event_index", tInt),
		},
		orderBy: []string{"session_id", "tool_call_message_ordinal", "call_index", "event_index"},
	},
	{
		name: "secret_findings",
		columns: []columnSpec{
			col("session_id", tString),
			// finding_index is the finding's position in the session's local
			// finding list; SQLite findings carry no id of their own.
			col("finding_index", tInt),
			col("rule_name", tString),
			col("confidence", tString),
			col("location_kind", tString),
			col("message_ordinal", tInt),
			col("call_index", tNullInt),
			col("event_index", tNullInt),
			col("match_start", tInt),
			col("match_end", tInt),
			col("match_index", tInt),
			col("redacted_match", tString),
			col("rules_version", tString),
			col("created_at", tNullTime),
		},
		orderBy: []string{"session_id", "finding_index"},
	},
	// Vector tables mirror the local vectors.db export (see vector_push.go).
	// A generation is keyed by its config fingerprint: ClickHouse has no
	// serial ids, and the fingerprint is what a serving process matches on.
	{
		name: "vector_generations",
		columns: []columnSpec{
			col("fingerprint", tString),
			col("model", tString),
			col("dimension", tInt),
			col("created_at", tNullTime),
		},
		orderBy: []string{"fingerprint"},
	},
	{
		name: "vector_documents",
		columns: []columnSpec{
			col("doc_key", tString),
			col("session_id", tString),
			col("source_uuid", tString),
			col("ordinal", tInt),
			col("ordinal_end", tInt),
			col("subordinate", tBool),
			colDefault("offsets", tString, "'[]'"),
			col("content", tString),
			col("content_hash", tString),
		},
		orderBy: []string{"doc_key"},
	},
	{
		// session_id is denormalized so a session's chunks delete in one
		// statement without a subquery over vector_documents.
		name: "vector_chunks",
		columns: []columnSpec{
			col("generation_fingerprint", tString),
			col("doc_key", tString),
			col("chunk_index", tInt),
			col("session_id", tString),
			col("embedding", tFloatArray),
		},
		orderBy: []string{"generation_fingerprint", "doc_key", "chunk_index"},
	},
	{
		name: "vector_push_state",
		columns: []columnSpec{
			col("source_archive_id", tString),
			col("generation_fingerprint", tString),
			col("session_id", tString),
			col("doc_agg_hash", tString),
		},
		orderBy: []string{"source_archive_id", "generation_fingerprint", "session_id"},
	},
	{
		name:    "starred_sessions",
		columns: []columnSpec{col("session_id", tString), col("created_at", tNullTime)},
		orderBy: []string{"session_id"},
	},
	{
		name: "pinned_messages",
		columns: []columnSpec{
			col("id", tInt),
			col("session_id", tString),
			col("message_id", tInt),
			col("ordinal", tInt),
			col("source_uuid", tString),
			col("note", tNullString),
			col("created_at", tNullTime),
		},
		orderBy: []string{"session_id", "message_id"},
	},
}

func tableByName(name string) (tableSpec, bool) {
	for _, t := range mirrorTables {
		if t.name == name {
			return t, true
		}
	}
	return tableSpec{}, false
}

// EnsureSchema creates the mirror database and every table, adds columns
// missing from older mirrors, and records the schema version. It opens two
// short-lived connections: one to the DSN's own database for CREATE
// DATABASE, then one to the mirror database for the tables.
func EnsureSchema(ctx context.Context, t Target) error {
	database, err := t.DatabaseName()
	if err != nil {
		return err
	}
	admin, err := OpenForAdmin(ctx, t)
	if err != nil {
		return err
	}
	_, err = admin.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+database)
	admin.Close()
	if err != nil {
		return fmt.Errorf("creating clickhouse database %s: %w", database, err)
	}
	conn, err := Open(ctx, t)
	if err != nil {
		return err
	}
	defer conn.Close()
	return EnsureSchemaOn(ctx, conn)
}

// EnsureSchemaOn creates missing tables and columns on an open mirror
// connection and records SchemaVersion.
func EnsureSchemaOn(ctx context.Context, conn *sql.DB) error {
	existing, err := readColumns(ctx, conn)
	if err != nil {
		return err
	}
	for _, t := range mirrorTables {
		cols, ok := existing[t.name]
		if !ok {
			if _, err := conn.ExecContext(ctx, t.createSQL()); err != nil {
				return fmt.Errorf("creating clickhouse table %s: %w", t.name, err)
			}
			continue
		}
		for _, c := range t.columns {
			if _, has := cols[c.name]; has {
				continue
			}
			if _, err := conn.ExecContext(ctx, t.addColumnSQL(c)); err != nil {
				return fmt.Errorf("adding clickhouse column %s.%s: %w", t.name, c.name, err)
			}
		}
	}
	if err := ensureUsageMessages(ctx, conn); err != nil {
		return err
	}
	if err := ensureTerminalEventSnapshots(ctx, conn); err != nil {
		return err
	}
	return writeMetadata(ctx, conn, map[string]string{
		schemaVersionKey:     strconv.Itoa(SchemaVersion),
		sourceDataVersionKey: strconv.Itoa(db.CurrentDataVersion()),
	})
}

// readColumns maps table name to the set of column names in the current
// database.
func readColumns(ctx context.Context, conn *sql.DB) (map[string]map[string]struct{}, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT table, name FROM system.columns WHERE database = currentDatabase()`)
	if err != nil {
		return nil, fmt.Errorf("reading clickhouse columns: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string]struct{}{}
	for rows.Next() {
		var table, name string
		if err := rows.Scan(&table, &name); err != nil {
			return nil, fmt.Errorf("scanning clickhouse columns: %w", err)
		}
		if out[table] == nil {
			out[table] = map[string]struct{}{}
		}
		out[table][name] = struct{}{}
	}
	return out, rows.Err()
}

// CheckSchemaCompat verifies every mirror table and column exists. It names
// what is missing so an operator knows to run `clickhouse push` (which runs
// EnsureSchema) with a role that may create tables.
func CheckSchemaCompat(ctx context.Context, conn *sql.DB) error {
	existing, err := readColumns(ctx, conn)
	if err != nil {
		return err
	}
	var missing []string
	for _, t := range mirrorTables {
		cols, ok := existing[t.name]
		if !ok {
			missing = append(missing, t.name)
			continue
		}
		for _, c := range append(t.columns, col(pushVersionCol, tVersion)) {
			if _, has := cols[c.name]; !has {
				missing = append(missing, t.name+"."+c.name)
			}
		}
	}
	for _, table := range []string{"usage_messages", "terminal_event_snapshots"} {
		if _, has := existing[table]; !has {
			missing = append(missing, table)
		}
	}
	if len(missing) == 0 {
		metadata, err := readMetadata(ctx, conn, "usage_messages_backfill", "terminal_event_snapshots_backfill")
		if err != nil {
			return err
		}
		for _, fill := range [][2]string{{"usage_messages_backfill", "usage"}, {"terminal_event_snapshots_backfill", "terminal event"}} {
			if metadata[fill[0]] != "1" {
				return fmt.Errorf("clickhouse %s backfill is incomplete; run `agentsview clickhouse push` with a role that can finish it", fill[1])
			}
		}
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf(
		"clickhouse mirror schema is missing %s; run `agentsview clickhouse push` with a role that can create tables",
		strings.Join(missing, ", "),
	)
}

// CheckDataVersionCompat rejects a mirror whose sessions were pushed by a
// newer AgentsView than this binary understands. ClickHouse has no global
// user_version, so the highest pushed data_version is the marker, as in the
// PostgreSQL store.
func CheckDataVersionCompat(ctx context.Context, conn *sql.DB) error {
	var maxVersion int
	err := conn.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(data_version), 0) FROM sessions`).Scan(&maxVersion)
	if err != nil {
		if isMissingTableError(err) {
			return nil
		}
		return fmt.Errorf("reading clickhouse data version: %w", err)
	}
	if current := db.CurrentDataVersion(); maxVersion > current {
		return &db.DataVersionTooNewError{DatabaseVersion: maxVersion, BinaryVersion: current}
	}
	return nil
}

// writeMetadata upserts sync_metadata rows. ReplacingMergeTree keyed on
// key plus final=1 makes the latest push_version win on read.
func writeMetadata(ctx context.Context, conn *sql.DB, values map[string]string) error {
	return writeMetadataVersion(ctx, conn, values, newPushVersion())
}

func writeMetadataVersion(
	ctx context.Context, conn *sql.DB, values map[string]string, version uint64,
) error {
	if len(values) == 0 {
		return nil
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting clickhouse metadata batch: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		"INSERT INTO sync_metadata (key, value, push_version)")
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("preparing clickhouse metadata batch: %w", err)
	}
	defer stmt.Close()
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := stmt.ExecContext(ctx, k, values[k], version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("writing clickhouse metadata %s: %w", k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing clickhouse metadata: %w", err)
	}
	return nil
}

// readMetadata returns the values stored for the given keys; missing keys
// are absent from the map.
func readMetadata(ctx context.Context, conn *sql.DB, keys ...string) (map[string]string, error) {
	if len(keys) == 0 {
		return map[string]string{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	rows, err := conn.QueryContext(ctx,
		"SELECT key, value FROM sync_metadata WHERE key IN ("+placeholders+")", args...)
	if err != nil {
		return nil, fmt.Errorf("reading clickhouse metadata: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string, len(keys))
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scanning clickhouse metadata: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}
