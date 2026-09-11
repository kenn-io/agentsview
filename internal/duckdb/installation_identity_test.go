//go:build !(windows && arm64)

package duckdb

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushInstallationAdoptionRebuildsWithoutSplittingHistory(t *testing.T) {
	const owner = "oldhost.example"
	const identity = "0123456789abcdef0123456789abcdef"
	local, path := newPushFixture(t, 1)
	require.NoError(t, local.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE sessions SET machine = ?`, owner)
		return err
	}))
	require.NoError(t, local.SetSyncState("artifact_local_machine_name", owner))
	_, err := Push(t.Context(), path, local, owner, SyncOptions{}, false, nil)
	require.NoError(t, err)
	require.NoError(t, local.EnsureInstallationIdentity(t.Context(), identity))
	result, err := Push(t.Context(), path, local, identity, SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.True(t, result.Diagnostics.Full)
	assert.Contains(t, result.Diagnostics.RebuildReason, "machine name changed")
	store, err := NewStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	session, err := store.GetSession(t.Context(), "sess-1")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, identity, session.Machine)
	aliases, err := store.GetMachineAliases(t.Context())
	require.NoError(t, err)
	assert.Equal(t, identity, aliases[owner])
	messages, err := store.GetAllMessages(t.Context(), "sess-1")
	require.NoError(t, err)
	assert.Len(t, messages, 2)
}
