package db_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestGetTrendsTermsSQLiteDateWindowEdges(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedTrendsWindow(t, d)
	dbtest.AssertTrendsWindow(t, d)
	dbtest.AssertTrendsPartsReuse(t, func(retained bool) db.Store {
		open := db.OpenReadOnly
		if retained {
			open = db.Open
		}
		opened, err := open(t.Context(), d.Path())
		require.NoError(t, err)
		return opened
	}, func(action string) { dbtest.ChangeTrendsWindow(t, d, action) })
}

func TestTrendsPartsSkipUnchangedWindowMiss(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("CACHE_DIRECTORY", cache)
	d := dbtest.OpenTestDB(t)
	dbtest.SeedTrendsWindow(t, d)
	terms, err := db.ParseTrendTerms([]string{"fallback"})
	require.NoError(t, err)
	filter := db.AnalyticsFilter{From: "2025-03-10", To: "2025-03-10", Timezone: "UTC"}
	first, err := d.GetTrendsTerms(t.Context(), filter, terms, "day")
	require.NoError(t, err)
	require.Equal(t, 1, first.Series[0].Total)
	files, err := filepath.Glob(filepath.Join(cache, "sqlite-trends", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	file, err := os.Open(files[0])
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	before, err := file.Stat()
	require.NoError(t, err)
	second, err := d.GetTrendsTerms(t.Context(), filter, terms, "week")
	require.NoError(t, err)
	require.Equal(t, 1, second.Series[0].Total)
	after, err := os.Stat(files[0])
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "an unchanged out-of-window session must not rewrite the parts file")

	require.NoError(t, d.SoftDeleteSession(t.Context(), "trends-window-fallback"))
	third, err := d.GetTrendsTerms(t.Context(), filter, terms, "day")
	require.NoError(t, err)
	require.Zero(t, third.Series[0].Total)
	afterDelete, err := os.Stat(files[0])
	require.NoError(t, err)
	require.False(t, os.SameFile(before, afterDelete), "deleting a stored session must update the parts file")
}
