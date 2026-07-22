-- Sessions table
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    project     TEXT NOT NULL,
    machine     TEXT NOT NULL DEFAULT 'local',
    agent       TEXT NOT NULL DEFAULT 'claude',
    agent_label TEXT NOT NULL DEFAULT '',
    entrypoint  TEXT NOT NULL DEFAULT '',
    first_message TEXT,
    display_name TEXT,
    session_name TEXT,
    started_at  TEXT,
    ended_at    TEXT,
    message_count INTEGER NOT NULL DEFAULT 0,
    user_message_count INTEGER NOT NULL DEFAULT 0,
    file_path   TEXT,
    file_size   INTEGER,
    file_mtime  INTEGER,
    next_ordinal INTEGER NOT NULL DEFAULT 0,
    last_entry_uuid TEXT,
    file_inode  INTEGER,
    file_device INTEGER,
    file_hash   TEXT,
    local_modified_at TEXT,
    transcript_revision TEXT NOT NULL DEFAULT '0',
    parent_session_id TEXT,
    relationship_type TEXT NOT NULL DEFAULT '',
    total_output_tokens INTEGER NOT NULL DEFAULT 0,
    peak_context_tokens INTEGER NOT NULL DEFAULT 0,
    has_total_output_tokens INTEGER NOT NULL DEFAULT 0,
    has_peak_context_tokens INTEGER NOT NULL DEFAULT 0,
    is_automated INTEGER NOT NULL DEFAULT 0,
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
    context_pressure_max REAL,
    health_score INTEGER,
    health_grade TEXT,
    has_tool_calls INTEGER NOT NULL DEFAULT 0,
    has_context_data INTEGER NOT NULL DEFAULT 0,
    quality_signal_version INTEGER NOT NULL DEFAULT 0,
    short_prompt_count INTEGER NOT NULL DEFAULT 0,
    unstructured_start INTEGER NOT NULL DEFAULT 0,
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
    is_truncated INTEGER NOT NULL DEFAULT 0,
    -- SQLite-only sync bookkeeping (like next_ordinal): TRUE when the
    -- last write to this row went through the incremental-append path
    -- rather than a full re-normalization. Consumed only by parse-diff
    -- to suppress benign incremental-vs-full skew; deliberately not
    -- mirrored to PG/DuckDB.
    last_write_incremental INTEGER NOT NULL DEFAULT 0,
    deleted_at  TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    termination_status TEXT,
    secret_leak_count INTEGER NOT NULL DEFAULT 0,
    secrets_rules_version TEXT NOT NULL DEFAULT '',
    sync_marker TEXT
);

-- Messages table with ordinal for efficient range queries
CREATE TABLE IF NOT EXISTS messages (
    id             INTEGER PRIMARY KEY,
    session_id     TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ordinal        INTEGER NOT NULL,
    role           TEXT NOT NULL,
    content        TEXT NOT NULL,
    thinking_text  TEXT NOT NULL DEFAULT '',
    timestamp      TEXT,
    has_thinking   INTEGER NOT NULL DEFAULT 0,
    has_tool_use   INTEGER NOT NULL DEFAULT 0,
    content_length INTEGER NOT NULL DEFAULT 0,
    is_system      INTEGER NOT NULL DEFAULT 0,
    model TEXT NOT NULL DEFAULT '',
    token_usage TEXT NOT NULL DEFAULT '',
    context_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    has_context_tokens INTEGER NOT NULL DEFAULT 0,
    has_output_tokens INTEGER NOT NULL DEFAULT 0,
    claude_message_id TEXT NOT NULL DEFAULT '',
    claude_request_id TEXT NOT NULL DEFAULT '',
    source_type TEXT NOT NULL DEFAULT '',
    source_subtype TEXT NOT NULL DEFAULT '',
    source_uuid TEXT NOT NULL DEFAULT '',
    source_parent_uuid TEXT NOT NULL DEFAULT '',
    is_sidechain INTEGER NOT NULL DEFAULT 0,
    is_compact_boundary INTEGER NOT NULL DEFAULT 0,
    UNIQUE(session_id, ordinal)
);

-- Durable, bounded artifact publication state. The export queue intentionally
-- has no foreign key: a deleted locally-owned session remains pending until a
-- checkpoint publishes its removal. Acknowledged rows remain as generation
-- authority, so this table is bounded by historical archive session IDs rather
-- than only the currently dirty set.
CREATE TABLE IF NOT EXISTS artifact_export_queue (
    session_id  TEXT PRIMARY KEY,
    enqueued_at TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    -- Compare-and-ack token. Repeated writes retain their FIFO timestamp but
    -- advance generation, including multiple writes in one SQLite millisecond.
    generation INTEGER NOT NULL DEFAULT 1,
    -- Acknowledgement clears pending but retains the row as durable generation
    -- authority, preventing an old claim from becoming valid after requeue.
    pending INTEGER NOT NULL DEFAULT 1 CHECK (pending IN (0, 1))
);
CREATE INDEX IF NOT EXISTS idx_artifact_export_queue_pending
    ON artifact_export_queue(pending, enqueued_at, session_id);

CREATE TABLE IF NOT EXISTS artifact_publications (
    origin             TEXT NOT NULL,
    session_id         TEXT NOT NULL,
    manifest_hash      TEXT NOT NULL,
    source_fingerprint TEXT NOT NULL,
    PRIMARY KEY(origin, session_id)
);

CREATE TABLE IF NOT EXISTS artifact_publication_revisions (
    origin   TEXT PRIMARY KEY,
    revision INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS artifact_checkpoint_heads (
    origin             TEXT PRIMARY KEY,
    sequence           INTEGER NOT NULL,
    publication_revision INTEGER NOT NULL,
    session_map_sha256 TEXT NOT NULL,
    checkpoint_sha256  TEXT NOT NULL,
    checkpoint_size    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS artifact_checkpoint_floors (
    origin   TEXT PRIMARY KEY,
    sequence INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS artifact_checkpoint_landings (
    origin   TEXT PRIMARY KEY,
    sequence INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS artifact_peer_checkpoint_heads (
    origin            TEXT PRIMARY KEY,
    sequence          INTEGER NOT NULL,
    checkpoint_sha256 TEXT NOT NULL,
    checkpoint_size   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS artifact_checkpoint_landing_sessions (
    origin        TEXT NOT NULL,
    gid           TEXT NOT NULL,
    manifest_hash TEXT NOT NULL,
    PRIMARY KEY(origin, gid),
    FOREIGN KEY(origin) REFERENCES artifact_checkpoint_landings(origin)
        ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS artifact_repair_queue (
    origin      TEXT NOT NULL,
    kind        TEXT NOT NULL,
    name        TEXT NOT NULL,
    sha256      TEXT NOT NULL,
    size        INTEGER NOT NULL,
    detected_at TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY(origin, kind, name)
);
CREATE INDEX IF NOT EXISTS idx_artifact_repair_queue_detected
    ON artifact_repair_queue(detected_at, origin, kind, name);

-- Session ownership is represented by machine='local'. Queue both sides of an
-- ownership transition: OLD local publishes a removal and NEW local publishes
-- content. A BEFORE DELETE trigger preserves the owner signal before child
-- cascades run.
DROP TRIGGER IF EXISTS artifact_sessions_insert_queue;
DROP TRIGGER IF EXISTS artifact_sessions_update_queue;
DROP TRIGGER IF EXISTS artifact_sessions_delete_queue;
DROP TRIGGER IF EXISTS artifact_messages_insert_queue;
DROP TRIGGER IF EXISTS artifact_messages_update_queue;
DROP TRIGGER IF EXISTS artifact_messages_delete_queue;
DROP TRIGGER IF EXISTS artifact_usage_events_insert_queue;
DROP TRIGGER IF EXISTS artifact_usage_events_update_queue;
DROP TRIGGER IF EXISTS artifact_usage_events_delete_queue;

CREATE TRIGGER IF NOT EXISTS artifact_sessions_insert_queue
AFTER INSERT ON sessions WHEN NEW.machine = 'local' BEGIN
    INSERT INTO artifact_export_queue(session_id) VALUES (NEW.id)
    ON CONFLICT(session_id) DO UPDATE SET
        enqueued_at = CASE WHEN pending = 0
            THEN strftime('%Y-%m-%dT%H:%M:%fZ','now') ELSE enqueued_at END,
        generation = generation + 1,
        pending = 1;
END;

CREATE TRIGGER IF NOT EXISTS artifact_sessions_update_queue
AFTER UPDATE ON sessions
WHEN (OLD.machine = 'local' OR NEW.machine = 'local') AND (
    OLD.project IS NOT NEW.project OR
    OLD.machine IS NOT NEW.machine OR
    OLD.agent IS NOT NEW.agent OR
    OLD.agent_label IS NOT NEW.agent_label OR
    OLD.entrypoint IS NOT NEW.entrypoint OR
    OLD.first_message IS NOT NEW.first_message OR
    OLD.display_name IS NOT NEW.display_name OR
    OLD.session_name IS NOT NEW.session_name OR
    OLD.started_at IS NOT NEW.started_at OR
    OLD.ended_at IS NOT NEW.ended_at OR
    OLD.message_count IS NOT NEW.message_count OR
    OLD.user_message_count IS NOT NEW.user_message_count OR
    OLD.transcript_revision IS NOT NEW.transcript_revision OR
    OLD.parent_session_id IS NOT NEW.parent_session_id OR
    OLD.relationship_type IS NOT NEW.relationship_type OR
    OLD.total_output_tokens IS NOT NEW.total_output_tokens OR
    OLD.peak_context_tokens IS NOT NEW.peak_context_tokens OR
    OLD.has_total_output_tokens IS NOT NEW.has_total_output_tokens OR
    OLD.has_peak_context_tokens IS NOT NEW.has_peak_context_tokens OR
    OLD.is_automated IS NOT NEW.is_automated OR
    OLD.tool_failure_signal_count IS NOT NEW.tool_failure_signal_count OR
    OLD.tool_retry_count IS NOT NEW.tool_retry_count OR
    OLD.edit_churn_count IS NOT NEW.edit_churn_count OR
    OLD.consecutive_failure_max IS NOT NEW.consecutive_failure_max OR
    OLD.outcome IS NOT NEW.outcome OR
    OLD.outcome_confidence IS NOT NEW.outcome_confidence OR
    OLD.ended_with_role IS NOT NEW.ended_with_role OR
    OLD.final_failure_streak IS NOT NEW.final_failure_streak OR
    OLD.signals_pending_since IS NOT NEW.signals_pending_since OR
    OLD.compaction_count IS NOT NEW.compaction_count OR
    OLD.mid_task_compaction_count IS NOT NEW.mid_task_compaction_count OR
    OLD.context_pressure_max IS NOT NEW.context_pressure_max OR
    OLD.health_score IS NOT NEW.health_score OR
    OLD.health_grade IS NOT NEW.health_grade OR
    OLD.has_tool_calls IS NOT NEW.has_tool_calls OR
    OLD.has_context_data IS NOT NEW.has_context_data OR
    OLD.quality_signal_version IS NOT NEW.quality_signal_version OR
    OLD.short_prompt_count IS NOT NEW.short_prompt_count OR
    OLD.unstructured_start IS NOT NEW.unstructured_start OR
    OLD.missing_success_criteria_count IS NOT NEW.missing_success_criteria_count OR
    OLD.missing_verification_count IS NOT NEW.missing_verification_count OR
    OLD.duplicate_prompt_count IS NOT NEW.duplicate_prompt_count OR
    OLD.no_code_context_count IS NOT NEW.no_code_context_count OR
    OLD.runaway_tool_loop_count IS NOT NEW.runaway_tool_loop_count OR
    OLD.data_version IS NOT NEW.data_version OR
    OLD.cwd IS NOT NEW.cwd OR
    OLD.git_branch IS NOT NEW.git_branch OR
    OLD.source_session_id IS NOT NEW.source_session_id OR
    OLD.source_version IS NOT NEW.source_version OR
    OLD.transcript_fidelity IS NOT NEW.transcript_fidelity OR
    OLD.parser_malformed_lines IS NOT NEW.parser_malformed_lines OR
    OLD.is_truncated IS NOT NEW.is_truncated OR
    OLD.deleted_at IS NOT NEW.deleted_at OR
    OLD.created_at IS NOT NEW.created_at OR
    OLD.termination_status IS NOT NEW.termination_status
) BEGIN
    INSERT INTO artifact_export_queue(session_id) VALUES (NEW.id)
    ON CONFLICT(session_id) DO UPDATE SET
        enqueued_at = CASE WHEN pending = 0
            THEN strftime('%Y-%m-%dT%H:%M:%fZ','now') ELSE enqueued_at END,
        generation = generation + 1,
        pending = 1;
END;

CREATE TRIGGER IF NOT EXISTS artifact_sessions_delete_queue
BEFORE DELETE ON sessions WHEN OLD.machine = 'local' BEGIN
    INSERT INTO artifact_export_queue(session_id) VALUES (OLD.id)
    ON CONFLICT(session_id) DO UPDATE SET
        enqueued_at = CASE WHEN pending = 0
            THEN strftime('%Y-%m-%dT%H:%M:%fZ','now') ELSE enqueued_at END,
        generation = generation + 1,
        pending = 1;
END;

-- Stats table maintained by triggers
CREATE TABLE IF NOT EXISTS stats (
    key   TEXT PRIMARY KEY,
    value INTEGER NOT NULL DEFAULT 0
);

INSERT OR IGNORE INTO stats (key, value) VALUES ('session_count', 0);
INSERT OR IGNORE INTO stats (key, value) VALUES ('message_count', 0);

-- Triggers for stats maintenance
CREATE TRIGGER IF NOT EXISTS sessions_insert_stats AFTER INSERT ON sessions BEGIN
    UPDATE stats SET value = value + 1 WHERE key = 'session_count';
END;

CREATE TRIGGER IF NOT EXISTS sessions_delete_stats AFTER DELETE ON sessions BEGIN
    UPDATE stats SET value = value - 1 WHERE key = 'session_count';
END;

CREATE TRIGGER IF NOT EXISTS messages_insert_stats AFTER INSERT ON messages BEGIN
    UPDATE stats SET value = value + 1 WHERE key = 'message_count';
END;

CREATE TRIGGER IF NOT EXISTS messages_delete_stats AFTER DELETE ON messages BEGIN
    UPDATE stats SET value = value - 1 WHERE key = 'message_count';
END;

-- Indexes
CREATE INDEX IF NOT EXISTS idx_sessions_ended
    ON sessions(ended_at DESC, id);
CREATE INDEX IF NOT EXISTS idx_sessions_project
    ON sessions(project);
CREATE INDEX IF NOT EXISTS idx_sessions_machine
    ON sessions(machine);
CREATE INDEX IF NOT EXISTS idx_messages_session_ordinal
    ON messages(session_id, ordinal);
CREATE INDEX IF NOT EXISTS idx_messages_velocity
    ON messages(session_id, ordinal, role, timestamp, content_length);
CREATE INDEX IF NOT EXISTS idx_messages_session_role
    ON messages(session_id, role);

CREATE INDEX IF NOT EXISTS idx_sessions_parent
    ON sessions(parent_session_id)
    WHERE parent_session_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_sessions_file_path
    ON sessions(file_path)
    WHERE file_path IS NOT NULL;

-- Analytics indexes
CREATE INDEX IF NOT EXISTS idx_sessions_started
    ON sessions(started_at);
CREATE INDEX IF NOT EXISTS idx_sessions_message_count
    ON sessions(message_count);
CREATE INDEX IF NOT EXISTS idx_sessions_user_message_count
    ON sessions(user_message_count);
CREATE INDEX IF NOT EXISTS idx_sessions_agent
    ON sessions(agent);

-- Session-level usage events. These complement message-level
-- messages.token_usage rows for agents that only expose aggregate
-- session accounting.
CREATE TABLE IF NOT EXISTS usage_events (
    id INTEGER PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    message_ordinal INTEGER,
    source TEXT NOT NULL,
    model TEXT NOT NULL,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd REAL,
    cost_status TEXT NOT NULL DEFAULT '',
    cost_source TEXT NOT NULL DEFAULT '',
    occurred_at TEXT,
    dedup_key TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_events_dedup
    ON usage_events(session_id, source, dedup_key)
    WHERE dedup_key != '';
CREATE INDEX IF NOT EXISTS idx_usage_events_session
    ON usage_events(session_id);
CREATE INDEX IF NOT EXISTS idx_usage_events_occurred
    ON usage_events(occurred_at);

CREATE TABLE IF NOT EXISTS cursor_usage_events (
    id INTEGER PRIMARY KEY,
    occurred_at TEXT NOT NULL,
    model TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT '',
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    charged_cents REAL NOT NULL DEFAULT 0,
    cursor_token_fee REAL NOT NULL DEFAULT 0,
    user_id TEXT NOT NULL DEFAULT '',
    user_email TEXT NOT NULL DEFAULT '',
    is_headless INTEGER NOT NULL DEFAULT 0,
    dedup_key TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cursor_usage_events_dedup
    ON cursor_usage_events(dedup_key)
    WHERE dedup_key != '';
CREATE INDEX IF NOT EXISTS idx_cursor_usage_events_occurred
    ON cursor_usage_events(occurred_at);
CREATE INDEX IF NOT EXISTS idx_cursor_usage_events_model
    ON cursor_usage_events(model);

-- Tool calls table
CREATE TABLE IF NOT EXISTS tool_calls (
    id         INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL
        REFERENCES messages(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL
        REFERENCES sessions(id) ON DELETE CASCADE,
    tool_name  TEXT NOT NULL,
    category   TEXT NOT NULL,
    tool_use_id TEXT,
    input_json  TEXT,
    skill_name  TEXT,
    result_content_length INTEGER,
    result_content        TEXT,
    subagent_session_id TEXT,
    file_path  TEXT,
    call_index INTEGER
);

CREATE INDEX IF NOT EXISTS idx_tool_calls_session
    ON tool_calls(session_id);
CREATE INDEX IF NOT EXISTS idx_tool_calls_session_category
    ON tool_calls(session_id, category);
-- idx_tool_calls_message backs the ON DELETE CASCADE from
-- messages(id). Without it SQLite full-scans tool_calls per
-- deleted message row, which makes ReplaceSessionMessages
-- O(messages * tool_calls) and stalls sync once tool_calls
-- grows large.
CREATE INDEX IF NOT EXISTS idx_tool_calls_message
    ON tool_calls(message_id);
CREATE INDEX IF NOT EXISTS idx_tool_calls_category
    ON tool_calls(category);
CREATE INDEX IF NOT EXISTS idx_tool_calls_skill
    ON tool_calls(skill_name)
    WHERE skill_name IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tool_calls_subagent
    ON tool_calls(subagent_session_id)
    WHERE subagent_session_id IS NOT NULL;

-- Tool result events table: canonical chronological tool outputs.
CREATE TABLE IF NOT EXISTS tool_result_events (
    id                       INTEGER PRIMARY KEY,
    session_id               TEXT NOT NULL
        REFERENCES sessions(id) ON DELETE CASCADE,
    tool_call_message_ordinal INTEGER NOT NULL,
    call_index               INTEGER NOT NULL DEFAULT 0,
    tool_use_id              TEXT,
    agent_id                 TEXT,
    subagent_session_id      TEXT,
    source                   TEXT NOT NULL,
    status                   TEXT NOT NULL,
    content                  TEXT NOT NULL,
    content_length           INTEGER NOT NULL DEFAULT 0,
    timestamp                TEXT,
    event_index              INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_tool_result_events_session
    ON tool_result_events(session_id);
CREATE INDEX IF NOT EXISTS idx_tool_result_events_call
    ON tool_result_events(
        session_id,
        tool_call_message_ordinal,
        call_index,
        event_index
    );

-- Insights table for AI-generated activity insights
CREATE TABLE IF NOT EXISTS insights (
    id          INTEGER PRIMARY KEY,
    type        TEXT NOT NULL,
    date_from   TEXT NOT NULL,
    date_to     TEXT NOT NULL,
    project     TEXT,
    agent       TEXT NOT NULL,
    model       TEXT,
    prompt      TEXT,
    content     TEXT NOT NULL,
    kind        TEXT NOT NULL DEFAULT '',
    schema_version TEXT NOT NULL DEFAULT '',
    template_id TEXT NOT NULL DEFAULT '',
    template_version TEXT NOT NULL DEFAULT '',
    aggregate_hash TEXT NOT NULL DEFAULT '',
    cache_key   TEXT NOT NULL DEFAULT '',
    cache_status TEXT NOT NULL DEFAULT '',
    provenance_json TEXT NOT NULL DEFAULT '',
    structured_json TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_insights_lookup
    ON insights(type, date_from, date_to, project);

CREATE INDEX IF NOT EXISTS idx_insights_created
    ON insights(created_at DESC);

-- Recall entries: reviewed, reusable facts learned from prior sessions.
-- These are not raw transcript chunks: each row is an accepted recall entry with
-- provenance back to the session archive.
CREATE TABLE IF NOT EXISTS recall_entries (
    id                TEXT PRIMARY KEY,
    type              TEXT NOT NULL,
    scope             TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'accepted',
    review_state      TEXT NOT NULL DEFAULT 'unreviewed_auto'
        CHECK (review_state IN (
            'human_reviewed', 'unreviewed_auto', 'calibrated_auto', 'eval_raw'
        )),
    title             TEXT NOT NULL,
    body              TEXT NOT NULL,
    trigger           TEXT NOT NULL DEFAULT '',
    confidence        REAL,
    uncertainty       TEXT NOT NULL DEFAULT '',
    project           TEXT NOT NULL DEFAULT '',
    cwd               TEXT NOT NULL DEFAULT '',
    git_branch        TEXT NOT NULL DEFAULT '',
    agent             TEXT NOT NULL DEFAULT '',
    source_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    source_episode_id TEXT NOT NULL DEFAULT '',
    source_run_id     TEXT NOT NULL DEFAULT '',
    extractor_method  TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    transferable      INTEGER NOT NULL DEFAULT 0,
    provenance_ok     INTEGER NOT NULL DEFAULT 0,
    supersedes_entry_id TEXT NOT NULL DEFAULT '',
    superseded_by_entry_id TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at        TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_recall_entries_context
    ON recall_entries(project, cwd, git_branch, agent);
CREATE INDEX IF NOT EXISTS idx_recall_entries_type_scope
    ON recall_entries(type, scope, status);
CREATE INDEX IF NOT EXISTS idx_recall_entries_source_session
    ON recall_entries(source_session_id);
CREATE INDEX IF NOT EXISTS idx_recall_entries_source_episode
    ON recall_entries(source_episode_id);
CREATE INDEX IF NOT EXISTS idx_recall_entries_source_run
    ON recall_entries(source_run_id, source_session_id, review_state);
CREATE INDEX IF NOT EXISTS idx_recall_entries_updated
    ON recall_entries(updated_at DESC, id);
CREATE INDEX IF NOT EXISTS idx_recall_entries_supersession
    ON recall_entries(supersedes_entry_id, superseded_by_entry_id);

CREATE TABLE IF NOT EXISTS recall_evidence (
    id                    INTEGER PRIMARY KEY,
    entry_id             TEXT NOT NULL
        REFERENCES recall_entries(id) ON DELETE CASCADE,
    session_id            TEXT NOT NULL
        REFERENCES sessions(id) ON DELETE CASCADE,
    message_start_ordinal INTEGER NOT NULL,
    message_end_ordinal   INTEGER NOT NULL,
    message_start_source_uuid TEXT NOT NULL DEFAULT '',
    message_end_source_uuid   TEXT NOT NULL DEFAULT '',
    content_digest            TEXT NOT NULL DEFAULT '',
    tool_use_id           TEXT NOT NULL DEFAULT '',
    snippet               TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_recall_evidence_entry
    ON recall_evidence(entry_id);
CREATE INDEX IF NOT EXISTS idx_recall_evidence_session
    ON recall_evidence(session_id);

-- Append-only demand and exposure snapshots. Exposures deliberately do not
-- reference recall_entries: measurements must survive recall/session deletion
-- and full resync even when an exposed entry no longer exists.
CREATE TABLE IF NOT EXISTS recall_query_events (
    id                   TEXT PRIMARY KEY,
    query_text           TEXT NOT NULL,
    surface              TEXT NOT NULL,
    filters_json         TEXT NOT NULL DEFAULT '{}',
    trusted_only         INTEGER NOT NULL DEFAULT 0,
    score_policy_version TEXT NOT NULL,
    result_count         INTEGER NOT NULL DEFAULT 0 CHECK (result_count >= 0),
    packed_count         INTEGER NOT NULL DEFAULT 0 CHECK (packed_count >= 0),
    top_score            REAL NOT NULL DEFAULT 0,
    miss_reason          TEXT NOT NULL DEFAULT '',
    created_at           TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_recall_query_events_created
    ON recall_query_events(created_at DESC, id);
CREATE INDEX IF NOT EXISTS idx_recall_query_events_surface
    ON recall_query_events(surface, created_at DESC);

CREATE TABLE IF NOT EXISTS recall_query_exposures (
    query_id TEXT NOT NULL
        REFERENCES recall_query_events(id) ON DELETE CASCADE,
    rank     INTEGER NOT NULL CHECK (rank >= 1),
    entry_id TEXT NOT NULL,
    score    REAL NOT NULL,
    packed   INTEGER NOT NULL DEFAULT 0 CHECK (packed IN (0, 1)),
    PRIMARY KEY (query_id, rank)
);

CREATE INDEX IF NOT EXISTS idx_recall_query_exposures_entry
    ON recall_query_exposures(entry_id);

-- One extraction generation per distillation configuration: the fingerprint
-- digests everything that changes output (model, segmenter identity, prompt
-- digests, request shape). At most one generation is active at a time;
-- changing configuration creates a new generation rather than mixing corpora.
CREATE TABLE IF NOT EXISTS recall_extract_generations (
    fingerprint TEXT PRIMARY KEY,
    state       TEXT NOT NULL DEFAULT 'building'
        CHECK (state IN ('building', 'active', 'retired')),
    model       TEXT NOT NULL,
    segmenter   TEXT NOT NULL,
    params_json TEXT NOT NULL DEFAULT '{}',
    created_at  TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at  TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- Per-session extraction progress for one generation. unit_cursor counts
-- completed units of the session's deterministic unit list, so a daemon
-- restart resumes mid-session; content_digest detects sessions that grew
-- after extraction so they can be reset and topped up.
CREATE TABLE IF NOT EXISTS recall_extract_progress (
    session_id             TEXT NOT NULL
        REFERENCES sessions(id) ON DELETE CASCADE,
    generation_fingerprint TEXT NOT NULL
        REFERENCES recall_extract_generations(fingerprint) ON DELETE CASCADE,
    unit_cursor    INTEGER NOT NULL DEFAULT 0,
    units_total    INTEGER NOT NULL DEFAULT 0,
    state          TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'partial', 'done', 'failed')),
    content_digest TEXT NOT NULL DEFAULT '',
    -- pre-read cutoff of the last coverage claim; advances on insert, digest
    -- reset, and same-digest revisits alike, so it marks the transcript
    -- snapshot the extraction was last verified against
    content_stamped_at TEXT NOT NULL DEFAULT '',
    last_error     TEXT NOT NULL DEFAULT '',
    updated_at     TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (session_id, generation_fingerprint)
);
-- The trailing updated_at bounds failed-retry discovery: without it every
-- failed row of a generation, backoff included, is fetched and filtered on
-- each scheduler pass.
CREATE INDEX IF NOT EXISTS idx_recall_extract_progress_retry
    ON recall_extract_progress(generation_fingerprint, state, updated_at);

-- Pinned messages table
CREATE TABLE IF NOT EXISTS pinned_messages (
    id          INTEGER PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    message_id  INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    ordinal     INTEGER NOT NULL,
    note        TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE(session_id, message_id)
);

CREATE INDEX IF NOT EXISTS idx_pinned_session
    ON pinned_messages(session_id);
CREATE INDEX IF NOT EXISTS idx_pinned_session_ordinal_id
    ON pinned_messages(session_id, ordinal, id);
-- idx_pinned_message backs the ON DELETE CASCADE from messages(id).
-- The UNIQUE(session_id, message_id) constraint creates an index
-- ordered (session_id, message_id), which the FK lookup on
-- message_id alone cannot use (leftmost-prefix rule).
CREATE INDEX IF NOT EXISTS idx_pinned_message
    ON pinned_messages(message_id);
CREATE INDEX IF NOT EXISTS idx_pinned_created
    ON pinned_messages(created_at DESC);

-- Starred sessions: persists user star/unstar decisions
CREATE TABLE IF NOT EXISTS starred_sessions (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- Excluded sessions: tracks session IDs that were permanently
-- deleted by the user so the sync engine does not re-import them.
CREATE TABLE IF NOT EXISTS excluded_sessions (
    id         TEXT PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
-- Skipped files cache: persists skip decisions for files that
-- produced no session (non-interactive, parse errors) so they
-- survive process restarts without re-parsing.
CREATE TABLE IF NOT EXISTS skipped_files (
    file_path  TEXT PRIMARY KEY,
    file_mtime INTEGER NOT NULL
);

-- Remote skip cache: tracks file mtimes per remote host
-- for SSH sync incremental optimization.
CREATE TABLE IF NOT EXISTS remote_skipped_files (
    host       TEXT NOT NULL,
    path       TEXT NOT NULL,
    file_mtime INTEGER NOT NULL,
    PRIMARY KEY (host, path)
);

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
);

-- Compact publication journals retain the latest change for each identity
-- key. They let mirror pushes publish bounded deltas while preserving
-- tombstones for targets that have been offline.
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

-- sync_marker's index and trigger-maintenance DDL live in
-- syncMarkerSchemaSQL (internal/db/db.go), executed post-migration in
-- migrateColumns rather than here. schema.sql runs unconditionally on every
-- Open() before schemaColumnMigrations adds columns to legacy archives, so a
-- trigger body referencing sync_marker here would fail to create against a
-- pre-migration sessions table that doesn't have the column yet.

-- Session-deletion journal: a compact publication journal recording hard
-- session deletions so mirror pushes can apply bounded tombstone deltas
-- instead of enumerating the whole archive. Unlike sync_marker, this only
-- references columns (sessions.id, sessions.project) that have existed
-- forever, so it is safe to define directly here.
CREATE TABLE IF NOT EXISTS session_deletion_changes (
    session_id TEXT PRIMARY KEY,
    project    TEXT NOT NULL,
    revision   INTEGER NOT NULL,
    deleted    INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1))
);

CREATE INDEX IF NOT EXISTS idx_session_deletion_changes_revision
    ON session_deletion_changes(revision);

DROP TRIGGER IF EXISTS trg_sessions_deletion_journal_delete;
CREATE TRIGGER IF NOT EXISTS trg_sessions_deletion_journal_delete
AFTER DELETE ON sessions
BEGIN
    INSERT INTO archive_metadata (key, value)
        VALUES ('session_deletion_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET
        value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO session_deletion_changes (session_id, project, revision, deleted)
        SELECT OLD.id, OLD.project,
               CAST(value AS INTEGER), 1
        FROM archive_metadata WHERE key = 'session_deletion_publication_revision'
    ON CONFLICT(session_id) DO UPDATE SET
        project = excluded.project, revision = excluded.revision, deleted = 1;
END;

DROP TRIGGER IF EXISTS trg_sessions_deletion_journal_insert;
CREATE TRIGGER IF NOT EXISTS trg_sessions_deletion_journal_insert
AFTER INSERT ON sessions
WHEN EXISTS (SELECT 1 FROM session_deletion_changes WHERE session_id = NEW.id)
BEGIN
    INSERT INTO archive_metadata (key, value)
        VALUES ('session_deletion_publication_revision', '1')
    ON CONFLICT(key) DO UPDATE SET
        value = CAST(CAST(value AS INTEGER) + 1 AS TEXT),
        updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now');
    INSERT INTO session_deletion_changes (session_id, project, revision, deleted)
        SELECT NEW.id, NEW.project,
               CAST(value AS INTEGER), 0
        FROM archive_metadata WHERE key = 'session_deletion_publication_revision'
    ON CONFLICT(session_id) DO UPDATE SET
        project = excluded.project, revision = excluded.revision, deleted = 0;
END;

-- PG sync state: stores watermarks for push sync
CREATE TABLE IF NOT EXISTS pg_sync_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Metadata replay: durable record of handled artifact events. This lets
-- import scan the small append-only feed repeatedly without replaying
-- duplicates or relying on an unsafe max-HLC watermark.
CREATE TABLE IF NOT EXISTS metadata_applied_events (
    origin        TEXT NOT NULL,
    order_key     TEXT NOT NULL,
    artifact_hash TEXT NOT NULL,
    applied_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (origin, order_key)
);

CREATE TABLE IF NOT EXISTS metadata_artifact_provenance (
    origin        TEXT NOT NULL,
    order_key     TEXT NOT NULL,
    artifact_hash TEXT NOT NULL,
    session_gid   TEXT NOT NULL,
    op            TEXT NOT NULL,
    PRIMARY KEY (origin, order_key)
);
CREATE INDEX IF NOT EXISTS idx_metadata_artifact_provenance_session
    ON metadata_artifact_provenance(origin, session_gid, op, order_key);

-- Per-field LWW winners for metadata replay.
CREATE TABLE IF NOT EXISTS metadata_replay_state (
    session_gid   TEXT NOT NULL,
    field         TEXT NOT NULL,
    order_key     TEXT NOT NULL,
    hlc           TEXT NOT NULL,
    artifact_hash TEXT NOT NULL,
    origin        TEXT NOT NULL,
    op            TEXT NOT NULL,
    value         TEXT NOT NULL DEFAULT '',
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (session_gid, field)
);

-- Losing metadata values that were overridden by deterministic LWW replay.
CREATE TABLE IF NOT EXISTS metadata_conflicts (
    id                INTEGER PRIMARY KEY,
    session_gid       TEXT NOT NULL,
    field             TEXT NOT NULL,
    winning_order_key TEXT NOT NULL,
    losing_order_key  TEXT NOT NULL,
    winning_origin    TEXT NOT NULL,
    losing_origin     TEXT NOT NULL,
    winning_op        TEXT NOT NULL,
    losing_op         TEXT NOT NULL,
    winning_value     TEXT NOT NULL DEFAULT '',
    losing_value      TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE(session_gid, field, winning_order_key, losing_order_key)
);

-- Model pricing for cost calculation
CREATE TABLE IF NOT EXISTS model_pricing (
    model_pattern    TEXT PRIMARY KEY,
    input_per_mtok   REAL NOT NULL DEFAULT 0,
    output_per_mtok  REAL NOT NULL DEFAULT 0,
    cache_creation_per_mtok REAL NOT NULL DEFAULT 0,
    cache_read_per_mtok     REAL NOT NULL DEFAULT 0,
    updated_at       TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- Git aggregation TTL cache: memoizes `git log --numstat` and
-- `gh pr list` results per (repo, author, window) tuple so
-- repeated `agentsview stats` invocations don't re-shell out.
CREATE TABLE IF NOT EXISTS git_cache (
    cache_key   TEXT PRIMARY KEY,          -- sha256(repo|author|since|until|kind)
    kind        TEXT NOT NULL,             -- 'log' | 'pr'
    payload     TEXT NOT NULL,             -- JSON-encoded result
    computed_at TEXT NOT NULL              -- RFC3339
);

-- Secret findings: persisted detections from internal/secrets.
-- Located by natural coordinates (no row IDs) so findings survive the
-- full-resync orphan copy. Only redacted values are stored.
CREATE TABLE IF NOT EXISTS secret_findings (
    id              INTEGER PRIMARY KEY,
    session_id      TEXT NOT NULL
        REFERENCES sessions(id) ON DELETE CASCADE,
    rule_name       TEXT NOT NULL,
    confidence      TEXT NOT NULL,
    location_kind   TEXT NOT NULL,
    message_ordinal INTEGER NOT NULL,
    call_index      INTEGER,
    event_index     INTEGER,
    match_start     INTEGER NOT NULL,
    match_end       INTEGER NOT NULL,
    match_index     INTEGER NOT NULL,
    redacted_match  TEXT NOT NULL,
    rules_version   TEXT NOT NULL,
    created_at      TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_secret_findings_session
    ON secret_findings(session_id);
CREATE INDEX IF NOT EXISTS idx_secret_findings_rule
    ON secret_findings(rule_name);
