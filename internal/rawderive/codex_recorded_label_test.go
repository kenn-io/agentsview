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

func TestCodexRawReplay(t *testing.T) {
	for _, layout := range []string{"recorded", "symlinked sessions", "overlapping roots", "path label outer first", "path label inner first", "unlabeled run home", "alias-homes label"} {
		t.Run(layout, func(t *testing.T) {
			base := t.TempDir()
			const id = "019eb791-cf7d-75c1-8439-9ed74c122e04"
			label := filepath.Join("team", "case")
			if layout == "alias-homes label" {
				label = filepath.Join("alias-homes", "case")
			}
			home := filepath.Join(base, label, ".codex-run-v1-test")
			sessions := filepath.Join(home, "sessions")
			rollout := filepath.Join(sessions, "2026", "01", "01", "rollout-2026-01-01T00-00-00-"+id+".jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(rollout), 0o700))
			require.NoError(t, os.WriteFile(rollout, []byte(testjsonl.NewSessionBuilder().AddCodexMeta("2026-01-01T00:00:00Z", id, "/work/project", "codex_exec").AddCodexMessage("2026-01-01T00:00:01Z", "user", "review").String()), 0o600))
			pathLabel := layout == "path label outer first" || layout == "path label inner first"
			if !pathLabel && layout != "unlabeled run home" && layout != "alias-homes label" {
				require.NoError(t, os.WriteFile(filepath.Join(home, parser.CodexRunMetadataFilename), []byte(`{"label":"recorded/label"}`), 0o600))
			}
			roots := []string{sessions}
			metadata := map[string][]string{sessions: {home}}
			primary := home
			if layout == "symlinked sessions" {
				primary = filepath.Join(base, ".codex-run-v1-configured")
				require.NoError(t, os.MkdirAll(primary, 0o700))
				if err := os.Symlink(sessions, filepath.Join(primary, "sessions")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				metadata[sessions] = []string{primary}
			} else if layout == "overlapping roots" {
				alias := filepath.Join(base, "alias")
				require.NoError(t, os.MkdirAll(alias, 0o700))
				aliasIndex := filepath.Join(alias, parser.CodexSessionIndexFilename)
				require.NoError(t, os.WriteFile(aliasIndex, []byte(`{"id":"`+id+`","thread_name":"Alias title"}`+"\n"), 0o600))
				now := time.Now().Add(time.Minute)
				require.NoError(t, os.Chtimes(aliasIndex, now, now))
				roots = []string{base, sessions}
				metadata[sessions] = []string{home, alias}
			} else if pathLabel {
				roots = []string{base, filepath.Join(base, "team")}
				if layout == "path label inner first" {
					roots[0], roots[1] = roots[1], roots[0]
				}
			} else if layout == "alias-homes label" {
				roots = []string{base}
				primary = filepath.Join(base, label)
				metadata[sessions] = []string{primary}
			}
			index := filepath.Join(primary, parser.CodexSessionIndexFilename)
			require.NoError(t, os.WriteFile(index, []byte(`{"id":"`+id+`","thread_name":"Primary title"}`+"\n"), 0o600))
			provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: roots, MetadataDirs: metadata})
			require.True(t, ok)
			discovery, err := parser.DiscoverRawCaptureSources(t.Context(), provider)
			require.NoError(t, err)
			require.True(t, discovery.Complete)
			require.Len(t, discovery.Sources, 1)
			if layout == "alias-homes label" {
				plan, supported, err := parser.ResolveRawCapturePlan(t.Context(), provider, discovery.Sources[0])
				require.NoError(t, err)
				require.True(t, supported)
				assert.Contains(t, plan.Entries, parser.RawCaptureEntry{Path: "alias-homes/case/session_index.jsonl", LocalPath: index})
			}
			if layout == "unlabeled run home" {
				plan, supported, err := parser.ResolveRawCapturePlan(t.Context(), provider, discovery.Sources[0])
				require.NoError(t, err)
				require.True(t, supported)
				assert.Equal(t, home, plan.CaptureRoot)
			}
			if layout == "overlapping roots" || layout == "symlinked sessions" {
				direct, err := provider.Parse(t.Context(), parser.ParseRequest{Source: discovery.Sources[0], ForceParse: true})
				require.NoError(t, err)
				require.Len(t, direct.Results, 1)
				want := "Primary title"
				if layout == "overlapping roots" {
					want = "Alias title"
				}
				assert.Equal(t, want, direct.Results[0].Result.Session.SessionName)
			}
			wantProject := "recorded/label"
			if pathLabel {
				wantProject = "team/case"
			} else if layout == "unlabeled run home" {
				wantProject = "project"
			} else if layout == "alias-homes label" {
				wantProject = "alias-homes/case"
			}
			assertCodexReplayMatchesDirect(t, provider, discovery.Sources[0], wantProject)
		})
	}
}

func TestCodexRawReplayMultipleHomeIndexes(t *testing.T) {
	base := t.TempDir()
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e04"
	label := filepath.Join(base, "alias-homes", "case")
	home := filepath.Join(label, ".codex-run-v1-test")
	sessions := filepath.Join(home, "sessions")
	rollout := filepath.Join(sessions, "2026", "01", "01", "rollout-2026-01-01T00-00-00-"+id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(rollout), 0o700))
	require.NoError(t, os.WriteFile(rollout, []byte(testjsonl.NewSessionBuilder().AddCodexMeta("2026-01-01T00:00:00Z", id, "/work/project", "codex_exec").AddCodexMessage("2026-01-01T00:00:01Z", "user", "review").String()), 0o600))
	for i, dir := range []string{home, label} {
		index := filepath.Join(dir, parser.CodexSessionIndexFilename)
		require.NoError(t, os.WriteFile(index, []byte(`{"id":"`+id+`","thread_name":"title `+string(rune('a'+i))+`"}`+"\n"), 0o600))
		mtime := time.Unix(int64(100+i), 0)
		require.NoError(t, os.Chtimes(index, mtime, mtime))
	}
	provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{
		Roots: []string{base}, MetadataDirs: map[string][]string{sessions: {home, label}}, StableSourceSnapshots: true,
	})
	require.True(t, ok)
	discovery, err := parser.DiscoverRawCaptureSources(t.Context(), provider)
	require.NoError(t, err)
	require.Len(t, discovery.Sources, 1)
	plan, supported, err := parser.ResolveRawCapturePlan(t.Context(), provider, discovery.Sources[0])
	require.NoError(t, err)
	require.True(t, supported)
	assert.Contains(t, plan.Entries, parser.RawCaptureEntry{Path: "alias-homes/case/session_index.jsonl", LocalPath: filepath.Join(label, parser.CodexSessionIndexFilename)})
	assert.Contains(t, plan.Entries, parser.RawCaptureEntry{Path: "alias-homes/case/.codex-run-v1-test/session_index.jsonl", LocalPath: filepath.Join(home, parser.CodexSessionIndexFilename)})
	manifest, objects := manifestFromCapturePlan(t, parser.AgentCodex, provider, discovery.Sources[0])
	materialized, err := (Materializer{Store: &materializerStore{objects: objects}, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}).Materialize(t.Context(), manifest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })
	roots := materializedProviderRoots(manifest, materialized)
	metadata := materializedProviderMetadataDirs(manifest, materialized, roots)
	for _, dirs := range metadata {
		assert.Equal(t, []string{filepath.Join(materialized.Root(), "alias-homes", "case", ".codex-run-v1-test"), filepath.Join(materialized.Root(), "alias-homes", "case")}, dirs)
	}
	assertCodexReplayMatchesDirect(t, provider, discovery.Sources[0], "alias-homes/case")
}

func assertCodexReplayMatchesDirect(t *testing.T, provider parser.Provider, source parser.SourceRef, wantProject string) {
	t.Helper()
	direct, err := provider.Parse(t.Context(), parser.ParseRequest{Source: source, ForceParse: true})
	require.NoError(t, err)
	require.Len(t, direct.Results, 1)
	assert.Equal(t, wantProject, direct.Results[0].Result.Session.Project)
	manifest, objects := manifestFromCapturePlan(t, parser.AgentCodex, provider, source)
	materialized, err := (Materializer{Store: &materializerStore{objects: objects}, BaseDir: t.TempDir(), MaxTotalBytes: 1 << 20}).Materialize(t.Context(), manifest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, materialized.Cleanup()) })
	dispatch, err := NewProviderParser(parser.ProviderFactories(), "hosted-worker")
	require.NoError(t, err)
	replayed, err := dispatch.Parse(t.Context(), manifest, materialized)
	require.NoError(t, err)
	require.Len(t, replayed.Outcome.Results, 1)
	want, got := direct.Results[0].Result.Session, replayed.Outcome.Results[0].Result.Session
	assert.Equal(t, want.Project, got.Project)
	assert.Equal(t, want.SessionKind, got.SessionKind)
	assert.Equal(t, want.ParentSessionID, got.ParentSessionID)
	assert.Equal(t, want.RelationshipType, got.RelationshipType)
	assert.Equal(t, want.SessionName, got.SessionName)
	assert.Equal(t, want.File.Mtime, got.File.Mtime)
}
