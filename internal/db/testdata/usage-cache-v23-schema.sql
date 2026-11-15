-- Schema from the format 23 usage cache writer (55fe6f595^).
CREATE TABLE usage_cache_metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE usage_cached_sessions (
    id INTEGER PRIMARY KEY,
    session_id TEXT NOT NULL UNIQUE,
    source_sync_marker TEXT NOT NULL,
    source_transcript_rev TEXT NOT NULL,
    usage_event_fingerprint TEXT NOT NULL,
    install_revision INTEGER NOT NULL,
    source_usage_generation TEXT NOT NULL,
    source_message_ordinal INTEGER
);
CREATE TABLE usage_facts (
    cached_session_id INTEGER NOT NULL REFERENCES usage_cached_sessions(id)
        ON DELETE CASCADE,
    fact_index INTEGER NOT NULL,
    source TEXT NOT NULL,
    message_ordinal INTEGER,
    timestamp_ms INTEGER,
    timestamp_ns INTEGER,
    raw_timestamp TEXT NOT NULL DEFAULT '',
    uses_session_start INTEGER NOT NULL,
    model TEXT NOT NULL,
    provider_id TEXT NOT NULL DEFAULT '',
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    reasoning_tokens INTEGER NOT NULL,
    cache_creation_tokens INTEGER NOT NULL,
    cache_creation_1h_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL,
    web_search_requests INTEGER NOT NULL,
    reported_cost_microdollars INTEGER,
    cost_source TEXT NOT NULL DEFAULT '',
    cost_status TEXT NOT NULL DEFAULT '',
    request_scoped INTEGER NOT NULL,
    claude_message_id TEXT NOT NULL DEFAULT '',
    claude_request_id TEXT NOT NULL DEFAULT '',
    source_uuid TEXT NOT NULL DEFAULT '',
    usage_dedup_key TEXT NOT NULL DEFAULT '',
    token_eligible INTEGER NOT NULL,
    activity_eligible INTEGER NOT NULL,
    PRIMARY KEY (cached_session_id, fact_index)
) WITHOUT ROWID;
CREATE INDEX usage_facts_claude_identity
    ON usage_facts (claude_message_id, claude_request_id)
    WHERE claude_message_id != '';
CREATE INDEX usage_facts_source_uuid
    ON usage_facts (source_uuid) WHERE source_uuid != '';
CREATE INDEX usage_facts_usage_dedup_key
    ON usage_facts (usage_dedup_key) WHERE usage_dedup_key != '';
CREATE TABLE cursor_usage_facts (
    source_id INTEGER PRIMARY KEY,
    agent TEXT NOT NULL,
    timestamp_ms INTEGER,
    raw_timestamp TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    cache_creation_tokens INTEGER NOT NULL,
    cache_read_tokens INTEGER NOT NULL,
    charged_microdollars INTEGER NOT NULL,
    is_headless INTEGER NOT NULL,
    dedup_key TEXT NOT NULL
);
CREATE INDEX cursor_usage_facts_dedup_key
    ON cursor_usage_facts (dedup_key);
CREATE TABLE usage_rollup_timezones (
    id INTEGER PRIMARY KEY,
    timezone_key TEXT NOT NULL UNIQUE,
    timezone_name TEXT NOT NULL,
    interval_fingerprint TEXT NOT NULL,
    last_requested_at TEXT NOT NULL,
    change_revision INTEGER,
    change_install_revision INTEGER NOT NULL DEFAULT 0,
    change_pricing TEXT NOT NULL DEFAULT ''
);
CREATE TABLE usage_rollup_days (
    timezone_id INTEGER NOT NULL REFERENCES usage_rollup_timezones(id)
        ON DELETE CASCADE,
    local_date TEXT NOT NULL,
    from_ms INTEGER NOT NULL,
    to_ms INTEGER NOT NULL,
    PRIMARY KEY (timezone_id, local_date)
) WITHOUT ROWID;
CREATE TABLE usage_rollup_installs (
    id INTEGER PRIMARY KEY,
    timezone_id INTEGER NOT NULL REFERENCES usage_rollup_timezones(id)
        ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    source_change_revision INTEGER NOT NULL,
    source_pricing TEXT NOT NULL,
    source_sync_marker TEXT NOT NULL,
    source_transcript_rev TEXT NOT NULL,
    usage_event_fingerprint TEXT NOT NULL,
    fact_install_revision INTEGER NOT NULL,
    baked_agent TEXT NOT NULL,
    group_key TEXT NOT NULL DEFAULT '',
    group_label TEXT NOT NULL DEFAULT '',
    baked_started_at TEXT NOT NULL,
    project TEXT NOT NULL DEFAULT '',
    machine TEXT NOT NULL DEFAULT '',
    git_branch TEXT NOT NULL DEFAULT '',
    is_automated INTEGER NOT NULL DEFAULT 0,
    user_message_count INTEGER NOT NULL DEFAULT 0,
    has_authoritative INTEGER NOT NULL DEFAULT 0,
    pricing_inputs TEXT NOT NULL,
	pricing_hash TEXT NOT NULL,
    install_revision INTEGER NOT NULL,
    cached_at TEXT NOT NULL,
    UNIQUE (timezone_id, session_id)
);
CREATE INDEX usage_rollup_installs_changes
    ON usage_rollup_installs (timezone_id, install_revision, source_change_revision);
CREATE TABLE usage_rollup_invalidations (
    timezone_id INTEGER NOT NULL REFERENCES usage_rollup_timezones(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL REFERENCES usage_cached_sessions(session_id) ON DELETE CASCADE,
    PRIMARY KEY (timezone_id, session_id)
) WITHOUT ROWID;
CREATE TRIGGER usage_rollup_invalidated AFTER DELETE ON usage_rollup_installs
WHEN EXISTS (SELECT 1 FROM usage_cached_sessions WHERE session_id=OLD.session_id)
 AND EXISTS (SELECT 1 FROM usage_rollup_timezones WHERE id=OLD.timezone_id)
BEGIN
    INSERT OR IGNORE INTO usage_rollup_invalidations(timezone_id, session_id)
        VALUES (OLD.timezone_id, OLD.session_id);
END;
CREATE TABLE usage_daily_rollups (
    rollup_install_id INTEGER NOT NULL REFERENCES usage_rollup_installs(id)
        ON DELETE CASCADE,
    local_date TEXT NOT NULL,
    reported_model TEXT NOT NULL,
    provider_id TEXT NOT NULL DEFAULT '',
    priced_model TEXT NOT NULL,
    matched_pattern TEXT NOT NULL,
    rate_ok INTEGER NOT NULL,
    rate_hash TEXT NOT NULL,
	pricing_timestamp TEXT NOT NULL,
    band_threshold INTEGER NOT NULL DEFAULT -1,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    reasoning_tokens INTEGER NOT NULL,
    cache_creation_tokens INTEGER NOT NULL,
    cache_read_tokens INTEGER NOT NULL,
    web_search_requests INTEGER NOT NULL,
    estimated_cost_microdollars INTEGER NOT NULL,
    savings_microdollars INTEGER NOT NULL,
    authoritative_cost_microdollars INTEGER,
    computed_request_count INTEGER NOT NULL,
    computed_aggregate_count INTEGER NOT NULL,
    reported_count INTEGER NOT NULL,
    base_request_count INTEGER NOT NULL,
    discarded_snapshot_output_tokens INTEGER NOT NULL,
    PRIMARY KEY (
        rollup_install_id, local_date, reported_model,
        provider_id, rate_hash, band_threshold
    )
) WITHOUT ROWID;
CREATE INDEX usage_daily_rollups_window
    ON usage_daily_rollups (local_date, rollup_install_id);

CREATE TABLE usage_day_totals (timezone_id INTEGER NOT NULL REFERENCES usage_rollup_timezones(id) ON DELETE CASCADE, local_date TEXT NOT NULL, project TEXT NOT NULL, agent TEXT NOT NULL, machine TEXT NOT NULL, git_branch TEXT NOT NULL, is_automated INTEGER NOT NULL, user_message_count INTEGER NOT NULL, reported_model TEXT NOT NULL, provider_id TEXT NOT NULL, rate_hash TEXT NOT NULL, band_threshold INTEGER NOT NULL, priced_model TEXT NOT NULL, pricing_timestamp TEXT NOT NULL, row_count INTEGER NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL, cache_creation_tokens INTEGER NOT NULL, cache_read_tokens INTEGER NOT NULL, estimated_cost_microdollars INTEGER NOT NULL, savings_microdollars INTEGER NOT NULL, computed_request_count INTEGER NOT NULL, computed_aggregate_count INTEGER NOT NULL, reported_count INTEGER NOT NULL, base_request_count INTEGER NOT NULL, PRIMARY KEY (timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold)) WITHOUT ROWID;
CREATE TRIGGER usage_day_totals_insert AFTER INSERT ON usage_daily_rollups
BEGIN
    INSERT INTO usage_day_totals (timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold, priced_model, pricing_timestamp, row_count, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, estimated_cost_microdollars, savings_microdollars, computed_request_count, computed_aggregate_count, reported_count, base_request_count)
    SELECT i.timezone_id, NEW.local_date, i.project, i.baked_agent, i.machine, i.git_branch, i.is_automated, i.user_message_count, NEW.reported_model, NEW.provider_id, NEW.rate_hash, NEW.band_threshold, NEW.priced_model, NEW.pricing_timestamp, 1, NEW.input_tokens, NEW.output_tokens, NEW.cache_creation_tokens, NEW.cache_read_tokens, NEW.estimated_cost_microdollars, NEW.savings_microdollars, NEW.computed_request_count, NEW.computed_aggregate_count, NEW.reported_count, NEW.base_request_count FROM usage_rollup_installs i WHERE i.id=NEW.rollup_install_id AND i.has_authoritative=0
    ON CONFLICT (timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold) DO UPDATE SET row_count=usage_day_totals.row_count+excluded.row_count, input_tokens=usage_day_totals.input_tokens+excluded.input_tokens, output_tokens=usage_day_totals.output_tokens+excluded.output_tokens, cache_creation_tokens=usage_day_totals.cache_creation_tokens+excluded.cache_creation_tokens, cache_read_tokens=usage_day_totals.cache_read_tokens+excluded.cache_read_tokens, estimated_cost_microdollars=usage_day_totals.estimated_cost_microdollars+excluded.estimated_cost_microdollars, savings_microdollars=usage_day_totals.savings_microdollars+excluded.savings_microdollars, computed_request_count=usage_day_totals.computed_request_count+excluded.computed_request_count, computed_aggregate_count=usage_day_totals.computed_aggregate_count+excluded.computed_aggregate_count, reported_count=usage_day_totals.reported_count+excluded.reported_count, base_request_count=usage_day_totals.base_request_count+excluded.base_request_count;
END;
CREATE TRIGGER usage_day_totals_delete AFTER DELETE ON usage_daily_rollups
BEGIN
    INSERT INTO usage_day_totals (timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold, priced_model, pricing_timestamp, row_count, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, estimated_cost_microdollars, savings_microdollars, computed_request_count, computed_aggregate_count, reported_count, base_request_count)
    SELECT i.timezone_id, OLD.local_date, i.project, i.baked_agent, i.machine, i.git_branch, i.is_automated, i.user_message_count, OLD.reported_model, OLD.provider_id, OLD.rate_hash, OLD.band_threshold, OLD.priced_model, OLD.pricing_timestamp, -1, -OLD.input_tokens, -OLD.output_tokens, -OLD.cache_creation_tokens, -OLD.cache_read_tokens, -OLD.estimated_cost_microdollars, -OLD.savings_microdollars, -OLD.computed_request_count, -OLD.computed_aggregate_count, -OLD.reported_count, -OLD.base_request_count FROM usage_rollup_installs i WHERE i.id=OLD.rollup_install_id AND i.has_authoritative=0
    ON CONFLICT (timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold) DO UPDATE SET row_count=usage_day_totals.row_count+excluded.row_count, input_tokens=usage_day_totals.input_tokens+excluded.input_tokens, output_tokens=usage_day_totals.output_tokens+excluded.output_tokens, cache_creation_tokens=usage_day_totals.cache_creation_tokens+excluded.cache_creation_tokens, cache_read_tokens=usage_day_totals.cache_read_tokens+excluded.cache_read_tokens, estimated_cost_microdollars=usage_day_totals.estimated_cost_microdollars+excluded.estimated_cost_microdollars, savings_microdollars=usage_day_totals.savings_microdollars+excluded.savings_microdollars, computed_request_count=usage_day_totals.computed_request_count+excluded.computed_request_count, computed_aggregate_count=usage_day_totals.computed_aggregate_count+excluded.computed_aggregate_count, reported_count=usage_day_totals.reported_count+excluded.reported_count, base_request_count=usage_day_totals.base_request_count+excluded.base_request_count;
    DELETE FROM usage_day_totals WHERE row_count=0 AND (timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold) =
        (SELECT i.timezone_id, OLD.local_date, i.project, i.baked_agent, i.machine, i.git_branch, i.is_automated, i.user_message_count, OLD.reported_model, OLD.provider_id, OLD.rate_hash, OLD.band_threshold FROM usage_rollup_installs i WHERE i.id=OLD.rollup_install_id);
END;

CREATE TRIGGER usage_day_totals_install_delete BEFORE DELETE ON usage_rollup_installs
WHEN EXISTS (SELECT 1 FROM usage_rollup_timezones WHERE id=OLD.timezone_id)
BEGIN
    DELETE FROM usage_daily_rollups WHERE rollup_install_id=OLD.id;
END;
CREATE TABLE usage_activity_rollups (
    rollup_install_id INTEGER NOT NULL REFERENCES usage_rollup_installs(id)
        ON DELETE CASCADE,
    local_date TEXT NOT NULL,
	model TEXT NOT NULL,
    user_message_count INTEGER NOT NULL,
    PRIMARY KEY (rollup_install_id, local_date, model)
) WITHOUT ROWID;
CREATE TABLE usage_rollup_exceptions (
    rollup_install_id INTEGER NOT NULL REFERENCES usage_rollup_installs(id)
        ON DELETE CASCADE,
    group_kind TEXT NOT NULL,
    group_key TEXT NOT NULL,
    cached_session_id INTEGER NOT NULL,
    fact_index INTEGER NOT NULL,
    source_session_id TEXT NOT NULL,
    local_date TEXT NOT NULL,
    source TEXT NOT NULL,
    message_ordinal INTEGER,
    timestamp_ms INTEGER,
    timestamp_ns INTEGER,
    raw_timestamp TEXT NOT NULL,
    uses_session_start INTEGER NOT NULL,
    model TEXT NOT NULL,
    provider_id TEXT NOT NULL DEFAULT '',
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    reasoning_tokens INTEGER NOT NULL,
    cache_creation_tokens INTEGER NOT NULL,
    cache_creation_1h_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL,
    web_search_requests INTEGER NOT NULL,
    reported_cost_microdollars INTEGER,
    cost_source TEXT NOT NULL,
    cost_status TEXT NOT NULL,
    request_scoped INTEGER NOT NULL,
    is_headless INTEGER NOT NULL,
    claude_message_id TEXT NOT NULL,
    claude_request_id TEXT NOT NULL,
    source_uuid TEXT NOT NULL,
    usage_dedup_key TEXT NOT NULL,
    PRIMARY KEY (
        rollup_install_id, group_kind, group_key, cached_session_id, fact_index
    )
) WITHOUT ROWID;
CREATE INDEX usage_rollup_exceptions_window
    ON usage_rollup_exceptions (
        local_date, rollup_install_id, group_kind, group_key
    );