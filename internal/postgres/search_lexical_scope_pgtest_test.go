//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestLexicalScope(t *testing.T) {
	syncer, local, _, ctx := newSessionProvenancePushSync(t, "agentsview_lexical_scope_test")
	dbtest.SeedLexicalScope(t, local)
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertLexicalScope(t, store)
	dbtest.AssertHybridKeywordOnlyScope(t, store)
}
