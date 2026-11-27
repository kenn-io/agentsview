package rawderive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexRecordedLabelSurvivesRawCaptureAndLocalReplay(t *testing.T) {
	for _, tc := range []struct {
		name string
		root func(base, home string) string
	}{
		{name: "log folder", root: func(base, _ string) string { return base }},
		{name: "nested label root", root: func(base, _ string) string { return filepath.Join(base, "team") }},
		{name: "run sessions directory", root: func(_, home string) string { return filepath.Join(home, "sessions") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			base := t.TempDir()
			const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
			home := filepath.Join(base, "team", "case", ".codex-run-v1-test")
			sourcePath := filepath.Join(home, "sessions", "2026", "01", "01", "rollout-2026-01-01T00-00-00-"+id+".jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o700))
			require.NoError(t, os.WriteFile(sourcePath, []byte(testjsonl.NewSessionBuilder().
				AddCodexMeta("2026-01-01T00:00:00Z", id, "/workspace/project", "codex_cli_rs").
				AddCodexMessage("2026-01-01T00:00:01Z", "user", "review").String()), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(home, parser.CodexRunMetadataFilename),
				[]byte(`{"label":"recorded/label"}`), 0o600))

			root := tc.root(base, home)
			provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{
				Roots: []string{root}, Machine: "fixture",
			})
			require.True(t, ok)
			discovery, err := parser.DiscoverRawCaptureSources(ctx, provider)
			require.NoError(t, err)
			require.True(t, discovery.Complete)
			require.Len(t, discovery.Sources, 1)
			source := discovery.Sources[0]

			localOutcome, err := provider.Parse(ctx, parser.ParseRequest{Source: source, ForceParse: true})
			require.NoError(t, err)
			require.Len(t, localOutcome.Results, 1)
			assert.Equal(t, "recorded/label", localOutcome.Results[0].Result.Session.Project)

			plan, supported, err := parser.ResolveRawCapturePlan(ctx, provider, source)
			require.NoError(t, err)
			require.True(t, supported)
			var metadataCaptured bool
			for _, entry := range plan.Entries {
				if filepath.Base(entry.Path) == parser.CodexRunMetadataFilename {
					metadataCaptured = true
					assert.Contains(t, filepath.ToSlash(entry.Path), ".codex-run-v1-test/")
				}
			}
			assert.True(t, metadataCaptured)

			manifest, objects := manifestFromCapturePlan(t, parser.AgentCodex, provider, source)
			materialized, err := (Materializer{
				Store: &materializerStore{objects: objects}, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20,
			}).Materialize(ctx, manifest)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })

			dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
			require.NoError(t, err)
			hosted, err := dispatch.Parse(ctx, manifest, materialized)
			require.NoError(t, err)
			require.Len(t, hosted.Outcome.Results, 1)
			assert.Equal(t, "recorded/label", hosted.Outcome.Results[0].Result.Session.Project)

			storedPath := filepath.Join("/captured-codex", "rollout-"+id+".jsonl")
			local, err := PrepareLocalSource(ctx, manifest, materialized, "fixture", storedPath)
			require.NoError(t, err)
			database := dbtest.OpenTestDB(t)
			engine := syncer.NewEngine(ctx, database, local.Config)
			t.Cleanup(engine.Close)
			require.NoError(t, engine.SyncPathsContext(ctx, []string{local.Path}))
			session, err := database.GetSessionFull(ctx, "codex:"+id)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, "recorded/label", session.Project)
		})
	}
}
