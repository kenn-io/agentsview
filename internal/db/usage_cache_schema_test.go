//go:build fts5

package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

func TestUsageCacheGenerationCreatesIdentifiedSchema(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "sessions.db")
	manager := newUsageCacheManager(archivePath)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })

	cache, err := manager.Generation(context.Background(), "database-id-one")
	require.NoError(t, err)
	require.False(t, cache.temporary)
	assert.Equal(t, filepath.Join(filepath.Dir(archivePath),
		"usage-cache-v7-980e32c89da32cb0d3588c0c06864b4e.db"), cache.path)

	info, err := os.Stat(cache.path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	var applicationID, autoVacuum int
	require.NoError(t, cache.db.QueryRow(`PRAGMA application_id`).Scan(&applicationID))
	require.NoError(t, cache.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&autoVacuum))
	assert.Equal(t, 1096176963, applicationID)
	assert.Equal(t, 2, autoVacuum, "INCREMENTAL auto-vacuum")

	metadata := readUsageCacheMetadata(t, cache.db)
	assert.Equal(t, "agentsview-usage-facts", metadata[usageCacheMetadataKind])
	assert.Equal(t, "7", metadata[usageCacheMetadataFormatVersion])
	assert.Equal(t, "database-id-one", metadata[usageCacheMetadataSourceDatabaseID])
	assert.Equal(t, "1", metadata[usageCacheMetadataNextInstallRevision])
	assert.Equal(t, "1", metadata[usageCacheMetadataNextRollupRevision])
	assert.Equal(t, "0", metadata[usageCacheMetadataDeletionRevision])
	assert.Equal(t, "0", metadata[usageCacheMetadataCursorHighWaterMark])
	assert.NotContains(t, metadata, "extractor_version")
	assert.NotContains(t, metadata, "backfill_progress_session_id")

	var hasPricingTimestamp bool
	require.NoError(t, cache.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM pragma_table_info('usage_daily_rollups')
			WHERE name = 'pricing_timestamp')`,
	).Scan(&hasPricingTimestamp))
	assert.True(t, hasPricingTimestamp)

	for table, columns := range map[string][]string{
		"usage_cached_sessions": {
			"fact_count", "min_fact_timestamp_ms", "max_fact_timestamp_ms",
		},
		"cursor_usage_facts": {
			"timestamp_ns", "kind", "cursor_token_fee_microdollars",
			"user_id", "user_email",
		},
		"usage_rollup_timezones": {"build_completed_at"},
	} {
		for _, column := range columns {
			var found bool
			require.NoError(t, cache.db.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name = ?)`,
				table, column,
			).Scan(&found))
			assert.False(t, found, table+"."+column)
		}
	}

	for _, table := range []string{
		"usage_cache_metadata", "usage_cached_sessions",
		"usage_facts", "cursor_usage_facts",
	} {
		var found bool
		require.NoError(t, cache.db.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`,
			table,
		).Scan(&found))
		assert.True(t, found, table)
	}
	for _, index := range []string{
		"usage_facts_timestamp", "usage_facts_snapshot", "usage_facts_session_start",
		"usage_facts_raw_timestamp",
	} {
		var found bool
		require.NoError(t, cache.db.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='index' AND name=?)`,
			index,
		).Scan(&found))
		assert.False(t, found, index)
	}

	result, err := cache.db.Exec(`INSERT INTO usage_cached_sessions(
		session_id, source_sync_marker, source_transcript_rev,
		usage_event_fingerprint, install_revision
	) VALUES ('s1', 'm1', 'r1', 'e1', 1)`)
	require.NoError(t, err)
	cachedSessionID, err := result.LastInsertId()
	require.NoError(t, err)
	_, err = cache.db.Exec(`INSERT INTO usage_facts(
		cached_session_id, fact_index, source, uses_session_start, model,
		input_tokens, output_tokens, reasoning_tokens,
		cache_creation_tokens, cache_read_tokens, web_search_requests,
		request_scoped, token_eligible, activity_eligible
	) VALUES (?, 0, 'message', 2, 'model', 0, 0, 0, 0, 0, 0, 1, 1, 1)`,
		cachedSessionID)
	require.Error(t, err, "boolean checks must reject invalid facts")
	_, err = cache.db.Exec(`INSERT INTO usage_facts(
		cached_session_id, fact_index, source, uses_session_start, model,
		input_tokens, output_tokens, reasoning_tokens,
		cache_creation_tokens, cache_read_tokens, web_search_requests,
		request_scoped, token_eligible, activity_eligible
	) VALUES (?, 0, 'message', 0, 'model', 0, 0, 0, 0, 0, 0, 1, 1, 1)`,
		cachedSessionID)
	require.NoError(t, err)
	_, err = cache.db.Exec(`DELETE FROM usage_cached_sessions WHERE id = ?`, cachedSessionID)
	require.NoError(t, err)
	var remainingFacts int
	require.NoError(t, cache.db.QueryRow(
		`SELECT count(*) FROM usage_facts WHERE cached_session_id = ?`, cachedSessionID,
	).Scan(&remainingFacts))
	assert.Zero(t, remainingFacts, "deleting a cached session must cascade to facts")

	probe := probeUsageCache(context.Background(), cache.path)
	require.NoError(t, probe.Err)
	assert.True(t, probe.Recognized)
	assert.True(t, probe.Compatible)
	assert.Equal(t, "database-id-one", probe.SourceDatabaseID)
}

func TestUsageCacheSchemaIncludesRollupTier(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "sessions.db")
	manager := newUsageCacheManager(archivePath)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })

	cache, err := manager.Generation(context.Background(), "database-a")
	require.NoError(t, err)
	for _, name := range []string{
		"usage_rollup_timezones", "usage_rollup_days",
		"usage_rollup_installs", "usage_daily_rollups",
		"usage_activity_rollups", "usage_rollup_exceptions",
		"usage_daily_rollups_window", "usage_rollup_exceptions_window",
	} {
		var found bool
		require.NoError(t, cache.db.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name = ?)`, name,
		).Scan(&found))
		assert.True(t, found, name)
	}
}

func TestUsageCacheProbeFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		seed func(*testing.T, string)
	}{
		{
			name: "wrong application id",
			seed: func(t *testing.T, path string) {
				conn := openRawUsageCacheTestDB(t, path)
				defer conn.Close()
				_, err := conn.Exec(`PRAGMA application_id = 1234;
					CREATE TABLE sentinel(value TEXT);
					INSERT INTO sentinel VALUES ('foreign')`)
				require.NoError(t, err)
			},
		},
		{
			name: "missing cache kind",
			seed: func(t *testing.T, path string) {
				conn := openRawUsageCacheTestDB(t, path)
				defer conn.Close()
				_, err := conn.Exec(`PRAGMA application_id = 1096176963;
					CREATE TABLE usage_cache_metadata(key TEXT PRIMARY KEY, value TEXT NOT NULL);
					INSERT INTO usage_cache_metadata VALUES ('sentinel', 'foreign')`)
				require.NoError(t, err)
			},
		},
		{
			name: "corrupt unrecognized file",
			seed: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, []byte("not sqlite"), 0o600))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			archivePath := filepath.Join(dir, "sessions.db")
			path := usageCacheGenerationPath(
				archivePath, usageCacheFormatVersion, "database-id-one")
			tc.seed(t, path)
			before, err := os.ReadFile(path)
			require.NoError(t, err)

			manager := newUsageCacheManager(archivePath)
			cache, err := manager.Generation(context.Background(), "database-id-one")
			require.NoError(t, err)
			require.True(t, cache.temporary)
			require.NoError(t, manager.Close())

			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after, "foreign or unverifiable file changed")
		})
	}
}

func TestUsageCacheGenerationPreservesRecognizedIncompleteCache(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "sessions.db")
	path := usageCacheGenerationPath(
		archivePath, usageCacheFormatVersion, "database-id-one")
	seedRecognizedUsageCache(t, path, usageCacheFormatVersion, "database-id-one")

	manager := newUsageCacheManager(archivePath)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	cache, err := manager.Generation(context.Background(), "database-id-one")
	require.NoError(t, err)
	require.True(t, cache.temporary)

	probe := probeUsageCache(context.Background(), path)
	require.NoError(t, probe.Err)
	assert.True(t, probe.Recognized)
	assert.False(t, probe.Compatible)
}

func TestUsageCacheGenerationChangesWithFormatAndDatabaseID(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "sessions.db")
	oldPath := usageCacheGenerationPath(archivePath, 4, "database-id-one")
	seedRecognizedUsageCache(t, oldPath, 4, "database-id-one")

	manager := newUsageCacheManager(archivePath)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	first, err := manager.Generation(context.Background(), "database-id-one")
	require.NoError(t, err)
	require.False(t, first.temporary,
		"format bump must create a persistent generation")
	assert.NotEqual(t, oldPath, first.path, "format bump must open a new generation")
	_, err = os.Stat(oldPath)
	require.NoError(t, err, "format bump must preserve the old generation")

	second, err := manager.Generation(context.Background(), "database-id-two")
	require.NoError(t, err)
	assert.NotEqual(t, first.path, second.path,
		"source database id change must open a new generation")
	assert.Equal(t, "database-id-two",
		readUsageCacheMetadata(t, second.db)[usageCacheMetadataSourceDatabaseID])
}

func TestUsageCacheGenerationPublishesConcurrently(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "sessions.db")
	managers := []*usageCacheManager{
		newUsageCacheManager(archivePath), newUsageCacheManager(archivePath),
	}
	t.Cleanup(func() {
		for _, manager := range managers {
			require.NoError(t, manager.Close())
		}
	})
	var wait sync.WaitGroup
	wait.Add(len(managers))
	errors := make(chan error, len(managers))
	paths := make(chan string, len(managers))
	temporary := make(chan bool, len(managers))
	for _, manager := range managers {
		go func(manager *usageCacheManager) {
			defer wait.Done()
			cache, err := manager.Generation(context.Background(), "database-id")
			if err == nil {
				paths <- cache.path
				temporary <- cache.temporary
			}
			errors <- err
		}(manager)
	}
	wait.Wait()
	close(errors)
	close(paths)
	close(temporary)
	for err := range errors {
		require.NoError(t, err)
	}
	for value := range temporary {
		assert.False(t, value)
	}
	var expected string
	for path := range paths {
		if expected == "" {
			expected = path
		}
		assert.Equal(t, expected, path)
	}
	probe := probeUsageCache(context.Background(), expected)
	require.NoError(t, probe.Err)
	assert.True(t, probe.Compatible)
}

func TestUsageCacheGenerationDoesNotDeleteHeldOldGeneration(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "sessions.db")
	oldPath := usageCacheGenerationPath(archivePath, 0, "old-database-id")
	seedRecognizedUsageCache(t, oldPath, 0, "old-database-id")
	held := openRawUsageCacheTestDB(t, oldPath)
	t.Cleanup(func() { require.NoError(t, held.Close()) })
	_, err := held.Exec(`BEGIN IMMEDIATE`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = held.Exec(`ROLLBACK`) })

	manager := newUsageCacheManager(archivePath)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	_, err = manager.Generation(context.Background(), "new-database-id")
	require.NoError(t, err)
	assert.FileExists(t, oldPath)
}

func TestUsageCacheTemporaryFallbackUsesSameSchema(t *testing.T) {
	dir := t.TempDir()
	blockedParent := filepath.Join(dir, "not-a-directory")
	require.NoError(t, os.WriteFile(blockedParent, []byte("blocked"), 0o600))
	manager := newUsageCacheManager(filepath.Join(blockedParent, "sessions.db"))

	cache, err := manager.Generation(context.Background(), "database-id-one")
	require.NoError(t, err)
	require.True(t, cache.temporary)
	tempPath := cache.path
	assert.Equal(t, "agentsview-usage-facts",
		readUsageCacheMetadata(t, cache.db)[usageCacheMetadataKind])
	require.NoError(t, manager.Close())
	assert.NoFileExists(t, tempPath)
}

func TestUsageCacheLifecycleFollowsArchiveReopenAndClose(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "sessions.db")
	database, err := Open(archivePath)
	require.NoError(t, err)
	databaseID, err := database.GetDatabaseID(context.Background())
	require.NoError(t, err)

	first, err := database.usageCache.Generation(context.Background(), databaseID)
	require.NoError(t, err)
	require.NoError(t, first.db.Ping())
	started := make(chan struct{}, 1)
	database.SetUsageCacheBackfillStarted(func() { started <- struct{}{} })
	// Reopen restarts backfill only after the daemon lifecycle
	// explicitly enabled it.
	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("explicit backfill start did not notify lifecycle observer")
	}

	require.NoError(t, database.Reopen())
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("reopen backfill did not notify lifecycle observer")
	}
	require.NoError(t, first.db.Ping(), "reopen must preserve active cache readers")
	second, err := database.usageCache.Generation(context.Background(), databaseID)
	require.NoError(t, err)
	require.Same(t, first, second)
	require.NoError(t, second.db.Ping())
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))
	assert.NotEmpty(t, readUsageCacheMetadata(t, second.db)[usageCacheMetadataBackfillCompletedAt])

	require.NoError(t, database.Close())
	require.Error(t, second.db.Ping(), "archive close must close its cache first")

	readOnly, err := OpenReadOnly(archivePath)
	require.NoError(t, err)
	readOnlyCache, err := readOnly.usageCache.Generation(
		context.Background(), databaseID,
	)
	require.NoError(t, err)
	require.False(t, readOnlyCache.temporary)
	require.NoError(t, readOnly.Close())
}

func TestUsageCacheReopenRetiresMismatchedGeneration(t *testing.T) {
	database := testDB(t)
	originalID, err := database.GetDatabaseID(t.Context())
	require.NoError(t, err)
	original, err := database.usageCache.Generation(t.Context(), originalID)
	require.NoError(t, err)
	require.NotNil(t, original.fill)
	originalDone := original.fill.done

	const replacementID = "replacement-database-id"
	_, err = database.getWriter().Exec(`UPDATE archive_metadata SET value = ?
		WHERE key = ?`, replacementID, archiveMetadataDatabaseIDKey)
	require.NoError(t, err)
	require.NoError(t, database.Reopen())

	select {
	case <-originalDone:
	default:
		t.Fatal("reopen left the mismatched usage fill coordinator running")
	}
	require.Error(t, original.db.Ping())
	replacement, err := database.usageCache.Generation(t.Context(), replacementID)
	require.NoError(t, err)
	require.NotSame(t, original, replacement)
}

func TestUsageCacheRetirementWaitsForActiveGeneration(t *testing.T) {
	database := testDB(t)
	databaseID, err := database.GetDatabaseID(t.Context())
	require.NoError(t, err)
	cache, release, err := database.usageCache.acquireGeneration(
		t.Context(), databaseID)
	require.NoError(t, err)
	fill, rollup := cache.fill, cache.rollup

	retired := make(chan error, 1)
	go func() {
		retired <- database.usageCache.RetireExcept("replacement-database-id")
	}()
	select {
	case <-fill.ctx.Done():
	case <-time.After(30 * time.Second):
		require.FailNow(t, "generation retirement did not cancel background work")
	}
	select {
	case err := <-retired:
		require.FailNow(t, "retirement closed an actively pinned generation", "%v", err)
	default:
	}
	assert.Same(t, fill, cache.fill)
	assert.Same(t, rollup, cache.rollup)
	require.NoError(t, cache.db.PingContext(t.Context()))

	release()
	require.NoError(t, <-retired)
	require.Error(t, cache.db.PingContext(t.Context()))
	_, _, err = database.usageCache.acquireGeneration(t.Context(), databaseID)
	require.ErrorIs(t, err, errUsageCacheSourceChanged)
}

func TestUsageCacheRetirementWaitsForDetachedFill(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "detached-fill", "project")
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "detached-fill", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T09:00:00Z", Model: "model",
		TokenUsage: []byte(`{"input_tokens":1}`),
	}}))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, releaseGeneration, err := database.usageCache.acquireGeneration(
		t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)

	started := make(chan struct{})
	releaseWork := make(chan struct{})
	var releaseOnce sync.Once
	releaseDetachedWork := func() {
		releaseOnce.Do(func() { close(releaseWork) })
	}
	t.Cleanup(releaseDetachedWork)
	cache.fill.observer.beforeExtract = func([]usageSourceVersion) {
		close(started)
		<-releaseWork
	}
	ctx, cancel := context.WithCancel(t.Context())
	fillDone := make(chan error, 1)
	go func() {
		_, fillErr := cache.fill.Ensure(ctx, snapshot.Versions, 0)
		fillDone <- fillErr
	}()
	<-started
	assert.Equal(t, 2, usageCacheActiveUsers(cache),
		"the caller and detached fill must each pin the generation")

	cancel()
	require.ErrorIs(t, <-fillDone, context.Canceled)
	releaseGeneration()
	assert.Equal(t, 1, usageCacheActiveUsers(cache),
		"caller cancellation must leave the detached fill pinned")

	retireDone := make(chan error, 1)
	go func() {
		retireDone <- database.usageCache.RetireExcept("replacement-database-id")
	}()
	<-cache.fill.ctx.Done()
	select {
	case <-cache.retired:
		assert.Fail(t, "retirement ignored detached fill")
	default:
	}
	require.NoError(t, cache.db.PingContext(t.Context()))
	releaseDetachedWork()
	require.NoError(t, <-retireDone)
	require.Error(t, cache.db.PingContext(t.Context()))
}

func TestUsageCacheRetirementWaitsForDetachedRollup(t *testing.T) {
	database := testDB(t)
	databaseID, err := database.GetDatabaseID(t.Context())
	require.NoError(t, err)
	cache, releaseGeneration, err := database.usageCache.acquireGeneration(
		t.Context(), databaseID)
	require.NoError(t, err)

	started := make(chan struct{})
	releaseWork := make(chan struct{})
	var releaseOnce sync.Once
	releaseDetachedWork := func() {
		releaseOnce.Do(func() { close(releaseWork) })
	}
	t.Cleanup(releaseDetachedWork)
	cache.rollup.observer.beforeEnsure = func() {
		close(started)
		<-releaseWork
	}
	ctx, cancel := context.WithCancel(t.Context())
	rollupDone := make(chan error, 1)
	go func() {
		_, _, rollupErr := cache.rollup.Ensure(
			ctx,
			usageQuerySnapshot{DatabaseID: databaseID, location: time.UTC},
			nil,
			export.NewPricingResolver(nil),
		)
		rollupDone <- rollupErr
	}()
	<-started
	assert.Equal(t, 2, usageCacheActiveUsers(cache),
		"the caller and detached rollup must each pin the generation")

	cancel()
	require.ErrorIs(t, <-rollupDone, context.Canceled)
	releaseGeneration()
	assert.Equal(t, 1, usageCacheActiveUsers(cache),
		"caller cancellation must leave the detached rollup pinned")

	retireDone := make(chan error, 1)
	go func() {
		retireDone <- database.usageCache.RetireExcept("replacement-database-id")
	}()
	<-cache.rollup.ctx.Done()
	select {
	case <-cache.retired:
		assert.Fail(t, "retirement ignored detached rollup")
	default:
	}
	require.NoError(t, cache.db.PingContext(t.Context()))
	releaseDetachedWork()
	require.NoError(t, <-retireDone)
	require.Error(t, cache.db.PingContext(t.Context()))
}

func usageCacheActiveUsers(cache *usageCache) int {
	cache.lifecycleMu.Lock()
	defer cache.lifecycleMu.Unlock()
	return cache.users
}

func readUsageCacheMetadata(t *testing.T, conn *sql.DB) map[string]string {
	t.Helper()
	rows, err := conn.Query(`SELECT key, value FROM usage_cache_metadata`)
	require.NoError(t, err)
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var key, value string
		require.NoError(t, rows.Scan(&key, &value))
		result[key] = value
	}
	require.NoError(t, rows.Err())
	return result
}

func openRawUsageCacheTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	require.NoError(t, conn.Ping())
	return conn
}

func seedRecognizedUsageCache(
	t *testing.T, path string, format int, databaseID string,
) {
	t.Helper()
	conn := openRawUsageCacheTestDB(t, path)
	defer conn.Close()
	_, err := conn.Exec(`PRAGMA application_id = 1096176963;
		CREATE TABLE usage_cache_metadata(key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	require.NoError(t, err)
	for key, value := range map[string]any{
		usageCacheMetadataKind:             usageCacheKind,
		usageCacheMetadataFormatVersion:    format,
		usageCacheMetadataSourceDatabaseID: databaseID,
	} {
		_, err = conn.Exec(
			`INSERT INTO usage_cache_metadata(key, value) VALUES (?, ?)`, key, value)
		require.NoError(t, err)
	}
}
