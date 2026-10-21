//go:build !(windows && arm64)

package archivesource

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/storage"
	syncer "go.kenn.io/agentsview/internal/sync"
)

func TestAttachedArchiveMirrorKeepsOriginalMachine(t *testing.T) {
	source, inputs := archiveFixture(t)
	require.NoError(t, os.RemoveAll(inputs))
	receiver := dbtest.OpenTestDB(t)
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}, ArchiveContent: config.ArchiveContentFull}
	manager, err := NewManager(t.Context(), cfg, receiver, engine, nil)
	require.NoError(t, err)
	defer manager.Close()
	require.NoError(t, drainError(manager, t.Context(), nil))
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	result, err := duckdb.Push(t.Context(), path, receiver, "receiver", storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	mirror, err := duckdb.NewStore(t.Context(), path)
	require.NoError(t, err)
	defer mirror.Close()
	for _, native := range []string{"019eb791-cf7d-75c1-8439-9ed74c122e02", "codex:019eb791-cf7d-75c1-8439-9ed74c122e02"} {
		id := sourceDevice + "~" + native
		session, err := mirror.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, sourceDevice, session.Machine)
		original, err := receiver.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		mirrored, err := mirror.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		require.Len(t, original, 1)
		require.Len(t, mirrored, 1)
		assert.Equal(t, original[0].Content, mirrored[0].Content)
		assert.Equal(t, original[0].Role, mirrored[0].Role)
	}
	labels, err := mirror.GetMachineLabels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "retired-desktop", labels[sourceDevice])
}
