//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestClickHouseSessionAnnotationParity(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	dbtest.SeedSessionAnnotations(t, local)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertSessionAnnotationParity(t, local, store)
}

func TestPushRepublishesLabelOnlyChanges(t *testing.T) {
	ctx := t.Context()
	store, syncer, local := newPushedStore(t)

	_, err := local.UpdateSessionLabels(ctx, fixtureBetaID, []string{"ticket-9"}, nil)
	require.NoError(t, err)
	res, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsPushed, "a label-only change re-pushes its session")

	_, err = local.SetSessionLabels(ctx, fixtureBetaID, nil)
	require.NoError(t, err)
	res, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsPushed, "removing every label re-pushes its session")
	beta, err := store.GetSession(ctx, fixtureBetaID)
	require.NoError(t, err)
	require.NotNil(t, beta)
	assert.Nil(t, beta.Labels)
}

// A mirror pushed before the annotation columns existed gains them in
// place, and its existing rows read back as unlabeled and unlinked.
func TestEnsureSchemaAddsAnnotationColumnsToOlderMirror(t *testing.T) {
	ctx := t.Context()
	store, _, _ := newPushedStore(t)
	conn := store.DB()
	for _, column := range []string{"pr_links", "labels"} {
		_, err := conn.ExecContext(ctx, "ALTER TABLE sessions DROP COLUMN "+column)
		require.NoError(t, err)
	}
	err := CheckSchemaCompat(ctx, conn)
	require.Error(t, err)
	assert.ErrorContains(t, err, "sessions.labels")
	assert.ErrorContains(t, err, "sessions.pr_links")

	require.NoError(t, EnsureSchemaOn(ctx, conn))
	require.NoError(t, CheckSchemaCompat(ctx, conn))
	got, err := store.GetSession(ctx, fixtureAlphaID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.Labels)
	assert.Nil(t, got.PRLinks)
}
