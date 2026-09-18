//go:build !(windows && arm64)

package duckdb

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushInstallationAdoptionRebuildsWithoutSplittingHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const owner = "oldhost.example"
	const identity = "0123456789abcdef0123456789abcdef"
	local, path := newPushFixture(t, 1)
	require.NoError(local.Update(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE sessions SET machine = ?`, owner)
		return err
	}))
	require.NoError(local.SetSyncState("artifact_local_machine_name", owner))
	_, err := Push(t.Context(), path, local, owner, SyncOptions{}, false, nil)
	require.NoError(err)
	_, err = local.EnsureInstallationIdentity(t.Context(), identity)
	require.NoError(err)
	result, err := Push(t.Context(), path, local, identity, SyncOptions{}, false, nil)
	require.NoError(err)
	assert.True(result.Diagnostics.Full)
	assert.Contains(result.Diagnostics.RebuildReason, "machine name changed")
	store, err := NewStore(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })
	session, err := store.GetSession(t.Context(), "sess-1")
	require.NoError(err)
	require.NotNil(session)
	assert.Equal(identity, session.Machine)
	aliases, err := store.GetMachineAliases(t.Context())
	require.NoError(err)
	assert.Equal(identity, aliases[owner])
	messages, err := store.GetAllMessages(t.Context(), "sess-1")
	require.NoError(err)
	assert.Len(messages, 2)
}
