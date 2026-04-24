package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/parser"
)

const tokenCoverageRepairMetadataKey = "token_coverage_repair_v1"
const tokenCoverageBackfillBatchSize = 1000

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
    project            TEXT NOT NULL,
    agent              TEXT NOT NULL,
    first_message      TEXT,
    display_name       TEXT,
    created_at         TIMESTAMPTZ,
    started_at         TIMESTAMPTZ,
    ended_at           TIMESTAMPTZ,
    deleted_at         TIMESTAMPTZ,
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
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

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

CREATE TABLE IF NOT EXISTS model_pricing (
    model_pattern TEXT PRIMARY KEY,
    input_per_mtok DOUBLE PRECISION NOT NULL DEFAULT 0,
    output_per_mtok DOUBLE PRECISION NOT NULL DEFAULT 0,
    cache_creation_per_mtok DOUBLE PRECISION NOT NULL DEFAULT 0,
    cache_read_per_mtok DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL DEFAULT ''
);

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
    FOREIGN KEY (session_id)
        REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_calls_dedup
    ON tool_calls (session_id, message_ordinal, call_index);

CREATE INDEX IF NOT EXISTS idx_tool_calls_session
    ON tool_calls (session_id);

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
	quoted, err := quoteIdentifier(schema)
	if err != nil {
		return fmt.Errorf("invalid schema name: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		"CREATE SCHEMA IF NOT EXISTS "+quoted,
	); err != nil {
		return fmt.Errorf("creating pg schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, coreDDL); err != nil {
		return fmt.Errorf("creating pg tables: %w", err)
	}

	// Idempotent column additions for forward compatibility.
	alters := []struct {
		table  string
		column string
		stmt   string
		desc   string
	}{
		{
			"sessions", "deleted_at",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ`,
			"adding sessions.deleted_at",
		},
		{
			"sessions", "created_at",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ`,
			"adding sessions.created_at",
		},
		{
			"sessions", "total_output_tokens",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS total_output_tokens
			 INT NOT NULL DEFAULT 0`,
			"adding sessions.total_output_tokens",
		},
		{
			"sessions", "peak_context_tokens",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS peak_context_tokens
			 INT NOT NULL DEFAULT 0`,
			"adding sessions.peak_context_tokens",
		},
		{
			"sessions", "has_total_output_tokens",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS has_total_output_tokens
			 BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.has_total_output_tokens",
		},
		{
			"sessions", "has_peak_context_tokens",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS has_peak_context_tokens
			 BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.has_peak_context_tokens",
		},
		{
			"messages", "model",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS model
			 TEXT NOT NULL DEFAULT ''`,
			"adding messages.model",
		},
		{
			"messages", "token_usage",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS token_usage
			 TEXT NOT NULL DEFAULT ''`,
			"adding messages.token_usage",
		},
		{
			"messages", "context_tokens",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS context_tokens
			 INT NOT NULL DEFAULT 0`,
			"adding messages.context_tokens",
		},
		{
			"messages", "output_tokens",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS output_tokens
			 INT NOT NULL DEFAULT 0`,
			"adding messages.output_tokens",
		},
		{
			"messages", "has_context_tokens",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS has_context_tokens
			 BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding messages.has_context_tokens",
		},
		{
			"messages", "has_output_tokens",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS has_output_tokens
			 BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding messages.has_output_tokens",
		},
		{
			"messages", "claude_message_id",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS claude_message_id
			 TEXT NOT NULL DEFAULT ''`,
			"adding messages.claude_message_id",
		},
		{
			"messages", "claude_request_id",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS claude_request_id
			 TEXT NOT NULL DEFAULT ''`,
			"adding messages.claude_request_id",
		},
		{
			"tool_calls", "call_index",
			`ALTER TABLE tool_calls
			 ADD COLUMN IF NOT EXISTS call_index
			 INT NOT NULL DEFAULT 0`,
			"adding tool_calls.call_index",
		},
		{
			"sessions", "is_automated",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS is_automated
			 BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.is_automated",
		},
		{
			"sessions", "tool_failure_signal_count",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS tool_failure_signal_count
			 INT NOT NULL DEFAULT 0`,
			"adding sessions.tool_failure_signal_count",
		},
		{
			"sessions", "tool_retry_count",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS tool_retry_count
			 INT NOT NULL DEFAULT 0`,
			"adding sessions.tool_retry_count",
		},
		{
			"sessions", "edit_churn_count",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS edit_churn_count
			 INT NOT NULL DEFAULT 0`,
			"adding sessions.edit_churn_count",
		},
		{
			"sessions", "consecutive_failure_max",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS consecutive_failure_max
			 INT NOT NULL DEFAULT 0`,
			"adding sessions.consecutive_failure_max",
		},
		{
			"sessions", "outcome",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS outcome
			 TEXT NOT NULL DEFAULT 'unknown'`,
			"adding sessions.outcome",
		},
		{
			"sessions", "outcome_confidence",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS outcome_confidence
			 TEXT NOT NULL DEFAULT 'low'`,
			"adding sessions.outcome_confidence",
		},
		{
			"sessions", "ended_with_role",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS ended_with_role
			 TEXT NOT NULL DEFAULT ''`,
			"adding sessions.ended_with_role",
		},
		{
			"sessions", "final_failure_streak",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS final_failure_streak
			 INT NOT NULL DEFAULT 0`,
			"adding sessions.final_failure_streak",
		},
		{
			"sessions", "signals_pending_since",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS signals_pending_since
			 TEXT`,
			"adding sessions.signals_pending_since",
		},
		{
			"sessions", "compaction_count",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS compaction_count
			 INT NOT NULL DEFAULT 0`,
			"adding sessions.compaction_count",
		},
		{
			"sessions", "mid_task_compaction_count",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS mid_task_compaction_count
			 INT NOT NULL DEFAULT 0`,
			"adding sessions.mid_task_compaction_count",
		},
		{
			"sessions", "context_pressure_max",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS context_pressure_max
			 DOUBLE PRECISION`,
			"adding sessions.context_pressure_max",
		},
		{
			"sessions", "health_score",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS health_score
			 INT`,
			"adding sessions.health_score",
		},
		{
			"sessions", "health_grade",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS health_grade
			 TEXT`,
			"adding sessions.health_grade",
		},
		{
			"sessions", "has_tool_calls",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS has_tool_calls
			 BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.has_tool_calls",
		},
		{
			"sessions", "has_context_data",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS has_context_data
			 BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.has_context_data",
		},
		{
			"sessions", "data_version",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS data_version
			 INT NOT NULL DEFAULT 0`,
			"adding sessions.data_version",
		},
		{
			"sessions", "cwd",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS cwd
			 TEXT NOT NULL DEFAULT ''`,
			"adding sessions.cwd",
		},
		{
			"sessions", "git_branch",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS git_branch
			 TEXT NOT NULL DEFAULT ''`,
			"adding sessions.git_branch",
		},
		{
			"sessions", "source_session_id",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS source_session_id
			 TEXT NOT NULL DEFAULT ''`,
			"adding sessions.source_session_id",
		},
		{
			"sessions", "source_version",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS source_version
			 TEXT NOT NULL DEFAULT ''`,
			"adding sessions.source_version",
		},
		{
			"sessions", "parser_malformed_lines",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS parser_malformed_lines
			 INT NOT NULL DEFAULT 0`,
			"adding sessions.parser_malformed_lines",
		},
		{
			"sessions", "is_truncated",
			`ALTER TABLE sessions
			 ADD COLUMN IF NOT EXISTS is_truncated
			 BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.is_truncated",
		},
		{
			"messages", "source_type",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS source_type
			 TEXT NOT NULL DEFAULT ''`,
			"adding messages.source_type",
		},
		{
			"messages", "source_subtype",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS source_subtype
			 TEXT NOT NULL DEFAULT ''`,
			"adding messages.source_subtype",
		},
		{
			"messages", "source_uuid",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS source_uuid
			 TEXT NOT NULL DEFAULT ''`,
			"adding messages.source_uuid",
		},
		{
			"messages", "source_parent_uuid",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS source_parent_uuid
			 TEXT NOT NULL DEFAULT ''`,
			"adding messages.source_parent_uuid",
		},
		{
			"messages", "is_sidechain",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS is_sidechain
			 BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding messages.is_sidechain",
		},
		{
			"messages", "is_compact_boundary",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS is_compact_boundary
			 BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding messages.is_compact_boundary",
		},
		{
			"messages", "thinking_text",
			`ALTER TABLE messages
			 ADD COLUMN IF NOT EXISTS thinking_text
			 TEXT NOT NULL DEFAULT ''`,
			"adding messages.thinking_text",
		},
	}
	tokenCoverageColumnsAdded := false
	for _, a := range alters {
		added, err := ensureColumn(ctx, db, a.table, a.column, a.stmt)
		if err != nil {
			return fmt.Errorf("%s: %w", a.desc, err)
		}
		switch a.column {
		case "has_total_output_tokens", "has_peak_context_tokens",
			"has_context_tokens", "has_output_tokens":
			tokenCoverageColumnsAdded = tokenCoverageColumnsAdded || added
		}
	}
	if err := backfillIsAutomatedPG(ctx, db); err != nil {
		return err
	}
	if err := createPartialIndexesPG(ctx, db); err != nil {
		return err
	}
	runRepair, err := shouldRunTokenCoverageRepair(
		ctx, db, tokenCoverageColumnsAdded,
	)
	if err != nil {
		return err
	}
	if !runRepair {
		return nil
	}
	if err := backfillTokenCoverageFlags(ctx, db); err != nil {
		return err
	}
	if err := markTokenCoverageRepairDone(ctx, db); err != nil {
		return err
	}
	return nil
}

// createPartialIndexesPG creates partial indexes on the PG schema.
// Idempotent via IF NOT EXISTS.
func createPartialIndexesPG(ctx context.Context, db *sql.DB) error {
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_sessions_cwd
		 ON sessions(cwd) WHERE cwd != ''`,
		`CREATE INDEX IF NOT EXISTS idx_messages_compact_boundary
		 ON messages(session_id, ordinal) WHERE is_compact_boundary = TRUE`,
		`CREATE INDEX IF NOT EXISTS idx_messages_sidechain
		 ON messages(session_id) WHERE is_sidechain = TRUE`,
		`CREATE INDEX IF NOT EXISTS idx_messages_source_uuid
		 ON messages(source_uuid) WHERE source_uuid != ''`,
	}
	for _, ddl := range indexes {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating PG index: %w", err)
		}
	}
	return nil
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
	current := db.ClassifierHash()
	var stored string
	err := pg.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = $1`,
		db.ClassifierHashKey,
	).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(
			"probing PG classifier hash: %w", err,
		)
	}

	rows, err := pg.QueryContext(ctx,
		`SELECT id, first_message, user_message_count,
			is_automated
		 FROM sessions`)
	if err != nil {
		return fmt.Errorf(
			"querying PG automated backfill candidates: %w",
			err,
		)
	}
	defer rows.Close()

	var setIDs, clearIDs []string
	for rows.Next() {
		var id string
		var fm sql.NullString
		var umc int
		var rowAutomated bool
		if err := rows.Scan(
			&id, &fm, &umc, &rowAutomated,
		); err != nil {
			return fmt.Errorf(
				"scanning PG backfill candidate: %w", err,
			)
		}
		want := false
		if fm.Valid {
			want = umc <= 1 && db.IsAutomatedSession(fm.String)
		}
		if want && !rowAutomated {
			setIDs = append(setIDs, id)
		} else if !want && rowAutomated {
			clearIDs = append(clearIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if err := batchUpdateAutomatedPG(
		ctx, pg, setIDs, true,
	); err != nil {
		return err
	}
	if err := batchUpdateAutomatedPG(
		ctx, pg, clearIDs, false,
	); err != nil {
		return err
	}

	if len(setIDs) > 0 || len(clearIDs) > 0 {
		log.Printf(
			"pg migration: recomputed is_automated"+
				" (set %d, cleared %d)",
			len(setIDs), len(clearIDs),
		)
	}

	if _, err := pg.ExecContext(ctx,
		`INSERT INTO sync_metadata (key, value)
		 VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE
		 SET value = EXCLUDED.value`,
		db.ClassifierHashKey, current,
	); err != nil {
		return fmt.Errorf(
			"storing PG classifier hash: %w", err,
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

func ensureColumn(
	ctx context.Context, db *sql.DB,
	table, column, stmt string,
) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = $1
			  AND column_name = $2
		)`,
		table, column,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf(
			"probing %s.%s: %w", table, column, err,
		)
	}
	if exists {
		return false, nil
	}
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return false, err
	}
	return true, nil
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
// required by query paths. This is a read-only probe that works
// against any PG role. Returns nil if compatible, or an error
// describing what is missing.
func CheckSchemaCompat(
	ctx context.Context, db *sql.DB,
) error {
	rows, err := db.QueryContext(ctx,
		`SELECT id, created_at, deleted_at, updated_at
		 FROM sessions LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"sessions table missing required columns: %w",
			err,
		)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx,
		`SELECT call_index FROM tool_calls LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"tool_calls table missing required columns: %w",
			err,
		)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx,
		`SELECT is_system, model, token_usage, context_tokens,
			output_tokens, has_context_tokens, has_output_tokens
		 FROM messages LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"messages table missing required columns: %w",
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
		`SELECT event_index FROM tool_result_events LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"tool_result_events table missing required columns: %w",
			err,
		)
	}
	rows.Close()
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
