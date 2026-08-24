//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureSchemaRepairsLegacySourceMissingDeletion(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err)
	defer store.Close()

	_, err = store.DB().Exec(`
		INSERT INTO sessions (
			id, machine, project, agent, message_count,
			user_message_count, deleted_at, source_deleted_at, deletion_cause
		) VALUES
			('source-single', 'machine', 'project', 'claude', 1, 1,
			 NOW(), NOW(), 'source_missing'),
			('source-batch', 'machine', 'project', 'claude', 1, 1,
			 NOW(), NOW(), 'source_missing')`)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(context.Background(), store.DB(), testSchema))

	visible, err := store.GetSession(t.Context(), "source-single")
	require.NoError(t, err)
	require.NotNil(t, visible)
	for _, id := range []string{"source-single", "source-batch"} {
		var deletedAt, sourceDeletedAt, cause *string
		require.NoError(t, store.DB().QueryRow(`
			SELECT deleted_at::text, source_deleted_at::text, deletion_cause
			FROM sessions WHERE id = $1`, id,
		).Scan(&deletedAt, &sourceDeletedAt, &cause))
		assert.Nil(t, deletedAt)
		assert.Nil(t, sourceDeletedAt)
		assert.Nil(t, cause)
	}

	require.NoError(t, store.SoftDeleteSession("source-single"))
	count, err := store.SoftDeleteSessions([]string{"source-batch"})
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	for _, id := range []string{"source-single", "source-batch"} {
		var deletedAt *string
		require.NoError(t, store.DB().QueryRow(
			`SELECT deleted_at::text FROM sessions WHERE id = $1`, id,
		).Scan(&deletedAt))
		assert.NotNil(t, deletedAt)
	}
}
