package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTauProviderDefinitionAndLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	project := filepath.Join(root, "project-a")
	require.NoError(os.MkdirAll(project, 0o755))
	valid := filepath.Join(project, "abc.def-ghi_jkl.jsonl")
	writeSourceFile(t, valid, `{"type":"session_info"}`+"\n")
	writeSourceFile(t, filepath.Join(project, "index.jsonl"), `{"title":"metadata"}`+"\n")
	writeSourceFile(t, filepath.Join(project, "session_index.jsonl"), `{"type":"session_info"}`+"\n")
	writeSourceFile(t, filepath.Join(root, "root.jsonl"), `{"type":"session_info"}`+"\n")
	writeSourceFile(t, filepath.Join(project, "nested", "deep.jsonl"), `{"type":"session_info"}`+"\n")

	provider, ok := NewProvider(AgentTau, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	def := provider.Definition()
	assert.Equal(AgentTau, def.Type)
	assert.Equal("Tau", def.DisplayName)
	assert.Equal("TAU_SESSIONS_DIR", def.EnvVar)
	assert.Equal("tau_dirs", def.ConfigKey)
	assert.Equal([]string{".tau/sessions"}, def.DefaultDirs)
	assert.Equal("tau:", def.IDPrefix)
	assert.True(def.FileBased)
	assert.False(def.RemoteSyncExcluded)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 2)
	assert.ElementsMatch([]string{valid, filepath.Join(project, "session_index.jsonl")}, sourceDisplayPaths(sources))
	assert.NotContains(sourceDisplayPaths(sources), filepath.Join(project, "index.jsonl"))
	assert.NotContains(sourceDisplayPaths(sources), filepath.Join(root, "root.jsonl"))
	assert.NotContains(sourceDisplayPaths(sources), filepath.Join(project, "nested", "deep.jsonl"))
	assert.Empty(sources[0].ProjectHint)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(root, plan.Roots[0].Path)
	assert.True(plan.Roots[0].Recursive)
	assert.Equal([]string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "abc.def-ghi_jkl",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(valid, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(err)
	info, err := os.Stat(valid)
	require.NoError(err)
	assert.Equal(int64(info.Size()), fingerprint.Size)
	assert.Equal(info.ModTime().UnixNano(), fingerprint.MTimeNS)
	assert.Empty(fingerprint.Hash)
}

func TestTauProviderDefaultIDsStayProjectSpecific(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	for _, project := range []string{"one", "two"} {
		dir := filepath.Join(root, project)
		require.NoError(os.MkdirAll(dir, 0o755))
		writeSourceFile(t, filepath.Join(dir, "default.jsonl"), `{"type":"session_info"}`+"\n")
	}
	provider, ok := NewProvider(AgentTau, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 2)
	foundIDs := make([]string, 0, len(sources))
	for _, source := range sources {
		found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
			RawSessionID: tauSessionIDFromPath(root, source.DisplayPath),
		})
		require.NoError(err)
		require.True(ok)
		foundIDs = append(foundIDs, tauSessionIDFromPath(root, found.DisplayPath))
	}
	assert.Len(foundIDs, 2)
	assert.NotEqual(foundIDs[0], foundIDs[1])
}

func TestTauProviderDefaultIDsStayRootSpecific(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	base := t.TempDir()
	roots := []string{
		filepath.Join(base, "first-root"),
		filepath.Join(base, "second-root"),
	}
	for _, root := range roots {
		project := filepath.Join(root, "shared-project")
		require.NoError(os.MkdirAll(project, 0o755))
		writeSourceFile(t, filepath.Join(project, "default.jsonl"), `{"type":"session_info"}`+"\n")
	}

	provider, ok := NewProvider(AgentTau, ProviderConfig{Roots: roots})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 2)

	foundIDs := make([]string, 0, len(sources))
	for _, source := range sources {
		id := tauSessionIDFromPath(source.Opaque.(JSONLSource).Root, source.DisplayPath)
		found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
			RawSessionID: id,
		})
		require.NoError(err)
		require.True(ok)
		assert.Equal(source.DisplayPath, found.DisplayPath)
		foundIDs = append(foundIDs, id)
	}
	assert.Len(map[string]struct{}{foundIDs[0]: {}, foundIDs[1]: {}}, 2)
}

func TestTauProviderDefaultIDsUseCanonicalRoot(t *testing.T) {
	require := require.New(t)

	base := t.TempDir()
	roots := []string{
		filepath.Join(base, "first-root"),
		filepath.Join(base, "second-root"),
	}
	for _, root := range roots {
		project := filepath.Join(root, "shared project")
		require.NoError(os.MkdirAll(project, 0o755))
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
		require.True(ok)
		sources, err := provider.Discover(t.Context())
		require.NoError(err)
		require.Len(sources, 1)
		outcome, err := provider.Parse(t.Context(), ParseRequest{
			Source: sources[0],
		})
		require.NoError(err)
		require.Len(outcome.Results, 1)
		ids = append(ids, outcome.Results[0].Result.Session.ID)
	}
	assert.Equal(t, ids[0], ids[1])
}

func TestTauProviderKeepsPiTranscriptWithPi(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(os.MkdirAll(project, 0o755))
	path := filepath.Join(project, "pi-session.jsonl")
	writeSourceFile(t, path, `{"type":"session","id":"pi-session","cwd":"/tmp/project"}`+"\n")

	tauProvider, ok := NewProvider(AgentTau, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	tauSources, err := tauProvider.Discover(t.Context())
	require.NoError(err)
	require.Len(tauSources, 1)
	tauOutcome, err := tauProvider.Parse(t.Context(), ParseRequest{Source: tauSources[0]})
	require.NoError(err)
	assert.Equal(SkipNoSession, tauOutcome.SkipReason)

	piProvider, ok := NewProvider(AgentPi, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	piSources, err := piProvider.Discover(t.Context())
	require.NoError(err)
	assert.Len(piSources, 1)
}

func TestTauProviderRejectsForeignMessageTranscript(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(os.MkdirAll(project, 0o755))
	writeSourceFile(t, filepath.Join(project, "foreign.jsonl"),
		`{"type":"message","id":"u1","role":"user","content":"hello"}`+"\n"+
			`{"type":"message","id":"a1","role":"assistant","content":"hi"}`+"\n",
	)

	provider, ok := NewProvider(AgentTau, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(err)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
}
