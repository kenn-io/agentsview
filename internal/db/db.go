package db

import (
	"context"
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
	"time"

	_ "github.com/mattn/go-sqlite3"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

// dataVersion tracks parser changes that require a full
// re-sync. Increment this when parsing logic changes in ways
// that affect stored data (e.g. new fields extracted, content
// formatting changes). Old databases with a lower user_version
// trigger a non-destructive re-sync (mtime reset + skip cache
// clear) so existing session data is preserved.
//
// Bumped to 50: parser-derived text is sanitized for PostgreSQL
// parity and fingerprints. Existing rows need re-parsing so stored
// message/session shape, timestamps, roles, token counts, and content
// fingerprints are based on the sanitized parse output.
//
// Bumped to 49: incremental JSONL resume now persists next_ordinal
// and last_entry_uuid, and Claude incremental parsing restores
// subagent linkage plus boundary fallback behavior from stored state.
// Existing rows need re-parsing so incremental appends resume from
// the raw parser tip instead of the filtered stored tail.
//
// Bumped to 48: the Codex and OpenCode parsers now persist cwd.
// Existing Codex and OpenCode rows need re-parsing so worktree
// project mappings can be applied to those sessions.
//
// Bumped to 47: the Visual Studio Copilot trace parser now
// persists per-chat model and token usage from gen_ai.usage
// attributes. Existing Visual Studio Copilot rows need re-parsing
// so Usage reports include those sessions.
//
// Bumped to 45: the Codex parser now imports renamed session
// titles from session_index.jsonl. Existing Codex rows need
// re-parsing so their titles reflect later renames.
//
// Bumped to 52: the Pi parser now persists per-message
// source_uuid and source_parent_uuid lineage. Existing Pi rows need
// re-parsing so stored message trees gain the new lineage anchors.
//
// Bumped to 44: the VSCode Copilot parser now extracts per-turn
// token usage (promptTokens/outputTokens) and the resolved model from
// result.metadata into usage events, session output totals, and peak
// context, so existing VSCode Copilot rows need re-parsing to gain
// usage and cost.
//
// Bumped to 43: the Pi parser now persists cwd from the
// session header. Existing Pi rows need re-parsing so their cwd column
// is populated.
//
// Bumped to 42: the Claude parser now infers subagent parent
// relationships from Claude Code companion directories
// (<session>/subagents/agent-*.jsonl) and resolves externalized
// tool-results content from <session>/tool-results. Existing Claude
// rows need re-parsing so companion subagents are linked and
// persisted tool outputs replace preview placeholders.
//
// Bumped to 41: the Cursor parser now stores structured tool-call
// input JSON for text transcripts and normalizes ApplyPatch calls as
// edits. Existing Cursor rows need re-parsing so archived ApplyPatch
// calls render with the new patch-aware UI.
//
// (40: the Codex parser now suppresses the parent history
// that `codex fork` replays at the top of a forked rollout, which was
// double counted as the fork's own messages and token usage, and kept
// the fork's own session id instead of letting the replayed parent
// session_meta overwrite it. Existing forked rows persist the
// double-counted totals under the parent's identity, so they need
// re-parsing to be rewritten with post-fork activity only. Resync's
// orphan copy also skips stale Codex rows whose file_path was
// reparsed under a different session id, so the old parent-ID row
// does not survive the rebuild when the parent's own file is gone
// (see CopyOrphanedDataFromExcluding.)
//
// (39: the Antigravity wire-walk hardened its output
// invariants (issue #648): model-name candidates must be printable,
// collected strings replace NUL bytes with U+FFFD, nanos values
// outside the protobuf Timestamp range no longer match
// timestamp-shaped fields, token blocks whose output+reasoning sum
// breaches the plausibility cap are rejected, and parses truncate
// at a total-fields allocation budget. Existing Antigravity rows
// may hold content/model/usage values the parser no longer
// produces and need re-parsing.)
//
// (38: two Antigravity parsing changes. (a) The Antigravity
// CLI parser extracts generatorMetadata token usage from agy-reader
// trajectory sidecars: usage events for legacy .pb sessions (and .db
// sessions without gen_metadata) and per-message model/token
// attribution on sidecar transcripts, so existing Antigravity CLI rows
// need re-parsing to gain usage data. (b) The gen_metadata model-name
// heuristic now rejects non-printable candidates: field 21/19
// sometimes carries a nested protobuf message whose low bytes are
// valid UTF-8, and the raw fragment (including NUL bytes) was
// persisted as messages.model, so existing Antigravity rows need
// re-parsing to clear the corrupt model values.)
//
// (37: Antigravity and Antigravity CLI parsers now extract
// per-generation model names and token usage (input, output,
// reasoning) from the gen_metadata table into per-message token
// fields, session totals, and usage events. Existing Antigravity
// rows need re-parsing so usage and cost reports include older
// sessions.)
//
// (36: the Antigravity CLI .pb branch dropped its sidecar
// mtime gate: a trajectory.json older than the .pb was rejected in
// favor of low-fidelity history fallbacks, but the encrypted .pb has no
// richer decode, .pb files are no longer produced, and their sidecars
// are final. Existing .pb rows whose sidecar was previously rejected
// need re-parsing to pick up the full-fidelity transcript.)
//
// (35: Antigravity CLI parser changed persisted data in two
// ways: (a) project inference (GitHub #579) now resolves a workspace
// for sessions whose history.jsonl rows lack a conversationId, changing
// stored session.Project, and (b) .db sessions now prefer the
// agy-reader trajectory.json sidecar (structured tool calls/results and
// thinking) over the heuristic SQLite decode. Existing Antigravity CLI
// rows would otherwise be skipped while file size/mtime and
// data_version look current, so they need a non-destructive resync to
// pick up inferred projects and sidecar-fidelity transcripts.)
//
// (34: added session_name column to sessions; existing rows
// need re-parsing so the parser can populate agent-provided session
// names (Claude /rename and native titles from other agents) into the
// new session_name field.)
//
// (33: Claude parser now skips content-free /usage probe
// sessions (the only user turn is the /usage command), and the Codex
// parser drops the initial user prompt when Codex re-emits it verbatim
// while continuing a task across turns. Existing rows need re-parsing
// so /usage probe sessions are dropped from the archive and Codex
// code-review sessions are recounted to a single user turn and
// re-flagged as automated.)
//
// (32: Antigravity DB parsers now filter internal protocol strings
// from visible message content, remove raw step headers, prefer
// prompt-like user text, and merge matching Antigravity CLI history
// prompts when DB decoding drops short user turns. Existing Antigravity
// DB rows need re-parsing so previously indexed noisy or assistant-only
// transcripts are rewritten.)
//
// (31: Copilot shutdown usage events use positional DedupKey to
// handle multi-segment sessions correctly.)
//
// (30: Hermes parser no longer treats cost_status
// "included" as a confident $0 when cost_source is "none"/empty (its
// default for models it does not price, e.g. gpt-5.5). Such rows now
// leave cost_usd nil so they are catalog-priced. Existing Hermes rows
// need re-parsing so their usage cost reflects the catalog instead of a
// baked-in $0.)
//
// (29: secret findings now record tool_result_event
// coordinates by the persisted slice position (matching
// tool_result_events.event_index) instead of the parser's raw event
// index. Existing rows need re-scanning so stored findings normalize
// and `secrets list --reveal` can re-read the source.)
//
// (28: Gemini parser now persists normalized
// (Anthropic-style) per-message token_usage JSON instead of the raw
// tokens object, and rolls thoughts tokens into OutputTokens so
// per-message and session output totals match the cost JSON.
// Existing Gemini rows need re-parsing so usage and cost reports
// reflect the new shape and include thoughts tokens.)
//
// (27: Piebald parser now persists normalized per-message
// token_usage JSON. Existing Piebald rows need re-parsing so Usage
// reports can include older Piebald sessions.)
//
// (26: Claude parser now (a) links Task / Agent tool
// calls to child subagent sessions via toolUseResult.agentId
// when queue/progress mappings are absent, populating
// tool_calls.subagent_session_id, and (b) merges additive
// same-message.id assistant chunks instead of keeping only the
// last entry, preserving sibling tool_use blocks and
// progressively-built text. Existing rows need re-parsing so
// these linkages and merged content show up.)
//
// (25: Codex parser now also links codex_app subagents
// via collab_agent_spawn_end event_msgs, wait_agent function
// calls, and agent_path subagent notifications. Existing rows
// need re-parsing so codex_app subagent linkage works.)
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
// (46: Cursor and Codex parsers now infer skill_name from
// read-like SKILL.md tool calls. Covers Read/ReadFile tool
// calls and Codex/Cursor shell reads across the Cursor JSONL
// and plain-text transcript paths, with ~ expansion, relative
// paths resolved against the tool-call workdir or session cwd,
// glob/space handling, and grep/rg pattern-vs-file
// classification, so historical skill usage is backfilled on
// re-parse.)
//
// (52: Pi source lineage reparse.)
// (51: Gemini cumulative-to-delta token reparse.)
// (17: Codex <skill> template filtering.)
// (16: <turn_aborted> system messages.)
const dataVersion = 52

const tokenCoverageRepairStatsKey = "token_coverage_repair_v1"

const (
	walJournalSizeLimitBytes = 256 * 1024 * 1024
	walCheckpointThreshold   = 512 * 1024 * 1024
	walCheckpointInterval    = 5 * time.Minute
	walCheckpointAttempts    = 3
	walCheckpointRetryDelay  = 250 * time.Millisecond
)

// ErrWALCheckpointBusy reports that a truncate checkpoint could not reset
// the WAL because another connection still had pages pinned.
var ErrWALCheckpointBusy = errors.New("wal checkpoint busy")

// DataVersionTooNewError reports that an archive was written by a newer
// agentsview parser than the current binary understands.
type DataVersionTooNewError struct {
	DatabaseVersion int
	BinaryVersion   int
}

func (e *DataVersionTooNewError) Error() string {
	return fmt.Sprintf(
		"database data version %d is newer than this agentsview binary's data version %d. Run \"agentsview update\" or install the latest AgentsView release before serving or syncing this archive",
		e.DatabaseVersion, e.BinaryVersion,
	)
}

// IsDataVersionTooNew reports whether err wraps DataVersionTooNewError.
func IsDataVersionTooNew(err error) bool {
	var tooNew *DataVersionTooNewError
	return errors.As(err, &tooNew)
}

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
	connMu    sync.RWMutex
	retired   []*sql.DB // old pools kept open for in-flight reads
	readOnly  bool
	dataStale atomic.Bool // set by Open when user_version < dataVersion

	cursorMu     sync.RWMutex
	cursorSecret []byte

	customPricing map[string]config.CustomModelRate

	checkpointMu   sync.Mutex
	checkpointStop chan struct{}
	checkpointDone chan struct{}
}

// Reader exposes guarded read-only query operations. It intentionally does
// not expose the underlying *sql.DB so callers cannot retain a raw pool across
// Reopen.
type Reader interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryContext(
		ctx context.Context, query string, args ...any,
	) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	QueryRowContext(
		ctx context.Context, query string, args ...any,
	) *sql.Row
}

type readerHandle struct {
	owner *DB
}

type errRow struct {
	err error
}

func (r errRow) Scan(...any) error { return r.err }

type writerHandle struct {
	owner *DB
}

func (r *readerHandle) current() *sql.DB {
	return r.owner.reader.Load()
}

func (r *readerHandle) Exec(
	query string, args ...any,
) (sql.Result, error) {
	r.owner.connMu.RLock()
	defer r.owner.connMu.RUnlock()
	return r.current().Exec(query, args...)
}

func (r *readerHandle) Query(
	query string, args ...any,
) (*sql.Rows, error) {
	r.owner.connMu.RLock()
	defer r.owner.connMu.RUnlock()
	return r.current().Query(query, args...)
}

func (r *readerHandle) QueryContext(
	ctx context.Context, query string, args ...any,
) (*sql.Rows, error) {
	r.owner.connMu.RLock()
	defer r.owner.connMu.RUnlock()
	return r.current().QueryContext(ctx, query, args...)
}

func (r *readerHandle) QueryRow(
	query string, args ...any,
) *sql.Row {
	r.owner.connMu.RLock()
	defer r.owner.connMu.RUnlock()
	return r.current().QueryRow(query, args...)
}

func (r *readerHandle) QueryRowContext(
	ctx context.Context, query string, args ...any,
) *sql.Row {
	r.owner.connMu.RLock()
	defer r.owner.connMu.RUnlock()
	return r.current().QueryRowContext(ctx, query, args...)
}

func (w *writerHandle) current() (*sql.DB, error) {
	if w.owner.readOnly {
		return nil, ErrReadOnly
	}
	db := w.owner.writer.Load()
	if db == nil {
		return nil, ErrReadOnly
	}
	return db, nil
}

func (w *writerHandle) Exec(query string, args ...any) (sql.Result, error) {
	w.owner.connMu.RLock()
	defer w.owner.connMu.RUnlock()
	db, err := w.current()
	if err != nil {
		return nil, err
	}
	return db.Exec(query, args...)
}

func (w *writerHandle) ExecContext(
	ctx context.Context, query string, args ...any,
) (sql.Result, error) {
	w.owner.connMu.RLock()
	defer w.owner.connMu.RUnlock()
	db, err := w.current()
	if err != nil {
		return nil, err
	}
	return db.ExecContext(ctx, query, args...)
}

func (w *writerHandle) Query(
	query string, args ...any,
) (*sql.Rows, error) {
	w.owner.connMu.RLock()
	defer w.owner.connMu.RUnlock()
	db, err := w.current()
	if err != nil {
		return nil, err
	}
	return db.Query(query, args...)
}

func (w *writerHandle) QueryContext(
	ctx context.Context, query string, args ...any,
) (*sql.Rows, error) {
	w.owner.connMu.RLock()
	defer w.owner.connMu.RUnlock()
	db, err := w.current()
	if err != nil {
		return nil, err
	}
	return db.QueryContext(ctx, query, args...)
}

func (w *writerHandle) QueryRow(query string, args ...any) rowScanner {
	w.owner.connMu.RLock()
	defer w.owner.connMu.RUnlock()
	db, err := w.current()
	if err != nil {
		return errRow{err: err}
	}
	// The lock protects pool selection, not Scan. database/sql keeps any
	// row's connection alive if the pool is closed after QueryRow returns.
	return db.QueryRow(query, args...)
}

func (w *writerHandle) QueryRowContext(
	ctx context.Context, query string, args ...any,
) rowScanner {
	w.owner.connMu.RLock()
	defer w.owner.connMu.RUnlock()
	db, err := w.current()
	if err != nil {
		return errRow{err: err}
	}
	return db.QueryRowContext(ctx, query, args...)
}

func (w *writerHandle) Begin() (*sql.Tx, error) {
	w.owner.connMu.RLock()
	defer w.owner.connMu.RUnlock()
	db, err := w.current()
	if err != nil {
		return nil, err
	}
	return db.Begin()
}

func (w *writerHandle) BeginTx(
	ctx context.Context, opts *sql.TxOptions,
) (*sql.Tx, error) {
	w.owner.connMu.RLock()
	defer w.owner.connMu.RUnlock()
	db, err := w.current()
	if err != nil {
		return nil, err
	}
	return db.BeginTx(ctx, opts)
}

func (w *writerHandle) Conn(ctx context.Context) (*sql.Conn, error) {
	w.owner.connMu.RLock()
	defer w.owner.connMu.RUnlock()
	db, err := w.current()
	if err != nil {
		return nil, err
	}
	return db.Conn(ctx)
}

func (w *writerHandle) Close() error {
	w.owner.connMu.RLock()
	defer w.owner.connMu.RUnlock()
	db, err := w.current()
	if err != nil {
		return err
	}
	return db.Close()
}

// getReader returns a guarded facade for the current read-only connection pool.
func (db *DB) getReader() *readerHandle { return &readerHandle{owner: db} }

func (db *DB) rawReader() *sql.DB { return db.reader.Load() }

func (db *DB) rawWriter() *sql.DB { return db.writer.Load() }

// getWriter returns a guarded facade for the current write connection pool.
func (db *DB) getWriter() *writerHandle { return &writerHandle{owner: db} }

// Path returns the file path of the database.
func (db *DB) Path() string {
	return db.path
}

// ReadOnly reports whether this local SQLite store was opened read-only.
func (db *DB) ReadOnly() bool { return db.readOnly }

func (db *DB) requireWritable() error {
	if db.readOnly {
		return ErrReadOnly
	}
	return nil
}

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
		return nil, fmt.Errorf("checking database: %w", err)
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
		d.dataStale.Store(true)
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

// OpenReadOnly opens an existing SQLite database without running migrations or
// any writable initialization. It is intended for cold CLI reads and recovery
// cases where another process may own writable access to the archive.
func OpenReadOnly(path string) (*DB, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf(
				"opening read-only database: %w", err,
			)
		}
		return nil, fmt.Errorf(
			"checking read-only database: %w", err,
		)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf(
			"opening read-only database: %s is empty", path,
		)
	}

	reader, err := sql.Open("sqlite3", makeDSN(path, true))
	if err != nil {
		return nil, fmt.Errorf("opening read-only reader: %w", err)
	}
	reader.SetMaxOpenConns(4)
	if err := reader.Ping(); err != nil {
		reader.Close()
		return nil, fmt.Errorf("opening read-only reader: %w", err)
	}

	schemaStale, _, err := probeDatabaseConn(reader)
	if err != nil {
		reader.Close()
		return nil, fmt.Errorf(
			"checking read-only database: %w", err,
		)
	}
	if schemaStale {
		reader.Close()
		return nil, fmt.Errorf(
			"opening read-only database: schema is stale or incomplete",
		)
	}
	if err := checkReadOnlySchemaCompatibility(reader); err != nil {
		reader.Close()
		return nil, err
	}

	db := &DB{path: path, readOnly: true}
	db.reader.Store(reader)
	db.cursorSecret = make([]byte, 32)
	if _, err := rand.Read(db.cursorSecret); err != nil {
		reader.Close()
		return nil, fmt.Errorf(
			"generating cursor secret: %w", err,
		)
	}
	return db, nil
}

var readOnlyRequiredTables = []string{
	"sessions",
	"messages",
	"stats",
	"usage_events",
	"tool_calls",
	"tool_result_events",
	"insights",
	"pinned_messages",
	"starred_sessions",
	"excluded_sessions",
	"worktree_project_mappings",
	"pg_sync_state",
	"model_pricing",
	"secret_findings",
}

var (
	readOnlyRequiredSchemaOnce sync.Once
	readOnlyRequiredSchemaMap  map[string][]string
	readOnlyRequiredSchemaErr  error
)

func readOnlyRequiredSchema() (map[string][]string, error) {
	readOnlyRequiredSchemaOnce.Do(func() {
		conn, err := sql.Open("sqlite3", ":memory:")
		if err != nil {
			readOnlyRequiredSchemaErr = fmt.Errorf(
				"opening schema probe: %w", err,
			)
			return
		}
		defer conn.Close()
		if _, err := conn.Exec(schemaSQL); err != nil {
			readOnlyRequiredSchemaErr = fmt.Errorf(
				"loading schema probe: %w", err,
			)
			return
		}
		schema, err := tableColumns(conn, readOnlyRequiredTables)
		if err != nil {
			readOnlyRequiredSchemaErr = err
			return
		}
		for _, table := range readOnlyRequiredTables {
			if len(schema[table]) == 0 {
				readOnlyRequiredSchemaErr =
					fmt.Errorf("schema table %s is missing", table)
				return
			}
		}
		readOnlyRequiredSchemaMap = schema
	})
	if readOnlyRequiredSchemaErr != nil {
		return nil, readOnlyRequiredSchemaErr
	}
	out := make(map[string][]string, len(readOnlyRequiredSchemaMap))
	for table, columns := range readOnlyRequiredSchemaMap {
		out[table] = append([]string(nil), columns...)
	}
	return out, nil
}

func tableColumns(
	conn *sql.DB,
	tables []string,
) (map[string][]string, error) {
	out := make(map[string][]string, len(tables))
	for _, table := range tables {
		rows, err := conn.Query(
			"SELECT name FROM pragma_table_info(?) ORDER BY cid",
			table,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"reading schema %s: %w", table, err,
			)
		}
		var columns []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return nil, fmt.Errorf(
					"reading schema %s: %w", table, err,
				)
			}
			columns = append(columns, name)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf(
				"reading schema %s: %w", table, err,
			)
		}
		out[table] = columns
	}
	return out, nil
}

func checkReadOnlySchemaCompatibility(conn *sql.DB) error {
	required, err := readOnlyRequiredSchema()
	if err != nil {
		return err
	}
	actual, err := tableColumns(conn, readOnlyRequiredTables)
	if err != nil {
		return fmt.Errorf("checking read-only schema: %w", err)
	}
	for table, columns := range required {
		have := make(map[string]bool, len(actual[table]))
		for _, column := range actual[table] {
			have[column] = true
		}
		for _, column := range columns {
			if !have[column] {
				return fmt.Errorf(
					"opening read-only database: schema missing %s.%s",
					table, column,
				)
			}
		}
	}
	return nil
}

// CheckDataVersion verifies that the database file, when present, was not
// written by a newer agentsview binary. Older data versions are compatible
// with startup because callers can run the normal non-destructive resync path.
func CheckDataVersion(path string) error {
	_, _, err := probeDatabase(path)
	return err
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

	return probeDatabaseConn(conn)
}

func probeDatabaseConn(
	conn *sql.DB,
) (schemaStale, dataStale bool, err error) {
	version, err := readUserVersion(conn)
	if err != nil {
		return false, false, err
	}
	if version > dataVersion {
		return false, false, &DataVersionTooNewError{
			DatabaseVersion: version,
			BinaryVersion:   dataVersion,
		}
	}

	schema, err := needsSchemaRebuild(conn)
	if err != nil {
		return false, false, err
	}
	if schema {
		return true, false, nil
	}

	return false, version < dataVersion, nil
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

func readUserVersion(conn *sql.DB) (int, error) {
	var version int
	err := conn.QueryRow(
		"PRAGMA user_version",
	).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf(
			"probing data version: %w", err,
		)
	}
	return version, nil
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
			"sessions", "session_name",
			"ALTER TABLE sessions ADD COLUMN session_name TEXT",
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
			"sessions", "quality_signal_version",
			"ALTER TABLE sessions ADD COLUMN quality_signal_version INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "short_prompt_count",
			"ALTER TABLE sessions ADD COLUMN short_prompt_count INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "unstructured_start",
			"ALTER TABLE sessions ADD COLUMN unstructured_start INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "missing_success_criteria_count",
			"ALTER TABLE sessions ADD COLUMN missing_success_criteria_count INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "missing_verification_count",
			"ALTER TABLE sessions ADD COLUMN missing_verification_count INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "duplicate_prompt_count",
			"ALTER TABLE sessions ADD COLUMN duplicate_prompt_count INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "no_code_context_count",
			"ALTER TABLE sessions ADD COLUMN no_code_context_count INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "runaway_tool_loop_count",
			"ALTER TABLE sessions ADD COLUMN runaway_tool_loop_count INTEGER NOT NULL DEFAULT 0",
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
			"sessions", "next_ordinal",
			"ALTER TABLE sessions ADD COLUMN next_ordinal INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "last_entry_uuid",
			"ALTER TABLE sessions ADD COLUMN last_entry_uuid TEXT",
		},
		{
			"messages", "thinking_text",
			"ALTER TABLE messages ADD COLUMN thinking_text TEXT NOT NULL DEFAULT ''",
		},
		{
			"sessions", "termination_status",
			"ALTER TABLE sessions ADD COLUMN termination_status TEXT",
		},
		{
			"sessions", "secret_leak_count",
			"ALTER TABLE sessions ADD COLUMN secret_leak_count INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "secrets_rules_version",
			"ALTER TABLE sessions ADD COLUMN secrets_rules_version TEXT NOT NULL DEFAULT ''",
		},
		{
			"insights", "kind",
			"ALTER TABLE insights ADD COLUMN kind TEXT NOT NULL DEFAULT ''",
		},
		{
			"insights", "schema_version",
			"ALTER TABLE insights ADD COLUMN schema_version TEXT NOT NULL DEFAULT ''",
		},
		{
			"insights", "template_id",
			"ALTER TABLE insights ADD COLUMN template_id TEXT NOT NULL DEFAULT ''",
		},
		{
			"insights", "template_version",
			"ALTER TABLE insights ADD COLUMN template_version TEXT NOT NULL DEFAULT ''",
		},
		{
			"insights", "aggregate_hash",
			"ALTER TABLE insights ADD COLUMN aggregate_hash TEXT NOT NULL DEFAULT ''",
		},
		{
			"insights", "cache_key",
			"ALTER TABLE insights ADD COLUMN cache_key TEXT NOT NULL DEFAULT ''",
		},
		{
			"insights", "cache_status",
			"ALTER TABLE insights ADD COLUMN cache_status TEXT NOT NULL DEFAULT ''",
		},
		{
			"insights", "provenance_json",
			"ALTER TABLE insights ADD COLUMN provenance_json TEXT NOT NULL DEFAULT ''",
		},
		{
			"insights", "structured_json",
			"ALTER TABLE insights ADD COLUMN structured_json TEXT NOT NULL DEFAULT ''",
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
	if _, err := w.Exec(
		`CREATE INDEX IF NOT EXISTS idx_insights_cache
		 ON insights(cache_key, created_at DESC)
		 WHERE cache_key != ''`,
	); err != nil {
		return fmt.Errorf(
			"creating idx_insights_cache: %w", err,
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

	if _, err := w.Exec(`
		CREATE TABLE IF NOT EXISTS worktree_project_mappings (
			id          INTEGER PRIMARY KEY,
			machine     TEXT NOT NULL,
			path_prefix TEXT NOT NULL,
			project     TEXT NOT NULL,
			enabled     INTEGER NOT NULL DEFAULT 1,
			created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			UNIQUE(machine, path_prefix)
		);
		CREATE INDEX IF NOT EXISTS idx_worktree_project_mappings_match
			ON worktree_project_mappings(machine, enabled, path_prefix);
		CREATE INDEX IF NOT EXISTS idx_worktree_project_mappings_project
			ON worktree_project_mappings(machine, project);
	`); err != nil {
		return fmt.Errorf(
			"creating worktree_project_mappings: %w", err,
		)
	}

	if err := db.ensureUsageEventsSchemaLocked(w); err != nil {
		return err
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
func (db *DB) createPartialIndexesLocked(w *writerHandle) error {
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_sessions_cwd
		 ON sessions(cwd) WHERE cwd != ''`,
		`CREATE INDEX IF NOT EXISTS idx_messages_compact_boundary
		 ON messages(session_id, ordinal) WHERE is_compact_boundary = 1`,
		`CREATE INDEX IF NOT EXISTS idx_messages_sidechain
		 ON messages(session_id) WHERE is_sidechain = 1`,
		`CREATE INDEX IF NOT EXISTS idx_messages_source_uuid
		 ON messages(source_uuid) WHERE source_uuid != ''`,
		`CREATE INDEX IF NOT EXISTS idx_messages_usage_covering
		 ON messages(timestamp, session_id, ordinal, model,
		             claude_message_id, claude_request_id, token_usage)
		 WHERE token_usage != ''
		   AND model != ''
		   AND model != '<synthetic>'`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_has_secret
		 ON sessions(secret_leak_count) WHERE secret_leak_count > 0`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_agent_file_path_active
		 ON sessions(agent, file_path)
		 WHERE file_path IS NOT NULL AND deleted_at IS NULL`,
	}
	for _, ddl := range indexes {
		if _, err := w.Exec(ddl); err != nil {
			return fmt.Errorf("creating index: %w", err)
		}
	}
	if _, err := w.Exec(
		`DROP INDEX IF EXISTS idx_messages_usage_timestamp`,
	); err != nil {
		return fmt.Errorf("dropping legacy usage index: %w", err)
	}
	// Rebuild the insight lookup index so it covers date_to (added for
	// range-aware lookups). DROP/CREATE only touches the index, never the
	// insights rows, so this is non-destructive.
	if _, err := w.Exec(
		`DROP INDEX IF EXISTS idx_insights_lookup`,
	); err != nil {
		return fmt.Errorf("recreating idx_insights_lookup: %w", err)
	}
	if _, err := w.Exec(
		`CREATE INDEX IF NOT EXISTS idx_insights_lookup
		 ON insights(type, date_from, date_to, project)`,
	); err != nil {
		return fmt.Errorf("recreating idx_insights_lookup: %w", err)
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
func (db *DB) backfillIsAutomatedLocked(w *writerHandle) error {
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
		`SELECT
			s.id,
			s.first_message,
			s.user_message_count,
			s.is_automated,
			(
				SELECT m.content
				FROM messages m
				WHERE m.session_id = s.id
				  AND m.role = 'user'
				  AND m.is_system = 0
				  AND TRIM(m.content) <> ''
				ORDER BY m.ordinal
				LIMIT 1
			) AS first_user_message
		 FROM sessions s`,
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
		var firstUser sql.NullString
		var umc int
		var rowAutomated bool
		if err := rows.Scan(
			&id, &fm, &umc, &rowAutomated, &firstUser,
		); err != nil {
			return fmt.Errorf(
				"scanning backfill candidate: %w", err,
			)
		}
		want := isAutomatedFromTextCandidates(
			umc, firstUser, fm,
		)
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
	w *writerHandle, ids []string, val int,
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
	w *writerHandle,
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
	w *writerHandle,
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
	w *writerHandle,
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
	w *writerHandle,
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
	w *writerHandle,
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
	w *writerHandle,
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
	w *writerHandle,
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
	w *writerHandle,
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
	w *writerHandle,
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
	return db.dataStale.Load()
}

// MarkDataCurrent records that a successful full resync has rebuilt the
// archive at the current parser data version.
func (db *DB) MarkDataCurrent() {
	db.dataStale.Store(false)
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
	if err := configureWAL(writer); err != nil {
		writer.Close()
		return nil, fmt.Errorf("configuring wal: %w", err)
	}

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
	db.startWALCheckpointLoop()
	return db, nil
}

func configureWAL(conn *sql.DB) error {
	var limit int64
	if err := conn.QueryRow(
		fmt.Sprintf(
			"PRAGMA journal_size_limit = %d",
			walJournalSizeLimitBytes,
		),
	).Scan(&limit); err != nil {
		return fmt.Errorf("setting journal_size_limit: %w", err)
	}
	return nil
}

// CheckpointWALTruncate runs a best-effort truncate checkpoint on the writer
// connection. It is safe to call while the app is running; SQLite reports
// ErrWALCheckpointBusy instead of blocking indefinitely when readers pin pages.
func (db *DB) CheckpointWALTruncate(ctx context.Context) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	var busy, logPages, checkpointedPages int
	err := db.getWriter().QueryRowContext(
		ctx, "PRAGMA wal_checkpoint(TRUNCATE)",
	).Scan(&busy, &logPages, &checkpointedPages)
	if err != nil {
		return fmt.Errorf("wal checkpoint truncate: %w", err)
	}
	if busy != 0 {
		return ErrWALCheckpointBusy
	}
	return nil
}

// CheckpointWALTruncateWithRetry gives short-lived readers a chance to release
// pages after large rewrites such as a full resync. Persistent readers simply
// leave the WAL for the next periodic attempt.
func (db *DB) CheckpointWALTruncateWithRetry(ctx context.Context) error {
	var lastErr error
	for i := range walCheckpointAttempts {
		err := db.CheckpointWALTruncate(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, ErrWALCheckpointBusy) {
			return err
		}
		if i == walCheckpointAttempts-1 {
			break
		}
		timer := time.NewTimer(walCheckpointRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

// MaybeCheckpointLargeWAL attempts a truncate checkpoint only when the WAL file
// has grown past the configured threshold.
func (db *DB) MaybeCheckpointLargeWAL(ctx context.Context) (bool, error) {
	info, err := os.Stat(db.path + "-wal")
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat wal: %w", err)
	}
	if info.Size() < walCheckpointThreshold {
		return false, nil
	}
	return true, db.CheckpointWALTruncate(ctx)
}

func (db *DB) startWALCheckpointLoop() {
	db.checkpointMu.Lock()
	defer db.checkpointMu.Unlock()
	if db.checkpointStop != nil {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	db.checkpointStop = stop
	db.checkpointDone = done

	go func() {
		defer close(done)
		ticker := time.NewTicker(walCheckpointInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				attempted, err := db.MaybeCheckpointLargeWAL(context.Background())
				if attempted && err != nil &&
					!errors.Is(err, ErrWALCheckpointBusy) {
					log.Printf("sqlite wal checkpoint: %v", err)
				}
			case <-stop:
				return
			}
		}
	}()
}

func (db *DB) stopWALCheckpointLoop() {
	db.checkpointMu.Lock()
	stop := db.checkpointStop
	done := db.checkpointDone
	db.checkpointStop = nil
	db.checkpointDone = nil
	db.checkpointMu.Unlock()

	if stop == nil {
		return
	}
	close(stop)
	<-done
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
	db.stopWALCheckpointLoop()
	db.mu.Lock()
	db.connMu.Lock()
	w := db.rawWriter()
	r := db.rawReader()
	retired := db.retired
	db.retired = nil
	db.connMu.Unlock()
	db.mu.Unlock()

	var errs []error
	if w != nil && w != r {
		errs = append(errs, w.Close())
	}
	if r != nil {
		errs = append(errs, r.Close())
	}
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
	if db.readOnly {
		return ErrReadOnly
	}
	db.stopWALCheckpointLoop()
	db.mu.Lock()
	defer db.mu.Unlock()
	db.connMu.Lock()
	defer db.connMu.Unlock()

	errs := []error{
		db.rawWriter().Close(),
		db.rawReader().Close(),
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
	if db.readOnly {
		return ErrReadOnly
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.reopenLocked(); err != nil {
		return err
	}
	db.startWALCheckpointLoop()
	return nil
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
	if err := configureWAL(writer); err != nil {
		writer.Close()
		return fmt.Errorf("configuring reopened wal: %w", err)
	}

	reader, err := sql.Open(
		"sqlite3", makeDSN(db.path, true),
	)
	if err != nil {
		writer.Close()
		return fmt.Errorf("reopening reader: %w", err)
	}
	reader.SetMaxOpenConns(4)

	db.connMu.Lock()
	retired := append([]*sql.DB(nil), db.retired...)
	oldWriter := db.writer.Swap(writer)
	oldReader := db.reader.Swap(reader)

	// Retire the just-swapped pools. Concurrent readers that
	// loaded the old pointer before the swap may still have
	// in-flight queries; these pools will be closed on the
	// next Reopen, CloseConnections, or Close call.
	db.retired = []*sql.DB{oldWriter, oldReader}
	db.connMu.Unlock()

	// Close pools from earlier reopens outside connMu. database/sql
	// may wait for active rows to finish, and that wait must not
	// block new reads from acquiring the guarded current reader.
	for _, p := range retired {
		if err := p.Close(); err != nil {
			log.Printf(
				"warning: closing retired db pool: %v", err,
			)
		}
	}
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

// Reader returns guarded read-only query access.
func (db *DB) Reader() Reader {
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

// GetOrCreateSyncState returns a sync-state value, atomically creating it
// with defaultValue when absent.
func (db *DB) GetOrCreateSyncState(key, defaultValue string) (string, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	w := db.getWriter()
	var value string
	err := w.QueryRow(
		`INSERT INTO pg_sync_state (key, value)
		 VALUES (?, ?)
		 ON CONFLICT(key) DO NOTHING
		 RETURNING value`,
		key, defaultValue,
	).Scan(&value)
	if err == nil {
		return value, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	err = w.QueryRow(
		"SELECT value FROM pg_sync_state WHERE key = ?", key,
	).Scan(&value)
	return value, err
}
