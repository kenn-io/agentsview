//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreReadsDeletionCauseAndUserDeleteConvertsSourceTombstones(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err)
	defer store.Close()

	_, err = store.DB().Exec(`
		INSERT INTO sessions (
			id, machine, project, agent, message_count,
			user_message_count, deleted_at, deletion_cause
		) VALUES
			('source-single', 'machine', 'project', 'claude', 1, 1,
			 NOW(), 'source_missing'),
			('source-batch', 'machine', 'project', 'claude', 1, 1,
			 NOW(), 'source_missing')`)
	require.NoError(t, err)

	full, err := store.GetSessionFull(t.Context(), "source-single")
	require.NoError(t, err)
	require.NotNil(t, full)
	require.NotNil(t, full.DeletionCause)
	assert.Equal(t, "source_missing", *full.DeletionCause)

	require.NoError(t, store.SoftDeleteSession("source-single"))
	count, err := store.SoftDeleteSessions([]string{"source-batch"})
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	for _, id := range []string{"source-single", "source-batch"} {
		var cause *string
		require.NoError(t, store.DB().QueryRow(
			`SELECT deletion_cause FROM sessions WHERE id = $1`, id,
		).Scan(&cause))
		assert.Nil(t, cause,
			"an explicit user deletion must replace the recoverable source tombstone")
	}
}
