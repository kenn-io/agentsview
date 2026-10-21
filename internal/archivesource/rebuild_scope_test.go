package archivesource

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/rawarchive"
	syncer "go.kenn.io/agentsview/internal/sync"
)

func TestAttachedArchiveRebuildRetainsUnselectedHistory(t *testing.T) {
	source, inputs := archiveFixture(t)
	receiver := dbtest.OpenTestDB(t)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveContent: config.ArchiveContentFull, ArchiveSources: []config.ArchiveSource{source}}
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true, Machine: "receiver"})
	defer engine.Close()
	manager, err := NewManager(t.Context(), cfg, receiver, engine, nil)
	require.NoError(t, err)
	require.NoError(t, drainError(manager, t.Context(), nil))
	manager.Close()
	const id = sourceDevice + "~codex:019eb791-cf7d-75c1-8439-9ed74c122e02"
	before, err := receiver.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, before, 1)

	// The same device can have another archive containing only supplemental files.
	capture := filepath.Join(inputs, "supplemental-capture")
	_, err = rawarchive.Capture(t.Context(), rawarchive.CaptureOptions{
		DataDir: filepath.Join(inputs, "application"), Destination: capture,
		Settings: rawarchive.RecoverySettings{LocalMachineName: "retired-desktop"},
		Roots:    []rawarchive.RootSpec{{Provider: "files", Path: filepath.Join(inputs, "other-provider")}},
	})
	require.NoError(t, err)
	directory := filepath.Join(inputs, "supplemental-archive")
	_, err = rawarchive.Seed(t.Context(), filepath.Join(capture, "capture.json"), directory, nil)
	require.NoError(t, err)
	cfg.ArchiveSources = []config.ArchiveSource{{Name: "supplemental", Directory: directory}}
	index, err := OpenIndex(t.Context(), cfg.ArchiveSources[0], filepath.Join(cfg.DataDir, "archive-sources"), nil)
	require.NoError(t, err)
	coverage, err := index.Coverage(t.Context())
	require.NoError(t, err)
	require.Zero(t, coverage.SupportedSources)
	require.Positive(t, coverage.SupplementalFiles)
	require.NoError(t, index.Close())

	options, err := RebuildContributors(t.Context(), cfg)
	require.NoError(t, err)
	stats, err := engine.ResyncAllWithOptions(t.Context(), nil, options)
	require.NoError(t, err)
	require.False(t, stats.Aborted, "%+v", stats)
	assert.Equal(t, 2, stats.OrphanedCopied)
	after, err := receiver.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, before[0].Content, after[0].Content)
}
