//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func pushAnnotatedMirror(t *testing.T) (*db.DB, string) {
	t.Helper()
	local := newLocalDB(t)
	dbtest.SeedSessionAnnotations(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err := Push(t.Context(), path, local, "m", storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)
	return local, path
}

func openPushedStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	return store
}

func TestDuckDBSessionAnnotationParity(t *testing.T) {
	local, path := pushAnnotatedMirror(t)
	store := openPushedStore(t, path)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertSessionAnnotationParity(t, local, store)
}

func TestPushIncrementalMirrorsLabelOnlyChange(t *testing.T) {
	ctx := t.Context()
	local, path := pushAnnotatedMirror(t)
	push := func() {
		t.Helper()
		_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
		require.NoError(t, err)
	}

	_, err := local.UpdateSessionLabels(ctx, dbtest.AnnotatedWorkerID, []string{"late"}, nil)
	require.NoError(t, err)
	push()
	_, err = local.SetSessionLabels(ctx, dbtest.AnnotatedLauncherID, nil)
	require.NoError(t, err)
	push()

	store := openPushedStore(t, path)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	worker, err := store.GetSession(ctx, dbtest.AnnotatedWorkerID)
	require.NoError(t, err)
	require.NotNil(t, worker)
	assert.Equal(t, []string{"late", "role=worker"}, worker.Labels)
	launcher, err := store.GetSession(ctx, dbtest.AnnotatedLauncherID)
	require.NoError(t, err)
	require.NotNil(t, launcher)
	assert.Nil(t, launcher.Labels)
	assert.Len(t, launcher.PRLinks, 1,
		"clearing labels must not drop the session's pull request links")
}
