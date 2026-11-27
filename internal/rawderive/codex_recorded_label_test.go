package rawderive

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexRecordedLabelSurvivesRawCapture(t *testing.T) {
	for _, tc := range []struct {
		name, metadata    string
		alias, parentRoot bool
	}{
		{"valid", `{"label":"recorded/label"}`, false, false},
		{"empty", `{"label":""}`, false, false},
		{"malformed", `{"label":`, false, false},
		{"alias index", `{"label":"recorded/label"}`, true, false},
		{"parent root malformed", `{"label":`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) { testCodexRecordedLabelSurvivesRawCapture(t, tc.metadata, tc.alias, tc.parentRoot) })
	}
}

func testCodexRecordedLabelSurvivesRawCapture(t *testing.T, metadata string, alias, parentRoot bool) {
	t.Helper()
	ctx := t.Context()
	home := filepath.Join(t.TempDir(), ".codex-run-v1-test")
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	sourcePath := filepath.Join(home, "sessions", "2026", "01", "01", "rollout-2026-01-01T00-00-00-"+id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o700))
	require.NoError(t, os.WriteFile(sourcePath, []byte(testjsonl.NewSessionBuilder().
		AddCodexMeta("2026-01-01T00:00:00Z", id, "/workspace/project", "codex_exec").
		AddCodexMessage("2026-01-01T00:00:01Z", "user", "review").String()), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, parser.CodexRunMetadataFilename), []byte(metadata), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, parser.CodexSessionIndexFilename), []byte(`{"id":"`+id+`","thread_name":"Review title"}`+"\n"), 0o600))
	root := filepath.Join(home, "sessions")
	if parentRoot {
		root = filepath.Dir(home)
	}
	dirs := []string{home}
	if alias {
		aliasHome := filepath.Join(t.TempDir(), "alias")
		require.NoError(t, os.MkdirAll(aliasHome, 0o700))
		aliasIndex := filepath.Join(aliasHome, parser.CodexSessionIndexFilename)
		require.NoError(t, os.WriteFile(aliasIndex, []byte(`{"id":"`+id+`","thread_name":"Alias title"}`+"\n"), 0o600))
		now := time.Now()
		require.NoError(t, os.Chtimes(aliasIndex, now.Add(time.Minute), now.Add(time.Minute)))
		dirs = append(dirs, aliasHome)
	}
	provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{root}, MetadataDirs: map[string][]string{root: dirs}, Machine: "fixture"})
	require.True(t, ok)
	discovery, err := parser.DiscoverRawCaptureSources(ctx, provider)
	require.NoError(t, err)
	require.True(t, discovery.Complete)
	require.Len(t, discovery.Sources, 1)
	source := discovery.Sources[0]
	direct, err := provider.Parse(ctx, parser.ParseRequest{Source: source, ForceParse: true})
	require.NoError(t, err)
	require.Len(t, direct.Results, 1)
	if metadata == `{"label":"recorded/label"}` {
		assert.Equal(t, "recorded/label", direct.Results[0].Result.Session.Project)
	} else {
		assert.Equal(t, "project", direct.Results[0].Result.Session.Project)
		assert.Equal(t, parser.SessionKindNonInteractive, direct.Results[0].Result.Session.SessionKind)
	}
	if alias {
		assert.Equal(t, "Alias title", direct.Results[0].Result.Session.SessionName)
	} else {
		assert.Equal(t, "Review title", direct.Results[0].Result.Session.SessionName)
	}
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
	assert.Equal(t, direct.Results[0].Result.Session.SessionKind, hosted.Outcome.Results[0].Result.Session.SessionKind)
	assert.Equal(t, direct.Results[0].Result.Session.SessionName, hosted.Outcome.Results[0].Result.Session.SessionName)
	assert.Equal(t, direct.Results[0].Result.Session.File.Mtime, hosted.Outcome.Results[0].Result.Session.File.Mtime)
}

func TestCodexRawReplayTitleIndexes(t *testing.T) {
	for _, layout := range []string{"symlinked sessions", "overlapping roots"} {
		t.Run(layout, func(t *testing.T) {
			base := t.TempDir()
			const id = "019eb791-cf7d-75c1-8439-9ed74c122e04"
			home := filepath.Join(base, "team", ".codex-run-v1-test")
			sessions := filepath.Join(home, "sessions")
			path := filepath.Join(sessions, "2026", "01", "01", "rollout-2026-01-01T00-00-00-"+id+".jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(testjsonl.NewSessionBuilder().AddCodexMeta("2026-01-01T00:00:00Z", id, "/work/project", "codex_exec").AddCodexMessage("2026-01-01T00:00:01Z", "user", "review").String()), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(home, parser.CodexRunMetadataFilename), []byte(`{"label":"team"}`), 0o600))
			alias := filepath.Join(base, "alias")
			require.NoError(t, os.MkdirAll(alias, 0o700))
			var roots []string
			metadata := map[string][]string{}
			primary := home
			if layout == "symlinked sessions" {
				primary = filepath.Join(base, ".codex-run-v1-configured")
				require.NoError(t, os.MkdirAll(primary, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(primary, parser.CodexRunMetadataFilename), []byte(`{"label":"team"}`), 0o600))
				root := filepath.Join(primary, "sessions")
				if err := os.Symlink(sessions, root); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				roots = []string{sessions}
				metadata[sessions] = []string{primary}
			} else {
				roots = []string{base, sessions}
				metadata[sessions] = []string{home, alias}
			}
			index := filepath.Join(primary, parser.CodexSessionIndexFilename)
			require.NoError(t, os.WriteFile(index, []byte(`{"id":"`+id+`","thread_name":"Primary title"}`+"\n"), 0o600))
			if layout == "overlapping roots" {
				index = filepath.Join(alias, parser.CodexSessionIndexFilename)
				require.NoError(t, os.WriteFile(index, []byte(`{"id":"`+id+`","thread_name":"Alias title"}`+"\n"), 0o600))
				now := time.Now().Add(time.Minute)
				require.NoError(t, os.Chtimes(index, now, now))
			}
			provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: roots, MetadataDirs: metadata})
			require.True(t, ok)
			discovery, err := parser.DiscoverRawCaptureSources(t.Context(), provider)
			require.NoError(t, err)
			require.True(t, discovery.Complete)
			require.Len(t, discovery.Sources, 1)
			source := discovery.Sources[0]
			direct, err := provider.Parse(t.Context(), parser.ParseRequest{Source: source, ForceParse: true})
			require.NoError(t, err)
			require.Len(t, direct.Results, 1)
			want := "Primary title"
			if layout == "overlapping roots" {
				want = "Alias title"
			}
			require.Equal(t, want, direct.Results[0].Result.Session.SessionName)
			manifest, objects := manifestFromCapturePlan(t, parser.AgentCodex, provider, source)
			materialized, err := (Materializer{Store: &materializerStore{objects: objects}, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}).Materialize(t.Context(), manifest)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })
			dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
			require.NoError(t, err)
			replayed, err := dispatch.Parse(t.Context(), manifest, materialized)
			require.NoError(t, err)
			require.Len(t, replayed.Outcome.Results, 1)
			assert.Equal(t, want, replayed.Outcome.Results[0].Result.Session.SessionName)
			assert.Equal(t, direct.Results[0].Result.Session.Project, replayed.Outcome.Results[0].Result.Session.Project)
			assert.Equal(t, direct.Results[0].Result.Session.File.Mtime, replayed.Outcome.Results[0].Result.Session.File.Mtime)
		})
	}
}
