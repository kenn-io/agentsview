//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushMachineMetadataWithoutSessionChanges(t *testing.T) {
	ctx := t.Context()
	pgURL := testPGURL(t)
	const schema = "agentsview_machine_labels_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	local := testDB(t)
	require.NoError(t, local.SetSyncState("machine_label:installation-a", "Laptop"))
	require.NoError(t, local.SetSyncState("machine_alias:old-owner", "installation-a"))
	syncer, err := New(pgURL, schema, local, "installation-a", true, SyncOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	require.NoError(t, syncer.EnsureSchema(ctx))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	store := &Store{pg: syncer.pg}
	labels, err := store.GetMachineLabels(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"installation-a": "Laptop"}, labels)
	aliases, err := store.GetMachineAliases(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"old-owner": "installation-a"}, aliases)

	require.NoError(t, local.SetSyncState("machine_label:installation-a", "Work laptop"))
	require.NoError(t, local.SetSyncState("machine_alias:older-owner", "installation-a"))
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, result.SessionsPushed)
	labels, err = store.GetMachineLabels(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"installation-a": "Work laptop"}, labels)
	aliases, err = store.GetMachineAliases(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"old-owner": "installation-a", "older-owner": "installation-a"}, aliases)

	other := testDB(t)
	require.NoError(t, other.SetSyncState("machine_alias:old-owner", "installation-b"))
	require.NoError(t, other.SetSyncState("machine_label:installation-b", "Desktop"))
	otherSync, err := New(pgURL, schema, other, "installation-b", true, SyncOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherSync.Close()) })
	_, err = otherSync.Push(ctx, false, nil)
	require.ErrorContains(t, err, "already belongs to another installation")
	aliases, err = store.GetMachineAliases(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"old-owner": "installation-a", "older-owner": "installation-a"}, aliases)
	labels, err = store.GetMachineLabels(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"installation-a": "Work laptop"}, labels)
}
