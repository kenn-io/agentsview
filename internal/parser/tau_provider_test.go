package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTauProviderDefinitionAndLifecycle(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(project, 0o755))
	valid := filepath.Join(project, "abc.def-ghi_jkl.jsonl")
	writeSourceFile(t, valid, `{"type":"session_info"}`+"\n")
	writeSourceFile(t, filepath.Join(project, "index.jsonl"), `{"title":"metadata"}`+"\n")
	writeSourceFile(t, filepath.Join(project, "session_index.jsonl"), `{"type":"session_info"}`+"\n")
	writeSourceFile(t, filepath.Join(root, "root.jsonl"), `{"type":"session_info"}`+"\n")
	writeSourceFile(t, filepath.Join(project, "nested", "deep.jsonl"), `{"type":"session_info"}`+"\n")

	provider, ok := NewProvider(AgentTau, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	def := provider.Definition()
	assert.Equal(t, AgentTau, def.Type)
	assert.Equal(t, "Tau", def.DisplayName)
	assert.Equal(t, "TAU_SESSIONS_DIR", def.EnvVar)
	assert.Equal(t, "tau_dirs", def.ConfigKey)
	assert.Equal(t, []string{".tau/sessions"}, def.DefaultDirs)
	assert.Equal(t, "tau:", def.IDPrefix)
	assert.True(t, def.FileBased)
	assert.False(t, def.RemoteSyncExcluded)

	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.ElementsMatch(t, []string{valid, filepath.Join(project, "session_index.jsonl")}, sourceDisplayPaths(sources))
	assert.NotContains(t, sourceDisplayPaths(sources), filepath.Join(project, "index.jsonl"))
	assert.NotContains(t, sourceDisplayPaths(sources), filepath.Join(root, "root.jsonl"))
	assert.NotContains(t, sourceDisplayPaths(sources), filepath.Join(project, "nested", "deep.jsonl"))
	assert.Empty(t, sources[0].ProjectHint)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "abc.def-ghi_jkl",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, valid, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	info, err := os.Stat(valid)
	require.NoError(t, err)
	assert.Equal(t, int64(info.Size()), fingerprint.Size)
	assert.Equal(t, info.ModTime().UnixNano(), fingerprint.MTimeNS)
	assert.Empty(t, fingerprint.Hash)
}

func TestTauProviderDefaultIDsStayProjectSpecific(t *testing.T) {
	root := t.TempDir()
	for _, project := range []string{"one", "two"} {
		dir := filepath.Join(root, project)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		writeSourceFile(t, filepath.Join(dir, "default.jsonl"), `{"type":"session_info"}`+"\n")
	}
	provider, ok := NewProvider(AgentTau, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	foundIDs := make([]string, 0, len(sources))
	for _, source := range sources {
		found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
			RawSessionID: tauSessionIDFromPath(root, source.DisplayPath),
		})
		require.NoError(t, err)
		require.True(t, ok)
		foundIDs = append(foundIDs, tauSessionIDFromPath(root, found.DisplayPath))
	}
	assert.Len(t, foundIDs, 2)
	assert.NotEqual(t, foundIDs[0], foundIDs[1])
}

func TestTauProviderDefaultIDsStayRootSpecific(t *testing.T) {
	base := t.TempDir()
	roots := []string{
		filepath.Join(base, "first-root"),
		filepath.Join(base, "second-root"),
	}
	for _, root := range roots {
		project := filepath.Join(root, "shared-project")
		require.NoError(t, os.MkdirAll(project, 0o755))
		writeSourceFile(t, filepath.Join(project, "default.jsonl"), `{"type":"session_info"}`+"\n")
	}

	provider, ok := NewProvider(AgentTau, ProviderConfig{Roots: roots})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)

	foundIDs := make([]string, 0, len(sources))
	for _, source := range sources {
		id := tauSessionIDFromPath(source.Opaque.(JSONLSource).Root, source.DisplayPath)
		found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
			RawSessionID: id,
		})
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, source.DisplayPath, found.DisplayPath)
		foundIDs = append(foundIDs, id)
	}
	assert.Len(t, map[string]struct{}{foundIDs[0]: {}, foundIDs[1]: {}}, 2)
}

func TestTauProviderDefaultIDsUseCanonicalRoot(t *testing.T) {
	base := t.TempDir()
	roots := []string{
		filepath.Join(base, "first-root"),
		filepath.Join(base, "second-root"),
	}
	for _, root := range roots {
		project := filepath.Join(root, "shared project")
		require.NoError(t, os.MkdirAll(project, 0o755))
		writeSourceFile(t, filepath.Join(project, "default.jsonl"), `{"type":"session_info"}`+"\n")
	}
	const canonicalRoot = "/remote/.tau/sessions"
	rewriter := func(path string) string {
		for _, root := range roots {
			if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
				return canonicalRoot + strings.TrimPrefix(path, root)
			}
		}
		return path
	}

	ids := make([]string, 0, len(roots))
	for _, root := range roots {
		provider, ok := NewProvider(AgentTau, ProviderConfig{
			Roots: []string{root}, PathRewriter: rewriter,
		})
		require.True(t, ok)
		sources, err := provider.Discover(context.Background())
		require.NoError(t, err)
		require.Len(t, sources, 1)
		outcome, err := provider.Parse(context.Background(), ParseRequest{
			Source: sources[0],
		})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		ids = append(ids, outcome.Results[0].Result.Session.ID)
	}
	assert.Equal(t, ids[0], ids[1])
}

func TestTauProviderKeepsPiTranscriptWithPi(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	path := filepath.Join(project, "pi-session.jsonl")
	writeSourceFile(t, path, `{"type":"session","id":"pi-session","cwd":"/tmp/project"}`+"\n")

	tauProvider, ok := NewProvider(AgentTau, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	tauSources, err := tauProvider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, tauSources, 1)
	tauOutcome, err := tauProvider.Parse(context.Background(), ParseRequest{Source: tauSources[0]})
	require.NoError(t, err)
	assert.Equal(t, SkipNoSession, tauOutcome.SkipReason)

	piProvider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	piSources, err := piProvider.Discover(context.Background())
	require.NoError(t, err)
	assert.Len(t, piSources, 1)
}

func TestTauProviderRejectsForeignMessageTranscript(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	writeSourceFile(t, filepath.Join(project, "foreign.jsonl"),
		`{"type":"message","id":"u1","role":"user","content":"hello"}`+"\n"+
			`{"type":"message","id":"a1","role":"assistant","content":"hi"}`+"\n",
	)

	provider, ok := NewProvider(AgentTau, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
}
