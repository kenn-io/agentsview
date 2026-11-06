package sync

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSourceFreshnessSnapshotPassParity(t *testing.T) {
	for _, scenario := range []string{"unchanged", "trashed", "stale", "project", "missing digest", "appended"} {
		t.Run(scenario, func(t *testing.T) {
			var fixture []byte
			var want *db.Session
			var wantMessages []db.Message
			var wantStats SyncStats
			for _, enabled := range []bool{false, true} {
				fx := newEngineFixture(t)
				path := fx.writeClaudeSession(t, "proj", "warm.jsonl", "hello")
				if fixture == nil {
					var err error
					fixture, err = os.ReadFile(path)
					require.NoError(t, err)
				} else {
					require.NoError(t, os.WriteFile(path, fixture, 0600))
				}
				fx.writeClaudeSession(t, "proj", "unchanged.jsonl", "stable")
				require.Equal(t, 2, fx.engine.SyncAll(t.Context(), nil).Synced)
				switch scenario {
				case "trashed":
					require.NoError(t, fx.db.SoftDeleteSession(t.Context(), "warm"))
				case "stale":
					require.NoError(t, fx.db.SetSessionDataVersion(t.Context(), "warm", 1))
				case "project":
					// Legacy generated labels require a parse even when file stats match.
					row, err := fx.db.GetSessionFull(t.Context(), "warm")
					require.NoError(t, err)
					row.Project = "_tmp_legacy"
					require.NoError(t, fx.db.UpsertSession(t.Context(), *row))
				case "missing digest":
					require.NoError(t, fx.db.DeleteProviderStatHash(t.Context(), parser.AgentClaude, path))
				case "appended":
					f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
					require.NoError(t, err)
					_, err = f.WriteString("{\"type\":\"user\",\"timestamp\":\"2024-01-01T10:05:00Z\",\"message\":{\"content\":\"more\"}}\n")
					require.NoError(t, err)
					require.NoError(t, f.Close())
				}
				fx.engine.Close()
				fx.engineWithEmitter(t.Context(), nil)
				t.Cleanup(fx.engine.Close)
				fx.engine.SetSoleArchiveWriter(enabled)
				stats := fx.engine.SyncAll(t.Context(), nil)
				require.Zero(t, stats.Failed)
				row, err := fx.db.GetSessionFull(t.Context(), "warm")
				require.NoError(t, err)
				// These values belong to each fixture's filesystem or write clock.
				row.FilePath = nil
				row.FileMtime = nil
				row.CreatedAt = ""
				row.LocalModifiedAt = nil
				row.TranscriptRevision = nil
				row.FileInode = nil
				row.FileDevice = nil
				if scenario == "trashed" {
					require.NotNil(t, row.DeletedAt)
				}
				row.DeletedAt = nil
				messages, err := fx.db.GetAllMessages(t.Context(), "warm")
				require.NoError(t, err)
				for i := range messages {
					messages[i].ID = 0
				}
				if !enabled {
					want = row
					wantMessages = messages
					wantStats = stats
				} else {
					require.Equal(t, wantStats, stats)
					require.Equal(t, want, row)
					require.Equal(t, wantMessages, messages)
				}
			}
		})
	}
}

func TestSourceFreshnessSnapshotProviderSelection(t *testing.T) {
	fx := newEngineFixture(t)
	t.Cleanup(fx.engine.Close)
	ctx := t.Context()
	for _, file := range []parser.DiscoveredFile{{Agent: parser.AgentVibe}, {Agent: parser.AgentClaude, ForceParse: true}} {
		require.Nil(t, sourceFreshnessSnapshotFrom(fx.engine.withSourceFreshnessSnapshot(ctx, []parser.DiscoveredFile{file})))
	}
	require.NotNil(t, sourceFreshnessSnapshotFrom(fx.engine.withSourceFreshnessSnapshot(ctx, []parser.DiscoveredFile{{Agent: parser.AgentClaude}})))
}
