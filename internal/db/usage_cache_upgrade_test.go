//go:build fts5

package db

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestUsageCacheUpgradePreservesWarmFacts(t *testing.T) {
	path, old, id := copyUsageV23Fixture(t)
	oldDB := openRawUsageCacheTestDB(t, old)
	factsQuery := `SELECT 0, cached_session_id, fact_index, input_tokens, output_tokens FROM usage_facts ORDER BY cached_session_id, fact_index`
	revisionsQuery := `SELECT 0, session_id, install_revision FROM usage_cached_sessions ORDER BY session_id`
	facts := usageSpoolFactValues(t, oldDB, factsQuery)
	revisions := usageSpoolFactValues(t, oldDB, revisionsQuery)
	require.NotEmpty(t, facts)
	require.NoError(t, oldDB.Close())
	database, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	cache, err := database.usageCache.Generation(t.Context(), id)
	require.NoError(t, err)
	var extracted []usageSourceVersion
	cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) { extracted = append(extracted, versions...) }
	freshPath, freshOld, _ := copyUsageV23Fixture(t)
	require.NoError(t, os.Remove(freshOld))
	fresh, err := Open(t.Context(), freshPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	filter := UsageFilter{Timezone: "UTC", Breakdowns: true, PairwiseSides: true}
	compare := func() {
		t.Helper()
		want, err := fresh.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err)
		got, err := database.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		wantTop, err := fresh.GetTopSessionsByCost(t.Context(), filter, 10)
		require.NoError(t, err)
		gotTop, err := database.GetTopSessionsByCost(t.Context(), filter, 10)
		require.NoError(t, err)
		assert.Equal(t, wantTop, gotTop)
		wantCounts, err := fresh.GetUsageSessionCounts(t.Context(), filter)
		require.NoError(t, err)
		gotCounts, err := database.GetUsageSessionCounts(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, wantCounts, gotCounts)
	}
	compare()
	assert.Empty(t, extracted)
	assert.Equal(t, facts, usageSpoolFactValues(t, cache.db, factsQuery))
	assert.Equal(t, revisions, usageSpoolFactValues(t, cache.db, revisionsQuery))
	probe := probeUsageCache(t.Context(), cache.path)
	require.NoError(t, probe.Err)
	assert.True(t, probe.Compatible)
	var appID, vacuum int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `PRAGMA application_id`).Scan(&appID))
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `PRAGMA auto_vacuum`).Scan(&vacuum))
	assert.Equal(t, usageCacheApplicationID, appID)
	assert.Equal(t, 2, vacuum)
	assert.NoFileExists(t, old)
	freshCache, err := fresh.usageCache.Generation(t.Context(), id)
	require.NoError(t, err)
	objects := `SELECT 0, type, name, tbl_name FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`
	freshObjects := usageSpoolFactValues(t, freshCache.db, objects)
	assert.Equal(t, freshObjects, usageSpoolFactValues(t, cache.db, objects))
	for _, object := range freshObjects {
		kind, name := object[0].(string), object[1].(string)
		var queries []string
		switch kind {
		case "table":
			queries = []string{
				`SELECT 0, name, type, "notnull", dflt_value, pk FROM pragma_table_info(?) ORDER BY name`,
				`SELECT 0, id, seq, "table", "from", "to", on_update, on_delete, match FROM pragma_foreign_key_list(?) ORDER BY id, seq`,
				`SELECT 0, name, "unique", origin, partial FROM pragma_index_list(?) ORDER BY name`,
			}
		}
		for _, query := range queries {
			assert.Equal(t, usageSpoolFactValues(t, freshCache.db, query, name), usageSpoolFactValues(t, cache.db, query, name), name)
		}
	}
	indexesQuery := `SELECT 0, name FROM sqlite_schema WHERE type='index' ORDER BY name`
	indexes := usageSpoolFactValues(t, freshCache.db, indexesQuery)
	assert.Equal(t, indexes, usageSpoolFactValues(t, cache.db, indexesQuery))
	for _, index := range indexes {
		query := `SELECT 0, seqno, cid, name, "desc", coll, key FROM pragma_index_xinfo(?) ORDER BY seqno`
		name := index[0].(string)
		assert.Equal(t, usageSpoolFactValues(t, freshCache.db, query, name), usageSpoolFactValues(t, cache.db, query, name), name)
	}
	for _, db := range []*DB{database, fresh} {
		require.NoError(t, db.InsertMessages(t.Context(), []Message{{SessionID: "one", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T23:40:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":50}`)}}))
		require.NoError(t, db.SoftDeleteSession(t.Context(), "two"))
		seedUsageSnapshotSession(t, db, "new-peer", "project-c", "2026-08-10T11:30:00Z", 0, 30, "model-a")
		db.SetCustomPricing(map[string]config.CustomModelRate{"journal-model": {InputMicrodollarsPerMTok: 1000000}})
	}
	compare()
	var ids []string
	for _, version := range extracted {
		ids = append(ids, version.SessionID)
	}
	assert.ElementsMatch(t, []string{"one", "new-peer"}, ids)
}

func TestUsageCacheUpgradeRejectsInvalidCandidate(t *testing.T) {
	for _, variant := range []string{"source", "incomplete", "cancelled"} {
		t.Run(variant, func(t *testing.T) {
			archive, old, id := copyUsageV23Fixture(t)
			var err error
			db := openRawUsageCacheTestDB(t, old)
			switch variant {
			case "source":
				_, err = db.ExecContext(t.Context(), `UPDATE usage_cache_metadata SET value='other' WHERE key='source_database_id'`)
			case "incomplete":
				_, err = db.ExecContext(t.Context(), `DROP TABLE usage_facts`)
			}
			require.NoError(t, err)
			require.NoError(t, db.Close())
			ctx := t.Context()
			if variant == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			candidate := filepath.Join(filepath.Dir(archive), "candidate.db")
			seeded, err := seedUpgradedUsageCache(ctx, archive, candidate, id)
			assert.False(t, seeded)
			if variant == "cancelled" {
				require.ErrorIs(t, err, context.Canceled)
				assert.NoFileExists(t, candidate)
				_, _, err = publishUsageCache(ctx, archive, usageCacheGenerationPath(archive, 24, id), id)
				require.ErrorIs(t, err, context.Canceled)
				candidates, err := filepath.Glob(filepath.Join(filepath.Dir(archive), ".usage-cache-init-*"))
				require.NoError(t, err)
				assert.Empty(t, candidates)
			} else {
				require.NoError(t, err)
				manager := newUsageCacheManager(archive)
				t.Cleanup(func() { require.NoError(t, manager.Close()) })
				opened, err := manager.Generation(t.Context(), id)
				require.NoError(t, err)
				assert.False(t, opened.temporary)
				assert.True(t, probeUsageCache(t.Context(), opened.path).Compatible)
			}
		})
	}
}

func TestUsageCacheUpgradeConcurrentPublishAndLease(t *testing.T) {
	archive, old, id := copyUsageV23Fixture(t)
	lease, err := acquireUsageCacheLease(old)
	require.NoError(t, err)
	managers := []*usageCacheManager{newUsageCacheManager(archive), newUsageCacheManager(archive)}
	t.Cleanup(func() {
		for _, manager := range managers {
			require.NoError(t, manager.Close())
		}
	})
	errs := make(chan error, len(managers))
	paths := make(chan string, len(managers))
	for _, manager := range managers {
		go func() {
			opened, err := manager.Generation(t.Context(), id)
			if err == nil {
				paths <- opened.path
			}
			errs <- err
		}()
	}
	for range managers {
		require.NoError(t, <-errs)
	}
	first, second := <-paths, <-paths
	assert.Equal(t, first, second)
	assert.FileExists(t, old)
	require.NoError(t, lease.Close())
	retired, err := retireUsageCacheGeneration(t.Context(), archive, old)
	require.NoError(t, err)
	assert.True(t, retired)
}

func copyUsageV23Fixture(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"archive.db", "cache.db"} {
		input, err := os.Open(filepath.Join("testdata/usage-cache-v23", name+".gz"))
		require.NoError(t, err)
		reader, err := gzip.NewReader(input)
		require.NoError(t, err)
		output, err := os.Create(filepath.Join(dir, name))
		require.NoError(t, err)
		_, err = io.Copy(output, reader)
		require.NoError(t, err)
		require.NoError(t, output.Close())
		require.NoError(t, reader.Close())
		require.NoError(t, input.Close())
	}
	archive := filepath.Join(dir, "archive.db")
	cache := filepath.Join(dir, "cache.db")
	db := openRawUsageCacheTestDB(t, cache)
	id := readUsageCacheMetadata(t, db)[usageCacheMetadataSourceDatabaseID]
	require.NotEmpty(t, id)
	require.NoError(t, db.Close())
	old := usageCacheGenerationPath(archive, 23, id)
	require.NoError(t, os.Rename(cache, old))
	return archive, old, id
}

func TestUsageCacheUpgradeCorruptFactsRebuilds(t *testing.T) {
	archive, old, id := copyUsageV23Fixture(t)
	db := openRawUsageCacheTestDB(t, old)
	var root, pageSize int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT rootpage FROM sqlite_schema WHERE name='usage_facts'`).Scan(&root))
	require.NoError(t, db.QueryRowContext(t.Context(), `PRAGMA page_size`).Scan(&pageSize))
	require.NoError(t, db.Close())
	file, err := os.OpenFile(old, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteAt([]byte{0xff, 0xff, 0xff, 0xff}, (root-1)*pageSize)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	probe := probeUsageCache(t.Context(), old)
	require.NoError(t, probe.Err)
	require.True(t, probe.Recognized)
	manager := newUsageCacheManager(archive)
	cache, err := manager.Generation(t.Context(), id)
	require.NoError(t, err)
	assert.False(t, cache.temporary)
	assert.NoFileExists(t, old)
	path := cache.path
	require.NoError(t, manager.Close())
	manager = newUsageCacheManager(archive)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	again, err := manager.Generation(t.Context(), id)
	require.NoError(t, err)
	assert.False(t, again.temporary)
	assert.Equal(t, path, again.path)
}
