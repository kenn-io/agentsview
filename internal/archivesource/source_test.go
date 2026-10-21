package archivesource

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	syncer "go.kenn.io/agentsview/internal/sync"
)

func TestAttachedSourceMatchesProviderHistory(t *testing.T) {
	source, inputs := archiveFixture(t)
	require.NoError(t, os.RemoveAll(inputs))
	index, err := OpenIndex(t.Context(), source, t.TempDir(), nil)
	require.NoError(t, err)
	defer index.Close()
	receiver := dbtest.OpenTestDB(t)
	_, err = receiver.EnsureInstallationIdentity(t.Context(), "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveContent: config.ArchiveContentFull, ArchiveSources: []config.ArchiveSource{source}}
	sources, err := index.Sources(t.Context(), "", 10)
	require.NoError(t, err)
	key := db.ArchiveAttachmentKey{RepositoryID: index.Info.RepositoryID, SnapshotID: index.Info.SnapshotID}
	require.NoError(t, receiver.RegisterArchiveAttachment(t.Context(), key, index.ArchiveID))
	targets := make([]db.ArchiveSourceTarget, 0, len(sources))
	for _, item := range sources {
		target, err := completionTarget(item, cfg)
		require.NoError(t, err)
		targets = append(targets, db.ArchiveSourceTarget{Provenance: index.provenance(item), Target: target})
	}
	require.NoError(t, receiver.SetArchiveAttachmentTargets(t.Context(), key, "current", targets))
	scratch := t.TempDir()
	for n, item := range sources {
		_, err := index.syncSource(t.Context(), source.Repository, item, cfg, engine, scratch, targets[n].Target, nil)
		require.NoError(t, err)
		native := "019eb791-cf7d-75c1-8439-9ed74c122e02"
		if item.Root.Provider == "codex" {
			native = "codex:" + native
		}
		session, err := receiver.GetSessionFull(t.Context(), sourceDevice+"~"+native)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, sourceDevice, session.Machine)
		require.NotNil(t, session.FilePath)
		assert.Contains(t, *session.FilePath, "archive://")
		messages, err := receiver.GetAllMessages(t.Context(), session.ID)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		want := "saved Claude question"
		if item.Root.Provider == "codex" {
			want = "saved Codex question"
		}
		assert.Equal(t, want, messages[0].Content)
		_, err = receiver.StarSession(t.Context(), session.ID)
		require.NoError(t, err)
		_, err = receiver.PinMessage(t.Context(), session.ID, messages[0].ID, new("kept note"))
		require.NoError(t, err)
		require.NoError(t, receiver.RenameSession(t.Context(), session.ID, new("kept title")))
		targets[n].Target += "-reparse"
		require.NoError(t, receiver.SetArchiveAttachmentTargets(t.Context(), key, "current", targets))
		_, err = index.syncSource(t.Context(), source.Repository, item, cfg, engine, scratch, targets[n].Target, nil)
		require.NoError(t, err)
		retried, err := receiver.GetAllMessages(t.Context(), session.ID)
		require.NoError(t, err)
		assert.Equal(t, messages, retried)
		renamed, err := receiver.GetSessionFull(t.Context(), session.ID)
		require.NoError(t, err)
		require.NotNil(t, renamed.DisplayName)
		assert.Equal(t, "kept title", *renamed.DisplayName)
		pins, err := receiver.ListPinnedMessages(t.Context(), session.ID, "")
		require.NoError(t, err)
		require.Len(t, pins, 1)
		assert.Equal(t, new("kept note"), pins[0].Note)
		stars, err := receiver.ListStarredSessionIDs(t.Context())
		require.NoError(t, err)
		assert.Contains(t, stars, session.ID)
		require.NoError(t, receiver.SoftDeleteSession(t.Context(), session.ID))
		targets[n].Target += "-reparse"
		require.NoError(t, receiver.SetArchiveAttachmentTargets(t.Context(), key, "current", targets))
		_, err = index.syncSource(t.Context(), source.Repository, item, cfg, engine, scratch, targets[n].Target, nil)
		require.NoError(t, err)
		trashed, err := receiver.GetSessionFull(t.Context(), session.ID)
		require.NoError(t, err)
		require.NotNil(t, trashed)
		assert.NotNil(t, trashed.DeletedAt)
		require.NoError(t, receiver.DeleteSession(t.Context(), session.ID))
		targets[n].Target += "-reparse"
		require.NoError(t, receiver.SetArchiveAttachmentTargets(t.Context(), key, "current", targets))
		_, err = index.syncSource(t.Context(), source.Repository, item, cfg, engine, scratch, targets[n].Target, nil)
		require.NoError(t, err)
		deleted, err := receiver.GetSessionFull(t.Context(), session.ID)
		require.NoError(t, err)
		assert.Nil(t, deleted)
	}
	status, err := receiver.ArchiveAttachmentStatus(t.Context(), key)
	require.NoError(t, err)
	assert.Zero(t, status.Pending)
	entries, err := os.ReadDir(scratch)
	require.NoError(t, err)
	assert.Empty(t, entries)
	targets[0].Target = "parser-upgrade"
	require.NoError(t, receiver.SetArchiveAttachmentTargets(t.Context(), key, "upgrade", targets))
	moved := filepath.Join(t.TempDir(), "offline-repository")
	require.NoError(t, os.Rename(source.Repository, moved))
	_, err = index.syncSource(t.Context(), source.Repository, sources[0], cfg, engine, scratch, targets[0].Target, nil)
	require.Error(t, err)
	status, err = receiver.ArchiveAttachmentStatus(t.Context(), key)
	require.NoError(t, err)
	assert.Equal(t, int64(1), status.Pending)
}

func TestAttachedSourceHonorsCapturedDeletion(t *testing.T) {
	const native = "codex:019eb791-cf7d-75c1-8439-9ed74c122e02"
	source, inputs := archiveFixture(t, native)
	require.NoError(t, os.RemoveAll(inputs))
	index, err := OpenIndex(t.Context(), source, t.TempDir(), nil)
	require.NoError(t, err)
	defer index.Close()
	receiver := dbtest.OpenTestDB(t)
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	sources, err := index.Sources(t.Context(), "", 10)
	require.NoError(t, err)
	for _, item := range sources {
		if item.Root.Provider != "codex" {
			continue
		}
		p := index.provenance(item)
		require.NoError(t, receiver.RegisterArchiveAttachment(t.Context(), p.Attachment, index.ArchiveID))
		require.NoError(t, receiver.SetArchiveAttachmentTargets(t.Context(), p.Attachment, "target", []db.ArchiveSourceTarget{{Provenance: p, Target: "target"}}))
		cfg := config.Config{DataDir: filepath.Dir(receiver.Path())}
		_, err := index.syncSource(t.Context(), source.Repository, item, cfg, engine, t.TempDir(), "target", nil)
		require.NoError(t, err)
		session, err := receiver.GetSessionFull(t.Context(), sourceDevice+"~"+native)
		require.NoError(t, err)
		assert.Nil(t, session)
		status, err := receiver.ArchiveAttachmentStatus(t.Context(), p.Attachment)
		require.NoError(t, err)
		assert.Zero(t, status.Pending)
		return
	}
	require.FailNow(t, "fixture has no Codex source")
}
