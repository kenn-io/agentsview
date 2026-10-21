package archivesource

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/kit/backup"
)

func TestAttachedArchiveRebuild(t *testing.T) {
	for _, mode := range []string{"available", "offline", "detached"} {
		t.Run(mode, func(t *testing.T) {
			source, inputs := archiveFixture(t)
			require.NoError(t, os.RemoveAll(inputs))
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
			_, err = receiver.PinMessage(t.Context(), id, before[0].ID, new("keep note"))
			require.NoError(t, err)
			require.NoError(t, receiver.RenameSession(t.Context(), id, new("keep title")))
			if mode == "offline" {
				require.NoError(t, os.Rename(source.Repository, source.Repository+"-offline"))
			}
			if mode == "detached" {
				cfg.ArchiveSources = nil
			}
			manager, err = NewManager(t.Context(), cfg, receiver, engine, nil)
			require.NoError(t, err)
			defer manager.Close()
			options, err := RebuildContributors(t.Context(), cfg)
			require.NoError(t, err)
			stats, err := engine.ResyncAllWithOptions(t.Context(), nil, options)
			require.NoError(t, err)
			require.False(t, stats.Aborted, "%+v", stats)
			if mode == "available" {
				assert.Equal(t, 2, stats.Synced)
			} else {
				assert.Equal(t, 2, stats.OrphanedCopied)
			}
			after, err := receiver.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, after, 1)
			assert.Equal(t, before[0].Content, after[0].Content)
			session, err := receiver.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, sourceDevice, session.Machine)
			assert.Equal(t, new("keep title"), session.DisplayName)
			pins, err := receiver.ListPinnedMessages(t.Context(), id, "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, new("keep note"), pins[0].Note)
			// Returning sources must still have ownership and a current completion record.
			if mode == "offline" {
				require.NoError(t, os.Rename(source.Repository+"-offline", source.Repository))
			}
			cfg.ArchiveSources = []config.ArchiveSource{source}
			retries := 0
			retry, err := NewManager(t.Context(), cfg, receiver, engine, func(context.Context) error { retries++; return nil })
			require.NoError(t, err)
			defer retry.Close()
			require.NoError(t, drainError(retry, t.Context(), nil))
			assert.Zero(t, retries, "a rebuild must preserve completed source targets")
		})
	}
}

func TestAttachedArchiveRebuildWaitsForBackup(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "cancel"}[cancelWait], func(t *testing.T) {
			source, _ := archiveFixture(t)
			receiver := dbtest.OpenTestDB(t)
			cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}}
			engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true, Machine: "receiver"})
			defer engine.Close()
			manager, err := NewManager(t.Context(), cfg, receiver, engine, nil)
			require.NoError(t, err)
			defer manager.Close()
			require.NoError(t, drainError(manager, t.Context(), nil))
			const id = sourceDevice + "~codex:019eb791-cf7d-75c1-8439-9ed74c122e02"
			before, err := receiver.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			options, err := RebuildContributors(t.Context(), cfg)
			require.NoError(t, err)
			repo, err := backup.Open(source.Repository)
			require.NoError(t, err)
			var lock *backup.RepoLock
			defer func() {
				if lock != nil {
					require.NoError(t, lock.Release())
				}
			}()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			copied, waited := 0, false
			stats, err := engine.ResyncAllWithOptions(ctx, func(p syncer.Progress) {
				if p.Detail == "Reading archived session" {
					copied++
					if copied == 1 {
						lock, err = repo.AcquireExclusiveLock("backup", false)
						require.NoError(t, err)
					}
				}
				if strings.HasPrefix(p.Detail, "Waiting for archive repository") {
					waited = true
					assert.Equal(t, 1, copied, "the completed first source must not restart")
					if cancelWait {
						cancel()
					} else {
						require.NoError(t, lock.Release())
						lock = nil
					}
				}
			}, options)
			assert.True(t, waited)
			if cancelWait {
				require.ErrorIs(t, err, context.Canceled)
				assert.True(t, stats.Aborted)
				after, readErr := receiver.GetAllMessages(t.Context(), id)
				require.NoError(t, readErr)
				assert.Equal(t, before, after)
			} else {
				require.NoError(t, err)
				assert.False(t, stats.Aborted)
				assert.Equal(t, 2, copied)
			}
		})
	}
}

func TestAttachedArchivePendingSurvivesGlobalUpgradeMarker(t *testing.T) {
	source, _ := archiveFixture(t)
	receiver := dbtest.OpenTestDB(t)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}}
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true, Machine: "receiver"})
	defer engine.Close()
	copies := 0
	manager, err := NewManager(t.Context(), cfg, receiver, engine, func(context.Context) error { copies++; return nil })
	require.NoError(t, err)
	defer manager.Close()
	require.NoError(t, drainError(manager, t.Context(), nil))
	require.Equal(t, 2, copies)
	index, err := OpenIndex(t.Context(), source, filepath.Join(cfg.DataDir, "archive-sources"), nil)
	require.NoError(t, err)
	sources, err := index.Sources(t.Context(), "", 10)
	require.NoError(t, err)
	require.NoError(t, index.Close())
	for _, item := range sources {
		if item.Root.Provider != "claude" {
			continue
		}
		require.NoError(t, receiver.Update(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `UPDATE archive_attachments SET target='previous parser';
    UPDATE archive_attachment_sources SET target='previous parser',completed_target='previous parser' WHERE root_id=?`, item.RootID)
			return err
		}))
	}
	receiver.MarkDataCurrent()
	assert.False(t, receiver.NeedsResync())
	require.NoError(t, drainError(manager, t.Context(), nil))
	assert.Equal(t, 3, copies, "only the outdated provider source needs another parse")
}

func TestAttachedArchiveFailedRebuildKeepsPublishedHistory(t *testing.T) {
	source, _ := archiveFixture(t)
	receiver := dbtest.OpenTestDB(t)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}}
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true, Machine: "receiver"})
	defer engine.Close()
	manager, err := NewManager(t.Context(), cfg, receiver, engine, nil)
	require.NoError(t, err)
	defer manager.Close()
	require.NoError(t, drainError(manager, t.Context(), nil))
	const id = sourceDevice + "~codex:019eb791-cf7d-75c1-8439-9ed74c122e02"
	before, err := receiver.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	options, err := RebuildContributors(t.Context(), cfg)
	require.NoError(t, err)
	removed := false
	stats, err := engine.ResyncAllWithOptions(t.Context(), func(p syncer.Progress) {
		if p.Detail == "Reading archived session" && !removed {
			removed = true
			assert.NoError(t, os.Rename(source.Repository, source.Repository+"-offline"))
		}
	}, options)
	require.Error(t, err)
	assert.True(t, stats.Aborted)
	assert.True(t, removed)
	after, err := receiver.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.NoFileExists(t, receiver.Path()+"-resync")
}
