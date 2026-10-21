package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestOrdinarySourceCannotReplaceAttachedHistory(t *testing.T) {
	for _, mode := range []string{"parser exclusion", "rebuild replacement", "rebuild parser exclusion"} {
		t.Run(mode, func(t *testing.T) {
			database := openTestDB(t)
			const device = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			const id = device + "~owned"
			key := db.ArchiveAttachmentKey{RepositoryID: "repository", SnapshotID: "snapshot"}
			provenance := db.ArchiveSessionProvenance{
				Attachment:      key,
				Source:          db.ArchiveSourceIdentity{ArchiveID: "archive", DeviceID: device, RootID: "root", SourceKey: "source"},
				ManifestID:      "manifest",
				NativeSessionID: "owned",
			}
			require.NoError(t, database.RegisterArchiveAttachment(t.Context(), key, "archive"))
			require.NoError(t, database.SetArchiveAttachmentTargets(t.Context(), key, "target", []db.ArchiveSourceTarget{{Provenance: provenance.ArchiveSourceProvenance, Target: "target"}}))
			_, err := database.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
				Session:         db.Session{ID: id, Project: "archived", Machine: device, Agent: "claude", FilePath: new("archive://source/owned.jsonl")},
				Messages:        []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "retained attached history"}},
				ReplaceMessages: true, ArchiveProvenance: &provenance,
			}})
			require.NoError(t, err)

			root := t.TempDir()
			path := filepath.Join(root, "project", "owned.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			content := "<command-name>/usage</command-name>\n<command-message>usage</command-message>\n<command-args></command-args>"
			if mode == "rebuild replacement" {
				content = "colliding contributor transcript"
			}
			require.NoError(t, os.WriteFile(path, []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", content).String()), 0o644))
			cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: device, IDPrefix: device + "~", Ephemeral: true}
			var stats SyncStats
			if mode == "parser exclusion" {
				engine := NewEngine(t.Context(), database, cfg)
				t.Cleanup(engine.Close)
				stats = engine.SyncAll(t.Context(), nil)
			} else {
				localRoot := t.TempDir()
				localPath := filepath.Join(localRoot, "project", "local.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(localPath), 0o755))
				require.NoError(t, os.WriteFile(localPath, []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "local history").String()), 0o644))
				engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {localRoot}}, Machine: "local"})
				t.Cleanup(engine.Close)
				stats, err = engine.ResyncAllWithOptions(t.Context(), nil, RebuildOptions{Contributors: []RebuildContributor{{Name: "ordinary", Config: cfg}}})
				require.NoError(t, err)
				assert.True(t, stats.Aborted)
			}
			assert.Positive(t, stats.Failed)
			messages, err := database.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "retained attached history", messages[0].Content)
			ids, err := database.ArchiveSourceSessionIDs(t.Context(), provenance.Source)
			require.NoError(t, err)
			assert.Equal(t, []string{id}, ids)
		})
	}
}
