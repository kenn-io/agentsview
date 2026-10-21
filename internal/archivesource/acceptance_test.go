package archivesource

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestAttachedArchiveOtherRoutesKeepOwnership(t *testing.T) {
	for _, route := range []string{"remote", "artifact"} {
		for _, otherFirst := range []bool{false, true} {
			for _, collide := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/first=%t/conflict=%t", route, otherFirst, collide), func(t *testing.T) {
					source, inputs := archiveFixture(t)
					require.NoError(t, os.RemoveAll(inputs))
					receiver := dbtest.OpenTestDB(t)
					_, err := receiver.EnsureInstallationIdentity(t.Context(), "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
					require.NoError(t, err)
					cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}}
					engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
					defer engine.Close()
					manager, err := NewManager(t.Context(), cfg, receiver, engine, nil)
					require.NoError(t, err)
					defer manager.Close()
					origin := "other-machine"
					if collide {
						origin = sourceDevice
					}
					const native = "019eb791-cf7d-75c1-8439-9ed74c122e02"
					importOther := otherRouteImport(t, receiver, route, origin, native)
					first, second := func() error { return drainError(manager, t.Context(), nil) }, importOther
					if otherFirst {
						first, second = second, first
					}
					require.NoError(t, first())
					id := sourceDevice + "~" + native
					if otherFirst {
						id = origin + "~" + native
					}
					before, err := receiver.GetAllMessages(t.Context(), id)
					require.NoError(t, err)
					require.Len(t, before, 1)
					err = second()
					if collide {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
					}
					after, err := receiver.GetAllMessages(t.Context(), id)
					require.NoError(t, err)
					assert.Equal(t, before, after, "another ingestion route cannot replace accepted content")
					if !collide {
						for _, prefix := range []string{sourceDevice, origin} {
							session, err := receiver.GetSessionFull(t.Context(), prefix+"~"+native)
							require.NoError(t, err)
							require.NotNil(t, session)
							assert.Equal(t, prefix, session.Machine)
						}
					}
				})
			}
		}
	}
}

func otherRouteImport(t *testing.T, receiver *db.DB, route, origin, native string) func() error {
	t.Helper()
	if route == "remote" {
		root := t.TempDir()
		dbtest.WriteTestFile(t, filepath.Join(root, "remote", "project", native+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "other route message", native).String()))
		return func() error {
			_, err := (remotesync.Importer{Host: origin, DB: receiver, Full: true, RequireComplete: true}).ImportExtracted(t.Context(), remotesync.TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentClaude: {"/remote"}}}, root)
			return err
		}
	}
	source := dbtest.OpenTestDB(t)
	require.NoError(t, source.SetSyncState(t.Context(), "artifact_origin_id", origin))

	root := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(root, "project", native+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "other route message", native).String()))
	engine := syncer.NewEngine(t.Context(), source, syncer.EngineConfig{Ephemeral: true, Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}})
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()

	repo, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repo.Close()) })
	exported, err := artifact.ExportToStore(t.Context(), source, repo.Content(), artifact.ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	require.True(t, exported.CheckpointCreated)
	require.Equal(t, 1, exported.ExportedSessions, "%+v", exported)
	coordinator := artifact.NewStoreImportCoordinator(receiver, repo.Content(), "receiver-origin")
	for _, kind := range []artifact.Kind{artifact.KindSegments, artifact.KindManifests, artifact.KindCheckpoints} {
		entries, err := repo.Content().Entries(t.Context(), origin, kind)
		require.NoError(t, err)
		for {
			page, err := entries.Next(t.Context(), 128)
			for _, entry := range page {
				require.NoError(t, coordinator.RecordChanged(t.Context(), entry))
			}
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
		}
		require.NoError(t, entries.Close())
	}
	return func() error {
		_, err := coordinator.Finalize(t.Context())
		return err
	}
}

func TestAttachedArchiveIsNotRepublishedAsLocalArtifacts(t *testing.T) {
	source, inputs := archiveFixture(t)
	require.NoError(t, os.RemoveAll(inputs))
	receiver := dbtest.OpenTestDB(t)
	_, err := receiver.EnsureInstallationIdentity(t.Context(), "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	require.NoError(t, receiver.SetSyncState(t.Context(), "artifact_origin_id", "receiver-origin"))
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	manager, err := NewManager(t.Context(), config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}}, receiver, engine, nil)
	require.NoError(t, err)
	defer manager.Close()
	require.NoError(t, drainError(manager, t.Context(), nil))
	repo, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	defer repo.Close()
	result, err := artifact.ExportToStore(t.Context(), receiver, repo.Content(), artifact.ExportOptions{Origin: "receiver-origin", Full: true})
	require.NoError(t, err)
	assert.Zero(t, result.ExportedSessions)
	entries, err := repo.Content().Entries(t.Context(), "receiver-origin", artifact.KindManifests)
	require.NoError(t, err)
	defer entries.Close()
	page, err := entries.Next(t.Context(), 128)
	require.ErrorIs(t, err, io.EOF)
	assert.Empty(t, page)
}

func TestAttachedArchiveKeepsReceiverAssets(t *testing.T) {
	source, inputs := archiveFixture(t)
	require.NoError(t, os.RemoveAll(inputs))
	receiver := dbtest.OpenTestDB(t)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveSources: []config.ArchiveSource{source}, ArchiveContent: config.ArchiveContentFull}
	image, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=")
	require.NoError(t, err)
	ref, _, err := assets.Put(filepath.Join(cfg.DataDir, "assets"), "image/png", image)
	require.NoError(t, err)
	_, err = receiver.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: db.Session{ID: "local-image", Agent: "chatgpt", Machine: "local", Project: "project"}, Messages: []db.Message{{SessionID: "local-image", Role: "user", Ordinal: 0, Content: "![image](" + ref + ")"}}, ReplaceMessages: true}})
	require.NoError(t, err)
	before, err := receiver.GetAllMessages(t.Context(), "local-image")
	require.NoError(t, err)
	engine := syncer.NewEngine(t.Context(), receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	manager, err := NewManager(t.Context(), cfg, receiver, engine, nil)
	require.NoError(t, err)
	defer manager.Close()
	require.NoError(t, drainError(manager, t.Context(), nil))
	after, err := receiver.GetAllMessages(t.Context(), "local-image")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	data, err := os.ReadFile(filepath.Join(cfg.DataDir, "assets", strings.TrimPrefix(ref, "asset://")))
	require.NoError(t, err)
	assert.Equal(t, image, data)
	entries, err := os.ReadDir(filepath.Join(cfg.DataDir, "archive-sources", "scratch"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}
