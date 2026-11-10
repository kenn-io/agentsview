//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
)

// TestPGSessionUsagePins pins the replica's session usage responses for
// sessions with and without usage. One store serves both reads of the
// growing session, so usage pushed after its first read must reach the
// second.
func TestPGSessionUsagePins(t *testing.T) {
	syncer, local, _, ctx := newSessionProvenancePushSync(t, "agentsview_session_usage_pins_test")
	dbtest.SeedSessionUsagePins(t, local)
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	dbtest.AssertSessionUsagePins(t, store)
	dbtest.GrowSessionUsagePin(t, local)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	dbtest.AssertGrownSessionUsagePin(t, store)
}
