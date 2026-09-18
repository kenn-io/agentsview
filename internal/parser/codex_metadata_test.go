package parser

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// aliasedCodexHomes builds a primary Codex home and a second home whose
// sessions directory and history.jsonl link to the primary, while keeping
// its own session_index.jsonl. This mirrors running two Codex instances
// against one transcript store.
func aliasedCodexHomes(t *testing.T) (primary, alias string) {
	t.Helper()
	base := t.TempDir()
	primary = filepath.Join(base, "codex")
	alias = filepath.Join(base, "codex-alt")
	require.NoError(t, os.MkdirAll(filepath.Join(primary, "sessions"), 0o755))
	require.NoError(t, os.MkdirAll(alias, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(primary, "history.jsonl"), nil, 0o600))
	require.NoError(t, os.Symlink(
		filepath.Join(primary, "sessions"), filepath.Join(alias, "sessions")))
	require.NoError(t, os.Symlink(
		filepath.Join(primary, "history.jsonl"), filepath.Join(alias, "history.jsonl")))
	return primary, alias
}

func writeIndex(t *testing.T, home string, lines string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(home, CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(path, []byte(lines), 0o600))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	EvictCodexSessionIndex(path)
}

func TestCodexThreadNameReadsAliasHomeIndex(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	primary, alias := aliasedCodexHomes(t)
	metadata := CodexMetadata{roots: map[string][]string{filepath.Join(primary, "sessions"): {primary, alias}}}
	session := filepath.Join(primary, "sessions", "2026", "09", "03",
		"rollout-2026-09-03T10-00-00-019f0000-0000-7000-8000-000000000001.jsonl")
	now := time.Now()
	writeIndex(t, primary,
		`{"id":"019f0000-0000-7000-8000-000000000001","thread_name":"from primary"}`+"\n"+
			`{"id":"019f0000-0000-7000-8000-000000000002","thread_name":"primary only"}`+"\n",
		now.Add(-time.Minute))
	writeIndex(t, alias,
		`{"id":"019f0000-0000-7000-8000-000000000001","thread_name":"from alias"}`+"\n"+
			`{"id":"019f0000-0000-7000-8000-000000000003","thread_name":"alias only"}`+"\n",
		now)

	name, ok, err := metadata.ReadThreadName(session, "019f0000-0000-7000-8000-000000000003")
	require.NoError(err)
	assert.True(ok)
	assert.Equal("alias only", name)

	name, ok, err = metadata.ReadThreadName(session, "019f0000-0000-7000-8000-000000000002")
	require.NoError(err)
	assert.True(ok)
	assert.Equal("primary only", name)

	// Both homes name the same session: the newer index wins.
	name, ok, err = metadata.ReadThreadName(session, "019f0000-0000-7000-8000-000000000001")
	require.NoError(err)
	assert.True(ok)
	assert.Equal("from alias", name)

	writeIndex(t, primary,
		`{"id":"019f0000-0000-7000-8000-000000000001","thread_name":"renamed in primary"}`+"\n",
		now.Add(time.Minute))
	name, ok, err = metadata.ReadThreadName(session, "019f0000-0000-7000-8000-000000000001")
	require.NoError(err)
	assert.True(ok)
	assert.Equal("renamed in primary", name)
	assert.Equal(now.Add(time.Minute).UnixNano(), metadata.EffectiveMtime(session, 0))
}

func TestCodexThreadNameAliasOnlyIndex(t *testing.T) {
	assert := assert.New(t)

	primary, alias := aliasedCodexHomes(t)
	metadata := CodexMetadata{roots: map[string][]string{filepath.Join(primary, "sessions"): {primary, alias}}}
	session := filepath.Join(primary, "sessions", "2026", "09", "03",
		"rollout-2026-09-03T10-00-00-019f0000-0000-7000-8000-000000000001.jsonl")
	writeIndex(t, alias,
		`{"id":"019f0000-0000-7000-8000-000000000001","thread_name":"alias title"}`+"\n",
		time.Now())

	name, ok, err := metadata.ReadThreadName(session, "019f0000-0000-7000-8000-000000000001")
	require.NoError(t, err)
	assert.True(ok)
	assert.Equal("alias title", name)
	assert.NoError(metadata.Verify(session))
}

func TestCodexAliasHomesShareHintsAndWatchIndexes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	primary, alias := aliasedCodexHomes(t)
	metadata := CodexMetadata{roots: map[string][]string{filepath.Join(primary, "sessions"): {primary, alias}}}
	root := filepath.Join(primary, "sessions")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}, MetadataDirs: metadata.roots})
	require.True(ok)

	// history.jsonl is one file reached through two homes: read it once.
	sources, err := provider.(ActivityHintProvider).ActivityHintSources(t.Context())
	require.NoError(err)
	assert.Equal([]ActivityHintSource{{
		Path: filepath.Join(primary, "history.jsonl"),
	}}, sources)

	// A home with its own hint log contributes a second source.
	require.NoError(os.Remove(filepath.Join(alias, "history.jsonl")))
	require.NoError(os.WriteFile(filepath.Join(alias, "history.jsonl"), nil, 0o600))
	sources, err = provider.(ActivityHintProvider).ActivityHintSources(t.Context())
	require.NoError(err)
	assert.Equal([]ActivityHintSource{
		{Path: filepath.Join(primary, "history.jsonl")},
		{Path: filepath.Join(alias, "history.jsonl")},
	}, sources)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	var shallow []string
	for _, watch := range plan.Roots {
		if !watch.Recursive {
			shallow = append(shallow, watch.Path)
		}
	}
	assert.ElementsMatch([]string{primary, alias}, shallow)
}

func TestCodexRawCaptureIncludesAliasHomeIndexes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	primary, alias := aliasedCodexHomes(t)
	metadata := CodexMetadata{roots: map[string][]string{filepath.Join(primary, "sessions"): {primary, alias}}}
	const id = "019f0000-0000-7000-8000-000000000009"
	rollout := filepath.Join(primary, "sessions", "2026", "09", "03",
		"rollout-2026-09-03T10-00-00-"+id+".jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(rollout), 0o755))
	require.NoError(os.WriteFile(rollout,
		[]byte(`{"timestamp":"2026-09-03T10:00:00Z","type":"session_meta","payload":{"id":"`+id+`","cwd":"/work"}}`+"\n"),
		0o600))
	writeIndex(t, primary, `{"id":"`+id+`","thread_name":"primary"}`+"\n", time.Now())
	writeIndex(t, alias, `{"id":"`+id+`","thread_name":"alias"}`+"\n", time.Now())

	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots: []string{filepath.Join(primary, "sessions")}, MetadataDirs: metadata.roots,
	})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, id)

	plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, source)
	require.NoError(err)
	require.True(supported)
	byLogical := make(map[string]string, len(plan.Entries))
	for _, entry := range plan.Entries {
		byLogical[entry.Path] = entry.LocalPath
	}
	assert.ElementsMatch([]string{
		"sessions/2026/09/03/rollout-2026-09-03T10-00-00-" + id + ".jsonl",
		CodexSessionIndexFilename,
		"alias-homes/1/" + CodexSessionIndexFilename,
	}, slices.Collect(maps.Keys(byLogical)))
	// Validation resolves symlinks in local paths, so compare resolved forms.
	wantAliasIndex, err := filepath.EvalSymlinks(filepath.Join(alias, CodexSessionIndexFilename))
	require.NoError(err)
	assert.Equal(wantAliasIndex, byLogical["alias-homes/1/"+CodexSessionIndexFilename])
	require.Len(plan.SidecarRoots, 1)
	gotRoot, err := filepath.EvalSymlinks(plan.SidecarRoots[0])
	require.NoError(err)
	assert.Equal(filepath.Dir(wantAliasIndex), gotRoot)
}

func TestCodexProvidersKeepIndependentMetadata(t *testing.T) {
	require := require.New(t)

	primary, alternate := aliasedCodexHomes(t)
	const id = "019f0000-0000-7000-8000-000000000009"
	root := filepath.Join(primary, "sessions")
	path := filepath.Join(root, "rollout-2026-09-03T10-00-00-"+id+".jsonl")
	require.NoError(os.WriteFile(path, []byte(
		`{"timestamp":"2026-09-03T10:00:00Z","type":"session_meta","payload":{"id":"`+id+`","cwd":"/work"}}`+"\n"+
			`{"timestamp":"2026-09-03T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Original prompt"}]}}`+"\n"), 0o600))
	writeIndex(t, primary, `{"id":"`+id+`","thread_name":"Primary title"}`+"\n", time.Now())
	writeIndex(t, alternate, `{"id":"`+id+`","thread_name":"Alternate title"}`+"\n", time.Now())
	first, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}, MetadataDirs: map[string][]string{root: {primary}}})
	require.True(ok)
	second, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}, MetadataDirs: map[string][]string{root: {alternate}}})
	require.True(ok)
	for _, tc := range []struct {
		provider Provider
		title    string
	}{{first, "Primary title"}, {second, "Alternate title"}, {first, "Primary title"}} {
		source := requireCodexProviderSource(t, tc.provider, id)
		result, err := tc.provider.Parse(t.Context(), ParseRequest{Source: source})
		require.NoError(err)
		require.Len(result.Results, 1)
		assert.Equal(t, tc.title, result.Results[0].Result.Session.SessionName)
	}
}
