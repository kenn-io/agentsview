package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

type sourceRankCountingFactory struct {
	parser.ProviderFactory
	lookups *int
}

func (f sourceRankCountingFactory) NewProvider(cfg parser.ProviderConfig) parser.Provider {
	return &sourceRankCountingProvider{Provider: f.ProviderFactory.NewProvider(cfg), lookups: f.lookups}
}

type sourceRankCountingProvider struct {
	parser.Provider
	lookups *int
}

func (p *sourceRankCountingProvider) DiscoverEach(ctx context.Context, yield func(parser.SourceRef) error) error {
	return p.Provider.(parser.StreamingDiscoverer).DiscoverEach(ctx, yield)
}

func (p *sourceRankCountingProvider) SourceForReconciliation(
	ctx context.Context, path, project string,
) (parser.SourceRef, bool, error) {
	*p.lookups++
	return p.Provider.(parser.ReconciliationSourceResolver).SourceForReconciliation(ctx, path, project)
}

func (p *sourceRankCountingProvider) ReconciliationSourceRank(source parser.SourceRef) parser.ReconciliationSourceRank {
	return p.Provider.(parser.ReconciliationSourceRanker).ReconciliationSourceRank(source)
}

func writeSourceRankOpenClawSession(t *testing.T, root, id, content string) string {
	t.Helper()
	path := filepath.Join(root, "main", "sessions", id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	data := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-09-22T10:00:00Z","cwd":"/workspace/project-a"}
{"type":"message","id":"m1","timestamp":"2026-09-22T10:00:01Z","message":{"role":"user","content":%q,"timestamp":"2026-09-22T10:00:01Z"}}
`, id, content)
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
	return path
}

func TestOpenClawUnchangedSourcesSkipStoredRankLookup(t *testing.T) {
	for _, count := range []int{1, 64} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			root := t.TempDir()
			for i := range count {
				path := writeSourceRankOpenClawSession(t, root, fmt.Sprintf("session-%d", i), "saved question")
				require.NoError(t, os.Rename(path, path+".deleted.2026-09-22T10-00-00.000Z"))
			}
			database := openTestDB(t)
			factory, ok := parser.ProviderFactoryByType(parser.AgentOpenClaw)
			require.True(t, ok)
			lookups := 0
			cfg := EngineConfig{
				AgentDirs:         map[parser.AgentType][]string{parser.AgentOpenClaw: {root}},
				Machine:           "local",
				ProviderFactories: []parser.ProviderFactory{sourceRankCountingFactory{factory, &lookups}},
			}
			engine := NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			require.Equal(t, count, engine.SyncAll(t.Context(), nil).Synced)

			// Exercise both the warm engine and the persisted freshness gates.
			fresh := NewEngine(t.Context(), database, cfg)
			t.Cleanup(fresh.Close)
			provider := factory.NewProvider(parser.ProviderConfig{Roots: []string{root}})
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, count)
			for _, current := range []*Engine{engine, fresh} {
				lookups = 0
				for _, source := range sources {
					result, used := current.processProviderFile(t.Context(), parser.DiscoveredFile{
						Agent: parser.AgentOpenClaw, Path: source.DisplayPath,
						ProviderSource: &source, ProviderProcess: true,
					})
					require.True(t, used)
					require.NoError(t, result.err)
					assert.True(t, result.skip)
				}
				assert.Zero(t, lookups, "unchanged sources must not resolve the stored source again")
			}
		})
	}
}

func TestStoredSourceRankUsesPhysicalPathsWhenAvailable(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	stored := writeSourceRankOpenClawSession(t, root, "duplicate", "preferred question")
	candidatePath := writeSourceRankOpenClawSession(t, other, "duplicate", "older question")
	newer := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Hour)
	require.NoError(t, os.Chtimes(stored, newer, newer))
	require.NoError(t, os.Chtimes(candidatePath, older, older))
	provider, ok := parser.NewProvider(parser.AgentOpenClaw, parser.ProviderConfig{Roots: []string{root, other}})
	require.True(t, ok)
	candidate, found, err := provider.FindSource(t.Context(), parser.FindSourceRequest{
		StoredFilePath: candidatePath, PreferStoredSource: true,
	})
	require.NoError(t, err)
	require.True(t, found)
	for _, tc := range []struct {
		name       string
		remote     bool
		resolver   bool
		mappedPath string
		wantRanked bool
		wantCalls  int
	}{
		{name: "local resolver is unused", resolver: true, wantRanked: true},
		{name: "remote without resolver", remote: true},
		{name: "remote missing mapping", remote: true, resolver: true, wantCalls: 1},
		{name: "remote mapped", remote: true, resolver: true, mappedPath: stored, wantRanked: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &Engine{}
			storedPath := stored
			if tc.remote {
				engine.pathRewriter = func(string) string { return "host:/sessions/duplicate.jsonl" }
				storedPath = "host:/sessions/duplicate.jsonl"
			}
			calls := 0
			if tc.resolver {
				engine.storedPathResolver = func(path string) (string, bool) {
					calls++
					assert.Equal(t, storedPath, path)
					return tc.mappedPath, tc.mappedPath != ""
				}
			}
			lookups := 0
			counting := &sourceRankCountingProvider{Provider: provider, lookups: &lookups}
			ranked, err := engine.storedSourceRanksHigher(t.Context(), counting, storedPath, candidate)
			require.NoError(t, err)
			assert.Equal(t, tc.wantRanked, ranked)
			assert.Equal(t, tc.wantCalls, calls)
			if tc.wantRanked {
				assert.Equal(t, 1, lookups)
			} else {
				assert.Zero(t, lookups, "unmapped stored paths must not reach the local provider")
			}
		})
	}
}

func TestReconciliationMissingMemberSkipsStoredRankLookup(t *testing.T) {
	root := t.TempDir()
	stored := writeSourceRankOpenClawSession(t, root, "missing", "saved question")
	provider, ok := parser.NewProvider(parser.AgentOpenClaw, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	require.NoError(t, os.Remove(stored))
	lookups := 0
	counting := &sourceRankCountingProvider{Provider: provider, lookups: &lookups}
	engine := &Engine{}
	relocated, err := engine.reconciliationMemberRelocated(t.Context(), counting, "openclaw:main:missing", stored)
	require.NoError(t, err)
	assert.False(t, relocated)
	assert.Zero(t, lookups, "a missing candidate needs no stored-source rank")
}
