//go:build chtest

package clickhouse

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
)

// A scan that fails part way must not memoize the counts it did not read;
// the next request scans those sessions again and returns full counts.
func TestFrustrationMarkersAreNotMemoizedAfterFailedScan(t *testing.T) {
	t.Parallel()
	store, _, _ := newPushedStore(t)
	rows := func() []db.SignalRow {
		return []db.SignalRow{{ID: fixtureAlphaID}, {ID: fixtureBetaID}}
	}
	versions := map[string]uint64{fixtureAlphaID: 7, fixtureBetaID: 7}
	// Listing rows without stored counts, as an older writer leaves them.
	sessions := []readbase.AnalyticsSession{
		{ID: fixtureAlphaID, PushVersion: 7}, {ID: fixtureBetaID, PushVersion: 7},
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.Error(t, store.chPopulateFrustrationMarkers(canceled, rows(), sessions))
	for id := range versions {
		_, hit := store.frustrationMarkers.lookup(id, versions[id])
		require.False(t, hit, "%s must be scanned again after a failed read", id)
	}
	scanned := rows()
	require.NoError(t, store.chPopulateFrustrationMarkers(t.Context(), scanned, sessions))
	fresh := rows()
	require.NoError(t, NewStoreFromDB(store.DB(), store.bulk).chPopulateFrustrationMarkers(t.Context(), fresh, nil))
	require.Equal(t, fresh, scanned)
	for id := range versions {
		_, hit := store.frustrationMarkers.lookup(id, versions[id])
		require.True(t, hit)
	}
}
