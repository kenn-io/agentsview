package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncStateValuesReadsOnlyExactKeysAcrossBatches(t *testing.T) {
	database := testDB(t)
	for i := range 905 {
		key := fmt.Sprintf("artifact_import:desk:desk~%04d", i)
		require.NoError(t, database.SetSyncState(key, fmt.Sprintf("hash-%04d", i)))
	}
	require.NoError(t, database.SetSyncState("unrelated", "keep-out"))

	got, err := database.SyncStateValues([]string{
		"artifact_import:desk:desk~0000",
		"artifact_import:desk:desk~0899",
		"artifact_import:desk:desk~0904",
		"missing",
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"artifact_import:desk:desk~0000": "hash-0000",
		"artifact_import:desk:desk~0899": "hash-0899",
		"artifact_import:desk:desk~0904": "hash-0904",
	}, got)
}
