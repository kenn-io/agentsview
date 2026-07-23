package db

import (
	"fmt"
	"path/filepath"
	"strings"
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

func TestCopySyncStatePreservesArtifactImportQueue(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	source, err := Open(sourcePath)
	require.NoError(t, err)
	meta := ArtifactImportWork{
		Origin: "peer-a1b2c3", Kind: "meta",
		Name:   artifactImportMetadataName("a"),
		SHA256: strings.Repeat("a", 64), Size: 11,
		Reason: "future metadata", RequiredFormatVersion: 2,
	}
	checkpoint5 := ArtifactImportWork{
		Origin: "peer-a1b2c3", Kind: "checkpoints", Name: "cp-0000000005.json",
		SHA256: strings.Repeat("5", 64), Size: 55,
		Reason: "missing segment", RequiredFormatVersion: 1,
	}
	require.NoError(t, source.EnqueueArtifactImport(t.Context(), meta))
	require.NoError(t, source.EnqueueArtifactImport(t.Context(), checkpoint5))
	require.NoError(t, source.Close())

	destination := testDB(t)
	require.NoError(t, destination.EnqueueArtifactImport(t.Context(), ArtifactImportWork{
		Origin: "peer-a1b2c3", Kind: "checkpoints", Name: "cp-0000000004.json",
		SHA256: strings.Repeat("4", 64), Size: 44,
		Reason: "older checkpoint", RequiredFormatVersion: 1,
	}))
	require.NoError(t, destination.CopySyncStateFrom(sourcePath))

	pending, err := destination.PendingArtifactImports(t.Context(), 2, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	var names []string
	for _, work := range pending {
		names = append(names, work.Name)
	}
	assert.ElementsMatch(t, []string{meta.Name, checkpoint5.Name}, names)
}

func TestCopySyncStateRejectsArtifactImportIdentityConflict(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	source, err := Open(sourcePath)
	require.NoError(t, err)
	work := ArtifactImportWork{
		Origin: "peer-a1b2c3", Kind: "meta",
		Name:   artifactImportMetadataName("a"),
		SHA256: strings.Repeat("a", 64), Size: 11,
		Reason: "source", RequiredFormatVersion: 1,
	}
	require.NoError(t, source.EnqueueArtifactImport(t.Context(), work))
	require.NoError(t, source.Close())

	destination := testDB(t)
	_, err = destination.getWriter().Exec(`
		INSERT INTO artifact_import_queue(
			origin, kind, name, sha256, size, reason, required_format_version
		) VALUES (?, ?, ?, ?, ?, ?, ?)`, work.Origin, work.Kind, work.Name,
		strings.Repeat("b", 64), work.Size, "destination", 1)
	require.NoError(t, err)

	require.ErrorContains(t, destination.CopySyncStateFrom(sourcePath), "conflicting identity")
}
