package db

import (
	"crypto/rand"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	_ "github.com/mattn/go-sqlite3"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/parser"
)

// dataVersion tracks parser changes that require a full
// re-sync. Increment this when parsing logic changes in ways
// that affect stored data (e.g. new fields extracted, content
// formatting changes). Old databases with a lower user_version
// trigger a non-destructive re-sync (mtime reset + skip cache
// clear) so existing session data is preserved.
//
// Bumped to 25: Codex parser now also links codex_app subagents
// via collab_agent_spawn_end event_msgs, wait_agent function
// calls, and agent_path subagent notifications. Existing rows
// need re-parsing so codex_app subagent linkage works.
//
// (24: Codex parser now annotates spawn_agent tool calls
// with subagent_session_id once the spawned agent id is known.
// Existing rows need re-parsing so inline subagent expansion can
// resolve child sessions from persisted tool call metadata.)
//
// (23: split termination_status into awaiting_user vs
// clean (Claude end_turn / Codex task_complete vs other clean
// stops); Codex parser now classifies based on task lifecycle
// events. Existing rows need re-parsing so the new awaiting_user
// value populates correctly.)
//
// (22: added termination_status column to sessions; existing
// rows need re-parsing so the Claude classifier can populate
// the new column.)
//
// (21: Copilot parser now reads workspace.yaml to use the
// LLM-generated session name as first_message. Existing
// directory-format sessions where workspace.yaml.mtime <=
// events.jsonl.mtime would be permanently skipped without this
// bump, leaving first_message as the raw first user message.)
//
// (20: Claude parser now surfaces queued_command attachment
// entries (user messages typed mid-tool-call) as real user
// messages with source_subtype="queued_command".)
//
// (19: Copilot parser now filters synthetic skill context
// user messages.)
//
// (18: Claude parser now skips /clear and /effort
// command envelopes when computing first_message, so sessions
// that opened with one of those commands show the next real
// user message in the sidebar instead of the command text.
// Re-parsing rewrites first_message with the new logic.)
//
// (17: Codex <skill> template filtering.)
// (16: <turn_aborted> system messages.)
const dataVersion = 25

const tokenCoverageRepairStatsKey = "token_coverage_repair_v1"

// ClassifierHashKey is the shared SQLite stats / PG sync_metadata key
// under which the current is_automated classifier hash is stored.
// Exported so the postgres package and the classifier rebuild CLI
// reference one definition instead of repeating the literal.
const ClassifierHashKey = "is_automated_classifier_hash"

//go:embed schema.sql
var schemaSQL string

// messagesADTriggerDDL is the AFTER DELETE trigger that mirrors row
// removals into the FTS5 shadow tables. ReplaceSessionMessages drops
// this trigger inside its transaction (replacing N per-row FTS deletes
// with a single bulk INSERT...SELECT) and then re-runs this DDL to
// restore it before commit. Keeping the statement in one place keeps
// the two installation sites byte-identical.
const messagesADTriggerDDL = `
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content)
        VALUES('delete', old.id, old.content);
END;
`

const schemaFTS = `
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    content='messages',
    content_rowid='id',
    tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;
` + messagesADTriggerDDL + `
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content)
        VALUES('delete', old.id, old.content);
    INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;
`

// DB manages a write connection and a read-only pool.
// The reader and writer fields use atomic.Pointer so that
// concurrent HTTP handler goroutines can safely read while
// Reopen/CloseConnections swap the underlying *sql.DB.
type DB struct {
	path      string
	writer    atomic.Pointer[sql.DB]
	reader    atomic.Pointer[sql.DB]
	mu        sync.Mutex // serializes writes
	retired   []*sql.DB  // old pools kept open for in-flight reads
	dataStale bool       // set by Open when user_version < dataVersion

	cursorMu     sync.RWMutex
	cursorSecret []byte

	customPricing map[string]config.CustomModelRate
}

// getReader returns the current read-only connection pool.
func (db *DB) getReader() *sql.DB { return db.reader.Load() }

// getWriter returns the current write connection.
func (db *DB) getWriter() *sql.DB { return db.writer.Load() }

// Path returns the file path of the database.
func (db *DB) Path() string {
	return db.path
}

// ReadOnly returns false for the local SQLite store.
func (db *DB) ReadOnly() bool { return false }

func (db *DB) SetCustomPricing(p map[string]config.CustomModelRate) {
	db.customPricing = p
}

// SetCursorSecret updates the secret key used for cursor signing.
func (db *DB) SetCursorSecret(secret []byte) {
	db.cursorMu.Lock()
	defer db.cursorMu.Unlock()
	db.cursorSecret = append([]byte(nil), secret...)
}

// makeDSN builds a SQLite connection string with shared pragmas.
func makeDSN(path string, readOnly bool) string {
	params := url.Values{}
	params.Set("_journal_mode", "WAL")
	params.Set("_busy_timeout", "5000")
	params.Set("_foreign_keys", "ON")
	params.Set("_mmap_size", "268435456")
	params.Set("_cache_size", "-64000")
	if readOnly {
		params.Set("mode", "ro")
	} else {
		params.Set("_synchronous", "NORMAL")
	}
	return path + "?" + params.Encode()
}

// Open creates or opens a SQLite database at the given path.
// It configures WAL mode, mmap, and returns a DB with separate
// writer and reader connections.
//
// If an existing database has an outdated schema (missing
// columns), it is deleted and recreated from scratch.
// If the schema is current but the data version is stale,
// the database is preserved and file mtimes are reset to
// trigger a re-sync on the next cycle.
func Open(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	schemaStale, dataStale, err := probeDatabase(path)
	if err != nil {
		return nil, fmt.Errorf("checking schema: %w", err)
	}
	if schemaStale {
		if err := dropDatabase(path); err != nil {
			return nil, fmt.Errorf(
				"rebuilding database: %w", err,
			)
		}
	}

	d, err := openAndInit(path)
	if err != nil {
		return nil, err
	}

	if err := d.migrateColumns(); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrating columns: %w", err)
	}

	if dataStale && !schemaStale {
		d.dataStale = true
		log.Printf(
			"data version outdated; full resync required",
		)
	} else {
		// Only stamp user_version when data is current.
		// When data is stale, preserve the old version so
		// the "needs resync" state survives process restarts
		// until ResyncAll completes successfully.
		if err := d.setDataVersion(); err != nil {
			d.Close()
			return nil, fmt.Errorf(
				"setting data version: %w", err,
			)
		}
	}

	return d, nil
}

// probeDatabase checks an existing database for schema and
// data staleness. Returns (schemaStale, dataStale, err).
// schemaStale means required columns are missing and the DB
// must be dropped and recreated. dataStale means the schema
// is fine but user_version < dataVersion, requiring a
// non-destructive re-sync.
func probeDatabase(
	path string,
) (schemaStale, dataStale bool, err error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf(
			"checking database file: %w", err,
		)
	}
	conn, err := sql.Open("sqlite3", makeDSN(path, true))
	if err != nil {
		return false, false, fmt.Errorf(
			"probing schema: %w", err,
		)
	}
	defer conn.Close()

	schema, err := needsSchemaRebuild(conn)
	if err != nil {
		return false, false, err
	}
	if schema {
		return true, false, nil
	}

	data, err := needsDataResync(conn)
	if err != nil {
		return false, false, err
	}
	return false, data, nil
}

// needsSchemaRebuild probes for required columns that may be
// missing in databases created by older releases. If any are
// absent, the DB must be dropped and recreated.
func needsSchemaRebuild(conn *sql.DB) (bool, error) {
	probes := []struct {
		table  string
		column string
	}{
		{"sessions", "parent_session_id"},
		{"insights", "date_from"},
		{"tool_calls", "tool_use_id"},
		{"sessions", "user_message_count"},
		{"sessions", "relationship_type"},
		{"tool_calls", "subagent_session_id"},
	}
	for _, p := range probes {
		var count int
		err := conn.QueryRow(fmt.Sprintf(
			"SELECT count(*) FROM pragma_table_info('%s')"+
				" WHERE name = '%s'",
			p.table, p.column,
		)).Scan(&count)
		if err != nil {
			return false, fmt.Errorf(
				"probing schema (%s.%s): %w",
				p.table, p.column, err,
			)
		}
		if count == 0 {
			return true, nil
		}
	}
	return false, nil
}

// needsDataResync checks whether user_version is behind the
// current dataVersion, indicating parser changes that require
// re-processing existing files.
func needsDataResync(conn *sql.DB) (bool, error) {
	var version int
	err := conn.QueryRow(
		"PRAGMA user_version",
	).Scan(&version)
	if err != nil {
		return false, fmt.Errorf(
			"probing data version: %w", err,
		)
	}
	return version < dataVersion, nil
}

// migrateColumns adds columns introduced by this branch to
// databases created by older releases. Each migration is
// idempotent — it only runs when the column is missing.
func (db *DB) migrateColumns() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	w := db.getWriter()

	migrations := []struct {
		table  string
		column string
		ddl    string
	}{
		{
			"sessions", "display_name",
			"ALTER TABLE sessions ADD COLUMN display_name TEXT",
		},
		{
			"sessions", "deleted_at",
			"ALTER TABLE sessions ADD COLUMN deleted_at TEXT",
		},
		{
			"messages", "is_system",
			"ALTER TABLE messages ADD COLUMN is_system INTEGER NOT NULL DEFAULT 0",
		},
		{
			"messages", "model",
			"ALTER TABLE messages ADD COLUMN model TEXT NOT NULL DEFAULT ''",
		},
		{
			"messages", "token_usage",
			"ALTER TABLE messages ADD COLUMN token_usage TEXT NOT NULL DEFAULT ''",
		},
		{
			"messages", "context_tokens",
			"ALTER TABLE messages ADD COLUMN context_tokens INTEGER NOT NULL DEFAULT 0",
		},
		{
			"messages", "output_tokens",
			"ALTER TABLE messages ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0",
		},
		{
			"messages", "has_context_tokens",
			"ALTER TABLE messages ADD COLUMN has_context_tokens INTEGER NOT NULL DEFAULT 0",
		},
		{
			"messages", "has_output_tokens",
			"ALTER TABLE messages ADD COLUMN has_output_tokens INTEGER NOT NULL DEFAULT 0",
		},
		{
			"messages", "claude_message_id",
			"ALTER TABLE messages ADD COLUMN claude_message_id TEXT NOT NULL DEFAULT ''",
		},
		{
			"messages", "claude_request_id",
			"ALTER TABLE messages ADD COLUMN claude_request_id TEXT NOT NULL DEFAULT ''",
		},
		{
			"messages", "source_type",
			"ALTER TABLE messages ADD COLUMN source_type TEXT NOT NULL DEFAULT ''",
		},
		{
			"messages", "source_subtype",
			"ALTER TABLE messages ADD COLUMN source_subtype TEXT NOT NULL DEFAULT ''",
		},
		{
			"messages", "source_uuid",
			"ALTER TABLE messages ADD COLUMN source_uuid TEXT NOT NULL DEFAULT ''",
		},
		{
			"messages", "source_parent_uuid",
			"ALTER TABLE messages ADD COLUMN source_parent_uuid TEXT NOT NULL DEFAULT ''",
		},
		{
			"messages", "is_sidechain",
			"ALTER TABLE messages ADD COLUMN is_sidechain INTEGER NOT NULL DEFAULT 0",
		},
		{
			"messages", "is_compact_boundary",
			"ALTER TABLE messages ADD COLUMN is_compact_boundary INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "total_output_tokens",
			"ALTER TABLE sessions ADD COLUMN total_output_tokens INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "peak_context_tokens",
			"ALTER TABLE sessions ADD COLUMN peak_context_tokens INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "has_total_output_tokens",
			"ALTER TABLE sessions ADD COLUMN has_total_output_tokens INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "has_peak_context_tokens",
			"ALTER TABLE sessions ADD COLUMN has_peak_context_tokens INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "local_modified_at",
			"ALTER TABLE sessions ADD COLUMN local_modified_at TEXT",
		},
		{
			"sessions", "is_automated",
			"ALTER TABLE sessions ADD COLUMN is_automated INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "tool_failure_signal_count",
			"ALTER TABLE sessions ADD COLUMN tool_failure_signal_count INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "tool_retry_count",
			"ALTER TABLE sessions ADD COLUMN tool_retry_count INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "edit_churn_count",
			"ALTER TABLE sessions ADD COLUMN edit_churn_count INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "consecutive_failure_max",
			"ALTER TABLE sessions ADD COLUMN consecutive_failure_max INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "outcome",
			"ALTER TABLE sessions ADD COLUMN outcome TEXT NOT NULL DEFAULT 'unknown'",
		},
		{
			"sessions", "outcome_confidence",
			"ALTER TABLE sessions ADD COLUMN outcome_confidence TEXT NOT NULL DEFAULT 'low'",
		},
		{
			"sessions", "ended_with_role",
			"ALTER TABLE sessions ADD COLUMN ended_with_role TEXT NOT NULL DEFAULT ''",
		},
		{
			"sessions", "final_failure_streak",
			"ALTER TABLE sessions ADD COLUMN final_failure_streak INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "signals_pending_since",
			"ALTER TABLE sessions ADD COLUMN signals_pending_since TEXT",
		},
		{
			"sessions", "compaction_count",
			"ALTER TABLE sessions ADD COLUMN compaction_count INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "context_pressure_max",
			"ALTER TABLE sessions ADD COLUMN context_pressure_max REAL",
		},
		{
			"sessions", "health_score",
			"ALTER TABLE sessions ADD COLUMN health_score INTEGER",
		},
		{
			"sessions", "health_grade",
			"ALTER TABLE sessions ADD COLUMN health_grade TEXT",
		},
		{
			"sessions", "has_tool_calls",
			"ALTER TABLE sessions ADD COLUMN has_tool_calls INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "has_context_data",
			"ALTER TABLE sessions ADD COLUMN has_context_data INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "data_version",
			"ALTER TABLE sessions ADD COLUMN data_version INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "mid_task_compaction_count",
			"ALTER TABLE sessions ADD COLUMN mid_task_compaction_count INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "cwd",
			"ALTER TABLE sessions ADD COLUMN cwd TEXT NOT NULL DEFAULT ''",
		},
		{
			"sessions", "git_branch",
			"ALTER TABLE sessions ADD COLUMN git_branch TEXT NOT NULL DEFAULT ''",
		},
		{
			"sessions", "source_session_id",
			"ALTER TABLE sessions ADD COLUMN source_session_id TEXT NOT NULL DEFAULT ''",
		},
		{
			"sessions", "source_version",
			"ALTER TABLE sessions ADD COLUMN source_version TEXT NOT NULL DEFAULT ''",
		},
		{
			"sessions", "parser_malformed_lines",
			"ALTER TABLE sessions ADD COLUMN parser_malformed_lines INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "is_truncated",
			"ALTER TABLE sessions ADD COLUMN is_truncated INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "file_inode",
			"ALTER TABLE sessions ADD COLUMN file_inode INTEGER",
		},
		{
			"sessions", "file_device",
			"ALTER TABLE sessions ADD COLUMN file_device INTEGER",
		},
		{
			"messages", "thinking_text",
			"ALTER TABLE messages ADD COLUMN thinking_text TEXT NOT NULL DEFAULT ''",
		},
		{
			"sessions", "termination_status",
			"ALTER TABLE sessions ADD COLUMN termination_status TEXT",
		},
	}

	for _, m := range migrations {
		var count int
		err := w.QueryRow(fmt.Sprintf(
			"SELECT count(*) FROM pragma_table_info('%s')"+
				" WHERE name = '%s'",
			m.table, m.column,
		)).Scan(&count)
		if err != nil {
			return fmt.Errorf(
				"probing %s.%s: %w",
				m.table, m.column, err,
			)
		}
		if count == 0 {
			if _, err := w.Exec(m.ddl); err != nil {
				return fmt.Errorf(
					"adding %s.%s: %w",
					m.table, m.column, err,
				)
			}
			log.Printf(
				"migration: added column %s.%s",
				m.table, m.column,
			)
		}
	}
	if err := db.createPartialIndexesLocked(w); err != nil {
		return err
	}
	if err := db.backfillIsAutomatedLocked(w); err != nil {
		return err
	}

	if _, err := w.Exec(
		`CREATE INDEX IF NOT EXISTS idx_sessions_termination_status
		 ON sessions(termination_status)`,
	); err != nil {
		return fmt.Errorf(
			"creating idx_sessions_termination_status: %w", err,
		)
	}

	if _, err := w.Exec(`
		CREATE TABLE IF NOT EXISTS remote_skipped_files (
			host       TEXT NOT NULL,
			path       TEXT NOT NULL,
			file_mtime INTEGER NOT NULL,
			PRIMARY KEY (host, path)
		)`,
	); err != nil {
		return fmt.Errorf(
			"creating remote_skipped_files: %w", err,
		)
	}

	runRepair, err := db.shouldRunTokenCoverageRepairLocked(w)
	if err != nil {
		return err
	}
	if !runRepair {
		return nil
	}
	if err := db.backfillTokenCoverageFlagsLocked(w); err != nil {
		return err
	}
	if err := db.markTokenCoverageRepairDoneLocked(w); err != nil {
		return err
	}
	return nil
}

// createPartialIndexesLocked creates partial indexes that are not
// covered by the initial schema DDL. Idempotent via IF NOT EXISTS.
func (db *DB) createPartialIndexesLocked(w *sql.DB) error {
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_sessions_cwd
		 ON sessions(cwd) WHERE cwd != ''`,
		`CREATE INDEX IF NOT EXISTS idx_messages_compact_boundary
		 ON messages(session_id, ordinal) WHERE is_compact_boundary = 1`,
		`CREATE INDEX IF NOT EXISTS idx_messages_sidechain
		 ON messages(session_id) WHERE is_sidechain = 1`,
		`CREATE INDEX IF NOT EXISTS idx_messages_source_uuid
		 ON messages(source_uuid) WHERE source_uuid != ''`,
	}
	for _, ddl := range indexes {
		if _, err := w.Exec(ddl); err != nil {
			return fmt.Errorf("creating index: %w", err)
		}
	}
	return nil
}

// backfillIsAutomatedLocked verifies is_automated for all
// sessions, correcting both false negatives (new patterns or
// stale imported rows) and stale false positives (patterns
// tightened since last run). The stored classifier hash records
// which classifier wrote the current audit, but it is not a
// complete integrity marker: rows can be copied from older DBs
// or stale remote machines after the hash was stamped.
func (db *DB) backfillIsAutomatedLocked(w *sql.DB) error {
	current := ClassifierHash()
	var stored string
	err := w.QueryRow(
		`SELECT value FROM stats WHERE key = ?`,
		ClassifierHashKey,
	).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(
			"probing classifier hash: %w", err,
		)
	}

	rows, err := w.Query(
		`SELECT id, first_message, user_message_count,
			is_automated
		 FROM sessions`,
	)
	if err != nil {
		return fmt.Errorf(
			"querying automated backfill candidates: %w", err,
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
				"scanning backfill candidate: %w", err,
			)
		}
		want := false
		if fm.Valid {
			want = umc <= 1 && IsAutomatedSession(fm.String)
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

	if err := batchUpdateAutomated(
		w, setIDs, 1,
	); err != nil {
		return err
	}
	if err := batchUpdateAutomated(
		w, clearIDs, 0,
	); err != nil {
		return err
	}

	if len(setIDs) > 0 || len(clearIDs) > 0 {
		log.Printf(
			"migration: recomputed is_automated"+
				" (set %d, cleared %d)",
			len(setIDs), len(clearIDs),
		)
	}

	// stats.value is INTEGER affinity; SQLite stores hex text
	// here verbatim. Switching to STRICT tables would require
	// moving this row to a TEXT-typed table.
	if _, err := w.Exec(
		`INSERT INTO stats (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		ClassifierHashKey, current,
	); err != nil {
		return fmt.Errorf(
			"storing classifier hash: %w", err,
		)
	}
	return nil
}

// ForceBackfillIsAutomated reclassifies is_automated across
// every session, ignoring any cached classifier hash. ResyncAll
// calls this after CopyOrphanedDataFrom because orphan-copied
// rows carry is_automated values computed against the *old* DB's
// classifier set; the temp DB's at-Open backfill already ran on
// an empty table and stamped the current hash, so without this
// call those rows would be permanently stuck with stale flags.
func (db *DB) ForceBackfillIsAutomated() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	w := db.getWriter()
	if _, err := w.Exec(
		`DELETE FROM stats WHERE key = ?`,
		ClassifierHashKey,
	); err != nil {
		return fmt.Errorf(
			"clearing classifier hash: %w", err,
		)
	}
	return db.backfillIsAutomatedLocked(w)
}

func batchUpdateAutomated(
	w *sql.DB, ids []string, val int,
) error {
	const batchSize = 500
	for i := 0; i < len(ids); i += batchSize {
		end := min(i+batchSize, len(ids))
		batch := ids[i:end]
		args := make([]any, len(batch)+1)
		phs := make([]string, len(batch))
		args[0] = val
		for j, id := range batch {
			args[j+1] = id
			phs[j] = "?"
		}
		_, err := w.Exec(
			"UPDATE sessions"+
				" SET is_automated = ?,"+
				"     local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')"+
				" WHERE id IN ("+
				strings.Join(phs, ",")+
				")",
			args...,
		)
		if err != nil {
			return fmt.Errorf(
				"updating is_automated: %w", err,
			)
		}
	}
	return nil
}

func (db *DB) shouldRunTokenCoverageRepairLocked(
	w *sql.DB,
) (bool, error) {
	var done int
	if err := w.QueryRow(
		`SELECT count(*)
		 FROM stats
		 WHERE key = ? AND value != 0`,
		tokenCoverageRepairStatsKey,
	).Scan(&done); err != nil {
		return false, fmt.Errorf(
			"probing token coverage repair marker: %w", err,
		)
	}
	return done == 0, nil
}

func (db *DB) markTokenCoverageRepairDoneLocked(
	w *sql.DB,
) error {
	if _, err := w.Exec(
		`INSERT INTO stats (key, value)
		 VALUES (?, 1)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		tokenCoverageRepairStatsKey,
	); err != nil {
		return fmt.Errorf(
			"storing token coverage repair marker: %w", err,
		)
	}
	return nil
}

func (db *DB) backfillTokenCoverageFlagsLocked(
	w *sql.DB,
) error {
	msgUpdates, err := db.backfillMessageTokenCoverageLocked(w)
	if err != nil {
		return err
	}
	sessUpdates, err := db.backfillSessionTokenCoverageLocked(w)
	if err != nil {
		return err
	}
	if msgUpdates > 0 || sessUpdates > 0 {
		log.Printf(
			"migration: backfilled token coverage flags (%d messages, %d sessions)",
			msgUpdates, sessUpdates,
		)
	}
	return nil
}

func (db *DB) backfillMessageTokenCoverageLocked(
	w *sql.DB,
) (int, error) {
	candidates, err := db.messageTokenCoverageBackfillCandidatesLocked(w)
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	tx, err := w.Begin()
	if err != nil {
		return 0, fmt.Errorf(
			"beginning message token backfill transaction: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(
		`UPDATE messages
		 SET has_context_tokens = ?, has_output_tokens = ?
		 WHERE id = ?`,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"preparing message token backfill update: %w", err,
		)
	}
	defer stmt.Close()

	for _, candidate := range candidates {
		if _, err := stmt.Exec(
			candidate.hasContext, candidate.hasOutput, candidate.id,
		); err != nil {
			return 0, fmt.Errorf(
				"updating message token backfill %d: %w",
				candidate.id, err,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf(
			"committing message token backfill transaction: %w",
			err,
		)
	}
	return len(candidates), nil
}

func (db *DB) messageTokenCoverageBackfillCandidatesLocked(
	w *sql.DB,
) ([]messageTokenCoverageBackfillCandidate, error) {
	rows, err := w.Query(
		`SELECT id, token_usage, context_tokens, output_tokens,
			has_context_tokens, has_output_tokens
		 FROM messages
		 WHERE (has_context_tokens = 0 OR has_output_tokens = 0)
		   AND (token_usage != ''
			OR context_tokens != 0
			OR output_tokens != 0)`,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"querying message token backfill candidates: %w", err,
		)
	}
	defer rows.Close()

	var candidates []messageTokenCoverageBackfillCandidate
	for rows.Next() {
		var id int64
		var tokenUsage string
		var contextTokens, outputTokens int
		var hasContextTokens, hasOutputTokens bool
		if err := rows.Scan(
			&id, &tokenUsage, &contextTokens,
			&outputTokens, &hasContextTokens,
			&hasOutputTokens,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning message token backfill candidate: %w", err,
			)
		}
		hasContext, hasOutput := parser.InferTokenPresence(
			[]byte(tokenUsage), contextTokens, outputTokens,
			hasContextTokens, hasOutputTokens,
		)
		if hasContext == hasContextTokens &&
			hasOutput == hasOutputTokens {
			continue
		}
		candidates = append(candidates, messageTokenCoverageBackfillCandidate{
			id:         id,
			hasContext: hasContext,
			hasOutput:  hasOutput,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

type messageTokenCoverageBackfillCandidate struct {
	id         int64
	hasContext bool
	hasOutput  bool
}

const tokenCoverageBackfillBatchSize = 1000

func (db *DB) backfillSessionTokenCoverageLocked(
	w *sql.DB,
) (int, error) {
	candidates, err := db.loadSessionCoverageCandidates(w)
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	msgCoverage, err := db.batchLoadMessageCoverage(
		w, candidates,
	)
	if err != nil {
		return 0, err
	}

	updates := ComputeSessionCoverageUpdates(
		candidates, msgCoverage,
	)
	if len(updates) == 0 {
		return 0, nil
	}
	return db.applySessionCoverageUpdates(w, updates)
}

func (db *DB) loadSessionCoverageCandidates(
	w *sql.DB,
) ([]SessionCoverageCandidate, error) {
	rows, err := w.Query(
		`SELECT id, total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens
		 FROM sessions
		 WHERE has_total_output_tokens = 0
		    OR has_peak_context_tokens = 0`,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"querying session token backfill candidates: %w", err,
		)
	}
	defer rows.Close()

	var candidates []SessionCoverageCandidate
	for rows.Next() {
		var c SessionCoverageCandidate
		if err := rows.Scan(
			&c.ID, &c.TotalOutputTokens,
			&c.PeakContextTokens, &c.HasTotal, &c.HasPeak,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning session token backfill candidate: %w",
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

func (db *DB) batchLoadMessageCoverage(
	w *sql.DB,
	candidates []SessionCoverageCandidate,
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
			placeholders[i] = "?"
		}
		rows, err := w.Query(
			`SELECT session_id, has_context_tokens,
				has_output_tokens
			 FROM messages
			 WHERE session_id IN (`+strings.Join(placeholders, ",")+`)`,
			args...,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"querying message coverage: %w", err,
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
					"scanning message coverage: %w", err,
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

func (db *DB) applySessionCoverageUpdates(
	w *sql.DB,
	updates []SessionCoverageUpdate,
) (int, error) {
	tx, err := w.Begin()
	if err != nil {
		return 0, fmt.Errorf(
			"beginning session token backfill transaction: %w",
			err,
		)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(
		`UPDATE sessions
		 SET has_total_output_tokens = ?,
		     has_peak_context_tokens = ?
		 WHERE id = ?`,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"preparing session token backfill update: %w", err,
		)
	}
	defer stmt.Close()

	for _, u := range updates {
		if _, err := stmt.Exec(
			u.HasTotal, u.HasPeak, u.ID,
		); err != nil {
			return 0, fmt.Errorf(
				"updating session token backfill %s: %w",
				u.ID, err,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf(
			"committing session token backfill transaction: %w",
			err,
		)
	}
	return len(updates), nil
}

// NeedsResync reports whether the database was opened with a
// stale data version, indicating the caller should trigger a
// full resync (build fresh DB, copy orphaned data, swap)
// rather than an incremental sync.
func (db *DB) NeedsResync() bool {
	return db.dataStale
}

// CurrentDataVersion returns the current parser data version.
func CurrentDataVersion() int {
	return dataVersion
}

// Vacuum runs VACUUM on the database to reclaim space.
func (db *DB) Vacuum() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec("VACUUM")
	return err
}

func dropDatabase(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil &&
			!os.IsNotExist(err) {
			return fmt.Errorf(
				"removing %s: %w", path+suffix, err,
			)
		}
	}
	return nil
}

func openAndInit(path string) (*DB, error) {
	writer, err := sql.Open("sqlite3", makeDSN(path, false))
	if err != nil {
		return nil, fmt.Errorf("opening writer: %w", err)
	}
	writer.SetMaxOpenConns(1)

	reader, err := sql.Open("sqlite3", makeDSN(path, true))
	if err != nil {
		writer.Close()
		return nil, fmt.Errorf("opening reader: %w", err)
	}
	reader.SetMaxOpenConns(4)

	db := &DB{path: path}
	db.writer.Store(writer)
	db.reader.Store(reader)

	db.cursorSecret = make([]byte, 32)
	if _, err := rand.Read(db.cursorSecret); err != nil {
		writer.Close()
		reader.Close()
		return nil, fmt.Errorf(
			"generating cursor secret: %w", err,
		)
	}

	if err := db.init(); err != nil {
		db.Close()
		return nil, fmt.Errorf("initializing schema: %w", err)
	}
	return db, nil
}

// DropFTS drops the FTS table and its triggers. This makes
// bulk message delete+reinsert fast by avoiding per-row FTS
// index updates. Call RebuildFTS after to restore search.
func (db *DB) DropFTS() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	stmts := []string{
		"DROP TRIGGER IF EXISTS messages_ai",
		"DROP TRIGGER IF EXISTS messages_ad",
		"DROP TRIGGER IF EXISTS messages_au",
		"DROP TABLE IF EXISTS messages_fts",
	}
	w := db.getWriter()
	for _, s := range stmts {
		if _, err := w.Exec(s); err != nil {
			return fmt.Errorf("drop fts (%s): %w", s, err)
		}
	}
	return nil
}

// RebuildFTS recreates the FTS table, triggers, and
// repopulates the index from the messages table.
func (db *DB) RebuildFTS() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	w := db.getWriter()
	if _, err := w.Exec(schemaFTS); err != nil {
		return fmt.Errorf("recreate fts: %w", err)
	}
	_, err := w.Exec(
		"INSERT INTO messages_fts(messages_fts)" +
			" VALUES('rebuild')",
	)
	if err != nil {
		return fmt.Errorf("rebuild fts index: %w", err)
	}
	return nil
}

// HasFTS checks if Full Text Search is available.
func (db *DB) HasFTS() bool {
	// We need to actually try to access the table, because it might exist
	// in sqlite_master but fail to load if the fts5 module is missing
	// in the current runtime.
	_, err := db.getReader().Exec(
		"SELECT 1 FROM messages_fts LIMIT 1",
	)
	return err == nil
}

// setDataVersion stamps the current dataVersion into
// user_version, but never downgrades a higher version left
// by a newer build. Called by Open() only when data is
// current (not stale), so the marker survives until
// ResyncAll completes.
func (db *DB) setDataVersion() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	var current int
	if err := db.getWriter().QueryRow(
		"PRAGMA user_version",
	).Scan(&current); err != nil {
		return fmt.Errorf("reading data version: %w", err)
	}
	if current >= dataVersion {
		return nil
	}

	_, err := db.getWriter().Exec(
		fmt.Sprintf("PRAGMA user_version = %d", dataVersion),
	)
	if err != nil {
		return fmt.Errorf("setting data version: %w", err)
	}
	return nil
}

func (db *DB) init() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	w := db.getWriter()
	if _, err := w.Exec(schemaSQL); err != nil {
		return err
	}

	// Add result_content column to tool_calls if not present
	// (non-destructive migration for existing databases).
	var rcCount int
	if err := w.QueryRow(
		`SELECT count(*) FROM pragma_table_info('tool_calls')` +
			` WHERE name = 'result_content'`,
	).Scan(&rcCount); err != nil {
		return fmt.Errorf("probing result_content column: %w", err)
	}
	if rcCount == 0 {
		if _, err := w.Exec(
			`ALTER TABLE tool_calls ADD COLUMN result_content TEXT`,
		); err != nil {
			return fmt.Errorf("adding result_content column: %w", err)
		}
	}

	// Check if FTS table exists before trying to create it
	var ftsCount int
	if err := w.QueryRow(
		"SELECT count(*) FROM sqlite_master" +
			" WHERE type='table' AND name='messages_fts'",
	).Scan(&ftsCount); err != nil {
		return fmt.Errorf("checking fts table: %w", err)
	}
	hadFTS := ftsCount > 0

	// Attempt to initialize FTS. Failure is non-fatal
	// (might be missing module).
	if _, err := w.Exec(schemaFTS); err != nil {
		if !strings.Contains(
			err.Error(), "no such module",
		) {
			return fmt.Errorf("initializing FTS: %w", err)
		}
	} else if !hadFTS {
		// Schema init succeeded and we didn't have FTS
		// before. Populate the index for existing messages.
		if _, err := w.Exec(
			"INSERT INTO messages_fts(messages_fts)" +
				" VALUES('rebuild')",
		); err != nil {
			return fmt.Errorf("backfilling FTS: %w", err)
		}
	}

	return nil
}

// Close closes both writer and reader connections, plus any
// retired pools left over from previous Reopen calls.
func (db *DB) Close() error {
	db.mu.Lock()
	w := db.getWriter()
	r := db.getReader()
	retired := db.retired
	db.retired = nil
	db.mu.Unlock()

	errs := []error{w.Close(), r.Close()}
	for _, p := range retired {
		errs = append(errs, p.Close())
	}
	return errors.Join(errs...)
}

// CloseConnections closes both connections without reopening,
// releasing file locks so the database file can be renamed.
// Also drains any retired pools from previous Reopen calls.
// Callers must call Reopen afterwards to restore service.
func (db *DB) CloseConnections() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	errs := []error{
		db.getWriter().Close(),
		db.getReader().Close(),
	}
	for _, p := range db.retired {
		errs = append(errs, p.Close())
	}
	db.retired = nil
	return errors.Join(errs...)
}

// Reopen closes and reopens both connections to the same
// path. Used after an atomic file swap to pick up the new
// database contents. Preserves cursorSecret.
func (db *DB) Reopen() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.reopenLocked()
}

// reopenLocked performs the reopen while db.mu is already
// held. New connections are opened before closing old ones
// so the struct never points at closed handles on failure.
func (db *DB) reopenLocked() error {
	writer, err := sql.Open(
		"sqlite3", makeDSN(db.path, false),
	)
	if err != nil {
		return fmt.Errorf("reopening writer: %w", err)
	}
	writer.SetMaxOpenConns(1)

	reader, err := sql.Open(
		"sqlite3", makeDSN(db.path, true),
	)
	if err != nil {
		writer.Close()
		return fmt.Errorf("reopening reader: %w", err)
	}
	reader.SetMaxOpenConns(4)

	// Close pools from any previous reopen. They have been
	// retired for at least one full Reopen cycle, so all
	// in-flight queries on them have long since completed.
	for _, p := range db.retired {
		if err := p.Close(); err != nil {
			log.Printf(
				"warning: closing retired db pool: %v", err,
			)
		}
	}
	db.retired = db.retired[:0]

	oldWriter := db.writer.Swap(writer)
	oldReader := db.reader.Swap(reader)

	// Retire the just-swapped pools. Concurrent readers that
	// loaded the old pointer before the swap may still have
	// in-flight queries; these pools will be closed on the
	// next Reopen, CloseConnections, or Close call.
	db.retired = append(db.retired, oldWriter, oldReader)
	return nil
}

// Update executes fn within a write lock and transaction.
// The transaction is committed if fn returns nil, rolled back
// otherwise.
func (db *DB) Update(fn func(tx *sql.Tx) error) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Reader returns the read-only connection pool.
func (db *DB) Reader() *sql.DB {
	return db.getReader()
}

// GetSyncState reads a value from the pg_sync_state table.
func (db *DB) GetSyncState(key string) (string, error) {
	var value string
	err := db.getReader().QueryRow(
		"SELECT value FROM pg_sync_state WHERE key = ?", key,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// SetSyncState writes a value to the pg_sync_state table.
func (db *DB) SetSyncState(key, value string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		`INSERT INTO pg_sync_state (key, value)
		 VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}
