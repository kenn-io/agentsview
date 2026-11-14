//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

type trendEngineCounter struct {
	storeExecutor
	versions atomic.Int64
}

func (c *trendEngineCounter) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if query == "SELECT version()" {
		c.versions.Add(1)
	}
	return c.storeExecutor.QueryRowContext(ctx, query, args...)
}

func TestPGTrendsCacheUsesConfiguredEngineVersion(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	_, syncer, store, _ := keptUsageFixture(t)
	_, err := store.pg.ExecContext(t.Context(), `UPDATE sessions SET message_count = 1`)
	require.NoError(t, err)
	counter := &trendEngineCounter{storeExecutor: store.pg}
	store.pg = counter
	ctx := t.Context()
	require.NoError(t, store.ConfigureUsageCache(ctx, t.TempDir(), testPGURL(t), syncer.schema))
	require.Equal(t, int64(1), counter.versions.Load())
	terms, err := db.ParseTrendTerms([]string{"answer"})
	require.NoError(t, err)
	f := db.AnalyticsFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
	var reads [][]string
	reset := db.SetTrendPartsReadObserver(func(ids []string) { reads = append(reads, append([]string(nil), ids...)) })
	t.Cleanup(reset)
	_, err = store.loadTrendsTerms(ctx, f, terms, "day")
	require.NoError(t, err)
	reads = nil
	_, err = store.loadTrendsTerms(ctx, f, terms, "day")
	require.NoError(t, err)
	assert.Empty(t, reads)
	assert.Equal(t, int64(1), counter.versions.Load())
}

func TestPGUsageCacheWhenTrendsDiskFails(t *testing.T) {
	cacheFile := filepath.Join(t.TempDir(), "cache-file")
	require.NoError(t, os.WriteFile(cacheFile, nil, 0600))
	t.Setenv("CACHE_DIRECTORY", cacheFile)
	_, syncer, store, _ := keptUsageFixture(t)
	ctx := t.Context()
	require.NoError(t, store.ConfigureUsageCache(ctx, t.TempDir(), testPGURL(t), syncer.schema))
	assert.NotNil(t, store.usageCache)
}
