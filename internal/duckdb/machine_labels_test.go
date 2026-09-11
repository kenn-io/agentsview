//go:build !(windows && arm64)

package duckdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushMachineMetadataWithoutSessionChanges(t *testing.T) {
	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	require.NoError(t, local.SetSyncState("machine_label:installation-a", "Laptop"))
	require.NoError(t, local.SetSyncState("machine_alias:old-owner", "installation-a"))
	_, err := Push(ctx, path, local, "installation-a", SyncOptions{}, true, nil)
	require.NoError(t, err)

	store, err := NewStore(path)
	require.NoError(t, err)
	labels, err := store.GetMachineLabels(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"installation-a": "Laptop"}, labels)
	aliases, err := store.GetMachineAliases(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"old-owner": "installation-a"}, aliases)
	require.NoError(t, store.Close())

	require.NoError(t, local.SetSyncState("machine_label:installation-a", "Work laptop"))
	require.NoError(t, local.SetSyncState("machine_alias:older-owner", "installation-a"))
	result, err := Push(ctx, path, local, "installation-a", SyncOptions{}, false, nil)
	require.NoError(t, err)
	assert.Zero(t, result.SessionsPushed)

	store, err = NewStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	labels, err = store.GetMachineLabels(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"installation-a": "Work laptop"}, labels)
	aliases, err = store.GetMachineAliases(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"old-owner": "installation-a", "older-owner": "installation-a"}, aliases)
}
