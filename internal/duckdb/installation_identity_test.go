//go:build !(windows && arm64)

package duckdb

import (
	"database/sql"
	"testing"

	"go.kenn.io/agentsview/internal/storage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushInstallationAdoptionRebuildsWithoutSplittingHistory(t *testing.T) {
	const owner = "oldhost.example"
	const identity = "0123456789abcdef0123456789abcdef"
	local, path := newPushFixture(t, 1)
	require.NoError(t, local.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE sessions SET machine = ?`, owner)
		return err
	}))
	require.NoError(t, local.SetSyncState(t.Context(), "artifact_local_machine_name", owner))
	_, err := Push(t.Context(), path, local, owner, storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	_, err = local.EnsureInstallationIdentity(t.Context(), identity)
	require.NoError(t, err)
	result, err := Push(t.Context(), path, local, identity, storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	assert.True(t, result.Diagnostics.Full)
	assert.Contains(t, result.Diagnostics.RebuildReason, "machine name changed")
	store, err := NewStore(t.Context(), path)
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
