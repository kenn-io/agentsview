package rawderive

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// A local archive must keep native session IDs while replacing disposable
// materialization paths with stable source identities. Exercise the real vault,
// provider, and SQLite write path with the original files already removed.
func TestLocalSyncFromVaultPreservesSourceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		agent    parser.AgentType
		stageMin int64
	}{
		{name: "claude", agent: parser.AgentClaude},
		{name: "codex", agent: parser.AgentCodex},
		{name: "codex staged", agent: parser.AgentCodex, stageMin: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			const nativeID = "019eb791-cf7d-75c1-8439-9ed74c122e02"
			wantID := nativeID
			var file, content string
			if tc.agent == parser.AgentCodex {
				file = filepath.Join(root, "rollout-2026-01-01T00-00-00-"+nativeID+".jsonl")
				content = testjsonl.NewSessionBuilder().
					AddCodexMeta("2026-01-01T00:00:00Z", nativeID, "/workspace/project-a", "codex_cli_rs").
					AddCodexMessage("2026-01-01T00:00:01Z", "user", "hello archive").
					AddCodexMessage("2026-01-01T00:00:02Z", "assistant", "archive reply").String()
				wantID = "codex:" + nativeID
			} else {
				file = filepath.Join(root, "project-a", nativeID+".jsonl")
				content = testjsonl.NewSessionBuilder().
					AddClaudeUserWithSessionID("2026-01-01T00:00:01Z", "hello archive", nativeID).
					AddClaudeAssistant("2026-01-01T00:00:02Z", "archive reply").String()
			}
			dbtest.WriteTestFile(t, file, []byte(content))
			provider, ok := parser.NewProvider(tc.agent, parser.ProviderConfig{
				Roots: []string{root}, Machine: "origin-device",
			})
			require.True(t, ok)
			sources, err := provider.Discover(ctx)
			require.NoError(t, err)
			require.Len(t, sources, 1)
			manifest, objects := manifestFromCapturePlan(t, tc.agent, provider, sources[0])
			repo, err := artifact.OpenRepository(ctx, t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, repo.Close()) })
			store, err := rawsync.NewArtifactObjectStore(repo.Content())
			require.NoError(t, err)
			for ref, raw := range objects {
				_, err = store.PutObject(ctx, manifest.Identity.TenantID, ref, bytes.NewReader(raw))
				require.NoError(t, err)
			}
			require.NoError(t, os.RemoveAll(root))
			materialized, err := (Materializer{
				Store: store, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20,
			}).Materialize(ctx, manifest)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, materialized.Cleanup()) })
			paths := newStablePathMap(manifest, materialized)
			roots := materializedProviderRoots(manifest, materialized)
			metadata := materializedProviderMetadataDirs(manifest, materialized, roots)
			restored, ok := parser.NewProvider(tc.agent, parser.ProviderConfig{
				Roots: roots, MetadataDirs: metadata, Machine: "origin-device",
				StableSourceSnapshots: true, PathRewriter: paths.rewrite,
			})
			require.True(t, ok)
			discovery, err := parser.DiscoverRawCaptureSources(
				parser.WithoutFilesystemProjectDiscovery(ctx), restored,
			)
			require.NoError(t, err)
			require.True(t, discovery.Complete)
			source, err := matchProviderSource(ctx, restored, discovery.Sources, manifest, materialized, false)
			require.NoError(t, err)
			paths.bindSource(source, manifest.Manifest.SourceKey)

			database := dbtest.OpenTestDB(t)
			engine := syncer.NewEngine(ctx, database, syncer.EngineConfig{
				AgentDirs:                         map[parser.AgentType][]string{tc.agent: roots},
				ProviderMetadata:                  map[parser.AgentType]map[string][]string{tc.agent: metadata},
				Machine:                           "origin-device",
				Ephemeral:                         true,
				DiscardPendingWritesOnCancel:      true,
				DisableFilesystemProjectDiscovery: true,
				StableSourceSnapshots:             true,
				PathRewriter:                      paths.rewrite,
				StoredPathResolver:                paths.resolve,
				StagedCodexParseMinBytes:          tc.stageMin,
			})
			t.Cleanup(engine.Close)
			require.NoError(t, engine.SyncPathsContext(ctx, []string{source.DisplayPath}))
			session, err := database.GetSessionFull(ctx, wantID)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, "origin-device", session.Machine)
			require.NotNil(t, session.FilePath)
			assert.Equal(t, manifest.Manifest.SourceKey, *session.FilePath)
			messages, err := database.GetAllMessages(ctx, wantID)
			require.NoError(t, err)
			require.Len(t, messages, 2)
			assert.Equal(t, "hello archive", messages[0].Content)
			assert.Equal(t, "archive reply", messages[1].Content)
		})
	}
}
