//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPGNativeSearchSources(t *testing.T) {
	url := testPGURL(t)
	cleanPGSchema(t, url)
	t.Cleanup(func() { cleanPGSchema(t, url) })
	archive := testDB(t)
	dbtest.SeedNativeSearch(t, archive)
	push, err := New(url, "agentsview", archive, "test", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, push.Close()) })
	require.NoError(t, push.EnsureSchema(t.Context()))
	_, err = push.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(url, "agentsview", true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.CheckNativeSearch(t, store, true)
}
