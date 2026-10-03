//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestCHNativeSearchSources(t *testing.T) {
	archive, target := seedFixture(t)
	dbtest.SeedNativeSearch(t, archive)
	push := newTestSync(t, archive, target, storage.PusherOptions{})
	_, err := push.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.CheckNativeSearch(t, store, false)
}
