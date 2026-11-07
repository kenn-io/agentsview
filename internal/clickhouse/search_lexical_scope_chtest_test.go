//go:build chtest

package clickhouse

import (
	"go.kenn.io/agentsview/internal/storage"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestLexicalScope(t *testing.T) {
	local, target := seedFixture(t)
	dbtest.SeedLexicalScope(t, local)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertLexicalScope(t, store)
	dbtest.AssertHybridKeywordOnlyScope(t, store)
}
