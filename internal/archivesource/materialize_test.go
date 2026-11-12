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

func TestMaterializeAttachedSnapshotOwnership(t *testing.T) {
	ctx := t.Context()
	source, inputs := archiveFixture(t)
	require.NoError(t, os.RemoveAll(inputs))
	index, err := OpenIndex(ctx, source, t.TempDir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	receiver := dbtest.OpenTestDB(t)
	_, err = receiver.EnsureInstallationIdentity(ctx, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	engine := syncer.NewEngine(ctx, receiver, syncer.EngineConfig{Ephemeral: true})
	t.Cleanup(func() { engine.Close() })
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveContent: config.ArchiveContentFull, ArchiveSources: []config.ArchiveSource{source}}
	sources, err := index.Sources(ctx, "", 10)
	require.NoError(t, err)
	key := db.ArchiveAttachmentKey{RepositoryID: index.Info.RepositoryID, SnapshotID: index.Info.SnapshotID}
	require.NoError(t, receiver.RegisterArchiveAttachment(ctx, key, index.ArchiveID))
	var claude Source
	var claudeTarget string
	var targets []db.ArchiveSourceTarget
	for _, item := range sources {
		target, err := completionTarget(item, cfg)
		require.NoError(t, err)
		targets = append(targets, db.ArchiveSourceTarget{Provenance: index.provenance(item), Target: target})
		if item.Root.Provider == "claude" {
			claude = item
			claudeTarget = target
		}
	}
	require.NotEmpty(t, claude.ManifestID)
	require.NoError(t, receiver.SetArchiveAttachmentTargets(ctx, key, "current", targets))
	_, err = index.syncSource(ctx, source.Repository, claude, cfg, engine, t.TempDir(), claudeTarget, nil)
	require.NoError(t, err)
	id := sourceDevice + "~019eb791-cf7d-75c1-8439-9ed74c122e02"
	owner, err := receiver.ArchiveSessionOwners(ctx, id)
	require.NoError(t, err)
	require.Len(t, owner, 1)
	tree, manifest, _, err := MaterializeSnapshot(ctx, cfg.ArchiveSources, owner, "claude", 512<<20)
	require.NoError(t, err)
	root := tree.Root()
	var transcript []byte
	for _, entry := range manifest.Manifest.Entries {
		if filepath.Base(entry.Path) == "019eb791-cf7d-75c1-8439-9ed74c122e02.jsonl" {
			name, err := tree.EntryPath(entry.Path)
			require.NoError(t, err)
			transcript, err = os.ReadFile(name)
			require.NoError(t, err)
		}
	}
	assert.Contains(t, string(transcript), "saved Claude question")
	require.NoError(t, tree.Cleanup())
	_, err = os.Stat(root)
	assert.True(t, os.IsNotExist(err))
	// Duplicate configuration and an unavailable equivalent membership do not
	// make the configured, verified snapshot ambiguous.
	other := owner[0]
	other.Attachment.SnapshotID = "unconfigured"
	duplicate := []config.ArchiveSource{source, source}
	tree, _, chosen, err := MaterializeSnapshot(ctx, duplicate, []db.ArchiveSessionProvenance{other, owner[0]}, "claude", 512<<20)
	require.NoError(t, err)
	assert.Equal(t, owner[0].Attachment, chosen)
	require.NoError(t, tree.Cleanup())
	other.Source.DeviceID = "different"
	_, _, _, err = MaterializeSnapshot(ctx, duplicate, []db.ArchiveSessionProvenance{owner[0], other}, "claude", 512<<20)
	require.ErrorContains(t, err, "conflicting snapshot source identities")
	for _, tc := range []struct {
		name   string
		change func(*db.ArchiveSessionProvenance)
	}{
		{"archive", func(p *db.ArchiveSessionProvenance) { p.Source.ArchiveID = "wrong" }},
		{"root", func(p *db.ArchiveSessionProvenance) { p.Source.RootID = "wrong" }},
		{"manifest", func(p *db.ArchiveSessionProvenance) { p.ManifestID = "wrong" }},
		{"session", func(p *db.ArchiveSessionProvenance) { p.NativeSessionID = "wrong" }},
		{"repository", func(p *db.ArchiveSessionProvenance) { p.Attachment.RepositoryID = "wrong" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modified := owner[0]
			tc.change(&modified)
			_, _, _, err := MaterializeSnapshot(t.Context(), cfg.ArchiveSources, []db.ArchiveSessionProvenance{modified}, "claude", 512<<20)
			require.Error(t, err)
		})
	}
}

func TestMaterializeSnapshotRejectsNewerInventory(t *testing.T) {
	source, inputs := archiveFixture(t)
	broken := invalidIndexSnapshot(t, source, inputs, "inventory version")
	index, err := OpenIndex(t.Context(), source, t.TempDir(), nil)
	require.NoError(t, err)
	defer index.Close()
	owner := db.ArchiveSessionProvenance{ArchiveSourceProvenance: db.ArchiveSourceProvenance{
		Attachment: db.ArchiveAttachmentKey{RepositoryID: index.Info.RepositoryID, SnapshotID: broken.Snapshot},
	}}
	_, _, _, err = MaterializeSnapshot(t.Context(), []config.ArchiveSource{broken}, []db.ArchiveSessionProvenance{owner}, "claude", 512<<20)
	require.ErrorContains(t, err, "unsupported recovery inventory version")
}
