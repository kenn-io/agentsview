package rawderive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexRecordedLabelSurvivesRawCapture(t *testing.T) {
	ctx := t.Context()
	home := filepath.Join(t.TempDir(), ".codex-run-v1-test")
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	sourcePath := filepath.Join(home, "sessions", "2026", "01", "01", "rollout-2026-01-01T00-00-00-"+id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o700))
	require.NoError(t, os.WriteFile(sourcePath, []byte(testjsonl.NewSessionBuilder().
		AddCodexMeta("2026-01-01T00:00:00Z", id, "/workspace/project", "codex_cli_rs").
		AddCodexMessage("2026-01-01T00:00:01Z", "user", "review").String()), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, parser.CodexRunMetadataFilename), []byte(`{"label":"recorded/label"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, parser.CodexSessionIndexFilename), []byte(`{"id":"`+id+`","thread_name":"Review title"}`+"\n"), 0o600))
	provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{filepath.Join(home, "sessions")}, Machine: "fixture"})
	require.True(t, ok)
	discovery, err := parser.DiscoverRawCaptureSources(ctx, provider)
	require.NoError(t, err)
	require.True(t, discovery.Complete)
	require.Len(t, discovery.Sources, 1)
	source := discovery.Sources[0]
	direct, err := provider.Parse(ctx, parser.ParseRequest{Source: source, ForceParse: true})
	require.NoError(t, err)
	require.Len(t, direct.Results, 1)
	assert.Equal(t, "recorded/label", direct.Results[0].Result.Session.Project)
	assert.Equal(t, "Review title", direct.Results[0].Result.Session.SessionName)
	manifest, objects := manifestFromCapturePlan(t, parser.AgentCodex, provider, source)
	materialized, err := (Materializer{Store: &materializerStore{objects: objects}, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}).Materialize(ctx, manifest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)
	hosted, err := dispatch.Parse(ctx, manifest, materialized)
	require.NoError(t, err)
	require.Len(t, hosted.Outcome.Results, 1)
	assert.Equal(t, direct.Results[0].Result.Session.Project, hosted.Outcome.Results[0].Result.Session.Project)
	assert.Equal(t, direct.Results[0].Result.Session.SessionName, hosted.Outcome.Results[0].Result.Session.SessionName)
}
