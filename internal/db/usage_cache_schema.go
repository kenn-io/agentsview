package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	usageCacheFormatVersion = 3
	usageCacheApplicationID = 0x41565543
	usageCacheKind          = "agentsview-usage-facts"

	usageCacheMetadataKind                = "cache_kind"
	usageCacheMetadataFormatVersion       = "format_version"
	usageCacheMetadataSourceDatabaseID    = "source_database_id"
	usageCacheMetadataNextInstallRevision = "next_install_revision"
	usageCacheMetadataNextRollupRevision  = "next_rollup_install_revision"
	usageCacheMetadataDeletionRevision    = "deletion_revision"
	usageCacheMetadataCursorHighWaterMark = "cursor_high_water_mark"
	usageCacheMetadataBackfillCompletedAt = "backfill_completed_at"
)

const usageCacheSchemaSQL = `
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
    install_revision INTEGER NOT NULL
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
    uses_session_start INTEGER NOT NULL CHECK (uses_session_start IN (0, 1)),
    model TEXT NOT NULL,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    reasoning_tokens INTEGER NOT NULL,
    cache_creation_tokens INTEGER NOT NULL,
    cache_read_tokens INTEGER NOT NULL,
    web_search_requests INTEGER NOT NULL,
    reported_cost_microdollars INTEGER,
    cost_source TEXT NOT NULL DEFAULT '',
    request_scoped INTEGER NOT NULL CHECK (request_scoped IN (0, 1)),
    claude_message_id TEXT NOT NULL DEFAULT '',
    claude_request_id TEXT NOT NULL DEFAULT '',
    source_uuid TEXT NOT NULL DEFAULT '',
    usage_dedup_key TEXT NOT NULL DEFAULT '',
    token_eligible INTEGER NOT NULL CHECK (token_eligible IN (0, 1)),
    activity_eligible INTEGER NOT NULL CHECK (activity_eligible IN (0, 1)),
    PRIMARY KEY (cached_session_id, fact_index)
) WITHOUT ROWID;
CREATE INDEX usage_facts_claude_identity
    ON usage_facts (claude_message_id, claude_request_id)
    WHERE claude_message_id != '' AND claude_request_id != '';
CREATE INDEX usage_facts_source_uuid
    ON usage_facts (source_uuid) WHERE source_uuid != '';
CREATE INDEX usage_facts_usage_dedup_key
    ON usage_facts (usage_dedup_key) WHERE usage_dedup_key != '';
CREATE TABLE cursor_usage_facts (
    source_id INTEGER PRIMARY KEY,
    timestamp_ms INTEGER,
    raw_timestamp TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    cache_creation_tokens INTEGER NOT NULL,
    cache_read_tokens INTEGER NOT NULL,
    charged_microdollars INTEGER NOT NULL,
    is_headless INTEGER NOT NULL CHECK (is_headless IN (0, 1)),
    dedup_key TEXT NOT NULL
);
CREATE INDEX cursor_usage_facts_dedup_key
    ON cursor_usage_facts (dedup_key);
CREATE TABLE usage_rollup_timezones (
    id INTEGER PRIMARY KEY,
    timezone_key TEXT NOT NULL UNIQUE,
    timezone_name TEXT NOT NULL,
    interval_fingerprint TEXT NOT NULL,
    last_requested_at TEXT NOT NULL
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
    source_sync_marker TEXT NOT NULL,
    source_transcript_rev TEXT NOT NULL,
    usage_event_fingerprint TEXT NOT NULL,
    fact_install_revision INTEGER NOT NULL,
    baked_agent TEXT NOT NULL,
    baked_started_at TEXT NOT NULL,
	pricing_hash TEXT NOT NULL,
    install_revision INTEGER NOT NULL,
    cached_at TEXT NOT NULL,
    UNIQUE (timezone_id, session_id)
);
CREATE TABLE usage_daily_rollups (
    rollup_install_id INTEGER NOT NULL REFERENCES usage_rollup_installs(id)
        ON DELETE CASCADE,
    local_date TEXT NOT NULL,
    reported_model TEXT NOT NULL,
    priced_model TEXT NOT NULL,
    matched_pattern TEXT NOT NULL,
    rate_ok INTEGER NOT NULL CHECK (rate_ok IN (0, 1)),
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
        rate_hash, band_threshold
    )
) WITHOUT ROWID;
CREATE INDEX usage_daily_rollups_window
    ON usage_daily_rollups (local_date, rollup_install_id);
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
    group_kind TEXT NOT NULL CHECK (group_kind IN ('snapshot', 'general')),
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
    uses_session_start INTEGER NOT NULL CHECK (uses_session_start IN (0, 1)),
    model TEXT NOT NULL,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    reasoning_tokens INTEGER NOT NULL,
    cache_creation_tokens INTEGER NOT NULL,
    cache_read_tokens INTEGER NOT NULL,
    web_search_requests INTEGER NOT NULL,
    reported_cost_microdollars INTEGER,
    cost_source TEXT NOT NULL,
    request_scoped INTEGER NOT NULL CHECK (request_scoped IN (0, 1)),
    is_headless INTEGER NOT NULL CHECK (is_headless IN (0, 1)),
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
    );`

type usageCache struct {
	db         *sql.DB
	path       string
	temporary  bool
	databaseID string
	fill       *usageFillCoordinator
	rollup     *usageRollupCoordinator
	cancel     context.CancelFunc

	lifecycleMu sync.Mutex
	users       int
	retiring    bool
	retired     chan struct{}
}

type usageCacheProbe struct {
	Exists           bool
	Recognized       bool
	Compatible       bool
	FormatVersion    int
	SourceDatabaseID string
	Err              error
}

type usageCacheManager struct {
	archivePath string
	archive     *DB
	ctx         context.Context
	cancel      context.CancelFunc

	mu          sync.Mutex
	generations map[string]*usageCache
	currentID   string
	closed      bool
}

func newUsageCacheManager(archivePath string) *usageCacheManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &usageCacheManager{
		archivePath: archivePath,
		generations: make(map[string]*usageCache),
		ctx:         ctx,
		cancel:      cancel,
	}
}

func (m *usageCacheManager) attachArchive(archive *DB) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.archive = archive
}

func usageCacheGenerationPath(
	archivePath string, formatVersion int, databaseID string,
) string {
	digest := sha256.Sum256([]byte(databaseID))
	name := fmt.Sprintf(
		"usage-cache-v%d-%x.db", formatVersion, digest[:16],
	)
	return filepath.Join(filepath.Dir(archivePath), name)
}

func (m *usageCacheManager) Generation(
	ctx context.Context, databaseID string,
) (*usageCache, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	databaseID = strings.TrimSpace(databaseID)
	if databaseID == "" {
		return nil, fmt.Errorf("usage cache source database id is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("usage cache manager is closed")
	}
	if m.currentID != "" && databaseID != m.currentID {
		return nil, fmt.Errorf("%w before opening generation %s",
			errUsageCacheSourceChanged, databaseID)
	}
	if cache := m.generations[databaseID]; cache != nil {
		return cache, nil
	}

	cache, err := m.openPersistentGeneration(ctx, databaseID)
	if err != nil {
		log.Printf(
			"warning: usage cache is non-persistent because %s is unavailable: %v",
			filepath.Dir(m.archivePath), err,
		)
		cache, err = openTemporaryUsageCache(ctx, databaseID)
		if err != nil {
			return nil, fmt.Errorf("opening temporary usage cache: %w", err)
		}
	}
	if cache == nil {
		return nil, fmt.Errorf("opening usage cache returned no database")
	}
	cacheContext, cancel := context.WithCancel(m.ctx)
	cache.cancel = cancel
	cache.retired = make(chan struct{})
	m.generations[databaseID] = cache
	if m.archive != nil {
		cache.fill = newUsageFillCoordinator(cacheContext, m.archive, cache)
		cache.rollup = newUsageRollupCoordinator(cacheContext, m.archive, cache)
	}
	return cache, nil
}

func (m *usageCacheManager) acquireGeneration(
	ctx context.Context, databaseID string,
) (*usageCache, func(), error) {
	cache, err := m.Generation(ctx, databaseID)
	if err != nil {
		return nil, nil, err
	}
	release, ok := cache.acquire()
	if !ok {
		return nil, nil, fmt.Errorf("%w while acquiring generation %s",
			errUsageCacheSourceChanged, databaseID)
	}
	return cache, release, nil
}

func (cache *usageCache) acquire() (func(), bool) {
	cache.lifecycleMu.Lock()
	defer cache.lifecycleMu.Unlock()
	if cache.retiring {
		return nil, false
	}
	cache.users++
	var once sync.Once
	return func() {
		once.Do(cache.release)
	}, true
}

func (cache *usageCache) startDetachedWork(work func()) bool {
	release, ok := cache.acquire()
	if !ok {
		return false
	}
	go func() {
		defer release()
		work()
	}()
	return true
}

func (cache *usageCache) release() {
	cache.lifecycleMu.Lock()
	defer cache.lifecycleMu.Unlock()
	cache.users--
	if cache.retiring && cache.users == 0 {
		close(cache.retired)
	}
}

func (cache *usageCache) beginRetirement() <-chan struct{} {
	cache.lifecycleMu.Lock()
	defer cache.lifecycleMu.Unlock()
	if !cache.retiring {
		cache.retiring = true
		if cache.cancel != nil {
			cache.cancel()
		}
		if cache.users == 0 {
			close(cache.retired)
		}
	}
	return cache.retired
}

func (m *usageCacheManager) openPersistentGeneration(
	ctx context.Context, databaseID string,
) (*usageCache, error) {
	path := usageCacheGenerationPath(
		m.archivePath, usageCacheFormatVersion, databaseID,
	)
	probe := probeUsageCache(ctx, path)
	if probe.Err != nil {
		return nil, probe.Err
	}
	if !probe.Exists {
		cache, published, err := publishUsageCache(ctx, path, databaseID)
		if err != nil {
			return nil, err
		}
		if published {
			return cache, nil
		}
		probe = probeUsageCacheWithBusyTimeout(ctx, path, 5000)
		if probe.Err != nil {
			return nil, probe.Err
		}
	}
	if !probe.Recognized {
		return nil, fmt.Errorf("usage cache path is foreign or unverifiable: %s", path)
	}
	if probe.SourceDatabaseID != databaseID {
		return nil, fmt.Errorf("usage cache source database id mismatch at %s", path)
	}
	if probe.Compatible {
		return openUsageCache(ctx, path, databaseID, false)
	}
	return nil, fmt.Errorf("usage cache generation is incompatible: %s", path)
}

func publishUsageCache(
	ctx context.Context, path, databaseID string,
) (*usageCache, bool, error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".usage-cache-init-*.db")
	if err != nil {
		return nil, false, fmt.Errorf("creating usage cache generation: %w", err)
	}
	temporaryPath := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return nil, false, err
	}
	_ = os.Remove(temporaryPath)
	cache, err := initializeUsageCache(ctx, temporaryPath, databaseID, false)
	if err != nil {
		_ = removeUsageCacheFiles(temporaryPath)
		return nil, false, err
	}
	if err := cache.db.Close(); err != nil {
		_ = removeUsageCacheFiles(temporaryPath)
		return nil, false, err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		_ = removeUsageCacheFiles(temporaryPath)
		if errors.Is(err, os.ErrExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("publishing usage cache %s: %w", path, err)
	}
	if err := removeUsageCacheFiles(temporaryPath); err != nil {
		return nil, false, err
	}
	cache, err = openUsageCache(ctx, path, databaseID, false)
	return cache, true, err
}

func openTemporaryUsageCache(
	ctx context.Context, databaseID string,
) (*usageCache, error) {
	file, err := os.CreateTemp("", "agentsview-usage-cache-*.db")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	cache, err := initializeUsageCache(ctx, path, databaseID, true)
	if err != nil {
		_ = removeUsageCacheFiles(path)
	}
	return cache, err
}

func initializeUsageCache(
	ctx context.Context, path, databaseID string, temporary bool,
) (*usageCache, error) {
	database, err := openUsageCacheDatabase(path)
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			_ = database.Close()
		}
	}()
	if _, err := database.ExecContext(ctx,
		`PRAGMA auto_vacuum=INCREMENTAL; VACUUM`); err != nil {
		return nil, fmt.Errorf("configuring usage cache auto-vacuum: %w", err)
	}
	if _, err := database.ExecContext(ctx,
		fmt.Sprintf("PRAGMA application_id=%d", usageCacheApplicationID)); err != nil {
		return nil, fmt.Errorf("identifying usage cache: %w", err)
	}
	if _, err := database.ExecContext(ctx, usageCacheSchemaSQL); err != nil {
		return nil, fmt.Errorf("creating usage cache schema: %w", err)
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("starting usage cache metadata transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	metadata := []struct{ key, value string }{
		{usageCacheMetadataKind, usageCacheKind},
		{usageCacheMetadataFormatVersion, strconv.Itoa(usageCacheFormatVersion)},
		{usageCacheMetadataSourceDatabaseID, databaseID},
		{usageCacheMetadataNextInstallRevision, "1"},
		{usageCacheMetadataNextRollupRevision, "1"},
		{usageCacheMetadataDeletionRevision, "0"},
		{usageCacheMetadataCursorHighWaterMark, "0"},
		{usageCacheMetadataBackfillCompletedAt, ""},
	}
	for _, item := range metadata {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO usage_cache_metadata(key, value) VALUES (?, ?)`,
			item.key, item.value,
		); err != nil {
			return nil, fmt.Errorf("writing usage cache metadata %s: %w", item.key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing usage cache metadata: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("securing usage cache: %w", err)
	}
	failed = false
	return &usageCache{
		db: database, path: path, temporary: temporary, databaseID: databaseID,
	}, nil
}

func openUsageCache(
	ctx context.Context, path, databaseID string, temporary bool,
) (*usageCache, error) {
	database, err := openUsageCacheDatabase(path)
	if err != nil {
		return nil, err
	}
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("opening usage cache %s: %w", path, err)
	}
	return &usageCache{
		db: database, path: path, temporary: temporary, databaseID: databaseID,
	}, nil
}

func openUsageCacheDatabase(path string) (*sql.DB, error) {
	database, err := sql.Open(sqliteUsageDriverName, makeDSN(path, false))
	if err != nil {
		return nil, fmt.Errorf("opening usage cache %s: %w", path, err)
	}
	database.SetMaxOpenConns(readerMaxOpenConns)
	database.SetMaxIdleConns(readerMaxOpenConns)
	if err := database.Ping(); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("opening usage cache %s: %w", path, err)
	}
	return database, nil
}

func probeUsageCache(ctx context.Context, path string) usageCacheProbe {
	return probeUsageCacheWithBusyTimeout(ctx, path, 0)
}

func probeUsageCacheWithBusyTimeout(
	ctx context.Context, path string, busyTimeoutMillis int,
) usageCacheProbe {
	probe := usageCacheProbe{}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return probe
		}
		probe.Err = fmt.Errorf("checking usage cache %s: %w", path, err)
		return probe
	}
	probe.Exists = true
	dsn := strings.Replace(makeDSN(path, true), "_busy_timeout=5000",
		"_busy_timeout="+strconv.Itoa(busyTimeoutMillis), 1)
	database, err := sql.Open("sqlite3", dsn)
	if err != nil {
		probe.Err = fmt.Errorf("probing usage cache %s: %w", path, err)
		return probe
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		probe.Err = fmt.Errorf("probing usage cache %s: %w", path, err)
		return probe
	}
	var applicationID int
	if err := database.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&applicationID); err != nil {
		probe.Err = fmt.Errorf("reading usage cache application id: %w", err)
		return probe
	}
	if applicationID != usageCacheApplicationID {
		return probe
	}
	var kind string
	if err := database.QueryRowContext(ctx,
		`SELECT value FROM usage_cache_metadata WHERE key = ?`,
		usageCacheMetadataKind,
	).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "no such table") {
			return probe
		}
		probe.Err = fmt.Errorf("reading usage cache kind: %w", err)
		return probe
	}
	if kind != usageCacheKind {
		return probe
	}
	probe.Recognized = true
	var formatText string
	formatErr := database.QueryRowContext(ctx,
		`SELECT value FROM usage_cache_metadata WHERE key = ?`,
		usageCacheMetadataFormatVersion,
	).Scan(&formatText)
	if formatErr != nil && !errors.Is(formatErr, sql.ErrNoRows) {
		probe.Err = fmt.Errorf("reading usage cache format version: %w", formatErr)
		return probe
	}
	probe.FormatVersion, _ = strconv.Atoi(formatText)
	sourceErr := database.QueryRowContext(ctx,
		`SELECT value FROM usage_cache_metadata WHERE key = ?`,
		usageCacheMetadataSourceDatabaseID,
	).Scan(&probe.SourceDatabaseID)
	if sourceErr != nil && !errors.Is(sourceErr, sql.ErrNoRows) {
		probe.Err = fmt.Errorf("reading usage cache source database id: %w", sourceErr)
		return probe
	}
	probe.Compatible = probe.FormatVersion == usageCacheFormatVersion &&
		probe.SourceDatabaseID != "" && usageCacheSchemaComplete(ctx, database)
	return probe
}

func usageCacheSchemaComplete(ctx context.Context, database *sql.DB) bool {
	var metadataCount int
	if err := database.QueryRowContext(ctx, `
		SELECT count(*) FROM usage_cache_metadata
		WHERE key IN (
			'cache_kind', 'format_version',
			'source_database_id', 'next_install_revision',
			'next_rollup_install_revision',
			'deletion_revision', 'cursor_high_water_mark',
			'backfill_completed_at'
		)`).Scan(&metadataCount); err != nil || metadataCount != 8 {
		return false
	}
	queries := []string{
		`SELECT key, value FROM usage_cache_metadata LIMIT 0`,
		`SELECT id, session_id, source_sync_marker, source_transcript_rev,
		        usage_event_fingerprint, install_revision
		 FROM usage_cached_sessions LIMIT 0`,
		`SELECT cached_session_id, fact_index, source, message_ordinal,
		        timestamp_ms, timestamp_ns, raw_timestamp, uses_session_start, model,
		        input_tokens, output_tokens, reasoning_tokens,
		        cache_creation_tokens, cache_read_tokens, web_search_requests,
		        reported_cost_microdollars, cost_source, request_scoped,
		        claude_message_id, claude_request_id, source_uuid,
		        usage_dedup_key, token_eligible, activity_eligible
		 FROM usage_facts LIMIT 0`,
		`SELECT source_id, timestamp_ms, raw_timestamp, model,
		        input_tokens, output_tokens, cache_creation_tokens,
		        cache_read_tokens, charged_microdollars,
		        is_headless, dedup_key
		 FROM cursor_usage_facts LIMIT 0`,
		`SELECT id, timezone_key, timezone_name, interval_fingerprint,
		        last_requested_at
		 FROM usage_rollup_timezones LIMIT 0`,
		`SELECT timezone_id, local_date, from_ms, to_ms
		 FROM usage_rollup_days LIMIT 0`,
		`SELECT id, timezone_id, session_id, source_sync_marker,
		        source_transcript_rev, usage_event_fingerprint,
		        fact_install_revision, baked_agent, baked_started_at,
		        pricing_hash, install_revision, cached_at
		 FROM usage_rollup_installs LIMIT 0`,
		`SELECT rollup_install_id, local_date, reported_model, priced_model,
		        matched_pattern, rate_ok, rate_hash, band_threshold,
		        input_tokens, output_tokens, reasoning_tokens,
		        cache_creation_tokens, cache_read_tokens, web_search_requests,
		        estimated_cost_microdollars, savings_microdollars,
		        authoritative_cost_microdollars, computed_request_count,
		        computed_aggregate_count, reported_count, base_request_count,
		        discarded_snapshot_output_tokens
		 FROM usage_daily_rollups LIMIT 0`,
		`SELECT rollup_install_id, local_date, model, user_message_count
		 FROM usage_activity_rollups LIMIT 0`,
		`SELECT rollup_install_id, group_kind, group_key, cached_session_id,
		        fact_index, source_session_id, local_date, source,
		        message_ordinal, timestamp_ms, timestamp_ns, raw_timestamp,
		        uses_session_start, model, input_tokens, output_tokens,
		        reasoning_tokens, cache_creation_tokens, cache_read_tokens,
		        web_search_requests, reported_cost_microdollars, cost_source,
		        request_scoped, is_headless, claude_message_id, claude_request_id,
		        source_uuid, usage_dedup_key
		 FROM usage_rollup_exceptions LIMIT 0`,
	}
	for _, query := range queries {
		rows, err := database.QueryContext(ctx, query)
		if err != nil {
			return false
		}
		if err := rows.Close(); err != nil {
			return false
		}
	}
	for _, index := range []string{
		"usage_daily_rollups_window", "usage_rollup_exceptions_window",
		"usage_facts_claude_identity", "usage_facts_source_uuid",
		"usage_facts_usage_dedup_key", "cursor_usage_facts_dedup_key",
	} {
		var exists bool
		if err := database.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='index' AND name=?)`,
			index,
		).Scan(&exists); err != nil || !exists {
			return false
		}
	}
	return true
}

func removeUsageCacheFiles(path string) error {
	var errs []error
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func retireUsageCaches(caches []*usageCache) error {
	retired := make([]<-chan struct{}, len(caches))
	for index, cache := range caches {
		retired[index] = cache.beginRetirement()
	}
	var errs []error
	for index, cache := range caches {
		<-retired[index]
		if cache.fill != nil {
			cache.fill.Close()
		}
		if cache.db != nil {
			errs = append(errs, cache.db.Close())
		}
		if cache.temporary {
			errs = append(errs, removeUsageCacheFiles(cache.path))
		}
	}
	return errors.Join(errs...)
}

func (m *usageCacheManager) detachAllLocked() []*usageCache {
	caches := make([]*usageCache, 0, len(m.generations))
	for databaseID, cache := range m.generations {
		caches = append(caches, cache)
		delete(m.generations, databaseID)
	}
	return caches
}

func (m *usageCacheManager) Reset() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.currentID = ""
	caches := m.detachAllLocked()
	m.mu.Unlock()
	return retireUsageCaches(caches)
}

func (m *usageCacheManager) RetireExcept(databaseID string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.currentID = databaseID
	caches := make([]*usageCache, 0, len(m.generations))
	for cachedID, cache := range m.generations {
		if cachedID == databaseID {
			continue
		}
		caches = append(caches, cache)
		delete(m.generations, cachedID)
	}
	m.mu.Unlock()
	return retireUsageCaches(caches)
}

func (m *usageCacheManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	caches := m.detachAllLocked()
	m.mu.Unlock()
	return retireUsageCaches(caches)
}
