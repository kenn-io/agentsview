package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
)

const tokenCoverageRepairMetadataKey = "token_coverage_repair_v1"
const sourceCurationBackfillMetadataKey = "source_curation_baseline_backfill_v1"
const projectIdentityRemoteScrubMetadataKey = "git_remote_credentials_scrub_v1"
const tokenCoverageBackfillBatchSize = 1000

type columnMigration struct {
	table  string
	column string
	def    string
	desc   string
}

// coreDDL creates the tables and indexes. It uses unqualified
// names because Open() sets search_path to the target schema.
const coreDDL = `
CREATE TABLE IF NOT EXISTS sync_metadata (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id                 TEXT PRIMARY KEY,
    machine            TEXT NOT NULL,
    owner_marker       TEXT NOT NULL DEFAULT '',
    project            TEXT NOT NULL,
    agent              TEXT NOT NULL,
    first_message      TEXT,
    display_name       TEXT,
    source_display_name TEXT,
    session_name       TEXT,
    created_at         TIMESTAMPTZ,
    started_at         TIMESTAMPTZ,
    ended_at           TIMESTAMPTZ,
    deleted_at         TIMESTAMPTZ,
    source_deleted_at  TIMESTAMPTZ,
    message_count      INT NOT NULL DEFAULT 0,
    user_message_count INT NOT NULL DEFAULT 0,
    parent_session_id  TEXT,
    relationship_type  TEXT NOT NULL DEFAULT '',
    total_output_tokens INT NOT NULL DEFAULT 0,
    peak_context_tokens INT NOT NULL DEFAULT 0,
    has_total_output_tokens BOOLEAN NOT NULL DEFAULT FALSE,
    has_peak_context_tokens BOOLEAN NOT NULL DEFAULT FALSE,
    is_automated       BOOLEAN NOT NULL DEFAULT FALSE,
    tool_failure_signal_count INT NOT NULL DEFAULT 0,
    tool_retry_count          INT NOT NULL DEFAULT 0,
    edit_churn_count          INT NOT NULL DEFAULT 0,
    consecutive_failure_max   INT NOT NULL DEFAULT 0,
    outcome                   TEXT NOT NULL DEFAULT 'unknown',
    outcome_confidence        TEXT NOT NULL DEFAULT 'low',
    ended_with_role           TEXT NOT NULL DEFAULT '',
    final_failure_streak      INT NOT NULL DEFAULT 0,
    signals_pending_since     TEXT,
    compaction_count          INT NOT NULL DEFAULT 0,
    mid_task_compaction_count INT NOT NULL DEFAULT 0,
    context_pressure_max      DOUBLE PRECISION,
    health_score              INT,
    health_grade              TEXT,
    quality_signal_version    INT NOT NULL DEFAULT 0,
    short_prompt_count        INT NOT NULL DEFAULT 0,
    unstructured_start        BOOLEAN NOT NULL DEFAULT FALSE,
    missing_success_criteria_count INT NOT NULL DEFAULT 0,
    missing_verification_count INT NOT NULL DEFAULT 0,
    duplicate_prompt_count    INT NOT NULL DEFAULT 0,
    no_code_context_count     INT NOT NULL DEFAULT 0,
    runaway_tool_loop_count   INT NOT NULL DEFAULT 0,
    termination_status        TEXT,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_sessions_parent
    ON sessions (parent_session_id)
    WHERE parent_session_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS messages (
    session_id     TEXT NOT NULL,
    ordinal        INT NOT NULL,
    role           TEXT NOT NULL,
    content        TEXT NOT NULL,
    thinking_text  TEXT NOT NULL DEFAULT '',
    timestamp      TIMESTAMPTZ,
    has_thinking   BOOLEAN NOT NULL DEFAULT FALSE,
    has_tool_use   BOOLEAN NOT NULL DEFAULT FALSE,
    content_length INT NOT NULL DEFAULT 0,
    is_system      BOOLEAN NOT NULL DEFAULT FALSE,
    model          TEXT NOT NULL DEFAULT '',
    token_usage    TEXT NOT NULL DEFAULT '',
    context_tokens INT NOT NULL DEFAULT 0,
    output_tokens  INT NOT NULL DEFAULT 0,
    has_context_tokens BOOLEAN NOT NULL DEFAULT FALSE,
    has_output_tokens  BOOLEAN NOT NULL DEFAULT FALSE,
    claude_message_id  TEXT NOT NULL DEFAULT '',
    claude_request_id  TEXT NOT NULL DEFAULT '',
    source_type        TEXT NOT NULL DEFAULT '',
    source_subtype     TEXT NOT NULL DEFAULT '',
    source_uuid        TEXT NOT NULL DEFAULT '',
    source_parent_uuid TEXT NOT NULL DEFAULT '',
    is_sidechain       BOOLEAN NOT NULL DEFAULT FALSE,
    is_compact_boundary BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (session_id, ordinal),
    FOREIGN KEY (session_id)
        REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_messages_velocity
    ON messages (session_id, ordinal, role, timestamp, content_length);

CREATE TABLE IF NOT EXISTS usage_events (
    id BIGSERIAL PRIMARY KEY,
    session_id TEXT NOT NULL,
    message_ordinal INT,
    source TEXT NOT NULL,
    model TEXT NOT NULL,
    input_tokens INT NOT NULL DEFAULT 0,
    output_tokens INT NOT NULL DEFAULT 0,
    cache_creation_input_tokens INT NOT NULL DEFAULT 0,
    cache_read_input_tokens INT NOT NULL DEFAULT 0,
    reasoning_tokens INT NOT NULL DEFAULT 0,
    cost_usd DOUBLE PRECISION,
    cost_status TEXT NOT NULL DEFAULT '',
    cost_source TEXT NOT NULL DEFAULT '',
    occurred_at TIMESTAMPTZ,
    dedup_key TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (session_id)
        REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_events_dedup
    ON usage_events (session_id, source, dedup_key)
    WHERE dedup_key != '';

CREATE INDEX IF NOT EXISTS idx_usage_events_session
    ON usage_events (session_id);

CREATE INDEX IF NOT EXISTS idx_usage_events_occurred
    ON usage_events (occurred_at);

CREATE TABLE IF NOT EXISTS cursor_usage_events (
    id BIGSERIAL PRIMARY KEY,
    occurred_at TIMESTAMPTZ NOT NULL,
    model TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT '',
    input_tokens INT NOT NULL DEFAULT 0,
    output_tokens INT NOT NULL DEFAULT 0,
    cache_write_tokens INT NOT NULL DEFAULT 0,
    cache_read_tokens INT NOT NULL DEFAULT 0,
    charged_cents DOUBLE PRECISION NOT NULL DEFAULT 0,
    cursor_token_fee DOUBLE PRECISION NOT NULL DEFAULT 0,
    user_id TEXT NOT NULL DEFAULT '',
    user_email TEXT NOT NULL DEFAULT '',
    is_headless BOOLEAN NOT NULL DEFAULT FALSE,
    dedup_key TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cursor_usage_events_dedup
    ON cursor_usage_events (dedup_key)
    WHERE dedup_key != '';

CREATE INDEX IF NOT EXISTS idx_cursor_usage_events_occurred
    ON cursor_usage_events (occurred_at);

CREATE INDEX IF NOT EXISTS idx_cursor_usage_events_model
    ON cursor_usage_events (model);

CREATE TABLE IF NOT EXISTS starred_sessions (
    session_id TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (session_id)
        REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS excluded_sessions (
    id         TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS session_aliases (
    session_id TEXT NOT NULL,
    alias_id   TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (session_id, alias_id),
    FOREIGN KEY (session_id)
        REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS pinned_messages (
    id          BIGSERIAL PRIMARY KEY,
    session_id  TEXT NOT NULL,
    message_id  INT NOT NULL,
    ordinal     INT NOT NULL,
    source_uuid TEXT NOT NULL DEFAULT '',
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (session_id)
        REFERENCES sessions(id) ON DELETE CASCADE,
    UNIQUE (session_id, message_id)
);

CREATE INDEX IF NOT EXISTS idx_pinned_session
    ON pinned_messages (session_id);

CREATE INDEX IF NOT EXISTS idx_pinned_created
    ON pinned_messages (created_at DESC);

CREATE INDEX IF NOT EXISTS idx_pinned_source_uuid
    ON pinned_messages (session_id, source_uuid)
    WHERE source_uuid <> '';

CREATE TABLE IF NOT EXISTS model_pricing (
    model_pattern TEXT PRIMARY KEY,
    input_per_mtok DOUBLE PRECISION NOT NULL DEFAULT 0,
    output_per_mtok DOUBLE PRECISION NOT NULL DEFAULT 0,
    cache_creation_per_mtok DOUBLE PRECISION NOT NULL DEFAULT 0,
    cache_read_per_mtok DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS project_identity_observations (
    project            TEXT NOT NULL,
    machine            TEXT NOT NULL,
    root_path          TEXT NOT NULL DEFAULT '',
    git_remote         TEXT NOT NULL DEFAULT '',
    git_remote_name    TEXT NOT NULL DEFAULT '',
    worktree_name      TEXT NOT NULL DEFAULT '',
    worktree_root_path TEXT NOT NULL DEFAULT '',
    observed_at        TIMESTAMPTZ NOT NULL,
    normalized_remote  TEXT NOT NULL DEFAULT '',
    key_source         TEXT NOT NULL DEFAULT '',
    key                TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (project, machine, root_path, git_remote)
);

CREATE INDEX IF NOT EXISTS idx_project_identity_observations_project
    ON project_identity_observations (project);

CREATE TABLE IF NOT EXISTS tool_calls (
    id                    BIGSERIAL PRIMARY KEY,
    session_id            TEXT NOT NULL,
    tool_name             TEXT NOT NULL,
    category              TEXT NOT NULL,
    call_index            INT NOT NULL DEFAULT 0,
    tool_use_id           TEXT NOT NULL DEFAULT '',
    input_json            TEXT,
    skill_name            TEXT,
    result_content_length INT,
    result_content        TEXT,
    subagent_session_id   TEXT,
    message_ordinal       INT NOT NULL,
    file_path             TEXT,
    FOREIGN KEY (session_id)
        REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_calls_dedup
    ON tool_calls (session_id, message_ordinal, call_index);

CREATE INDEX IF NOT EXISTS idx_tool_calls_session
    ON tool_calls (session_id);

CREATE INDEX IF NOT EXISTS idx_tool_calls_session_category
    ON tool_calls (session_id, category);

CREATE TABLE IF NOT EXISTS tool_result_events (
    id                        BIGSERIAL PRIMARY KEY,
    session_id                TEXT NOT NULL,
    tool_call_message_ordinal INT NOT NULL,
    call_index                INT NOT NULL DEFAULT 0,
    tool_use_id               TEXT,
    agent_id                  TEXT,
    subagent_session_id       TEXT,
    source                    TEXT NOT NULL,
    status                    TEXT NOT NULL,
    content                   TEXT NOT NULL,
    content_length            INT NOT NULL DEFAULT 0,
    timestamp                 TIMESTAMPTZ,
    event_index               INT NOT NULL DEFAULT 0,
    FOREIGN KEY (session_id)
        REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_tool_result_events_session
    ON tool_result_events (session_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_result_events_dedup
    ON tool_result_events (
        session_id, tool_call_message_ordinal,
        call_index, event_index
    );

CREATE TABLE IF NOT EXISTS secret_findings (
    id               BIGINT GENERATED ALWAYS AS IDENTITY
                         PRIMARY KEY,
    session_id       TEXT NOT NULL
                         REFERENCES sessions(id)
                         ON DELETE CASCADE,
    rule_name        TEXT NOT NULL,
    confidence       TEXT NOT NULL,
    location_kind    TEXT NOT NULL,
    message_ordinal  INTEGER NOT NULL,
    call_index       INTEGER,
    event_index      INTEGER,
    match_start      INTEGER NOT NULL,
    match_end        INTEGER NOT NULL,
    match_index      INTEGER NOT NULL,
    redacted_match   TEXT NOT NULL,
    rules_version    TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_secret_findings_session
    ON secret_findings (session_id);

CREATE INDEX IF NOT EXISTS idx_secret_findings_rule
    ON secret_findings (rule_name);

CREATE TABLE IF NOT EXISTS insights (
    id               BIGSERIAL PRIMARY KEY,
    type             TEXT NOT NULL DEFAULT '',
    date_from        TEXT NOT NULL DEFAULT '',
    date_to          TEXT NOT NULL DEFAULT '',
    project          TEXT,
    agent            TEXT NOT NULL DEFAULT '',
    model            TEXT,
    prompt           TEXT,
    content          TEXT NOT NULL DEFAULT '',
    kind             TEXT NOT NULL DEFAULT '',
    schema_version   TEXT NOT NULL DEFAULT '',
    template_id      TEXT NOT NULL DEFAULT '',
    template_version TEXT NOT NULL DEFAULT '',
    aggregate_hash   TEXT NOT NULL DEFAULT '',
    cache_key        TEXT NOT NULL DEFAULT '',
    cache_status     TEXT NOT NULL DEFAULT '',
    provenance_json  TEXT NOT NULL DEFAULT '',
    structured_json  TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_insights_lookup
    ON insights (type, date_from, date_to, project);

CREATE INDEX IF NOT EXISTS idx_insights_cache
    ON insights (cache_key, created_at DESC)
    WHERE cache_key <> '';
`

// EnsureSchema creates the schema (if needed), then runs
// idempotent CREATE TABLE / ALTER TABLE statements. The schema
// parameter is the unquoted schema name (e.g. "agentsview").
//
// After CREATE SCHEMA, all table DDL uses unqualified names
// because Open() sets search_path to the target schema.
func EnsureSchema(
	ctx context.Context, db *sql.DB, schema string,
) error {
	start := time.Now()
	quoted, err := quoteIdentifier(schema)
	if err != nil {
		return fmt.Errorf("invalid schema name: %w", err)
	}
	if err := CheckDataVersionCompat(ctx, db); err != nil {
		return err
	}
	step := time.Now()
	if _, err := db.ExecContext(ctx,
		"CREATE SCHEMA IF NOT EXISTS "+quoted,
	); err != nil {
		return fmt.Errorf("creating pg schema: %w", err)
	}
	log.Printf(
		"pg schema: create schema step completed in %s",
		time.Since(step).Round(time.Millisecond),
	)
	step = time.Now()
	if _, err := db.ExecContext(ctx, coreDDL); err != nil {
		return fmt.Errorf("creating pg tables: %w", err)
	}
	log.Printf(
		"pg schema: core DDL step completed in %s",
		time.Since(step).Round(time.Millisecond),
	)

	// Idempotent column additions for forward compatibility.
	alters := []columnMigration{
		{
			"sessions", "owner_marker",
			`owner_marker TEXT NOT NULL DEFAULT ''`,
			"adding sessions.owner_marker",
		},
		{
			"sessions", "deleted_at",
			`deleted_at TIMESTAMPTZ`,
			"adding sessions.deleted_at",
		},
		{
			"sessions", "created_at",
			`created_at TIMESTAMPTZ`,
			"adding sessions.created_at",
		},
		{
			"sessions", "source_display_name",
			`source_display_name TEXT`,
			"adding sessions.source_display_name",
		},
		{
			"sessions", "source_deleted_at",
			`source_deleted_at TIMESTAMPTZ`,
			"adding sessions.source_deleted_at",
		},
		{
			"sessions", "total_output_tokens",
			`total_output_tokens INT NOT NULL DEFAULT 0`,
			"adding sessions.total_output_tokens",
		},
		{
			"sessions", "peak_context_tokens",
			`peak_context_tokens INT NOT NULL DEFAULT 0`,
			"adding sessions.peak_context_tokens",
		},
		{
			"sessions", "has_total_output_tokens",
			`has_total_output_tokens BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.has_total_output_tokens",
		},
		{
			"sessions", "has_peak_context_tokens",
			`has_peak_context_tokens BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.has_peak_context_tokens",
		},
		{
			"messages", "model",
			`model TEXT NOT NULL DEFAULT ''`,
			"adding messages.model",
		},
		{
			"messages", "token_usage",
			`token_usage TEXT NOT NULL DEFAULT ''`,
			"adding messages.token_usage",
		},
		{
			"messages", "context_tokens",
			`context_tokens INT NOT NULL DEFAULT 0`,
			"adding messages.context_tokens",
		},
		{
			"messages", "output_tokens",
			`output_tokens INT NOT NULL DEFAULT 0`,
			"adding messages.output_tokens",
		},
		{
			"messages", "has_context_tokens",
			`has_context_tokens BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding messages.has_context_tokens",
		},
		{
			"messages", "has_output_tokens",
			`has_output_tokens BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding messages.has_output_tokens",
		},
		{
			"messages", "claude_message_id",
			`claude_message_id TEXT NOT NULL DEFAULT ''`,
			"adding messages.claude_message_id",
		},
		{
			"messages", "claude_request_id",
			`claude_request_id TEXT NOT NULL DEFAULT ''`,
			"adding messages.claude_request_id",
		},
		{
			"tool_calls", "call_index",
			`call_index INT NOT NULL DEFAULT 0`,
			"adding tool_calls.call_index",
		},
		{
			"tool_calls", "file_path",
			`file_path TEXT`,
			"adding tool_calls.file_path",
		},
		{
			"sessions", "is_automated",
			`is_automated BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.is_automated",
		},
		{
			"sessions", "tool_failure_signal_count",
			`tool_failure_signal_count INT NOT NULL DEFAULT 0`,
			"adding sessions.tool_failure_signal_count",
		},
		{
			"sessions", "tool_retry_count",
			`tool_retry_count INT NOT NULL DEFAULT 0`,
			"adding sessions.tool_retry_count",
		},
		{
			"sessions", "edit_churn_count",
			`edit_churn_count INT NOT NULL DEFAULT 0`,
			"adding sessions.edit_churn_count",
		},
		{
			"sessions", "consecutive_failure_max",
			`consecutive_failure_max INT NOT NULL DEFAULT 0`,
			"adding sessions.consecutive_failure_max",
		},
		{
			"sessions", "outcome",
			`outcome TEXT NOT NULL DEFAULT 'unknown'`,
			"adding sessions.outcome",
		},
		{
			"sessions", "outcome_confidence",
			`outcome_confidence TEXT NOT NULL DEFAULT 'low'`,
			"adding sessions.outcome_confidence",
		},
		{
			"sessions", "ended_with_role",
			`ended_with_role TEXT NOT NULL DEFAULT ''`,
			"adding sessions.ended_with_role",
		},
		{
			"sessions", "final_failure_streak",
			`final_failure_streak INT NOT NULL DEFAULT 0`,
			"adding sessions.final_failure_streak",
		},
		{
			"sessions", "signals_pending_since",
			`signals_pending_since TEXT`,
			"adding sessions.signals_pending_since",
		},
		{
			"sessions", "compaction_count",
			`compaction_count INT NOT NULL DEFAULT 0`,
			"adding sessions.compaction_count",
		},
		{
			"sessions", "mid_task_compaction_count",
			`mid_task_compaction_count INT NOT NULL DEFAULT 0`,
			"adding sessions.mid_task_compaction_count",
		},
		{
			"sessions", "context_pressure_max",
			`context_pressure_max DOUBLE PRECISION`,
			"adding sessions.context_pressure_max",
		},
		{
			"sessions", "health_score",
			`health_score INT`,
			"adding sessions.health_score",
		},
		{
			"sessions", "health_grade",
			`health_grade TEXT`,
			"adding sessions.health_grade",
		},
		{
			"sessions", "has_tool_calls",
			`has_tool_calls BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.has_tool_calls",
		},
		{
			"sessions", "has_context_data",
			`has_context_data BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.has_context_data",
		},
		{
			"sessions", "quality_signal_version",
			`quality_signal_version INT NOT NULL DEFAULT 0`,
			"adding sessions.quality_signal_version",
		},
		{
			"sessions", "short_prompt_count",
			`short_prompt_count INT NOT NULL DEFAULT 0`,
			"adding sessions.short_prompt_count",
		},
		{
			"sessions", "unstructured_start",
			`unstructured_start BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.unstructured_start",
		},
		{
			"sessions", "missing_success_criteria_count",
			`missing_success_criteria_count INT NOT NULL DEFAULT 0`,
			"adding sessions.missing_success_criteria_count",
		},
		{
			"sessions", "missing_verification_count",
			`missing_verification_count INT NOT NULL DEFAULT 0`,
			"adding sessions.missing_verification_count",
		},
		{
			"sessions", "duplicate_prompt_count",
			`duplicate_prompt_count INT NOT NULL DEFAULT 0`,
			"adding sessions.duplicate_prompt_count",
		},
		{
			"sessions", "no_code_context_count",
			`no_code_context_count INT NOT NULL DEFAULT 0`,
			"adding sessions.no_code_context_count",
		},
		{
			"sessions", "runaway_tool_loop_count",
			`runaway_tool_loop_count INT NOT NULL DEFAULT 0`,
			"adding sessions.runaway_tool_loop_count",
		},
		{
			"sessions", "data_version",
			`data_version INT NOT NULL DEFAULT 0`,
			"adding sessions.data_version",
		},
		{
			"sessions", "cwd",
			`cwd TEXT NOT NULL DEFAULT ''`,
			"adding sessions.cwd",
		},
		{
			"sessions", "git_branch",
			`git_branch TEXT NOT NULL DEFAULT ''`,
			"adding sessions.git_branch",
		},
		{
			"sessions", "source_session_id",
			`source_session_id TEXT NOT NULL DEFAULT ''`,
			"adding sessions.source_session_id",
		},
		{
			"sessions", "source_version",
			`source_version TEXT NOT NULL DEFAULT ''`,
			"adding sessions.source_version",
		},
		{
			"sessions", "transcript_fidelity",
			`transcript_fidelity TEXT NOT NULL DEFAULT ''`,
			"adding sessions.transcript_fidelity",
		},
		{
			"sessions", "parser_malformed_lines",
			`parser_malformed_lines INT NOT NULL DEFAULT 0`,
			"adding sessions.parser_malformed_lines",
		},
		{
			"sessions", "is_truncated",
			`is_truncated BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.is_truncated",
		},
		{
			"messages", "source_type",
			`source_type TEXT NOT NULL DEFAULT ''`,
			"adding messages.source_type",
		},
		{
			"messages", "source_subtype",
			`source_subtype TEXT NOT NULL DEFAULT ''`,
			"adding messages.source_subtype",
		},
		{
			"messages", "source_uuid",
			`source_uuid TEXT NOT NULL DEFAULT ''`,
			"adding messages.source_uuid",
		},
		{
			"messages", "source_parent_uuid",
			`source_parent_uuid TEXT NOT NULL DEFAULT ''`,
			"adding messages.source_parent_uuid",
		},
		{
			"messages", "is_sidechain",
			`is_sidechain BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding messages.is_sidechain",
		},
		{
			"messages", "is_compact_boundary",
			`is_compact_boundary BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding messages.is_compact_boundary",
		},
		{
			"messages", "thinking_text",
			`thinking_text TEXT NOT NULL DEFAULT ''`,
			"adding messages.thinking_text",
		},
		{
			"sessions", "termination_status",
			`termination_status TEXT`,
			"adding sessions.termination_status",
		},
		{
			"sessions", "secret_leak_count",
			`secret_leak_count INTEGER NOT NULL DEFAULT 0`,
			"adding sessions.secret_leak_count",
		},
		{
			"sessions", "secrets_rules_version",
			`secrets_rules_version TEXT NOT NULL DEFAULT ''`,
			"adding sessions.secrets_rules_version",
		},
		{
			"sessions", "session_name",
			`session_name TEXT`,
			"adding sessions.session_name",
		},
	}
	step = time.Now()
	existingColumns, err := loadExistingColumns(ctx, db, alters)
	if err != nil {
		return err
	}
	log.Printf(
		"pg schema: loaded existing columns in %s",
		time.Since(step).Round(time.Millisecond),
	)
	step = time.Now()
	tokenCoverageColumnsAdded := false
	sourceCurationColumnsAdded := false
	addedColumns, err := ensureColumns(ctx, db, existingColumns, alters)
	if err != nil {
		return err
	}
	for _, column := range addedColumns {
		switch column {
		case "has_total_output_tokens", "has_peak_context_tokens",
			"has_context_tokens", "has_output_tokens":
			tokenCoverageColumnsAdded = true
		case "source_display_name", "source_deleted_at":
			sourceCurationColumnsAdded = true
		}
	}
	log.Printf(
		"pg schema: column migration step completed in %s"+
			" (%d column(s) added)",
		time.Since(step).Round(time.Millisecond),
		len(addedColumns),
	)
	step = time.Now()
	sourceBackfilled, err := runSourceCurationBackfill(
		ctx, db, sourceCurationColumnsAdded,
	)
	if err != nil {
		return err
	}
	if sourceBackfilled {
		log.Printf(
			"pg schema: source curation baseline backfill"+
				" completed in %s",
			time.Since(step).Round(time.Millisecond),
		)
	} else {
		log.Printf(
			"pg schema: source curation baseline backfill"+
				" check completed in %s (repair skipped)",
			time.Since(step).Round(time.Millisecond),
		)
	}
	step = time.Now()
	remoteScrubbed, err := scrubProjectIdentityGitRemoteCredentialsPG(ctx, db)
	if err != nil {
		return err
	}
	if remoteScrubbed {
		log.Printf(
			"pg schema: project identity remote scrub completed in %s",
			time.Since(step).Round(time.Millisecond),
		)
	} else {
		log.Printf(
			"pg schema: project identity remote scrub check completed"+
				" in %s (repair skipped)",
			time.Since(step).Round(time.Millisecond),
		)
	}
	step = time.Now()
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_sessions_termination_status
		 ON sessions(termination_status)`,
	); err != nil {
		return fmt.Errorf(
			"creating idx_sessions_termination_status: %w", err,
		)
	}
	if err := backfillIsAutomatedPG(ctx, db); err != nil {
		return err
	}
	log.Printf(
		"pg schema: automated-session backfill step completed in %s",
		time.Since(step).Round(time.Millisecond),
	)
	step = time.Now()
	if err := createPartialIndexesPG(ctx, db); err != nil {
		return err
	}
	log.Printf(
		"pg schema: partial indexes step completed in %s",
		time.Since(step).Round(time.Millisecond),
	)
	step = time.Now()
	createContentSearchIndexesPG(ctx, db)
	log.Printf(
		"pg schema: content search index step completed in %s",
		time.Since(step).Round(time.Millisecond),
	)
	if _, err := ensureVectorBaseSchemaPG(ctx, db); err != nil {
		log.Printf("pg schema: vector schema setup failed: %v", err)
	}
	step = time.Now()
	runRepair, err := shouldRunTokenCoverageRepair(
		ctx, db, tokenCoverageColumnsAdded,
	)
	if err != nil {
		return err
	}
	if !runRepair {
		log.Printf(
			"pg schema: token coverage repair check completed"+
				" in %s (repair skipped)",
			time.Since(step).Round(time.Millisecond),
		)
		log.Printf(
			"pg schema: EnsureSchema completed in %s",
			time.Since(start).Round(time.Millisecond),
		)
		return nil
	}
	log.Printf(
		"pg schema: token coverage repair check completed"+
			" in %s (repair needed)",
		time.Since(step).Round(time.Millisecond),
	)
	step = time.Now()
	if err := backfillTokenCoverageFlags(ctx, db); err != nil {
		return err
	}
	log.Printf(
		"pg schema: token coverage backfill step completed in %s",
		time.Since(step).Round(time.Millisecond),
	)
	step = time.Now()
	if err := markTokenCoverageRepairDone(ctx, db); err != nil {
		return err
	}
	log.Printf(
		"pg schema: token coverage repair marker stored in %s",
		time.Since(step).Round(time.Millisecond),
	)
	log.Printf(
		"pg schema: EnsureSchema completed in %s",
		time.Since(start).Round(time.Millisecond),
	)
	return nil
}

// createPartialIndexesPG creates partial indexes on the PG schema.
// Idempotent via IF NOT EXISTS.
func createPartialIndexesPG(ctx context.Context, db *sql.DB) error {
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_sessions_cwd
		 ON sessions(cwd) WHERE cwd != ''`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_project_git_branch
		 ON sessions(project, git_branch) WHERE git_branch != ''`,
		`CREATE INDEX IF NOT EXISTS idx_messages_compact_boundary
		 ON messages(session_id, ordinal) WHERE is_compact_boundary = TRUE`,
		`CREATE INDEX IF NOT EXISTS idx_messages_sidechain
		 ON messages(session_id) WHERE is_sidechain = TRUE`,
		`CREATE INDEX IF NOT EXISTS idx_messages_source_uuid
		 ON messages(source_uuid) WHERE source_uuid != ''`,
		`CREATE INDEX IF NOT EXISTS idx_messages_usage_covering
		 ON messages(timestamp, session_id, ordinal, model,
		             claude_message_id, claude_request_id)
		 WHERE token_usage != ''
		   AND model != ''
		   AND model != '<synthetic>'`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_has_secret
		 ON sessions(secret_leak_count) WHERE secret_leak_count > 0`,
		// idx_tool_calls_file_path backs the cross-session Recent Edits feed.
		// Created here, after the file_path column migration, mirroring the
		// SQLite partial index so legacy schemas migrate cleanly.
		`CREATE INDEX IF NOT EXISTS idx_tool_calls_file_path
		 ON tool_calls(file_path) WHERE file_path IS NOT NULL`,
		// idx_messages_session_role backs the dense-flow unit-range boundary
		// fetch (user ordinals by session), mirroring the SQLite index.
		`CREATE INDEX IF NOT EXISTS idx_messages_session_role
		 ON messages(session_id, role)`,
	}
	for _, ddl := range indexes {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating PG index: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`DROP INDEX IF EXISTS idx_messages_usage_timestamp`,
	); err != nil {
		return fmt.Errorf("dropping legacy PG usage index: %w", err)
	}
	return nil
}

// createContentSearchIndexesPG adds a pg_trgm GIN index on messages.content so
// the content search (ILIKE '%pattern%') is index-accelerated instead of
// sequentially scanning the messages table as history grows.
//
// It is best-effort and never fails schema setup. pg_trgm is a trusted
// extension on PostgreSQL 13+, but a role without CREATE privilege (or a
// managed instance that blocks it) must still get a working — if slower —
// store. Each statement runs in its own implicit transaction, so a failed
// CREATE EXTENSION does not poison the rest of EnsureSchema. The gin_trgm_ops
// operator class lives in whatever schema pg_trgm was installed in, which may
// not be on the connection search_path (Open sets it to the target schema
// only), so the opclass is schema-qualified to the extension's namespace.
func createContentSearchIndexesPG(ctx context.Context, db *sql.DB) {
	if _, err := db.ExecContext(ctx,
		`CREATE EXTENSION IF NOT EXISTS pg_trgm`,
	); err != nil {
		log.Printf(
			"pg schema: pg_trgm unavailable, content search will scan: %v", err,
		)
		return
	}
	var extSchema string
	if err := db.QueryRowContext(ctx,
		`SELECT n.nspname FROM pg_extension e
		 JOIN pg_namespace n ON n.oid = e.extnamespace
		 WHERE e.extname = 'pg_trgm'`,
	).Scan(&extSchema); err != nil {
		log.Printf("pg schema: locating pg_trgm schema failed: %v", err)
		return
	}
	quotedExt, err := quoteIdentifier(extSchema)
	if err != nil {
		log.Printf("pg schema: invalid pg_trgm schema %q: %v", extSchema, err)
		return
	}
	// fastupdate=off keeps the index bounded: the default fastupdate=on
	// buffers inserts into a pending list that only VACUUM merges, which grows
	// unbounded when continuous ingest starves autovacuum.
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS idx_messages_content_trgm
		 ON messages USING gin (content %s.gin_trgm_ops)
		 WITH (fastupdate = off)`, quotedExt,
	)); err != nil {
		log.Printf(
			"pg schema: creating messages.content trigram index failed: %v", err,
		)
		return
	}
	// CREATE INDEX IF NOT EXISTS only applies WITH (fastupdate = off) on
	// first creation. Re-apply on every boot so stores upgraded from a
	// prior schema (which left fastupdate=on) also get the bounded index.
	if _, err := db.ExecContext(ctx,
		`ALTER INDEX idx_messages_content_trgm SET (fastupdate = off)`,
	); err != nil {
		log.Printf(
			"pg schema: disabling fastupdate on messages.content trigram index failed: %v",
			err,
		)
	}
}

// backfillIsAutomatedPG verifies is_automated for all PG
// sessions, correcting both false negatives (new patterns or
// stale imported rows) and stale false positives (patterns
// tightened since last run). The stored classifier hash records
// which classifier wrote the current audit, but it is not a
// complete integrity marker: rows can arrive from stale clients
// after the hash was stamped.
func backfillIsAutomatedPG(
	ctx context.Context, pg *sql.DB,
) error {
	_, err := backfillIsAutomatedPGWithProgress(ctx, pg)
	return err
}

// runSchemaDataRepairsPG runs the non-DDL correctness repairs that
// EnsureSchema performs: it recomputes is_automated, backfills
// source-curation baselines, scrubs stored project remotes, and repairs
// token-coverage flags. These issue only row-level writes, so the
// compatible-schema fast path can run them without the index and
// column DDL that can block concurrent pg serve reads (issue #887).
func runSchemaDataRepairsPG(ctx context.Context, db *sql.DB) error {
	if err := backfillIsAutomatedPG(ctx, db); err != nil {
		return err
	}
	if _, err := runSourceCurationBackfill(ctx, db, false); err != nil {
		return err
	}
	if _, err := scrubProjectIdentityGitRemoteCredentialsPG(ctx, db); err != nil {
		return err
	}
	runRepair, err := shouldRunTokenCoverageRepair(ctx, db, false)
	if err != nil {
		return err
	}
	if !runRepair {
		return nil
	}
	if err := backfillTokenCoverageFlags(ctx, db); err != nil {
		return err
	}
	return markTokenCoverageRepairDone(ctx, db)
}

func backfillSourceCurationBaselines(
	ctx context.Context, pg *sql.DB,
) error {
	if _, err := pg.ExecContext(ctx,
		`UPDATE sessions
		 SET source_display_name = display_name
		 WHERE source_display_name IS NULL`,
	); err != nil {
		return fmt.Errorf(
			"backfilling PG source display-name baselines: %w", err,
		)
	}
	if _, err := pg.ExecContext(ctx,
		`UPDATE sessions
		 SET source_deleted_at = deleted_at
		 WHERE source_deleted_at IS NULL`,
	); err != nil {
		return fmt.Errorf(
			"backfilling PG source deleted-at baselines: %w", err,
		)
	}
	return nil
}

func runSourceCurationBackfill(
	ctx context.Context, db *sql.DB, sourceCurationColumnsAdded bool,
) (bool, error) {
	runRepair, err := shouldRunSourceCurationBackfill(
		ctx, db, sourceCurationColumnsAdded,
	)
	if err != nil {
		return false, err
	}
	if !runRepair {
		return false, nil
	}
	if err := backfillSourceCurationBaselines(ctx, db); err != nil {
		return false, err
	}
	if err := markSourceCurationBackfillDone(ctx, db); err != nil {
		return false, err
	}
	return true, nil
}

func shouldRunSourceCurationBackfill(
	ctx context.Context, db *sql.DB, sourceCurationColumnsAdded bool,
) (bool, error) {
	if sourceCurationColumnsAdded {
		return true, nil
	}

	var done bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM sync_metadata
			WHERE key = $1
		)`,
		sourceCurationBackfillMetadataKey,
	).Scan(&done); err != nil {
		return false, fmt.Errorf(
			"probing source curation backfill metadata: %w", err,
		)
	}
	return !done, nil
}

func markSourceCurationBackfillDone(
	ctx context.Context, db *sql.DB,
) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value)
		 VALUES ($1, '1')
		 ON CONFLICT (key) DO UPDATE
		 SET value = EXCLUDED.value`,
		sourceCurationBackfillMetadataKey,
	)
	if err != nil {
		return fmt.Errorf(
			"storing source curation backfill metadata: %w", err,
		)
	}
	return nil
}

func scrubProjectIdentityGitRemoteCredentialsPG(
	ctx context.Context, db *sql.DB,
) (bool, error) {
	var done bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM sync_metadata
			WHERE key = $1
		)`,
		projectIdentityRemoteScrubMetadataKey,
	).Scan(&done); err != nil {
		return false, fmt.Errorf(
			"probing project identity remote scrub metadata: %w", err,
		)
	}
	if done {
		return false, nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		FROM project_identity_observations
		WHERE git_remote != ''
		ORDER BY project, machine, root_path, git_remote`)
	if err != nil {
		return false, fmt.Errorf(
			"listing pg project identity remotes for scrub: %w", err,
		)
	}

	type pendingScrub struct {
		obs       export.ProjectIdentityObservation
		rawRemote string
	}
	var pending []pendingScrub
	for rows.Next() {
		var obs export.ProjectIdentityObservation
		if err := rows.Scan(
			&obs.Project,
			&obs.Machine,
			&obs.RootPath,
			&obs.GitRemote,
			&obs.GitRemoteName,
			&obs.WorktreeName,
			&obs.WorktreeRootPath,
			&obs.ObservedAt,
			&obs.NormalizedRemote,
			&obs.KeySource,
			&obs.Key,
		); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf(
				"scanning pg project identity remote for scrub: %w", err,
			)
		}
		sanitized := export.SanitizeGitRemoteForStorage(obs.GitRemote)
		if sanitized == obs.GitRemote {
			continue
		}
		pending = append(pending, pendingScrub{
			obs:       obs,
			rawRemote: obs.GitRemote,
		})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, fmt.Errorf(
			"iterating pg project identity remotes for scrub: %w", err,
		)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf(
			"closing pg project identity remotes for scrub: %w", err,
		)
	}

	for _, scrub := range pending {
		obs := export.SanitizeStoredProjectIdentityObservation(scrub.obs)
		if err := upsertProjectIdentityObservation(
			ctx, db, obs, scrub.rawRemote,
		); err != nil {
			return false, fmt.Errorf(
				"upserting scrubbed pg project identity remote: %w", err,
			)
		}
		if _, err := db.ExecContext(ctx, `
			DELETE FROM project_identity_observations
			WHERE project = $1 AND machine = $2 AND root_path = $3
			  AND git_remote = $4`,
			scrub.obs.Project, scrub.obs.Machine, scrub.obs.RootPath,
			scrub.rawRemote,
		); err != nil {
			return false, fmt.Errorf(
				"removing raw pg project identity remote: %w", err,
			)
		}
	}
	if err := markProjectIdentityRemoteScrubDone(ctx, db); err != nil {
		return false, err
	}
	return len(pending) > 0, nil
}

func markProjectIdentityRemoteScrubDone(
	ctx context.Context, db *sql.DB,
) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value)
		 VALUES ($1, '1')
		 ON CONFLICT (key) DO UPDATE
		 SET value = EXCLUDED.value`,
		projectIdentityRemoteScrubMetadataKey,
	)
	if err != nil {
		return fmt.Errorf(
			"storing project identity remote scrub metadata: %w", err,
		)
	}
	return nil
}

func batchUpdateAutomatedPG(
	ctx context.Context, pg *sql.DB,
	ids []string, val bool,
) error {
	const batchSize = 500
	for i := 0; i < len(ids); i += batchSize {
		end := min(i+batchSize, len(ids))
		batch := ids[i:end]
		pb := &paramBuilder{}
		valPh := pb.add(val)
		phs := make([]string, len(batch))
		for j, id := range batch {
			phs[j] = pb.add(id)
		}
		_, err := pg.ExecContext(ctx,
			"UPDATE sessions SET is_automated = "+valPh+
				" WHERE id IN ("+
				strings.Join(phs, ",")+
				")",
			pb.args...,
		)
		if err != nil {
			return fmt.Errorf(
				"updating is_automated in PG: %w", err,
			)
		}
	}
	return nil
}

func loadExistingColumns(
	ctx context.Context, db *sql.DB, alters []columnMigration,
) (map[string]map[string]bool, error) {
	tablesSeen := map[string]bool{}
	var tables []string
	for _, a := range alters {
		if tablesSeen[a.table] {
			continue
		}
		tablesSeen[a.table] = true
		tables = append(tables, a.table)
	}

	existing := map[string]map[string]bool{}
	if len(tables) == 0 {
		return existing, nil
	}

	pb := &paramBuilder{}
	phs := make([]string, len(tables))
	for i, table := range tables {
		phs[i] = pb.add(table)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT table_name, column_name
		 FROM information_schema.columns
		 WHERE table_schema = current_schema()
		   AND table_name IN (`+strings.Join(phs, ",")+`)`,
		pb.args...,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"loading existing PG columns: %w", err,
		)
	}
	defer rows.Close()

	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return nil, fmt.Errorf(
				"scanning existing PG columns: %w", err,
			)
		}
		if existing[table] == nil {
			existing[table] = map[string]bool{}
		}
		existing[table][column] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating existing PG columns: %w", err,
		)
	}
	return existing, nil
}

func ensureColumns(
	ctx context.Context, db *sql.DB,
	existing map[string]map[string]bool,
	migrations []columnMigration,
) ([]string, error) {
	type tableAdds struct {
		table      string
		migrations []columnMigration
	}

	byTable := map[string]*tableAdds{}
	var tables []*tableAdds
	for _, migration := range migrations {
		if existing[migration.table][migration.column] {
			continue
		}
		adds := byTable[migration.table]
		if adds == nil {
			adds = &tableAdds{table: migration.table}
			byTable[migration.table] = adds
			tables = append(tables, adds)
		}
		adds.migrations = append(adds.migrations, migration)
	}

	var added []string
	for _, adds := range tables {
		quotedTable, err := quoteIdentifier(adds.table)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid PG migration table %q: %w",
				adds.table, err,
			)
		}
		clauses := make([]string, len(adds.migrations))
		for i, migration := range adds.migrations {
			clauses[i] = "ADD COLUMN IF NOT EXISTS " +
				migration.def
		}
		stmt := "ALTER TABLE " + quotedTable + " " +
			strings.Join(clauses, ", ")
		step := time.Now()
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf(
				"adding %d column(s) to %s: %w",
				len(adds.migrations), adds.table, err,
			)
		}
		log.Printf(
			"pg schema: added %d column(s) to %s in %s",
			len(adds.migrations), adds.table,
			time.Since(step).Round(time.Millisecond),
		)
		if existing[adds.table] == nil {
			existing[adds.table] = map[string]bool{}
		}
		for _, migration := range adds.migrations {
			existing[adds.table][migration.column] = true
			added = append(added, migration.column)
		}
	}
	return added, nil
}

func shouldRunTokenCoverageRepair(
	ctx context.Context, db *sql.DB, tokenCoverageColumnsAdded bool,
) (bool, error) {
	if tokenCoverageColumnsAdded {
		return true, nil
	}

	var done bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM sync_metadata
			WHERE key = $1
		)`,
		tokenCoverageRepairMetadataKey,
	).Scan(&done); err != nil {
		return false, fmt.Errorf(
			"probing token coverage repair metadata: %w", err,
		)
	}
	if done {
		return false, nil
	}

	var hasSessions bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM sessions LIMIT 1)`,
	).Scan(&hasSessions); err != nil {
		return false, fmt.Errorf(
			"probing token coverage repair sessions: %w", err,
		)
	}
	return hasSessions, nil
}

func markTokenCoverageRepairDone(
	ctx context.Context, db *sql.DB,
) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value)
		 VALUES ($1, '1')
		 ON CONFLICT (key) DO UPDATE
		 SET value = EXCLUDED.value`,
		tokenCoverageRepairMetadataKey,
	)
	if err != nil {
		return fmt.Errorf(
			"storing token coverage repair metadata: %w", err,
		)
	}
	return nil
}

func backfillTokenCoverageFlags(
	ctx context.Context, db *sql.DB,
) error {
	if _, err := backfillMessageTokenCoverage(ctx, db); err != nil {
		return err
	}
	if _, err := backfillSessionTokenCoverage(ctx, db); err != nil {
		return err
	}
	return nil
}

func backfillMessageTokenCoverage(
	ctx context.Context, db *sql.DB,
) (int, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT session_id, ordinal, token_usage, context_tokens,
			output_tokens, has_context_tokens, has_output_tokens
		 FROM messages
		 WHERE (has_context_tokens = FALSE OR has_output_tokens = FALSE)
		   AND (token_usage != ''
			OR context_tokens != 0
			OR output_tokens != 0)`,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"querying pg message token backfill candidates: %w", err,
		)
	}
	defer rows.Close()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf(
			"beginning pg message token backfill transaction: %w",
			err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`UPDATE messages
		 SET has_context_tokens = $1, has_output_tokens = $2
		 WHERE session_id = $3 AND ordinal = $4`,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"preparing pg message token backfill update: %w", err,
		)
	}
	defer stmt.Close()

	updated := 0
	for rows.Next() {
		var sessionID, tokenUsage string
		var ordinal, contextTokens, outputTokens int
		var hasContext, hasOutput bool
		if err := rows.Scan(
			&sessionID, &ordinal, &tokenUsage, &contextTokens,
			&outputTokens, &hasContext, &hasOutput,
		); err != nil {
			return updated, fmt.Errorf(
				"scanning pg message token backfill candidate: %w", err,
			)
		}
		backfilledContext, backfilledOutput := inferTokenCoverage(
			[]byte(tokenUsage), contextTokens, outputTokens,
			hasContext, hasOutput,
		)
		if backfilledContext == hasContext &&
			backfilledOutput == hasOutput {
			continue
		}
		if _, err := stmt.ExecContext(
			ctx, backfilledContext, backfilledOutput,
			sessionID, ordinal,
		); err != nil {
			return updated, fmt.Errorf(
				"updating pg message token backfill %s/%d: %w",
				sessionID, ordinal, err,
			)
		}
		updated++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return updated, fmt.Errorf(
			"committing pg message token backfill transaction: %w",
			err,
		)
	}
	return updated, nil
}

func backfillSessionTokenCoverage(
	ctx context.Context, conn *sql.DB,
) (int, error) {
	candidates, err := loadPGSessionCoverageCandidates(ctx, conn)
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	msgCoverage, err := batchLoadPGMessageCoverage(
		ctx, conn, candidates,
	)
	if err != nil {
		return 0, err
	}

	updates := db.ComputeSessionCoverageUpdates(
		candidates, msgCoverage,
	)
	if len(updates) == 0 {
		return 0, nil
	}
	return applyPGSessionCoverageUpdates(ctx, conn, updates)
}

func loadPGSessionCoverageCandidates(
	ctx context.Context, conn *sql.DB,
) ([]db.SessionCoverageCandidate, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT id, total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens
		 FROM sessions
		 WHERE has_total_output_tokens = FALSE
		    OR has_peak_context_tokens = FALSE`,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"querying pg session token backfill candidates: %w",
			err,
		)
	}
	defer rows.Close()

	var candidates []db.SessionCoverageCandidate
	for rows.Next() {
		var c db.SessionCoverageCandidate
		if err := rows.Scan(
			&c.ID, &c.TotalOutputTokens,
			&c.PeakContextTokens, &c.HasTotal, &c.HasPeak,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning pg session token backfill candidate: %w",
				err,
			)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

func batchLoadPGMessageCoverage(
	ctx context.Context, conn *sql.DB,
	candidates []db.SessionCoverageCandidate,
) (map[string][2]bool, error) {
	coverage := map[string][2]bool{}
	for start := 0; start < len(candidates); start += tokenCoverageBackfillBatchSize {
		end := min(
			start+tokenCoverageBackfillBatchSize,
			len(candidates),
		)
		batch := candidates[start:end]
		args := make([]any, len(batch))
		placeholders := make([]string, len(batch))
		for i, c := range batch {
			args[i] = c.ID
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
		rows, err := conn.QueryContext(ctx,
			`SELECT session_id, has_context_tokens,
				has_output_tokens
			 FROM messages
			 WHERE session_id IN (`+strings.Join(placeholders, ",")+`)`,
			args...,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"querying pg session message coverage: %w", err,
			)
		}
		for rows.Next() {
			var sessionID string
			var hasContext, hasOutput bool
			if err := rows.Scan(
				&sessionID, &hasContext, &hasOutput,
			); err != nil {
				rows.Close()
				return nil, fmt.Errorf(
					"scanning pg session message coverage: %w",
					err,
				)
			}
			entry := coverage[sessionID]
			entry[0] = entry[0] || hasContext
			entry[1] = entry[1] || hasOutput
			coverage[sessionID] = entry
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return coverage, nil
}

func applyPGSessionCoverageUpdates(
	ctx context.Context, conn *sql.DB,
	updates []db.SessionCoverageUpdate,
) (int, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf(
			"beginning pg session token backfill transaction: %w",
			err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`UPDATE sessions
		 SET has_total_output_tokens = $1,
		     has_peak_context_tokens = $2
		 WHERE id = $3`,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"preparing pg session token backfill update: %w", err,
		)
	}
	defer stmt.Close()

	updated := 0
	for _, u := range updates {
		if _, err := stmt.ExecContext(
			ctx, u.HasTotal, u.HasPeak, u.ID,
		); err != nil {
			return updated, fmt.Errorf(
				"updating pg session token backfill %s: %w",
				u.ID, err,
			)
		}
		updated++
	}
	if err := tx.Commit(); err != nil {
		return updated, fmt.Errorf(
			"committing pg session token backfill transaction: %w",
			err,
		)
	}
	return updated, nil
}

func inferTokenCoverage(
	tokenUsage []byte,
	contextTokens, outputTokens int,
	hasContext, hasOutput bool,
) (bool, bool) {
	return parser.InferTokenPresence(
		tokenUsage, contextTokens, outputTokens,
		hasContext, hasOutput,
	)
}

// CheckSchemaCompat verifies that the PG schema has all columns
func pgHasTable(ctx context.Context, db *sql.DB, name string) bool {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1",
		name,
	).Scan(&n)
	return err == nil && n == 1
}

// pgHasIndex reports whether an index of the given name exists in the
// current schema.
func pgHasIndex(ctx context.Context, db *sql.DB, name string) bool {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM pg_indexes
		 WHERE schemaname = current_schema() AND indexname = $1`,
		name,
	).Scan(&n)
	return err == nil && n == 1
}

// required by query paths. This is a read-only probe that works
// against any PG role. Returns nil if compatible, or an error
// describing what is missing.
func CheckSchemaCompat(
	ctx context.Context, db *sql.DB,
) error {
	rows, err := db.QueryContext(ctx,
		`SELECT updated_at, `+pgSessionCols+`
		 FROM sessions LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"sessions table missing required columns: %w",
			err,
		)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx,
		`SELECT source_display_name, source_deleted_at
		 FROM sessions LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"sessions table missing curation columns: %w", err,
		)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx,
		`SELECT id FROM excluded_sessions LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"excluded_sessions table missing required columns: %w", err,
		)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx,
		`SELECT session_id, alias_id FROM session_aliases LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"session_aliases table missing required columns: %w", err,
		)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx,
		`SELECT call_index, file_path FROM tool_calls LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"tool_calls table missing required columns: %w",
			err,
		)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx,
		`SELECT session_id, ordinal, role, content, thinking_text,
			timestamp, has_thinking, has_tool_use,
			content_length, is_system, model, token_usage,
			context_tokens, output_tokens,
			has_context_tokens, has_output_tokens,
			claude_message_id, claude_request_id,
			source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain,
			is_compact_boundary
		 FROM messages LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"messages table missing required columns: %w",
			err,
		)
	}
	rows.Close()
	rows, err = db.QueryContext(ctx,
		`SELECT session_id, created_at
		 FROM starred_sessions LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"starred_sessions table missing required columns: %w",
			err,
		)
	}
	rows.Close()
	rows, err = db.QueryContext(ctx,
		`SELECT id, session_id, message_id, ordinal,
			source_uuid, note, created_at
		 FROM pinned_messages LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"pinned_messages table missing required columns: %w",
			err,
		)
	}
	rows.Close()
	rows, err = db.QueryContext(ctx,
		`SELECT total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens
		 FROM sessions LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"sessions table missing token columns: %w",
			err,
		)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx,
		`SELECT quality_signal_version, short_prompt_count,
			unstructured_start, missing_success_criteria_count,
			missing_verification_count, duplicate_prompt_count,
			no_code_context_count, runaway_tool_loop_count
		 FROM sessions LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"sessions table missing quality signal columns: %w",
			err,
		)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx,
		`SELECT event_index FROM tool_result_events LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"tool_result_events table missing required columns: %w",
			err,
		)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx,
		`SELECT id FROM usage_events LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"usage_events table missing required columns: %w",
			err,
		)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx,
		`SELECT id, type, date_from, date_to, project, agent,
			model, prompt, content, kind, schema_version,
			template_id, template_version, aggregate_hash,
			cache_key, cache_status, provenance_json,
			structured_json, created_at
		 FROM insights LIMIT 0`)
	if err != nil {
		return fmt.Errorf("insights table missing required columns: %w", err)
	}
	rows.Close()

	if pgHasTable(ctx, db, "cursor_usage_events") {
		rows, err = db.QueryContext(ctx,
			`SELECT id, occurred_at, model, kind,
				input_tokens, output_tokens,
				cache_write_tokens, cache_read_tokens,
				charged_cents, cursor_token_fee,
				user_id, user_email, is_headless, dedup_key
			 FROM cursor_usage_events LIMIT 0`)
		if err != nil {
			return fmt.Errorf(
				"cursor_usage_events table missing required columns: %w",
				err,
			)
		}
		rows.Close()
	}

	rows, err = db.QueryContext(ctx,
		`SELECT id, session_id, rule_name, confidence, location_kind,
			message_ordinal, call_index, event_index,
			match_start, match_end, match_index,
			redacted_match, rules_version
		 FROM secret_findings LIMIT 0`)
	if err != nil {
		return fmt.Errorf("secret_findings table missing required columns: %w", err)
	}
	rows.Close()
	rows, err = db.QueryContext(ctx,
		`SELECT project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		 FROM project_identity_observations LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"project_identity_observations table missing required columns: %w",
			err,
		)
	}
	rows.Close()
	return nil
}

// checkPushSchemaCompat verifies schema elements that only push needs. PG serve
// never reads sync_metadata or owner_marker, so they live outside
// CheckSchemaCompat (which gates read-only serve startup) and are checked only
// on the push fast path.
func checkPushSchemaCompat(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT key, value FROM sync_metadata LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"sync_metadata table missing required columns: %w", err)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx,
		`SELECT owner_marker FROM sessions LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"sessions table missing push ownership columns: %w", err)
	}
	rows.Close()
	return nil
}

// pushSchemaCurrent reports whether the PG schema has everything a push
// needs. CheckSchemaCompat covers the read and PG serve write paths but does
// not require push-only sync_metadata or sessions.owner_marker (verified by
// checkPushSchemaCompat), model_pricing (always queried by syncModelPricing)
// or cursor_usage_events (written by syncCursorUsageEvents), so probe those
// explicitly. It also requires the cursor dedup index, which the cursor usage
// insert relies on for ON CONFLICT dedup. When any of these is missing the
// caller must run EnsureSchema so push migrates the schema instead of failing
// or duplicating rows.
func pushSchemaCurrent(ctx context.Context, db *sql.DB) bool {
	if err := CheckSchemaCompat(ctx, db); err != nil {
		return false
	}
	if err := checkPushSchemaCompat(ctx, db); err != nil {
		return false
	}
	if !pgHasTable(ctx, db, "model_pricing") ||
		!pgHasTable(ctx, db, "project_identity_observations") ||
		!pgHasTable(ctx, db, "cursor_usage_events") {
		return false
	}
	// bulkInsertCursorUsageEvents dedups via a targetless ON CONFLICT
	// DO NOTHING, which only suppresses duplicates when this partial
	// unique index exists. Fall back to EnsureSchema when it is missing
	// so repeated pushes cannot duplicate cursor usage rows.
	return pgHasIndex(ctx, db, "idx_cursor_usage_events_dedup")
}

// CheckDataVersionCompat rejects PG datasets containing rows written by a
// newer agentsview parser. PG does not have SQLite's global user_version, so
// the highest session data_version is the compatibility marker.
func CheckDataVersionCompat(ctx context.Context, pg *sql.DB) error {
	var version int
	err := pg.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(data_version), 0) FROM sessions`,
	).Scan(&version)
	if err != nil {
		if isUndefinedTable(err) || isUndefinedColumn(err) {
			return nil
		}
		return fmt.Errorf("checking PG data version: %w", err)
	}
	if version > db.CurrentDataVersion() {
		return &db.DataVersionTooNewError{
			DatabaseVersion: version,
			BinaryVersion:   db.CurrentDataVersion(),
		}
	}
	return nil
}

// IsReadOnlyError returns true when the error indicates a PG
// read-only or insufficient-privilege condition (SQLSTATE 25006
// or 42501). Uses pgconn.PgError for reliable SQLSTATE matching.
func IsReadOnlyError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "25006" || pgErr.Code == "42501"
	}
	return false
}
