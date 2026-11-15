//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
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
	_, identity := (analyticsSQL{store: store}).TrendsDisk()
	require.NoError(t, store.Close())
	dbtest.AssertTrendsPartsReuse(t, func(retained bool) db.Store {
		opened := openPushedStore(t, path)
		_, reopenedIdentity := (analyticsSQL{store: opened}).TrendsDisk()
		require.Equal(t, identity, reopenedIdentity)
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

type legacyTrendSource struct{ analyticsSQL }

func (s legacyTrendSource) TrendWindowSessions(ctx context.Context, f db.AnalyticsFilter, ids []string) ([]string, error) {
	var selected []string
	err := s.VisitSessionIDSets(ctx, ids, func(ctx context.Context, set string, args []any) error {
		window, windowArgs := s.TrendSessionWindow(f, set, args)
		found, err := db.ScanTrendIDs(s.QueryContext(ctx, "SELECT s.id FROM sessions s WHERE s.id IN "+set+window, append(args, windowArgs...)...))
		selected = append(selected, found...)
		return err
	})
	return selected, err
}

func (s legacyTrendSource) VisitTrendMessages(ctx context.Context, _ db.AnalyticsFilter, ids []string, visit func(string, string, time.Time)) error {
	return s.VisitSessionIDSets(ctx, ids, func(ctx context.Context, set string, args []any) error {
		query, args := s.TrendMessagesSQL(set, args)
		rows, err := s.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, content string
			var ts time.Time
			if err := rows.Scan(&id, &content, &ts); err != nil {
				return err
			}
			visit(id, content, ts.UTC())
		}
		return rows.Err()
	})
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
	source := legacyTrendSource{analyticsSQL{store: store}}
	old, handled, err := db.TrendsFromParts(t.Context(), &store.usageDisk, "duckdb", f, terms, "day", source)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, 1, old.Series[0].Total)
	require.NoError(t, store.Close())
	writeUsageMirror(t, path, `UPDATE messages SET is_system_prefixed = TRUE WHERE session_id = 'trends-window-fallback'`)
	store = openUsagePartsStore(t, path)
	var reads []string
	reset := db.SetTrendPartsReadObserver(func(ids []string) { reads = append(reads, ids...) })
	defer reset()
	source = legacyTrendSource{analyticsSQL{store: store}}
	stale, handled, err := db.TrendsFromParts(t.Context(), &store.usageDisk, "duckdb", f, terms, "day", source)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, old, stale)
	require.Empty(t, reads)
	versions, err := source.TrendSessionVersions(t.Context(), f)
	require.NoError(t, err)
	ids := make([]string, 0, len(versions))
	for id := range versions {
		ids = append(ids, id)
	}
	windowIDs, err := source.TrendWindowSessions(t.Context(), f, ids)
	require.NoError(t, err)
	fresh, err := store.GetTrendsTerms(t.Context(), f, terms, "day")
	require.NoError(t, err)
	require.Zero(t, fresh.Series[0].Total)
	require.ElementsMatch(t, windowIDs, reads)
	require.NoError(t, store.Close())
	store = openUsagePartsStore(t, path)
	reads = nil
	_, err = store.GetTrendsTerms(t.Context(), f, terms, "day")
	require.NoError(t, err)
	require.Empty(t, reads)
	require.NoError(t, store.Close())
}
