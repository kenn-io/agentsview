package archivesource

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestMaterializeAttachedSnapshotOwnership(t *testing.T) {
	ctx := t.Context()
	source, inputs := archiveFixture(t)
	require.NoError(t, os.RemoveAll(inputs))
	index, err := OpenIndex(ctx, source, t.TempDir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	sources, err := index.Sources(ctx, "", 10)
	require.NoError(t, err)
	var claude Source
	for _, item := range sources {
		if item.Root.Provider == "claude" {
			claude = item
		}
	}
	require.NotEmpty(t, claude.ManifestID)
	owner := db.ArchiveSessionProvenance{ArchiveSourceProvenance: index.provenance(claude), NativeSessionID: "019eb791-cf7d-75c1-8439-9ed74c122e02"}
	// Duplicate configuration and an unavailable equivalent membership do not
	// make the configured, verified snapshot ambiguous.
	other := owner
	other.Attachment.SnapshotID = "unconfigured"
	duplicate := []config.ArchiveSource{source, source}
	tree, _, chosen, err := MaterializeSnapshot(ctx, duplicate, []db.ArchiveSessionProvenance{other, owner}, "claude", 512<<20)
	require.NoError(t, err)
	assert.Equal(t, owner.Attachment, chosen)
	require.NoError(t, tree.Cleanup())
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
			modified := owner
			tc.change(&modified)
			_, _, _, err := MaterializeSnapshot(t.Context(), []config.ArchiveSource{source}, []db.ArchiveSessionProvenance{modified}, "claude", 512<<20)
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
