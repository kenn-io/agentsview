//go:build !(windows && arm64)

package duckdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushMachineMetadataWithoutSessionChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	local, path := newPushFixture(t, 1)
	require.NoError(local.SetSyncState("machine_label:installation-a", "Laptop"))
	require.NoError(local.SetSyncState("machine_alias:old-owner", "installation-a"))
	_, err := Push(ctx, path, local, "installation-a", SyncOptions{}, true, nil)
	require.NoError(err)

	store, err := NewStore(path)
	require.NoError(err)
	labels, err := store.GetMachineLabels(ctx)
	require.NoError(err)
	assert.Equal(map[string]string{"installation-a": "Laptop"}, labels)
	aliases, err := store.GetMachineAliases(ctx)
	require.NoError(err)
	assert.Equal(map[string]string{"old-owner": "installation-a"}, aliases)
	require.NoError(store.Close())

	require.NoError(local.SetSyncState("machine_label:installation-a", "Work laptop"))
	require.NoError(local.SetSyncState("machine_alias:older-owner", "installation-a"))
	result, err := Push(ctx, path, local, "installation-a", SyncOptions{}, false, nil)
	require.NoError(err)
	assert.Zero(result.SessionsPushed)

	store, err = NewStore(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })
	labels, err = store.GetMachineLabels(ctx)
	require.NoError(err)
	assert.Equal(map[string]string{"installation-a": "Work laptop"}, labels)
	aliases, err = store.GetMachineAliases(ctx)
	require.NoError(err)
	assert.Equal(map[string]string{"old-owner": "installation-a", "older-owner": "installation-a"}, aliases)
}
