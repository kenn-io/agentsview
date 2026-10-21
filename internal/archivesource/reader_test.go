package archivesource

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/docbank/docbank"
)

func TestArchiveSourceReaderChecksRepositoryIdentity(t *testing.T) {
	source, inputs := archiveFixture(t)
	require.NoError(t, os.RemoveAll(inputs))
	index, err := OpenIndex(t.Context(), source, t.TempDir(), nil)
	require.NoError(t, err)
	defer index.Close()
	moved := filepath.Join(t.TempDir(), "moved")
	require.NoError(t, os.Rename(source.Repository, moved))
	_, err = docbank.InitBackupRepository(source.Repository)
	require.NoError(t, err)
	reader, err := index.OpenReader(t.Context(), source.Repository)
	require.ErrorIs(t, err, db.ErrArchiveSourceConflict)
	require.Nil(t, reader)
	reader, err = index.OpenReader(t.Context(), moved)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
}
