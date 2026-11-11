//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

// TestDuckDBSessionUsagePins pins the mirror's session usage responses for
// sessions with and without usage, including one that gains usage in a
// later push.
func TestDuckDBSessionUsagePins(t *testing.T) {
	local := newLocalDB(t)
	dbtest.SeedSessionUsagePins(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	push := func(full bool) {
		t.Helper()
		_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, full, nil)
		require.NoError(t, err)
	}
	push(true)
	store := openPushedStore(t, path)
	dbtest.AssertSessionUsagePins(t, store)
	require.NoError(t, store.Close())

	dbtest.GrowSessionUsagePin(t, local)
	push(false)
	store = openPushedStore(t, path)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertGrownSessionUsagePin(t, store)
}

// TestDuckDBSessionUsageWithoutUsageSkipsUsageQuery checks that a session
// with no usage rows is answered without the usage aggregate query, while a
// session whose usage comes only from usage events, which leaves the session
// row without token totals, still runs it.
func TestDuckDBSessionUsageWithoutUsageSkipsUsageQuery(t *testing.T) {
	local := newLocalDB(t)
	dbtest.SeedSessionUsagePins(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)
	store := openPushedStore(t, path)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	var usageQueries int
	previousObserver := analyticsQueryObserver
	analyticsQueryObserver = func(query string) {
		if strings.Contains(query, "usage_localized") {
			usageQueries++
		}
	}
	t.Cleanup(func() { analyticsQueryObserver = previousObserver })

	for _, tc := range []struct {
		id   string
		want int
	}{{dbtest.UsagePinEmptyID, 0}, {dbtest.UsagePinEventsID, 1}} {
		usageQueries = 0
		_, err := store.GetSessionUsage(t.Context(), tc.id, false)
		require.NoError(t, err)
		assert.Equal(t, tc.want, usageQueries, "usage aggregate queries for %s", tc.id)
	}
}
