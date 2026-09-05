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
	SetCodexRootAliases(map[string][]string{
		filepath.Join(primary, "sessions"): {filepath.Join(alias, "sessions")},
	})
	t.Cleanup(func() { SetCodexRootAliases(nil) })
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
	primary, alias := aliasedCodexHomes(t)
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

	name, ok := LookupCodexThreadNameEntry(session, "019f0000-0000-7000-8000-000000000003")
	assert.True(t, ok)
	assert.Equal(t, "alias only", name)

	name, ok = LookupCodexThreadNameEntry(session, "019f0000-0000-7000-8000-000000000002")
	assert.True(t, ok)
	assert.Equal(t, "primary only", name)

	// Both homes name the same session: the newer index wins.
	name, ok = LookupCodexThreadNameEntry(session, "019f0000-0000-7000-8000-000000000001")
	assert.True(t, ok)
	assert.Equal(t, "from alias", name)

	writeIndex(t, primary,
		`{"id":"019f0000-0000-7000-8000-000000000001","thread_name":"renamed in primary"}`+"\n",
		now.Add(time.Minute))
	name, ok = LookupCodexThreadNameEntry(session, "019f0000-0000-7000-8000-000000000001")
	assert.True(t, ok)
	assert.Equal(t, "renamed in primary", name)
	assert.Equal(t, now.Add(time.Minute).UnixNano(), CodexEffectiveMtime(session, 0))
}

func TestCodexThreadNameAliasOnlyIndex(t *testing.T) {
	primary, alias := aliasedCodexHomes(t)
	session := filepath.Join(primary, "sessions", "2026", "09", "03",
		"rollout-2026-09-03T10-00-00-019f0000-0000-7000-8000-000000000001.jsonl")
	writeIndex(t, alias,
		`{"id":"019f0000-0000-7000-8000-000000000001","thread_name":"alias title"}`+"\n",
		time.Now())

	name, ok := LookupCodexThreadNameEntry(session, "019f0000-0000-7000-8000-000000000001")
	assert.True(t, ok)
	assert.Equal(t, "alias title", name)
	assert.NoError(t, VerifyCodexSessionIndex(session))
}

func TestCodexAliasHomesShareHintsAndWatchIndexes(t *testing.T) {
	primary, alias := aliasedCodexHomes(t)
	root := filepath.Join(primary, "sessions")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	// history.jsonl is one file reached through two homes: read it once.
	sources, err := provider.(ActivityHintProvider).ActivityHintSources(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []ActivityHintSource{{
		Path: filepath.Join(primary, "history.jsonl"),
	}}, sources)

	// A home with its own hint log contributes a second source.
	require.NoError(t, os.Remove(filepath.Join(alias, "history.jsonl")))
	require.NoError(t, os.WriteFile(filepath.Join(alias, "history.jsonl"), nil, 0o600))
	sources, err = provider.(ActivityHintProvider).ActivityHintSources(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []ActivityHintSource{
		{Path: filepath.Join(primary, "history.jsonl")},
		{Path: filepath.Join(alias, "history.jsonl")},
	}, sources)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(t, err)
	var shallow []string
	for _, watch := range plan.Roots {
		if !watch.Recursive {
			shallow = append(shallow, watch.Path)
		}
	}
	assert.ElementsMatch(t, []string{primary, alias}, shallow)
}

func TestCodexRawCaptureIncludesAliasHomeIndexes(t *testing.T) {
	primary, alias := aliasedCodexHomes(t)
	const id = "019f0000-0000-7000-8000-000000000009"
	rollout := filepath.Join(primary, "sessions", "2026", "09", "03",
		"rollout-2026-09-03T10-00-00-"+id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(rollout), 0o755))
	require.NoError(t, os.WriteFile(rollout,
		[]byte(`{"timestamp":"2026-09-03T10:00:00Z","type":"session_meta","payload":{"id":"`+id+`","cwd":"/work"}}`+"\n"),
		0o600))
	writeIndex(t, primary, `{"id":"`+id+`","thread_name":"primary"}`+"\n", time.Now())
	writeIndex(t, alias, `{"id":"`+id+`","thread_name":"alias"}`+"\n", time.Now())

	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots: []string{filepath.Join(primary, "sessions")},
	})
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, id)

	plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, source)
	require.NoError(t, err)
	require.True(t, supported)
	byLogical := make(map[string]string, len(plan.Entries))
	for _, entry := range plan.Entries {
		byLogical[entry.Path] = entry.LocalPath
	}
	assert.ElementsMatch(t, []string{
		"sessions/2026/09/03/rollout-2026-09-03T10-00-00-" + id + ".jsonl",
		CodexSessionIndexFilename,
		"alias-homes/1/" + CodexSessionIndexFilename,
	}, slices.Collect(maps.Keys(byLogical)))
	// Validation resolves symlinks in local paths, so compare resolved forms.
	wantAliasIndex, err := filepath.EvalSymlinks(filepath.Join(alias, CodexSessionIndexFilename))
	require.NoError(t, err)
	assert.Equal(t, wantAliasIndex, byLogical["alias-homes/1/"+CodexSessionIndexFilename])
	require.Len(t, plan.SidecarRoots, 1)
	gotRoot, err := filepath.EvalSymlinks(plan.SidecarRoots[0])
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(wantAliasIndex), gotRoot)
}
