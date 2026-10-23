package db

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmptyReadOnlyArchiveIsDisposable(t *testing.T) {
	database, err := OpenEmptyReadOnly(t.Context())
	require.NoError(t, err)
	path := database.Path()
	require.True(t, database.ReadOnly())
	stats, err := database.GetStats(t.Context(), false, false)
	require.NoError(t, err)
	require.Zero(t, stats.SessionCount)
	require.NoError(t, database.Close())
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))
}
