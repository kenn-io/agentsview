package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMachineLabelsPersistUpdatesAndSurviveResync(t *testing.T) {
	ctx := t.Context()
	source := testDB(t)
	require.NoError(t, source.SetSyncState("last_push_at", "unrelated metadata"))
	require.NoError(t, source.SetSyncState("machineXlabel:other", "not a label"))
	require.NoError(t, source.SetSyncState("machine_label:installation-a", "Laptop"))
	require.NoError(t, source.SetSyncState("machine_label:installation-b", "Desktop"))
	require.NoError(t, source.SetSyncState("machine_label:installation-a", "Work laptop"))

	require.NoError(t, source.SetSyncState("machine_alias:oldhost.example", "installation-a"))
	labels, err := source.GetMachineLabels(ctx)
	require.NoError(t, err)
	want := map[string]string{
		"installation-a": "Work laptop",
		"installation-b": "Desktop",
	}
	assert.Equal(t, want, labels)

	replacement := testDB(t)
	require.NoError(t, replacement.CopySyncStateFrom(source.Path()))
	labels, err = replacement.GetMachineLabels(ctx)
	require.NoError(t, err)
	assert.Equal(t, want, labels)
	aliases, err := replacement.GetMachineAliases(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"oldhost.example": "installation-a"}, aliases)
}
