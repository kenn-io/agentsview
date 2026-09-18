package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMachineLabelsPersistUpdatesAndSurviveResync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	source := testDB(t)
	require.NoError(source.SetSyncState("last_push_at", "unrelated metadata"))
	require.NoError(source.SetSyncState("machineXlabel:other", "not a label"))
	require.NoError(source.SetSyncState("machine_label:installation-a", "Laptop"))
	require.NoError(source.SetSyncState("machine_label:installation-b", "Desktop"))
	require.NoError(source.SetSyncState("machine_label:installation-a", "Work laptop"))

	require.NoError(source.SetSyncState("machine_alias:oldhost.example", "installation-a"))
	labels, err := source.GetMachineLabels(ctx)
	require.NoError(err)
	want := map[string]string{
		"installation-a": "Work laptop",
		"installation-b": "Desktop",
	}
	assert.Equal(want, labels)

	replacement := testDB(t)
	require.NoError(replacement.CopySyncStateFrom(source.Path()))
	labels, err = replacement.GetMachineLabels(ctx)
	require.NoError(err)
	assert.Equal(want, labels)
	aliases, err := replacement.GetMachineAliases(ctx)
	require.NoError(err)
	assert.Equal(map[string]string{"oldhost.example": "installation-a"}, aliases)
}
