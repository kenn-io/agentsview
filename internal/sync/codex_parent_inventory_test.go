package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Serialize real parses and restore a page after the first fork warms the batch.
// The fixture controls only the filesystem change, never the parser outcome.
type codexPageRestoreFactory struct {
	parser.ProviderFactory
	mu        stdsync.Mutex
	restore   func() error
	parses    int
	forkPaths map[string]bool
}

type codexPageRestoreProvider struct {
	parser.Provider
	factory *codexPageRestoreFactory
}

func (f *codexPageRestoreFactory) NewProvider(cfg parser.ProviderConfig) parser.Provider {
	return &codexPageRestoreProvider{Provider: f.ProviderFactory.NewProvider(cfg), factory: f}
}

func (p *codexPageRestoreProvider) Parse(ctx context.Context, req parser.ParseRequest) (parser.ParseOutcome, error) {
	p.factory.mu.Lock()
	defer p.factory.mu.Unlock()
	out, err := p.Provider.Parse(ctx, req)
	if err != nil || !p.factory.forkPaths[req.Source.DisplayPath] {
		return out, err
	}
	p.factory.parses++
	if p.factory.restore != nil {
		err = p.factory.restore()
		p.factory.restore = nil
	}
	return out, err
}

func (p *codexPageRestoreProvider) DiscoverEach(ctx context.Context, yield func(parser.SourceRef) error) error {
	return p.Provider.(parser.StreamingDiscoverer).DiscoverEach(ctx, yield)
}

func TestCodexParentInventoryBatchRetriesUnchangedForks(t *testing.T) {
	const parent = "11111111-1111-4111-8111-111111111111"
	const page = "44444444-4444-4444-8444-444444444444"
	const timestamp = "2026-09-01T10:00:00Z"
	for _, route := range []string{"changed plan", "changed paths", "full sync", "reconciliation"} {
		t.Run(route, func(t *testing.T) {
			root := t.TempDir()
			parentPath := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+parent+".jsonl")
			require.NoError(t, os.WriteFile(parentPath, []byte(testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", timestamp),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "head-turn", timestamp),
			)), 0o600))
			pagePath := filepath.Join(root, "rollout-2026-09-01T10-01-00-"+parent+"_"+page+".jsonl")
			pageContent := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", timestamp),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "page-turn", timestamp),
			)
			delegate, ok := parser.ProviderFactoryByType(parser.AgentCodex)
			require.True(t, ok)
			factory := &codexPageRestoreFactory{ProviderFactory: delegate, forkPaths: make(map[string]bool), restore: func() error {
				return os.WriteFile(pagePath, []byte(pageContent), 0o600)
			}}
			forks := []string{"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"}
			var files []parser.DiscoveredFile
			var paths []string
			for _, fork := range forks {
				path := filepath.Join(root, "rollout-2026-09-01T11-00-00-"+fork+".jsonl")
				require.NoError(t, os.WriteFile(path, []byte(testjsonl.JoinJSONL(
					testjsonl.CodexForkedSessionMetaJSON(fork, parent, "/work/project", "user", timestamp),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "page-turn", timestamp),
					testjsonl.CodexMsgJSON("assistant", "copied page", timestamp),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "new-turn", timestamp),
					testjsonl.CodexMsgJSON("assistant", "new answer", timestamp),
				)), 0o600))
				source, found, err := delegate.NewProvider(parser.ProviderConfig{Roots: []string{root}}).FindSource(t.Context(), parser.FindSourceRequest{RawSessionID: fork})
				require.NoError(t, err)
				require.True(t, found)
				files = append(files, parser.DiscoveredFile{Path: path, Agent: parser.AgentCodex, ProviderSource: &source, ProviderProcess: true})
				paths = append(paths, path)
				factory.forkPaths[path] = true
			}
			database := openTestDB(t)
			config := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", ProviderFactories: []parser.ProviderFactory{factory}, DisableFilesystemProjectDiscovery: true}
			engine := NewEngine(t.Context(), database, config)
			t.Cleanup(engine.Close)
			run := func() (SyncStats, error) {
				if route == "full sync" {
					stats := engine.SyncAll(t.Context(), nil)
					if !stats.ProcessingComplete() {
						return stats, errors.New("sync incomplete")
					}
					return stats, nil
				}
				if route == "reconciliation" {
					stats, _, err := engine.ReconcileWatchRootsWithStats(t.Context(), []string{root}, false, nil)
					return stats, err
				}
				if route == "changed plan" {
					result, err := engine.SyncChangedPathPlanContext(t.Context(), ChangedPathPlan{Files: files}, nil)
					return result.Stats, err
				}
				err := engine.SyncPathsContext(t.Context(), paths)
				return engine.LastSyncStats(), err
			}
			stats, err := run()
			require.Error(t, err, "a raced parent inventory must leave retry work")
			assert.Equal(t, 2, stats.Deferred)
			require.Equal(t, 2, factory.parses)
			for i, fork := range forks {
				assert.Less(t, database.GetSessionDataVersion(t.Context(), "codex:"+fork), db.CurrentDataVersion())
				assert.Equal(t, []string{"copied page", "new answer"}, messageContents(t, database, "codex:"+fork))
				assert.NotContains(t, engine.skipCache, providerAgentSkipCacheKey(paths[i], parser.AgentCodex))
			}
			// A new engine exercises persisted freshness, not just the in-memory cache.
			engine.Close()
			engine = NewEngine(t.Context(), database, config)
			t.Cleanup(engine.Close)
			stats, err = run()
			require.NoError(t, err)
			assert.Zero(t, stats.Deferred)
			require.Equal(t, 4, factory.parses, "unchanged fork files must be reparsed")
			for _, fork := range forks {
				assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), "codex:"+fork))
				assert.Equal(t, []string{"new answer"}, messageContents(t, database, "codex:"+fork))
			}
			_, err = run()
			require.NoError(t, err)
			assert.Equal(t, 4, factory.parses, "verified unchanged forks should use freshness again")
		})
	}
}
