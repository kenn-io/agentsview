//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestPGSessionAnnotationParity(t *testing.T) {
	syncer, local, _, ctx := newSessionProvenancePushSync(
		t, "agentsview_session_annotations_test",
	)
	dbtest.SeedSessionAnnotations(t, local)
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err, "Push")

	store, err := NewStore(testPGURL(t), syncer.schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()
	dbtest.AssertSessionAnnotationParity(t, local, store)
}

func TestPGPushRepushesAnnotationOnlyChanges(t *testing.T) {
	syncer, local, pg, ctx := newSessionProvenancePushSync(
		t, "agentsview_session_annotations_repush_test",
	)
	dbtest.SeedSessionAnnotations(t, local)
	const id = dbtest.AnnotatedOtherID
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err, "initial Push")

	readPG := func(t *testing.T) (string, []string) {
		t.Helper()
		var prLinks string
		var labels []string
		require.NoError(t, pg.QueryRowContext(ctx,
			`SELECT pr_links, array_to_json(labels)::text
			 FROM sessions WHERE id = $1`, id,
		).Scan(&prLinks, db.LabelsScanner(&labels)))
		return prLinks, labels
	}

	unchanged, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err, "unchanged Push")
	assert.Equal(t, 0, unchanged.SessionsPushed)

	_, err = local.UpdateSessionLabels(ctx, id, []string{"role=lead"}, nil)
	require.NoError(t, err, "UpdateSessionLabels")
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err, "label-only Push")
	assert.Equal(t, 1, result.SessionsPushed)
	_, labels := readPG(t)
	assert.Equal(t, []string{"role=lead", "ticket=XYZ-9"}, labels)

	stored, err := local.GetSession(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, stored)
	// A new pr-link record arrives with a transcript write, which moves the
	// source mtime into the push window; mtime is not part of the push
	// fingerprint, so only the link change can trigger the re-push.
	stored.PRLinks = append(stored.PRLinks, dbtest.AnnotatedPRLink("acme/widgets", 9))
	stored.FileMtime = new(time.Now().UnixNano())
	require.NoError(t, local.UpsertSession(ctx, *stored), "UpsertSession")
	result, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err, "pr-link-only Push")
	assert.Equal(t, 1, result.SessionsPushed)
	prLinks, _ := readPG(t)
	assert.Equal(t, db.EncodePRLinks(stored.PRLinks), prLinks)

	_, err = local.SetSessionLabels(ctx, id, nil)
	require.NoError(t, err, "clear labels")
	result, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err, "label-clearing Push")
	assert.Equal(t, 1, result.SessionsPushed)
	_, labels = readPG(t)
	assert.Nil(t, labels)
}

func TestPGPushAddsAnnotationColumnsToOlderSchema(t *testing.T) {
	syncer, local, pg, ctx := newSessionProvenancePushSync(
		t, "agentsview_session_annotations_upgrade_test",
	)
	_, err := pg.ExecContext(ctx,
		`ALTER TABLE sessions DROP COLUMN pr_links, DROP COLUMN labels`)
	require.NoError(t, err, "drop annotation columns")
	require.False(t, pushSchemaCurrent(ctx, pg),
		"a schema without the annotation columns is not current")

	dbtest.SeedSessionAnnotations(t, local)
	syncer.schemaDone = false
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err, "Push migrates the schema")
	assert.True(t, pushSchemaCurrent(ctx, pg))

	store, err := NewStore(testPGURL(t), syncer.schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()
	got, err := store.GetSession(ctx, dbtest.AnnotatedLauncherID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{"role=lead", "ticket=ABC-1"}, got.Labels)
	require.Len(t, got.PRLinks, 1)
}
