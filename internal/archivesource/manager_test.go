package archivesource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawarchive"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
	"go.kenn.io/docbank/docbank"
	"go.kenn.io/kit/backup"
)

func TestArchiveDrainSingleFlight(t *testing.T) {
	source, inputs := archiveFixture(t)
	require.NoError(t, os.RemoveAll(inputs))
	receiver := dbtest.OpenTestDB(t)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}, ArchiveContent: config.ArchiveContentFull}
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	boundary := make(chan struct{})
	release := make(chan struct{})
	var copies atomic.Int32
	manager, err := NewManager(t.Context(), cfg, receiver, engine, func(ctx context.Context) error {
		if copies.Add(1) == 1 {
			close(boundary)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	require.NoError(t, err)
	defer manager.Close()
	type result struct {
		stats syncer.SyncStats
		err   error
	}
	finished := make(chan result, 1)
	go func() {
		stats, err := manager.Drain(t.Context(), nil)
		finished <- result{stats, err}
	}()
	<-boundary
	joined := &drainWaitContext{Context: t.Context(), waiting: make(chan struct{})}
	joinedResult := make(chan result, 1)
	go func() {
		stats, err := manager.Drain(joined, nil)
		joinedResult <- result{stats, err}
	}()
	<-joined.waiting
	cancelled, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelledWaiter := &drainWaitContext{Context: cancelled, waiting: make(chan struct{})}
	cancelledResult := make(chan error, 1)
	go func() { cancelledResult <- drainError(manager, cancelledWaiter, nil) }()
	<-cancelledWaiter.waiting
	cancel()
	require.ErrorIs(t, <-cancelledResult, context.Canceled)
	close(release)
	first, second := <-finished, <-joinedResult
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	assert.Equal(t, 2, first.stats.Synced)
	assert.Equal(t, first.stats, second.stats, "live callers share the same completed drain")
	assert.Equal(t, int32(2), copies.Load())
	labels, err := receiver.GetMachineLabels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "retired-desktop", labels[sourceDevice])
	// A current pass must not copy or parse any source again.
	cacheFiles, err := filepath.Glob(filepath.Join(cfg.DataDir, "archive-sources", "*.db"))
	require.NoError(t, err)
	require.Len(t, cacheFiles, 1)
	index, err := OpenIndex(t.Context(), source, filepath.Dir(cacheFiles[0]), nil)
	require.NoError(t, err)
	key := db.ArchiveAttachmentKey{RepositoryID: index.Info.RepositoryID, SnapshotID: index.Info.SnapshotID}
	require.NoError(t, index.Close())
	require.NoError(t, drainError(manager, t.Context(), nil))
	assert.Equal(t, int32(2), copies.Load(), "unchanged pass performs no source copies")
	status, err := receiver.ArchiveAttachmentStatus(t.Context(), key)
	require.NoError(t, err)
	assert.Zero(t, status.Pending)
	offline := source.Repository + "-offline"
	require.NoError(t, os.Rename(source.Repository, offline))
	require.Error(t, drainError(manager, t.Context(), nil))
	session, err := receiver.GetSessionFull(t.Context(), sourceDevice+"~codex:019eb791-cf7d-75c1-8439-9ed74c122e02")
	require.NoError(t, err)
	require.NotNil(t, session)
}

// Done is observed only after Drain has selected an active run to wait for.
type drainWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *drainWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestArchiveDrainCancellationDetachesProgress(t *testing.T) {
	source, _ := archiveFixture(t)
	receiver := dbtest.OpenTestDB(t)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}}
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	secondSource, releaseSource := make(chan struct{}), make(chan struct{})
	var copies atomic.Int32
	manager, err := NewManager(t.Context(), cfg, receiver, engine, func(ctx context.Context) error {
		if copies.Add(1) == 2 {
			close(secondSource)
			select {
			case <-releaseSource:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	require.NoError(t, err)
	defer manager.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	callbackStarted, releaseCallback := make(chan struct{}), make(chan struct{})
	var callbacks atomic.Int32
	var reading atomic.Int32
	finished := make(chan error, 1)
	go func() {
		finished <- drainError(manager, ctx, func(p syncer.Progress) {
			callbacks.Add(1)
			if p.Detail == "Reading archived session" && reading.Add(1) == 1 {
				close(callbackStarted)
				<-releaseCallback
			}
		})
	}()
	<-callbackStarted
	joined := &drainWaitContext{Context: t.Context(), waiting: make(chan struct{})}
	joinedResult := make(chan error, 1)
	go func() { joinedResult <- drainError(manager, joined, nil) }()
	<-joined.waiting
	cancel()
	var returned bool
	select {
	case <-finished:
		returned = true
		assert.Fail(t, "Drain returned while its progress callback was still running")
	case <-time.After(time.Second):
	}
	close(releaseCallback)
	if !returned {
		require.ErrorIs(t, <-finished, context.Canceled)
	}
	<-secondSource
	before := callbacks.Load()
	close(releaseSource)
	require.NoError(t, <-joinedResult)
	assert.Equal(t, before, callbacks.Load(), "a cancelled caller receives no progress after Drain returns")
	assert.Equal(t, int32(2), copies.Load(), "caller cancellation leaves shared work running")
	session, err := receiver.GetSessionFull(t.Context(), sourceDevice+"~codex:019eb791-cf7d-75c1-8439-9ed74c122e02")
	require.NoError(t, err)
	require.NotNil(t, session)
}

func TestArchiveDrainRecordsFailuresAndContinues(t *testing.T) {
	source, _ := archiveFixture(t)
	receiver := dbtest.OpenTestDB(t)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}}
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	localFailed := errors.New("local write failed")
	var handoffs atomic.Int32
	manager, err := NewManager(t.Context(), cfg, receiver, engine, func(context.Context) error {
		if handoffs.Add(1) == 1 {
			return localFailed
		}
		return nil
	})
	require.NoError(t, err)
	defer manager.Close()
	require.ErrorIs(t, drainError(manager, t.Context(), nil), localFailed)
	// Failed local reconciliation defers the archive rather than taking priority.
	require.NoError(t, drainError(manager, t.Context(), nil))
	assert.Equal(t, int32(3), handoffs.Load())
}

func TestArchiveDrainRefusesArchiveOnlyReceiver(t *testing.T) {
	receiver := dbtest.OpenTestDB(t)
	require.NoError(t, receiver.EnableArchiveOnly(t.Context()))
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	_, err := NewManager(t.Context(), config.Config{ArchiveSources: []config.ArchiveSource{{Name: "saved", Repository: "/missing", Snapshot: "pinned"}}}, receiver, engine, nil)
	require.ErrorIs(t, err, db.ErrArchiveOnly)
}

func TestArchiveDrainYieldsToPendingLocalWork(t *testing.T) {
	inputs := t.TempDir()
	root := filepath.Join(inputs, "claude")
	for _, id := range []string{"first", "second", "third"} {
		dbtest.WriteTestFile(t, filepath.Join(root, "project", id+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", id, id).String()))
	}
	source := backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "claude", Path: root}})
	require.NoError(t, os.RemoveAll(inputs))
	receiver := dbtest.OpenTestDB(t)
	local := filepath.Join(t.TempDir(), "project", "local.jsonl")
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}, ArchiveContent: config.ArchiveContentFull}
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true, Machine: "receiver", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {filepath.Dir(filepath.Dir(local))}}})
	defer engine.Close()
	dbtest.WriteTestFile(t, local, []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "new local message", "local").String()))
	var boundaries int
	manager, err := NewManager(t.Context(), cfg, receiver, engine, func(ctx context.Context) error {
		count := 0
		for _, id := range []string{sourceDevice + "~first", sourceDevice + "~second", sourceDevice + "~third", "local"} {
			messages, err := receiver.GetAllMessages(ctx, id)
			if err != nil {
				return err
			}
			count += len(messages)
		}
		if boundaries < 2 {
			assert.Equal(t, boundaries, count)
		} else {
			assert.Equal(t, boundaries+1, count)
		}
		if boundaries == 1 {
			if err := engine.ReparsePathsContext(ctx, []string{local}); err != nil {
				return err
			}
			session, err := receiver.GetSessionFull(ctx, "local")
			if err != nil {
				return err
			}
			if session == nil {
				return errors.New("local handoff did not publish")
			}
		}
		boundaries++
		return nil
	})
	require.NoError(t, err)
	defer manager.Close()
	require.NoError(t, drainError(manager, t.Context(), nil))
	assert.Equal(t, 3, boundaries)
}

func TestArchiveDrainDefersWhileBackupRuns(t *testing.T) {
	inputs := t.TempDir()
	root := filepath.Join(inputs, "claude")
	for _, id := range []string{"first", "second", "third"} {
		dbtest.WriteTestFile(t, filepath.Join(root, "project", id+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", id, id).String()))
	}
	source := backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "claude", Path: root}})
	repo, err := backup.Open(source.Repository)
	require.NoError(t, err)
	receiver := dbtest.OpenTestDB(t)
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	var lock *backup.RepoLock
	defer func() {
		if lock != nil {
			require.NoError(t, lock.Release())
		}
	}()
	handoffs := 0
	manager, err := NewManager(t.Context(), config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}}, receiver, engine, func(context.Context) error {
		handoffs++
		if handoffs == 2 {
			var err error
			lock, err = repo.AcquireExclusiveLock("backup", false)
			return err
		}
		return nil
	})
	require.NoError(t, err)
	defer manager.Close()
	require.ErrorIs(t, drainError(manager, t.Context(), nil), docbank.ErrBackupRepositoryLocked)
	assert.Equal(t, 2, handoffs, "defer the remaining sources while a backup owns the repository")
}

// Tests that inspect persisted output do not need the command's aggregate counters.
func drainError(manager *Manager, ctx context.Context, progress syncer.ProgressFunc) error {
	_, err := manager.Drain(ctx, progress)
	return err
}

func TestArchiveDrainDoesNotRepeatRebuiltSources(t *testing.T) {
	source, inputs := archiveFixture(t)
	require.NoError(t, os.RemoveAll(inputs))
	receiver := dbtest.OpenTestDB(t)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}}
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true, Machine: "receiver", RebuildSources: func(ctx context.Context) (syncer.RebuildOptions, error) { return RebuildContributors(ctx, cfg) }})
	defer engine.Close()
	boundary, release := make(chan struct{}), make(chan struct{})
	var handoffs atomic.Int32
	manager, err := NewManager(t.Context(), cfg, receiver, engine, func(ctx context.Context) error {
		if handoffs.Add(1) == 1 {
			close(boundary)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	require.NoError(t, err)
	defer manager.Close()
	done := make(chan error, 1)
	var drainCopies atomic.Int32
	go func() {
		done <- drainError(manager, t.Context(), func(p syncer.Progress) {
			if p.Detail == "Reading archived session" {
				drainCopies.Add(1)
			}
		})
	}()
	<-boundary
	rebuilt, err := engine.ResyncAllWithOptions(t.Context(), nil, syncer.RebuildOptions{})
	before, readErr := receiver.GetAllMessages(t.Context(), sourceDevice+"~codex:019eb791-cf7d-75c1-8439-9ed74c122e02")
	moveErr := os.Rename(source.Repository, source.Repository+"-offline")
	close(release)
	require.NoError(t, moveErr)
	require.NoError(t, readErr)
	require.NoError(t, err)
	require.False(t, rebuilt.Aborted)
	require.Equal(t, 2, rebuilt.Synced)
	require.NoError(t, <-done)
	after, readErr := receiver.GetAllMessages(t.Context(), sourceDevice+"~codex:019eb791-cf7d-75c1-8439-9ed74c122e02")
	require.NoError(t, readErr)
	assert.Equal(t, before, after, "redundant reparse preserved transcript and message identities")
	var pending int
	require.NoError(t, receiver.Reader().QueryRow(t.Context(), `SELECT sum(pending_count) FROM archive_attachments`).Scan(&pending))
	assert.Zero(t, pending)
	assert.Zero(t, drainCopies.Load(), "the successful rebuild completed every pending source")
}
