//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/readbase"
	"go.kenn.io/agentsview/internal/storage"
)

func TestDuckTrendsTermsDateWindowEdges(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local := newLocalDB(t)
	dbtest.SeedTrendsWindow(t, local)
	path := filepath.Join(t.TempDir(), "trends.duckdb")
	_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	store := openPushedStore(t, path)
	dbtest.AssertTrendsWindow(t, store)
	require.NoError(t, store.Close())
	dbtest.AssertTrendsPartsReuse(t, func(retained bool) db.Store {
		opened := openPushedStore(t, path)
		if retained {
			require.NoError(t, opened.OpenActivityReportDisk(t.Context()))
		}
		return opened
	}, func(action string) {
		dbtest.ChangeTrendsWindow(t, local, action)
		_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
		require.NoError(t, err)
	})
}

func TestDuckTrendsUnversionedSessions(t *testing.T) {
	for _, tc := range []struct {
		name, fingerprint string
	}{{"null", "NULL"}, {"empty", "''"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			local := newLocalDB(t)
			dbtest.SeedTrendsWindow(t, local)
			path := filepath.Join(t.TempDir(), "trends.duckdb")
			_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
			require.NoError(t, err)
			writeUsageMirror(t, path, `UPDATE sessions SET agentsview_push_fingerprint = `+tc.fingerprint+` WHERE id = 'trends-window-fallback'`)
			terms, err := db.ParseTrendTerms([]string{"fallback"})
			require.NoError(t, err)
			store := openUsagePartsStore(t, path)
			got, err := store.GetTrendsTerms(t.Context(), db.AnalyticsFilter{From: "2025-03-10", To: "2025-03-10", Timezone: "UTC"}, terms, "day")
			require.NoError(t, err)
			require.Len(t, got.Series, 1)
			require.Equal(t, 1, got.Series[0].Total)
			require.NoError(t, store.Close())
			dbtest.ChangeTrendsWindow(t, local, "rewrite")
			_, err = Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
			require.NoError(t, err)
			store = openUsagePartsStore(t, path)
			var fingerprint string
			require.NoError(t, store.queryRowContext(t.Context(), `SELECT agentsview_push_fingerprint FROM sessions WHERE id = 'trends-window-fallback'`).Scan(&fingerprint))
			require.NotEmpty(t, fingerprint)
			got, err = store.GetTrendsTerms(t.Context(), db.AnalyticsFilter{From: "2025-03-10", To: "2025-03-10", Timezone: "UTC"}, terms, "day")
			require.NoError(t, err)
			require.Len(t, got.Series, 1)
			require.Equal(t, 2, got.Series[0].Total)
			require.NoError(t, store.Close())
		})
	}
}

type legacyTrendBackend struct{ analyticsSQL }

func (s legacyTrendBackend) TrendsDisk() (*kept.Disk, string) {
	return &s.store.usageDisk, "duckdb"
}

func TestDuckTrendsPartsInvalidateLegacyIdentity(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local := newLocalDB(t)
	dbtest.SeedTrendsWindow(t, local)
	path := filepath.Join(t.TempDir(), "trends.duckdb")
	_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	terms, err := db.ParseTrendTerms([]string{"fallback"})
	require.NoError(t, err)
	f := db.AnalyticsFilter{From: "2025-03-10", To: "2025-03-10", Timezone: "UTC"}
	store := openUsagePartsStore(t, path)
	_, identity := (analyticsSQL{store: store}).TrendsDisk()
	require.Equal(t, "duckdb|"+strconv.Itoa(SchemaVersion), identity)
	var reads []string
	reset := db.SetTrendPartsReadObserver(func(ids []string) { reads = append(reads, ids...) })
	defer reset()
	old, err := readbase.NewAnalytics(legacyTrendBackend{analyticsSQL{store: store}}, "duckdb").GetTrendsTerms(t.Context(), f, terms, "day")
	require.NoError(t, err)
	require.Equal(t, 1, old.Series[0].Total)
	windowIDs := append([]string(nil), reads...)
	require.NotEmpty(t, windowIDs)
	reads = nil
	fresh, err := store.GetTrendsTerms(t.Context(), f, terms, "day")
	require.NoError(t, err)
	require.Equal(t, old, fresh)
	require.ElementsMatch(t, windowIDs, reads)
	require.NoError(t, store.Close())
}
