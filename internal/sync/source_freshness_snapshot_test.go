package sync

import (
	"context"
	"database/sql"
	"os"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
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
				if scenario == "unchanged" {
					// Restart the reader pool too, so cached statements cannot hide
					// fallback queries from the SQLite authorizer.
					require.NoError(t, fx.db.CloseConnections(t.Context()))
					require.NoError(t, fx.db.Reopen())
				}
				fx.engineWithEmitter(t.Context(), nil)
				t.Cleanup(fx.engine.Close)
				fx.engine.SetSoleArchiveWriter(enabled)
				var statHashReads, fileSizeReads atomic.Int32
				if scenario == "unchanged" {
					reader := fx.db.Reader().(interface {
						Conn(context.Context) (*sql.Conn, error)
					})
					// Hold all four reader connections while registering, so every
					// worker uses an observed connection once they're released.
					var conns []*sql.Conn
					for range 4 {
						conn, err := reader.Conn(t.Context())
						require.NoError(t, err)
						conns = append(conns, conn)
						t.Cleanup(func() { _ = conn.Close() })
						require.NoError(t, conn.Raw(func(raw any) error {
							raw.(interface {
								RegisterAuthorizer(func(int, string, string, string) int)
							}).RegisterAuthorizer(func(op int, table, column, _ string) int {
								if op == sqlite3.SQLITE_READ {
									if table == "provider_freshness" && column == "stat_hash" {
										statHashReads.Add(1)
									}
									if table == "sessions" && column == "file_size" {
										fileSizeReads.Add(1)
									}
								}
								return sqlite3.SQLITE_OK
							})
							return nil
						}))
					}
					for _, conn := range conns {
						require.NoError(t, conn.Close())
					}
				}
				stats := fx.engine.SyncAll(t.Context(), nil)
				require.Zero(t, stats.Failed)
				if scenario == "unchanged" {
					if enabled {
						require.EqualValues(t, 1, statHashReads.Load(), "only the bulk snapshot may read stat hashes")
						require.EqualValues(t, 1, fileSizeReads.Load(), "only the bulk snapshot may read file sizes")
					} else {
						require.Positive(t, statHashReads.Load(), "live stat-hash fallbacks must be observed")
						require.Positive(t, fileSizeReads.Load(), "live source-state fallbacks must be observed")
					}
				}
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
	require.Nil(t, sourceFreshnessSnapshotFrom(fx.engine.withSourceFreshnessSnapshot(ctx, []parser.DiscoveredFile{{Agent: parser.AgentVibe}})))
	require.NotNil(t, sourceFreshnessSnapshotFrom(fx.engine.withSourceFreshnessSnapshot(ctx, []parser.DiscoveredFile{{Agent: parser.AgentClaude}})))
}

func TestSourceFreshnessSnapshotLookupPath(t *testing.T) {
	fx := newEngineFixture(t)
	t.Cleanup(fx.engine.Close)
	path := t.TempDir() + "/source.jsonl"
	fx.engine.pathRewriter = func(path string) string { return path + ".logical" }
	require.NoError(t, fx.db.UpsertProviderStatHash(t.Context(), parser.AgentClaude, path+".logical", 42))
	ctx := fx.engine.withSourceFreshnessSnapshot(t.Context(), []parser.DiscoveredFile{{
		Agent: parser.AgentClaude,
		Path:  path + ".discovered",
		ProviderSource: &parser.SourceRef{
			Provider: parser.AgentClaude, DisplayPath: path,
		},
	}})
	hash, found := sourceFreshnessSnapshotFrom(ctx).StatHash("claude", path+".logical")
	require.True(t, found)
	require.EqualValues(t, 42, hash)
}
