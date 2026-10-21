package sync

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// Both collecting and scratch-streaming Codex writes must pass the archive's
// identity/deletion policy before writing normalized content.
func TestArchivePolicySuppressesCodexPublication(t *testing.T) {
	for _, threshold := range []int64{0, 1} {
		t.Run(map[int64]string{0: "collecting", 1: "staged"}[threshold], func(t *testing.T) {
			const id = "019eb791-cf7d-75c1-8439-9ed74c122b13"
			root := writeCodexParityRoot(t, id)
			database := openTestDB(t)
			var observed []string
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
				Machine:   "source", IDPrefix: "installation~", Ephemeral: true,
				ArchiveReparse: true, StagedCodexParseMinBytes: threshold,
				DiscardPendingWritesOnCancel: true,
				ArchiveSessionPolicy: func(_ context.Context, s *db.Session, native string) (bool, error) {
					observed = append(observed, native)
					assert.Equal(t, "installation~codex:"+id, s.ID)
					return false, nil
				},
			})
			defer engine.Close()
			stats := engine.SyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			assert.Equal(t, []string{"codex:" + id}, observed)
			stored, err := database.GetSessionFull(t.Context(), "installation~codex:"+id)
			require.NoError(t, err)
			assert.Nil(t, stored)
			messages, err := database.GetAllMessages(t.Context(), "installation~codex:"+id)
			require.NoError(t, err)
			assert.Empty(t, messages)
		})
	}
}

func TestArchiveProvenanceCodexPublication(t *testing.T) {
	for _, threshold := range []int64{0, 1} {
		t.Run(map[int64]string{0: "collecting", 1: "staged"}[threshold], func(t *testing.T) {
			const id = "019eb791-cf7d-75c1-8439-9ed74c122b13"
			const device = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			root := writeCodexParityRoot(t, id)
			database := openTestDB(t)
			key := db.ArchiveAttachmentKey{RepositoryID: "repository", SnapshotID: "snapshot"}
			p := db.ArchiveSourceProvenance{Attachment: key, ManifestID: "manifest", Source: db.ArchiveSourceIdentity{
				ArchiveID: "archive", DeviceID: device, RootID: "root", SourceKey: "source",
			}}
			require.NoError(t, database.RegisterArchiveAttachment(t.Context(), key, "archive"))
			require.NoError(t, database.SetArchiveAttachmentTargets(t.Context(), key, "target", []db.ArchiveSourceTarget{{Provenance: p, Target: "target"}}))
			cfg := EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
				Machine:   device, IDPrefix: device + "~", Ephemeral: true,
				ArchiveReparse: true, StagedCodexParseMinBytes: threshold, DiscardPendingWritesOnCancel: true,
				ArchiveWriteProvenance: func(s *db.Session, native string) (db.ArchiveSessionProvenance, error) {
					assert.Equal(t, device+"~codex:"+id, s.ID)
					return db.ArchiveSessionProvenance{ArchiveSourceProvenance: p, NativeSessionID: native}, nil
				},
			}
			engine := NewEngine(t.Context(), database, cfg)
			defer engine.Close()
			stats := engine.SyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			require.Positive(t, stats.Synced)
			before, err := database.GetSessionFull(t.Context(), device+"~codex:"+id)
			require.NoError(t, err)
			require.NotNil(t, before)
			messages, err := database.GetAllMessages(t.Context(), before.ID)
			require.NoError(t, err)
			require.NotEmpty(t, messages)
			// Change the producer's claimed owner while retaining the same resulting ID.
			p.ManifestID = "conflicting-manifest"
			stats = engine.SyncAllForceParse(t.Context(), nil)
			require.Positive(t, stats.Failed)
			after, err := database.GetSessionFull(t.Context(), before.ID)
			require.NoError(t, err)
			afterMessages, err := database.GetAllMessages(t.Context(), before.ID)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Equal(t, messages, afterMessages)
		})
	}
}

func TestAttachedSourceCleanupFailureStaysPending(t *testing.T) {
	const id = "019eb791-cf7d-75c1-8439-9ed74c122b13"
	const device = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	root := writeCodexParityRoot(t, id)
	database := openTestDB(t)
	key := db.ArchiveAttachmentKey{RepositoryID: "repository", SnapshotID: "snapshot"}
	p := db.ArchiveSourceProvenance{Attachment: key, ManifestID: "manifest", Source: db.ArchiveSourceIdentity{
		ArchiveID: "archive", DeviceID: device, RootID: "root", SourceKey: "source",
	}}
	require.NoError(t, database.RegisterArchiveAttachment(t.Context(), key, "archive"))
	require.NoError(t, database.SetArchiveAttachmentTargets(t.Context(), key, "target", []db.ArchiveSourceTarget{{Provenance: p, Target: "target"}}))
	cfg := EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: device, IDPrefix: device + "~",
		Ephemeral: true, ArchiveReparse: true, DiscardPendingWritesOnCancel: true, StagedCodexParseMinBytes: 1,
		ArchiveWriteProvenance: func(_ *db.Session, native string) (db.ArchiveSessionProvenance, error) {
			return db.ArchiveSessionProvenance{ArchiveSourceProvenance: p, NativeSessionID: native}, nil
		},
	}
	parent := NewEngine(t.Context(), database, EngineConfig{Ephemeral: true})
	defer parent.Close()
	path := filepath.Join(root, "2024", "01", "01", "rollout-2024-01-01T10-00-00-"+id+".jsonl")
	_, err := parent.SyncAttachedSource(t.Context(), p, "target", func() (EngineConfig, string, func() error, error) {
		return cfg, path, func() error { return errors.New("scratch removal failed") }, nil
	}, nil)
	require.ErrorContains(t, err, "scratch removal failed")
	status, err := database.ArchiveAttachmentStatus(t.Context(), key)
	require.NoError(t, err)
	assert.Equal(t, int64(1), status.Pending)
	_, err = parent.SyncAttachedSource(t.Context(), p, "target", func() (EngineConfig, string, func() error, error) {
		return cfg, path, func() error { return nil }, nil
	}, nil)
	require.NoError(t, err)
	status, err = database.ArchiveAttachmentStatus(t.Context(), key)
	require.NoError(t, err)
	assert.Zero(t, status.Pending)
	require.NoError(t, database.SetArchiveAttachmentTargets(t.Context(), key, "suppressed", []db.ArchiveSourceTarget{{Provenance: p, Target: "suppressed"}}))
	cfg.ArchiveSessionPolicy = func(context.Context, *db.Session, string) (bool, error) { return false, nil }
	_, err = parent.SyncAttachedSource(t.Context(), p, "suppressed", func() (EngineConfig, string, func() error, error) {
		return cfg, path, func() error { return nil }, nil
	}, nil)
	require.NoError(t, err)
	status, err = database.ArchiveAttachmentStatus(t.Context(), key)
	require.NoError(t, err)
	assert.Zero(t, status.Pending)
}
