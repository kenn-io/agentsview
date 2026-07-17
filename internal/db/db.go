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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
)

const projectIdentityRemoteScrubCompletedKey = "project_identity_remote_scrub_v1"

// dataVersion tracks parser changes that require a full
// re-sync. Increment this when parsing logic changes in ways
// that affect stored data (e.g. new fields extracted, content
// formatting changes). Old databases with a lower user_version
// trigger a non-destructive re-sync (mtime reset + skip cache
// clear) so existing session data is preserved.
//
// Bumped to 63: the Codex parser now persists current subagent lineage,
// links spawn events, restores plaintext agent messages, suppresses opaque
// encrypted payloads, and derives titles for encrypted child sessions.
// Existing Codex rows need re-parsing to backfill the corrected sessions.
//
// Bumped to 61: the ZCode parser now persists transcript messages,
// tool calls, and tool results from the message/part tables.
// Existing ZCode rows need re-parsing so stored sessions backfill
// message counts and transcript content.
//
// Bumped to 60: the Codex parser removes the recommended-plugins
// discovery envelope injected ahead of the first genuine user turn.
// Existing Codex rows need re-parsing so the synthetic plugin list is
// removed from stored messages, previews, and user-message counts.
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
// (59: Ingest sanitization now covers tool_calls.input_json, which v58
// left raw. Existing live rows need re-parsing so NUL/control bytes in
// stored inputs are stripped before they can poison PostgreSQL/DuckDB
// pushes; orphaned/trashed rows are cleaned by the copy-time input pass
// during the same resync.)
//
// (58: Persisted message/result content sanitization now covers
// tool_calls.result_content and tool_result_events.content. Existing rows
// need re-parsing so NUL/control bytes accepted by SQLite are stripped before
// they can poison DuckDB mirrors.)
//
// (57: Antigravity-CLI transcript fidelity classification. Re-parsing
// populates transcript_fidelity ("full"/"summary") on existing
// Antigravity CLI rows so sessions built from summary transcripts are
// distinguishable from full-fidelity captures.)
// (55: Kimi session-level usage events and native step.end model
// backfill. Re-parsing persists estimated usage events for existing
// aggregate-only Kimi sessions and preserves explicit native event
// model names instead of the proxy fallback.)
// (56: Codex goal-continuation context wrappers are filtered from
// persisted messages and user_message_count. Existing Codex rows need
// re-parsing so synthetic /goal continuation records are removed.)
// (54: Antigravity .db sessions record a schema-fingerprint
// source_version. Re-parsing populates source_version on existing
// Antigravity IDE and CLI rows so "which agy release produced this
// session" is queryable instead of blank.)
// (53: Recent Edits tool-call file_path extraction. Re-parsing
// populates tool_calls.file_path for edit/write calls -- including
// Kiro raw-diff inputs the JSON-only SQL backfill cannot recover --
// and the resync's fresh created_at re-pushes affected sessions to the
// PostgreSQL and DuckDB mirrors.)
// (52: Pi source lineage reparse.)
// (51: Gemini cumulative-to-delta token reparse.)
// (17: Codex <skill> template filtering.)
// (16: <turn_aborted> system messages.)
// (60: Codex recommended-plugins prefix filtering.)
// (62: Local session machine identity now uses the operating-system hostname
// instead of the ambiguous literal "local". Re-parsing updates existing
// source-backed rows while the resync archive copy preserves orphaned history.)
// (65: Claude leading system-reminder blocks are stripped from mixed
// user prompts before persistence, while reminder-only content still
// promotes to system_reminder. Existing rows need re-parsing so reminder
// metadata stops hiding real prompts and inflating reminder-only storage.)
// (66: Claude session identity metadata. Re-parsing populates the new
// agent_label and entrypoint session columns from top-level agentSetting
// and entrypoint fields on existing Claude rows.)
// (67: Antigravity CLI reader metadata. Re-parsing populates parent_session_id
// and relationship_type from agyReader.parentCascadeId in trajectory sidecars.)
// (68: Hermes skill_view metadata. Re-parsing populates tool_calls.skill_name
// for existing Hermes sessions so historical skill usage appears in analytics.)
const dataVersion = 68

const tokenCoverageRepairStatsKey = "token_coverage_repair_v1"

const toolCallFieldBackfillStatsKey = "tool_call_field_backfill_v1"

const (
	walJournalSizeLimitBytes = 256 * 1024 * 1024
	walCheckpointThreshold   = 512 * 1024 * 1024
	walCheckpointInterval    = 5 * time.Minute
	walCheckpointAttempts    = 3
	walCheckpointRetryDelay  = 250 * time.Millisecond
	sqliteCacheSizeKiB       = -8 * 1024
	readerMaxOpenConns       = 4
	readerConnMaxIdleTime    = 5 * time.Minute
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
		"database data version %d is newer than this agentsview binary's data version %d, so this binary cannot safely open the archive. Use an AgentsView build with data version %d or newer, or restore an archive backup compatible with data version %d. The archive was not modified",
		e.DatabaseVersion, e.BinaryVersion,
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

const recallEntriesFTS = `
CREATE VIRTUAL TABLE IF NOT EXISTS recall_entries_fts USING fts5(
    title,
    body,
    trigger,
    content='recall_entries',
    content_rowid='rowid',
    tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS recall_entries_ai AFTER INSERT ON recall_entries BEGIN
    INSERT INTO recall_entries_fts(rowid, title, body, trigger)
        VALUES (new.rowid, new.title, new.body, new.trigger);
END;
CREATE TRIGGER IF NOT EXISTS recall_entries_ad AFTER DELETE ON recall_entries BEGIN
    INSERT INTO recall_entries_fts(recall_entries_fts, rowid, title, body, trigger)
        VALUES('delete', old.rowid, old.title, old.body, old.trigger);
END;
CREATE TRIGGER IF NOT EXISTS recall_entries_au AFTER UPDATE ON recall_entries BEGIN
    INSERT INTO recall_entries_fts(recall_entries_fts, rowid, title, body, trigger)
        VALUES('delete', old.rowid, old.title, old.body, old.trigger);
    INSERT INTO recall_entries_fts(rowid, title, body, trigger)
        VALUES (new.rowid, new.title, new.body, new.trigger);
END;
`

const recallEntriesFTS4 = `
CREATE VIRTUAL TABLE IF NOT EXISTS recall_entries_fts USING fts4(
    title,
    body,
    trigger,
    tokenize=porter
);

CREATE TRIGGER IF NOT EXISTS recall_entries_ai AFTER INSERT ON recall_entries BEGIN
    INSERT INTO recall_entries_fts(rowid, title, body, trigger)
        VALUES (new.rowid, new.title, new.body, new.trigger);
END;
CREATE TRIGGER IF NOT EXISTS recall_entries_ad AFTER DELETE ON recall_entries BEGIN
    DELETE FROM recall_entries_fts WHERE rowid = old.rowid;
END;
CREATE TRIGGER IF NOT EXISTS recall_entries_au AFTER UPDATE ON recall_entries BEGIN
    DELETE FROM recall_entries_fts WHERE rowid = old.rowid;
    INSERT INTO recall_entries_fts(rowid, title, body, trigger)
        VALUES (new.rowid, new.title, new.body, new.trigger);
END;
`

const recallEvidenceFTS = `
CREATE VIRTUAL TABLE IF NOT EXISTS recall_evidence_fts USING fts5(
    snippet,
    content='recall_evidence',
    content_rowid='id',
    tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS recall_evidence_ai AFTER INSERT ON recall_evidence BEGIN
    INSERT INTO recall_evidence_fts(rowid, snippet)
        VALUES (new.id, new.snippet);
END;
CREATE TRIGGER IF NOT EXISTS recall_evidence_ad AFTER DELETE ON recall_evidence BEGIN
    INSERT INTO recall_evidence_fts(recall_evidence_fts, rowid, snippet)
        VALUES('delete', old.id, old.snippet);
END;
CREATE TRIGGER IF NOT EXISTS recall_evidence_au AFTER UPDATE ON recall_evidence BEGIN
    INSERT INTO recall_evidence_fts(recall_evidence_fts, rowid, snippet)
        VALUES('delete', old.id, old.snippet);
    INSERT INTO recall_evidence_fts(rowid, snippet)
        VALUES (new.id, new.snippet);
END;
`

const recallEvidenceFTS4 = `
CREATE VIRTUAL TABLE IF NOT EXISTS recall_evidence_fts USING fts4(
    snippet,
    tokenize=porter
);

CREATE TRIGGER IF NOT EXISTS recall_evidence_ai AFTER INSERT ON recall_evidence BEGIN
    INSERT INTO recall_evidence_fts(rowid, snippet)
        VALUES (new.id, new.snippet);
END;
CREATE TRIGGER IF NOT EXISTS recall_evidence_ad AFTER DELETE ON recall_evidence BEGIN
    DELETE FROM recall_evidence_fts WHERE rowid = old.id;
END;
CREATE TRIGGER IF NOT EXISTS recall_evidence_au AFTER UPDATE ON recall_evidence BEGIN
    DELETE FROM recall_evidence_fts WHERE rowid = old.id;
    INSERT INTO recall_evidence_fts(rowid, snippet)
        VALUES (new.id, new.snippet);
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

	customPricing        map[string]config.CustomModelRate
	customPricingSources map[string]export.PricingRowSource

	checkpointMu   sync.Mutex
	checkpointStop chan struct{}
	checkpointDone chan struct{}

	vectorMu       sync.RWMutex
	vectorSearcher VectorSearcher
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

func (r *readerHandle) BeginTx(
	ctx context.Context, opts *sql.TxOptions,
) (*sql.Tx, error) {
	r.owner.connMu.RLock()
	defer r.owner.connMu.RUnlock()
	return r.current().BeginTx(ctx, opts)
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
	db.customPricingSources = nil
}

// SetEffectivePricing installs in-memory pricing rows with explicit provenance
// sources for read-only fallback paths that cannot seed model_pricing.
func (db *DB) SetEffectivePricing(
	p map[string]config.CustomModelRate,
	sources map[string]export.PricingRowSource,
) {
	db.customPricing = p
	db.customPricingSources = sources
}

// SetCursorSecret updates the secret key used for cursor signing.
func (db *DB) SetCursorSecret(secret []byte) {
	db.cursorMu.Lock()
	defer db.cursorMu.Unlock()
	db.cursorSecret = append([]byte(nil), secret...)
}

// makeDSN builds a SQLite connection string with shared pragmas.
//
// Both branches emit a file: URI. mattn/go-sqlite3 forwards the `_`-prefixed
// pragma params either way, but it only honors mode=ro when the DSN carries
// the file: scheme — a bare path silently opens read-write, so the ro
// contract depends on the prefix.
//
// The path component is percent-encoded (slashes kept intact): SQLite
// percent-decodes URI paths and splits params at `?`, so a raw path
// containing `%`, `?`, or `#` would be misparsed — e.g. a literal "%41" in a
// directory name would silently open a different file.
//
// _journal_mode=WAL is set only on writable DSNs (mirroring vectorDSN):
// PRAGMA journal_mode=WAL is a write, so with mode=ro honored it would fail
// outright on a database left in a non-WAL journal mode. Read-only
// connections just adopt whatever journal mode the file already has.
func makeDSN(path string, readOnly bool) string {
	params := url.Values{}
	params.Set("_busy_timeout", "5000")
	params.Set("_foreign_keys", "ON")
	params.Set("_cache_size", strconv.Itoa(sqliteCacheSizeKiB))
	if readOnly {
		params.Set("mode", "ro")
	} else {
		params.Set("_journal_mode", "WAL")
		params.Set("_synchronous", "NORMAL")
	}
	escaped := (&url.URL{Path: path}).EscapedPath()
	return "file:" + escaped + "?" + params.Encode()
}

func configureReaderPool(reader *sql.DB) {
	reader.SetMaxOpenConns(readerMaxOpenConns)
	// Keep burst readers warm. The database/sql default retains only two,
	// which makes concurrent sync checks repeatedly reopen SQLite and parse
	// the full schema.
	reader.SetMaxIdleConns(readerMaxOpenConns)
	reader.SetConnMaxIdleTime(readerConnMaxIdleTime)
}

// Open creates or opens a SQLite database at the given path.
// It configures WAL mode and returns a DB with separate
// writer and reader connections.
//
// If an existing database has an outdated schema (missing required
// legacy columns), those columns are added before schema indexes
// are initialized. The database is then marked for a non-destructive
// re-sync so the new fields are populated without losing archived data.
// If the schema is current but the data version is stale, the database
// is also preserved and marked for a re-sync on the next cycle.
func Open(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	schemaRepairNeeded, dataStale, err := probeDatabase(path)
	if err != nil {
		return nil, fmt.Errorf("checking database: %w", err)
	}

	d, err := openAndInit(path, schemaRepairNeeded)
	if err != nil {
		return nil, err
	}

	if err := d.migrateColumns(); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrating columns: %w", err)
	}
	if _, err := d.GetOrCreateDatabaseID(context.Background()); err != nil {
		d.Close()
		return nil, fmt.Errorf("initializing database id: %w", err)
	}
	if _, err := d.GetOrCreateArchiveID(context.Background()); err != nil {
		d.Close()
		return nil, fmt.Errorf("initializing archive id: %w", err)
	}
	if _, err := d.GetOrCreateArchiveSalt(context.Background()); err != nil {
		d.Close()
		return nil, fmt.Errorf("initializing archive salt: %w", err)
	}
	if err := d.EnsureProjectIdentityBackfillQueued(context.Background()); err != nil {
		d.Close()
		return nil, fmt.Errorf("queueing project identity backfill: %w", err)
	}

	if dataStale || schemaRepairNeeded {
		d.dataStale.Store(true)
		log.Printf(
			"database upgrade requires full resync",
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

const projectIdentityRevisionSchemaSQL = `
CREATE TABLE IF NOT EXISTS project_identity_observation_changes (
    project     TEXT NOT NULL,
    machine     TEXT NOT NULL,
    root_path   TEXT NOT NULL DEFAULT '',
    git_remote  TEXT NOT NULL DEFAULT '',
    revision    INTEGER NOT NULL,
    deleted     INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1)),
    PRIMARY KEY (project, machine, root_path, git_remote)
);
CREATE INDEX IF NOT EXISTS idx_project_identity_observation_changes_revision
    ON project_identity_observation_changes(revision);
CREATE TABLE IF NOT EXISTS session_project_identity_snapshot_changes (
    session_id  TEXT NOT NULL,
    project     TEXT NOT NULL,
    revision    INTEGER NOT NULL,
    deleted     INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1)),
    PRIMARY KEY (session_id, project)
);
CREATE INDEX IF NOT EXISTS idx_session_project_identity_snapshot_changes_revision
    ON session_project_identity_snapshot_changes(revision);
DROP TRIGGER IF EXISTS trg_project_identity_observations_revision_insert;
DROP TRIGGER IF EXISTS trg_project_identity_observations_revision_update;
DROP TRIGGER IF EXISTS trg_project_identity_observations_revision_delete;
DROP TRIGGER IF EXISTS trg_session_project_identity_snapshots_revision_insert;
DROP TRIGGER IF EXISTS trg_session_project_identity_snapshots_revision_update;
DROP TRIGGER IF EXISTS trg_session_project_identity_snapshots_revision_delete;
CREATE TRIGGER IF NOT EXISTS trg_project_identity_observations_revision_insert
AFTER INSERT ON project_identity_observations BEGIN
    INSERT INTO archive_metadata (key, value) VALUES ('project_identity_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO project_identity_observation_changes (
        project, machine, root_path, git_remote, revision, deleted
    ) VALUES (
        NEW.project, NEW.machine, NEW.root_path, NEW.git_remote,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 0
    ) ON CONFLICT(project, machine, root_path, git_remote) DO UPDATE SET
        revision = excluded.revision, deleted = 0;
END;
CREATE TRIGGER IF NOT EXISTS trg_project_identity_observations_revision_update
AFTER UPDATE ON project_identity_observations BEGIN
    INSERT INTO archive_metadata (key, value) VALUES ('project_identity_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO project_identity_observation_changes (
        project, machine, root_path, git_remote, revision, deleted
    ) VALUES (
        OLD.project, OLD.machine, OLD.root_path, OLD.git_remote,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 1
    ) ON CONFLICT(project, machine, root_path, git_remote) DO UPDATE SET
        revision = excluded.revision, deleted = 1;
    INSERT INTO project_identity_observation_changes (
        project, machine, root_path, git_remote, revision, deleted
    ) VALUES (
        NEW.project, NEW.machine, NEW.root_path, NEW.git_remote,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 0
    ) ON CONFLICT(project, machine, root_path, git_remote) DO UPDATE SET
        revision = excluded.revision, deleted = 0;
END;
CREATE TRIGGER IF NOT EXISTS trg_project_identity_observations_revision_delete
AFTER DELETE ON project_identity_observations BEGIN
    INSERT INTO archive_metadata (key, value) VALUES ('project_identity_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO project_identity_observation_changes (
        project, machine, root_path, git_remote, revision, deleted
    ) VALUES (
        OLD.project, OLD.machine, OLD.root_path, OLD.git_remote,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 1
    ) ON CONFLICT(project, machine, root_path, git_remote) DO UPDATE SET
        revision = excluded.revision, deleted = 1;
END;
CREATE TRIGGER IF NOT EXISTS trg_session_project_identity_snapshots_revision_insert
AFTER INSERT ON session_project_identity_snapshots BEGIN
    INSERT INTO archive_metadata (key, value) VALUES ('project_identity_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO session_project_identity_snapshot_changes (
        session_id, project, revision, deleted
    ) VALUES (
        NEW.session_id, NEW.project,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 0
    ) ON CONFLICT(session_id, project) DO UPDATE SET
        revision = excluded.revision, deleted = 0;
END;
CREATE TRIGGER IF NOT EXISTS trg_session_project_identity_snapshots_revision_update
AFTER UPDATE ON session_project_identity_snapshots BEGIN
    INSERT INTO archive_metadata (key, value) VALUES ('project_identity_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO session_project_identity_snapshot_changes (
        session_id, project, revision, deleted
    ) VALUES (
        OLD.session_id, OLD.project,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 1
    ) ON CONFLICT(session_id, project) DO UPDATE SET
        revision = excluded.revision, deleted = 1;
    INSERT INTO session_project_identity_snapshot_changes (
        session_id, project, revision, deleted
    ) VALUES (
        NEW.session_id, NEW.project,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 0
    ) ON CONFLICT(session_id, project) DO UPDATE SET
        revision = excluded.revision, deleted = 0;
END;
CREATE TRIGGER IF NOT EXISTS trg_session_project_identity_snapshots_revision_delete
AFTER DELETE ON session_project_identity_snapshots BEGIN
    INSERT INTO archive_metadata (key, value) VALUES ('project_identity_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO session_project_identity_snapshot_changes (
        session_id, project, revision, deleted
    ) VALUES (
        OLD.session_id, OLD.project,
        (SELECT CAST(value AS INTEGER) FROM archive_metadata
         WHERE key = 'project_identity_publication_revision'), 1
    ) ON CONFLICT(session_id, project) DO UPDATE SET
        revision = excluded.revision, deleted = 1;
END;
`

const projectIdentitySnapshotInvariantSchemaSQL = `
CREATE TRIGGER IF NOT EXISTS trg_sessions_create_project_identity_snapshot
AFTER INSERT ON sessions BEGIN
    INSERT INTO session_project_identity_snapshots (
        session_id, project, machine, root_path, worktree_relationship,
        checkout_state, git_branch, remote_resolution, observed_at
    ) VALUES (
        NEW.id, NEW.project, NEW.machine, NEW.cwd, 'unknown',
        CASE WHEN NEW.git_branch != '' THEN 'branch' ELSE 'unknown' END,
        NEW.git_branch, 'unknown', strftime('%Y-%m-%dT%H:%M:%fZ','now')
    ) ON CONFLICT(session_id) DO NOTHING;
END;
`

const exportIdentitySchemaSQL = `
CREATE TABLE IF NOT EXISTS archive_metadata (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS project_identity_observations (
    session_id         TEXT NOT NULL DEFAULT '',
    source_archive_id   TEXT NOT NULL DEFAULT '',
    source_archive_salt TEXT NOT NULL DEFAULT '',
    project            TEXT NOT NULL,
    machine            TEXT NOT NULL,
    root_path          TEXT NOT NULL DEFAULT '',
    git_remote         TEXT NOT NULL DEFAULT '',
    git_remote_name    TEXT NOT NULL DEFAULT '',
    repository_path    TEXT NOT NULL DEFAULT '',
    worktree_name      TEXT NOT NULL DEFAULT '',
    worktree_root_path TEXT NOT NULL DEFAULT '',
    worktree_relationship TEXT NOT NULL DEFAULT 'unknown',
    checkout_state     TEXT NOT NULL DEFAULT 'unknown',
    git_branch         TEXT NOT NULL DEFAULT '',
    remote_resolution  TEXT NOT NULL DEFAULT 'unknown',
    remote_candidate_count INTEGER NOT NULL DEFAULT 0,
    observed_at        TEXT NOT NULL,
    normalized_remote  TEXT NOT NULL DEFAULT '',
    key_source         TEXT NOT NULL DEFAULT '',
    key                TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (project, machine, root_path, git_remote)
);

CREATE INDEX IF NOT EXISTS idx_project_identity_observations_project
    ON project_identity_observations(project);

CREATE TABLE IF NOT EXISTS session_project_identity_snapshots (
    session_id         TEXT PRIMARY KEY,
    project            TEXT NOT NULL,
    machine            TEXT NOT NULL,
    root_path          TEXT NOT NULL DEFAULT '',
    git_remote         TEXT NOT NULL DEFAULT '',
    git_remote_name    TEXT NOT NULL DEFAULT '',
    repository_path    TEXT NOT NULL DEFAULT '',
    worktree_name      TEXT NOT NULL DEFAULT '',
    worktree_root_path TEXT NOT NULL DEFAULT '',
    worktree_relationship TEXT NOT NULL DEFAULT 'unknown',
    checkout_state     TEXT NOT NULL DEFAULT 'unknown',
    git_branch         TEXT NOT NULL DEFAULT '',
    remote_resolution  TEXT NOT NULL DEFAULT 'unknown',
    remote_candidate_count INTEGER NOT NULL DEFAULT 0,
    observed_at        TEXT NOT NULL,
    normalized_remote  TEXT NOT NULL DEFAULT '',
    key_source         TEXT NOT NULL DEFAULT '',
    key                TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS background_migrations (
    name            TEXT PRIMARY KEY,
    state           TEXT NOT NULL,
    total_items     INTEGER NOT NULL DEFAULT 0,
    completed_items INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    started_at      TEXT,
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    completed_at    TEXT
);` + projectIdentityRevisionSchemaSQL + projectIdentitySnapshotInvariantSchemaSQL

var exportIdentityColumnMigrations = []schemaColumnMigration{
	{"project_identity_observations", "session_id", "ALTER TABLE project_identity_observations ADD COLUMN session_id TEXT NOT NULL DEFAULT ''"},
	{"project_identity_observations", "source_archive_id", "ALTER TABLE project_identity_observations ADD COLUMN source_archive_id TEXT NOT NULL DEFAULT ''"},
	{"project_identity_observations", "source_archive_salt", "ALTER TABLE project_identity_observations ADD COLUMN source_archive_salt TEXT NOT NULL DEFAULT ''"},
	{"project_identity_observations", "repository_path", "ALTER TABLE project_identity_observations ADD COLUMN repository_path TEXT NOT NULL DEFAULT ''"},
	{"project_identity_observations", "worktree_relationship", "ALTER TABLE project_identity_observations ADD COLUMN worktree_relationship TEXT NOT NULL DEFAULT 'unknown'"},
	{"project_identity_observations", "checkout_state", "ALTER TABLE project_identity_observations ADD COLUMN checkout_state TEXT NOT NULL DEFAULT 'unknown'"},
	{"project_identity_observations", "git_branch", "ALTER TABLE project_identity_observations ADD COLUMN git_branch TEXT NOT NULL DEFAULT ''"},
	{"project_identity_observations", "remote_resolution", "ALTER TABLE project_identity_observations ADD COLUMN remote_resolution TEXT NOT NULL DEFAULT 'unknown'"},
	{"project_identity_observations", "remote_candidate_count", "ALTER TABLE project_identity_observations ADD COLUMN remote_candidate_count INTEGER NOT NULL DEFAULT 0"},
}

var exportIdentityUpgradeTables = map[string]struct{}{
	"archive_metadata":                   {},
	"background_migrations":              {},
	"project_identity_observations":      {},
	"session_project_identity_snapshots": {},
}

func exportSchemaUpgradeTarget(err error) (*SchemaUpgradeRequiredError, bool) {
	var target *SchemaUpgradeRequiredError
	if !errors.As(err, &target) {
		return nil, false
	}
	_, ok := exportIdentityUpgradeTables[target.Table]
	return target, ok
}

func exportSchemaUpgradeEligible(
	ctx context.Context, tx *sql.Tx, target *SchemaUpgradeRequiredError,
) (bool, error) {
	var tableExists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?
		)`, target.Table).Scan(&tableExists); err != nil {
		return false, fmt.Errorf("checking export schema eligibility: %w", err)
	}
	if !tableExists {
		return true, nil
	}
	for _, migration := range exportIdentityColumnMigrations {
		if migration.table == target.Table && migration.column == target.Column {
			return true, nil
		}
	}
	return false, nil
}

// UpgradeExportSchemaInPlace applies only the additive identity schema needed
// by daemonless exports. Other schema gaps still require the normal writable
// daemon migration or rebuild path.
func UpgradeExportSchemaInPlace(path string, cause error) (retErr error) {
	target, ok := exportSchemaUpgradeTarget(cause)
	if !ok {
		return fmt.Errorf("schema gap is not eligible for export upgrade: %w", cause)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("checking database for schema upgrade: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("upgrading database schema: %s is empty", path)
	}

	writer, err := sql.Open("sqlite3", makeDSN(path, false))
	if err != nil {
		return fmt.Errorf("opening schema upgrade writer: %w", err)
	}
	defer func() {
		if closeErr := writer.Close(); closeErr != nil {
			retErr = errors.Join(retErr,
				fmt.Errorf("closing schema upgrade writer: %w", closeErr))
		}
	}()
	writer.SetMaxOpenConns(1)
	if err := writer.Ping(); err != nil {
		return fmt.Errorf("opening schema upgrade writer: %w", err)
	}

	tx, err := writer.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("starting schema upgrade transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	eligible, err := exportSchemaUpgradeEligible(context.Background(), tx, target)
	if err != nil {
		return err
	}
	if !eligible {
		return fmt.Errorf("schema gap is not eligible for export upgrade: %w", cause)
	}
	if _, err := tx.Exec(exportIdentitySchemaSQL); err != nil {
		return fmt.Errorf("initializing export identity schema: %w", err)
	}
	if err := applyColumnMigrations(
		exportIdentityColumnMigrations,
		func(query string, args ...any) rowScanner {
			return tx.QueryRow(query, args...)
		},
		func(query string, args ...any) (sql.Result, error) {
			return tx.Exec(query, args...)
		},
	); err != nil {
		return err
	}
	if err := initializeSchemaUpgradeMetadata(tx); err != nil {
		return err
	}
	if err := ensureProjectIdentityBackfillQueuedTx(
		context.Background(), tx,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing schema upgrade: %w", err)
	}
	return nil
}

func initializeSchemaUpgradeMetadata(tx *sql.Tx) error {
	databaseID, err := newUUIDv4()
	if err != nil {
		return fmt.Errorf("generating database id: %w", err)
	}
	archiveID, err := newUUIDv4()
	if err != nil {
		return fmt.Errorf("generating archive id: %w", err)
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return fmt.Errorf("generating archive salt: %w", err)
	}
	for _, entry := range []struct {
		key   string
		value string
	}{
		{archiveMetadataDatabaseIDKey, databaseID},
		{archiveMetadataArchiveIDKey, archiveID},
		{archiveMetadataArchiveSaltKey, fmt.Sprintf("%x", random)},
	} {
		if _, err := tx.Exec(`
			INSERT INTO archive_metadata (key, value)
			VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET
				value = excluded.value,
				updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			WHERE trim(archive_metadata.value) = ''`,
			entry.key, entry.value,
		); err != nil {
			return fmt.Errorf("initializing archive metadata %s: %w",
				entry.key, err)
		}
	}
	return nil
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
	configureReaderPool(reader)
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
	"archive_metadata",
	"background_migrations",
	"project_identity_observations",
	"session_project_identity_snapshots",
	"pg_sync_state",
	"model_pricing",
	"secret_findings",
	"recall_entries",
	"recall_evidence",
	"recall_query_events",
	"recall_query_exposures",
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

// SchemaUpgradeRequiredError reports that a read-only open failed because the
// on-disk archive is missing a column the current binary's schema defines. The
// file is not corrupt: it was written by an older agentsview version and has
// not been migrated yet. Read-only opens never run migrations, so the archive
// can only be upgraded by a writable process (the daemon). Callers can detect
// this with IsSchemaUpgradeRequired and point the user at restarting the daemon
// so the migration runs. The Error text preserves the historical
// "schema missing <table>.<column>" wording so existing diagnostics still match.
type SchemaUpgradeRequiredError struct {
	Table  string
	Column string
}

func (e *SchemaUpgradeRequiredError) Error() string {
	return fmt.Sprintf(
		"opening read-only database: schema missing %s.%s",
		e.Table, e.Column,
	)
}

// IsSchemaUpgradeRequired reports whether err indicates a read-only open failed
// because the archive predates this binary's schema and needs a writable
// migration to run.
func IsSchemaUpgradeRequired(err error) bool {
	var target *SchemaUpgradeRequiredError
	return errors.As(err, &target)
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
				return &SchemaUpgradeRequiredError{
					Table:  table,
					Column: column,
				}
			}
		}
	}
	return nil
}

func (db *DB) hasCursorUsageTable() bool {
	var n int
	err := db.getReader().QueryRow(
		"SELECT 1 FROM sqlite_master WHERE type='table' AND name='cursor_usage_events'",
	).Scan(&n)
	return err == nil && n == 1
}

// CheckDataVersion verifies that the database file, when present, was not
// written by a newer agentsview binary. Older data versions are compatible
// with startup because callers can run the normal non-destructive resync path.
func CheckDataVersion(path string) error {
	_, _, err := probeDatabase(path)
	return err
}

// probeDatabase checks an existing database for schema and data staleness.
// It returns (schemaRepairNeeded, dataStale, err). A writable Open repairs
// missing legacy columns before initializing schema indexes, then requires a
// non-destructive resync. dataStale means user_version < dataVersion.
func probeDatabase(
	path string,
) (schemaRepairNeeded, dataStale bool, err error) {
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
) (schemaRepairNeeded, dataStale bool, err error) {
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

	schema, err := needsSchemaRepair(conn)
	if err != nil {
		return false, false, err
	}
	if schema {
		return true, false, nil
	}

	return false, version < dataVersion, nil
}

// needsSchemaRepair probes for required legacy columns that may be missing in
// databases created by older releases. Open adds them before initializing
// schema indexes, then triggers a non-destructive full resync.
func needsSchemaRepair(conn *sql.DB) (bool, error) {
	for _, migration := range legacySchemaColumnMigrations() {
		var count int
		err := conn.QueryRow(fmt.Sprintf(
			"SELECT count(*) FROM pragma_table_info('%s')"+
				" WHERE name = '%s'",
			migration.table, migration.column,
		)).Scan(&count)
		if err != nil {
			return false, fmt.Errorf(
				"probing schema (%s.%s): %w",
				migration.table, migration.column, err,
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

type schemaColumnMigration struct {
	table  string
	column string
	ddl    string
}

// legacySchemaColumnMigrations repairs the complete historical table shapes
// that must exist before db.init executes schema.sql.
func legacySchemaColumnMigrations() []schemaColumnMigration {
	return []schemaColumnMigration{
		{
			"sessions", "parent_session_id",
			"ALTER TABLE sessions ADD COLUMN parent_session_id TEXT",
		},
		{
			"tool_calls", "tool_use_id",
			"ALTER TABLE tool_calls ADD COLUMN tool_use_id TEXT",
		},
		{
			"tool_calls", "input_json",
			"ALTER TABLE tool_calls ADD COLUMN input_json TEXT",
		},
		{
			"tool_calls", "skill_name",
			"ALTER TABLE tool_calls ADD COLUMN skill_name TEXT",
		},
		{
			"tool_calls", "result_content_length",
			"ALTER TABLE tool_calls ADD COLUMN result_content_length INTEGER",
		},
		{
			"sessions", "user_message_count",
			"ALTER TABLE sessions ADD COLUMN user_message_count INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "relationship_type",
			"ALTER TABLE sessions ADD COLUMN relationship_type TEXT NOT NULL DEFAULT ''",
		},
		{
			"tool_calls", "subagent_session_id",
			"ALTER TABLE tool_calls ADD COLUMN subagent_session_id TEXT",
		},
	}
}

func schemaColumnMigrations() []schemaColumnMigration {
	return []schemaColumnMigration{
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
			"sessions", "transcript_revision",
			"ALTER TABLE sessions ADD COLUMN transcript_revision TEXT NOT NULL DEFAULT '0'",
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
			"sessions", "agent_label",
			"ALTER TABLE sessions ADD COLUMN agent_label TEXT NOT NULL DEFAULT ''",
		},
		{
			"sessions", "entrypoint",
			"ALTER TABLE sessions ADD COLUMN entrypoint TEXT NOT NULL DEFAULT ''",
		},
		{
			"sessions", "transcript_fidelity",
			"ALTER TABLE sessions ADD COLUMN transcript_fidelity TEXT NOT NULL DEFAULT ''",
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
			// Non-destructive column add: no dataVersion bump and no
			// resync. The column defaults false and self-heals to true on
			// the next incremental write of each row; a pre-migration
			// archive simply reads false everywhere until then, which
			// keeps parse-diff scrutiny conservative (drift is reported,
			// never masked) rather than the reverse.
			"sessions", "last_write_incremental",
			"ALTER TABLE sessions ADD COLUMN last_write_incremental INTEGER NOT NULL DEFAULT 0",
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
		{
			"tool_calls", "result_content",
			"ALTER TABLE tool_calls ADD COLUMN result_content TEXT",
		},
		{
			"tool_calls", "file_path",
			"ALTER TABLE tool_calls ADD COLUMN file_path TEXT",
		},
		{
			"tool_calls", "call_index",
			"ALTER TABLE tool_calls ADD COLUMN call_index INTEGER",
		},
		{
			"worktree_project_mappings", "layout",
			"ALTER TABLE worktree_project_mappings ADD COLUMN layout TEXT NOT NULL DEFAULT 'explicit'",
		},
		{
			"project_identity_observations", "session_id",
			"ALTER TABLE project_identity_observations ADD COLUMN session_id TEXT NOT NULL DEFAULT ''",
		},
		{
			"project_identity_observations", "source_archive_id",
			"ALTER TABLE project_identity_observations ADD COLUMN source_archive_id TEXT NOT NULL DEFAULT ''",
		},
		{
			"project_identity_observations", "source_archive_salt",
			"ALTER TABLE project_identity_observations ADD COLUMN source_archive_salt TEXT NOT NULL DEFAULT ''",
		},
		{
			"project_identity_observations", "repository_path",
			"ALTER TABLE project_identity_observations ADD COLUMN repository_path TEXT NOT NULL DEFAULT ''",
		},
		{
			"project_identity_observations", "worktree_relationship",
			"ALTER TABLE project_identity_observations ADD COLUMN worktree_relationship TEXT NOT NULL DEFAULT 'unknown'",
		},
		{
			"project_identity_observations", "checkout_state",
			"ALTER TABLE project_identity_observations ADD COLUMN checkout_state TEXT NOT NULL DEFAULT 'unknown'",
		},
		{
			"project_identity_observations", "git_branch",
			"ALTER TABLE project_identity_observations ADD COLUMN git_branch TEXT NOT NULL DEFAULT ''",
		},
		{
			"project_identity_observations", "remote_resolution",
			"ALTER TABLE project_identity_observations ADD COLUMN remote_resolution TEXT NOT NULL DEFAULT 'unknown'",
		},
		{
			"project_identity_observations", "remote_candidate_count",
			"ALTER TABLE project_identity_observations ADD COLUMN remote_candidate_count INTEGER NOT NULL DEFAULT 0",
		},
	}
}

func applySchemaColumnMigrations(
	queryRow func(string, ...any) rowScanner,
	exec func(string, ...any) (sql.Result, error),
) error {
	return applyColumnMigrations(schemaColumnMigrations(), queryRow, exec)
}

func applyColumnMigrations(
	migrations []schemaColumnMigration,
	queryRow func(string, ...any) rowScanner,
	exec func(string, ...any) (sql.Result, error),
) error {
	for _, m := range migrations {
		var tableCount int
		err := queryRow(
			`SELECT count(*) FROM sqlite_master
			 WHERE type = 'table' AND name = ?`, m.table,
		).Scan(&tableCount)
		if err != nil {
			return fmt.Errorf("checking table %s: %w", m.table, err)
		}
		if tableCount == 0 {
			continue
		}

		var count int
		err = queryRow(fmt.Sprintf(
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
			if _, err := exec(m.ddl); err != nil {
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
	return nil
}

// repairLegacySchemaBeforeInit adds legacy columns before schema initialization.
// The stale data marker is committed in the same transaction so a restart
// cannot skip the required full resync.
func repairLegacySchemaBeforeInit(w *writerHandle) error {
	tx, err := w.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("starting schema repair transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := applyColumnMigrations(
		legacySchemaColumnMigrations(),
		func(query string, args ...any) rowScanner {
			return tx.QueryRow(query, args...)
		},
		tx.Exec,
	); err != nil {
		return err
	}
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading repaired archive version: %w", err)
	}
	if version >= dataVersion {
		if _, err := tx.Exec(
			fmt.Sprintf("PRAGMA user_version = %d", dataVersion-1),
		); err != nil {
			return fmt.Errorf("marking repaired archive stale: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing schema repair: %w", err)
	}
	return nil
}

// migrateColumns adds columns introduced by this branch to databases created
// by older releases, then runs the data repairs required by a normal writable
// startup. Schema-only callers use applySchemaColumnMigrations directly.
func (db *DB) migrateColumns() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	w := db.getWriter()
	if err := applySchemaColumnMigrations(w.QueryRow, w.Exec); err != nil {
		return err
	}
	if err := db.createPartialIndexesLocked(w); err != nil {
		return err
	}
	if err := db.backfillIsAutomatedLocked(w); err != nil {
		return err
	}
	if err := db.backfillToolCallFieldsLocked(w); err != nil {
		return err
	}

	if _, err := w.Exec(
		`CREATE INDEX IF NOT EXISTS idx_tool_calls_file_path
		 ON tool_calls(file_path)
		 WHERE file_path IS NOT NULL`,
	); err != nil {
		return fmt.Errorf(
			"creating idx_tool_calls_file_path: %w", err,
		)
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
			"creating post-migration tables and indexes: %w", err,
		)
	}

	if _, err := w.Exec(`
		CREATE TABLE IF NOT EXISTS worktree_project_mappings (
			id          INTEGER PRIMARY KEY,
			machine     TEXT NOT NULL,
			path_prefix TEXT NOT NULL,
			layout      TEXT NOT NULL DEFAULT 'explicit',
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
	if _, err := w.Exec(`
		CREATE TABLE IF NOT EXISTS archive_metadata (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		);
		CREATE TABLE IF NOT EXISTS project_identity_observations (
			session_id         TEXT NOT NULL DEFAULT '',
			source_archive_id   TEXT NOT NULL DEFAULT '',
			source_archive_salt TEXT NOT NULL DEFAULT '',
			project            TEXT NOT NULL,
			machine            TEXT NOT NULL,
			root_path          TEXT NOT NULL DEFAULT '',
			git_remote         TEXT NOT NULL DEFAULT '',
			git_remote_name    TEXT NOT NULL DEFAULT '',
			repository_path    TEXT NOT NULL DEFAULT '',
			worktree_name      TEXT NOT NULL DEFAULT '',
			worktree_root_path TEXT NOT NULL DEFAULT '',
			worktree_relationship TEXT NOT NULL DEFAULT 'unknown',
			checkout_state     TEXT NOT NULL DEFAULT 'unknown',
			git_branch         TEXT NOT NULL DEFAULT '',
			remote_resolution  TEXT NOT NULL DEFAULT 'unknown',
			remote_candidate_count INTEGER NOT NULL DEFAULT 0,
			observed_at        TEXT NOT NULL,
			normalized_remote  TEXT NOT NULL DEFAULT '',
			key_source         TEXT NOT NULL DEFAULT '',
			key                TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (project, machine, root_path, git_remote)
		);
		CREATE INDEX IF NOT EXISTS idx_project_identity_observations_project
			ON project_identity_observations(project);
		CREATE TABLE IF NOT EXISTS session_project_identity_snapshots (
			session_id         TEXT PRIMARY KEY,
			project            TEXT NOT NULL,
			machine            TEXT NOT NULL,
			root_path          TEXT NOT NULL DEFAULT '',
			git_remote         TEXT NOT NULL DEFAULT '',
			git_remote_name    TEXT NOT NULL DEFAULT '',
			repository_path    TEXT NOT NULL DEFAULT '',
			worktree_name      TEXT NOT NULL DEFAULT '',
			worktree_root_path TEXT NOT NULL DEFAULT '',
			worktree_relationship TEXT NOT NULL DEFAULT 'unknown',
			checkout_state     TEXT NOT NULL DEFAULT 'unknown',
			git_branch         TEXT NOT NULL DEFAULT '',
			remote_resolution  TEXT NOT NULL DEFAULT 'unknown',
			remote_candidate_count INTEGER NOT NULL DEFAULT 0,
			observed_at        TEXT NOT NULL,
			normalized_remote  TEXT NOT NULL DEFAULT '',
			key_source         TEXT NOT NULL DEFAULT '',
			key                TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf(
			"creating project identity metadata: %w", err,
		)
	}
	if _, err := w.Exec(projectIdentityRevisionSchemaSQL); err != nil {
		return fmt.Errorf("creating project identity revision triggers: %w", err)
	}
	if _, err := w.Exec(projectIdentitySnapshotInvariantSchemaSQL); err != nil {
		return fmt.Errorf("creating project identity snapshot trigger: %w", err)
	}
	if err := db.scrubProjectIdentityGitRemoteCredentialsLocked(w); err != nil {
		return err
	}

	if err := db.ensureUsageEventsSchemaLocked(w); err != nil {
		return err
	}
	if err := db.ensureCursorUsageEventsSchemaLocked(w); err != nil {
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

func (db *DB) scrubProjectIdentityGitRemoteCredentialsLocked(
	w *writerHandle,
) error {
	var completed string
	err := w.QueryRow(`SELECT value FROM stats WHERE key = ?`,
		projectIdentityRemoteScrubCompletedKey).Scan(&completed)
	if err == nil && completed == "1" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("checking project identity remote scrub marker: %w", err)
	}
	tx, err := w.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("starting project identity remote scrub: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		DELETE FROM project_identity_observations
		WHERE git_remote = ''
		  AND EXISTS (
			SELECT 1 FROM project_identity_observations remote
			WHERE remote.project = project_identity_observations.project
			  AND remote.machine = project_identity_observations.machine
			  AND remote.root_path = project_identity_observations.root_path
			  AND remote.git_remote != ''
		  )`); err != nil {
		return fmt.Errorf("removing stale project identity root fallbacks: %w", err)
	}
	if err := scrubProjectIdentityGitRemoteCredentialsTx(
		context.Background(), tx,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO stats (key, value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		projectIdentityRemoteScrubCompletedKey,
	); err != nil {
		return fmt.Errorf("marking project identity remote scrub complete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing project identity remote scrub: %w", err)
	}
	return nil
}

// createPartialIndexesLocked creates partial indexes that are not
// covered by the initial schema DDL. Idempotent via IF NOT EXISTS.
func (db *DB) createPartialIndexesLocked(w *writerHandle) error {
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_sessions_cwd
		 ON sessions(cwd) WHERE cwd != ''`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_project_git_branch
		 ON sessions(project, git_branch) WHERE git_branch != ''`,
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

	patterns := snapshotAutomationPatterns()
	var setIDs, clearIDs []string
	if stored == current {
		setIDs, clearIDs, err = auditAutomatedMatchingHash(w, patterns)
	} else {
		setIDs, clearIDs, err = auditAutomatedFull(w, patterns)
	}
	if err != nil {
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

// backfillToolCallFieldsLocked fills file_path and call_index on tool_calls
// rows that predate those columns. file_path is extracted from valid JSON
// only (raw-diff inputs stay NULL); call_index is the 0-based position within
// the message by insertion id. Both UPDATEs touch only NULL rows, so the work
// is idempotent, but a stats sentinel makes it run once per database: after a
// resync (or the first populate) every row already carries the columns, so
// later Opens skip the unindexed full-table NULL scan. Caller holds db.mu.
func (db *DB) backfillToolCallFieldsLocked(w *writerHandle) error {
	should, err := db.shouldRunToolCallFieldBackfillLocked(w)
	if err != nil {
		return err
	}
	if !should {
		return nil
	}
	if _, err := w.Exec(`
		UPDATE tool_calls
		SET file_path = COALESCE(
			json_extract(input_json,'$.file_path'),
			json_extract(input_json,'$.path'),
			json_extract(input_json,'$.filePath'),
			json_extract(input_json,'$.file'))
		WHERE category IN ('Edit','Write')
		  AND file_path IS NULL
		  AND input_json IS NOT NULL
		  AND json_valid(input_json)`); err != nil {
		return fmt.Errorf("backfilling tool_calls.file_path: %w", err)
	}
	if _, err := w.Exec(`
		UPDATE tool_calls
		SET call_index = (
			SELECT COUNT(*) FROM tool_calls t2
			WHERE t2.message_id = tool_calls.message_id
			  AND t2.id < tool_calls.id)
		WHERE call_index IS NULL`); err != nil {
		return fmt.Errorf("backfilling tool_calls.call_index: %w", err)
	}
	return db.markToolCallFieldBackfillDoneLocked(w)
}

// shouldRunToolCallFieldBackfillLocked reports whether the one-time
// tool_calls file_path/call_index backfill still needs to run. Caller holds
// db.mu.
func (db *DB) shouldRunToolCallFieldBackfillLocked(
	w *writerHandle,
) (bool, error) {
	var done int
	if err := w.QueryRow(
		`SELECT count(*)
		 FROM stats
		 WHERE key = ? AND value != 0`,
		toolCallFieldBackfillStatsKey,
	).Scan(&done); err != nil {
		return false, fmt.Errorf(
			"probing tool_call field backfill marker: %w", err,
		)
	}
	return done == 0, nil
}

// markToolCallFieldBackfillDoneLocked records that the one-time tool_calls
// field backfill has completed so later Opens skip it. Caller holds db.mu.
func (db *DB) markToolCallFieldBackfillDoneLocked(
	w *writerHandle,
) error {
	if _, err := w.Exec(
		`INSERT INTO stats (key, value)
		 VALUES (?, 1)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		toolCallFieldBackfillStatsKey,
	); err != nil {
		return fmt.Errorf(
			"storing tool_call field backfill marker: %w", err,
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
//
// Note: entries uses a TEXT primary key, so its rowids are not an INTEGER
// PRIMARY KEY alias, and the SQLite docs warn VACUUM "may change" such
// rowids -- which would detach the external-content recall_entries_fts index
// (joined on rowid). The bundled SQLite preserves rowids through VACUUM, so
// no FTS rebuild is needed; TestVacuumPreservesRecallEntriesFTSSearchable guards
// that assumption and will fail if a future SQLite bump changes it.
func (db *DB) Vacuum() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec("VACUUM")
	return err
}

func openAndInit(path string, schemaRepairNeeded bool) (*DB, error) {
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
	configureReaderPool(reader)

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
	if schemaRepairNeeded {
		db.mu.Lock()
		err = repairLegacySchemaBeforeInit(db.getWriter())
		db.mu.Unlock()
		if err != nil {
			db.Close()
			return nil, fmt.Errorf(
				"repairing legacy schema before initialization: %w", err,
			)
		}
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

	var recallFTSCount int
	if err := w.QueryRow(
		"SELECT count(*) FROM sqlite_master" +
			" WHERE type='table' AND name='recall_entries_fts'",
	).Scan(&recallFTSCount); err != nil {
		return fmt.Errorf("checking recall entries fts table: %w", err)
	}
	hadRecallFTS := recallFTSCount > 0
	if _, err := w.Exec(recallEntriesFTS); err != nil {
		if !strings.Contains(
			err.Error(), "no such module",
		) {
			return fmt.Errorf("initializing recall entries FTS: %w", err)
		}
		if _, err := w.Exec(recallEntriesFTS4); err != nil {
			if !strings.Contains(
				err.Error(), "no such module",
			) {
				return fmt.Errorf("initializing recall entries FTS4: %w", err)
			}
		} else if !hadRecallFTS {
			if _, err := w.Exec(
				"INSERT INTO recall_entries_fts(rowid, title, body, trigger)" +
					" SELECT rowid, title, body, trigger FROM recall_entries",
			); err != nil {
				return fmt.Errorf("backfilling recall entries FTS4: %w", err)
			}
		}
	} else if !hadRecallFTS {
		if _, err := w.Exec(
			"INSERT INTO recall_entries_fts(recall_entries_fts)" +
				" VALUES('rebuild')",
		); err != nil {
			return fmt.Errorf("backfilling recall entries FTS: %w", err)
		}
	}

	var recallEvidenceFTSCount int
	if err := w.QueryRow(
		"SELECT count(*) FROM sqlite_master" +
			" WHERE type='table' AND name='recall_evidence_fts'",
	).Scan(&recallEvidenceFTSCount); err != nil {
		return fmt.Errorf("checking recall evidence fts table: %w", err)
	}
	hadRecallEvidenceFTS := recallEvidenceFTSCount > 0
	if _, err := w.Exec(recallEvidenceFTS); err != nil {
		if !strings.Contains(
			err.Error(), "no such module",
		) {
			return fmt.Errorf("initializing recall evidence FTS: %w", err)
		}
		if _, err := w.Exec(recallEvidenceFTS4); err != nil {
			if !strings.Contains(
				err.Error(), "no such module",
			) {
				return fmt.Errorf(
					"initializing recall evidence FTS4: %w", err,
				)
			}
		} else if !hadRecallEvidenceFTS {
			if _, err := w.Exec(
				"INSERT INTO recall_evidence_fts(rowid, snippet)" +
					" SELECT id, snippet FROM recall_evidence",
			); err != nil {
				return fmt.Errorf(
					"backfilling recall evidence FTS4: %w", err,
				)
			}
		}
	} else if !hadRecallEvidenceFTS {
		if _, err := w.Exec(
			"INSERT INTO recall_evidence_fts(recall_evidence_fts)" +
				" VALUES('rebuild')",
		); err != nil {
			return fmt.Errorf("backfilling recall evidence FTS: %w", err)
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

	// Close the writer last: SQLite checkpoints and removes the WAL when
	// the final connection closes, and the reader pool is mode=ro so its
	// close cannot perform that checkpoint.
	var errs []error
	for _, p := range retired {
		errs = append(errs, p.Close())
	}
	if r != nil {
		errs = append(errs, r.Close())
	}
	if w != nil && w != r {
		errs = append(errs, w.Close())
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

	// Close the writer last: SQLite checkpoints and removes the WAL when
	// the final connection closes, and the reader pool is mode=ro so its
	// close cannot perform that checkpoint. Callers rename or delete the
	// WAL file after this returns, so a skipped checkpoint would lose
	// every write still sitting in the log.
	var errs []error
	for _, p := range db.retired {
		errs = append(errs, p.Close())
	}
	errs = append(errs,
		db.rawReader().Close(),
		db.rawWriter().Close(),
	)
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
	configureReaderPool(reader)

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
