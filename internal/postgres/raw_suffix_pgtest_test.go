//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestRawSuffixMatchesArchivePG(t *testing.T) {
	syncer, local, _, ctx := newSessionProvenancePushSync(t, "agentsview_raw_suffix_shared_test")
	dbtest.SeedRawSuffix(t, local)
	dbtest.TrashRawSuffix(t, local)
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertRawSuffix(t, store)
}
