package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestIssue1717PassMemoReusesAcrossPages(t *testing.T) {
	const sourceCount = reconciliationPageSize + 1
	provider, root := newIssue1717Provider(t, sourceCount, reconciliationPageSize-1)
	engine := newIssue1717Engine(t, provider, root)

	require.NoError(t, engine.ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	))
	for i := range sourceCount {
		session, err := engine.db.GetSession(
			t.Context(), issue1717SessionID(i),
		)
		require.NoError(t, err)
		require.NotNil(t, session, "session %d must be archived", i)
		assert.Equal(t, "main_repo", session.Project,
			"page boundary must keep the memoized project")
	}
	t.Logf("sources=%d page_size=%d stable_project=%s",
		sourceCount, reconciliationPageSize, "main_repo")
}

func TestIssue1717ChangedPathPlanMemoLifetime(t *testing.T) {
	provider, root := newIssue1717Provider(t, 2, 0)
	engine := newIssue1717Engine(t, provider, root)
	files := make([]parser.DiscoveredFile, len(provider.sources))
	for i, source := range provider.sources {
		files[i] = parser.DiscoveredFile{
			Path:            source.Key,
			Agent:           source.Provider,
			ProviderSource:  &provider.sources[i],
			ProviderProcess: true,
			ForceParse:      true,
		}
	}

	result, err := engine.SyncChangedPathPlanContext(t.Context(), ChangedPathPlan{
		Files: files,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, len(files), result.FilesProcessed)
	for i := range files {
		session, err := engine.db.GetSession(t.Context(), issue1717SessionID(i))
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, "main_repo", session.Project)
	}
}

func TestIssue1717NextOperationRefresh(t *testing.T) {
	provider, root := newIssue1717Provider(t, 1, -1)
	engine := newIssue1717Engine(t, provider, root)

	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{root}, false,
	))
	require.NoError(t, os.RemoveAll(provider.mainRepo))
	makeIssue1717Repo(t, provider.newRepo)
	provider.resetForOperation()

	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{root}, false,
	))
	session, err := engine.db.GetSession(t.Context(), issue1717SessionID(0))
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "new_repo", session.Project)
}

type issue1717Provider struct {
	parser.ProviderBase
	sources  []parser.SourceRef
	indices  map[string]int
	cwd      string
	mutate   func()
	mutateAt int
	mainRepo string
	newRepo  string

	mu   stdsync.Mutex
	cond *stdsync.Cond
	next int
}

func (p *issue1717Provider) Discover(
	context.Context,
) ([]parser.SourceRef, error) {
	return append([]parser.SourceRef(nil), p.sources...), nil
}

func (p *issue1717Provider) DiscoverEach(
	ctx context.Context, yield func(parser.SourceRef) error,
) error {
	for _, source := range p.sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := yield(source); err != nil {
			return err
		}
	}
	return nil
}

func (p *issue1717Provider) SourceForReconciliation(
	ctx context.Context, path, project string,
) (parser.SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return parser.SourceRef{}, false, err
	}
	for _, source := range p.sources {
		if source.DisplayPath == path {
			source.ProjectHint = project
			return source, true, nil
		}
	}
	return parser.SourceRef{}, false, nil
}

func (p *issue1717Provider) WatchPlan(
	context.Context,
) (parser.WatchPlan, error) {
	return parser.WatchPlan{}, nil
}

func (p *issue1717Provider) Fingerprint(
	context.Context, parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return parser.SourceFingerprint{Hash: "issue1717"}, nil
}

func (p *issue1717Provider) Parse(
	ctx context.Context, req parser.ParseRequest,
) (parser.ParseOutcome, error) {
	index := p.indices[req.Source.Key]
	p.mu.Lock()
	for index != p.next {
		p.cond.Wait()
	}
	p.mu.Unlock()

	project := parser.ExtractProjectFromCwdWithBranchContext(ctx, p.cwd, "")
	if index == p.mutateAt && p.mutate != nil {
		p.mutate()
	}

	p.mu.Lock()
	p.next++
	p.cond.Broadcast()
	p.mu.Unlock()

	started := time.Unix(1704067200, 0)
	return parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{{
			Result: parser.ParseResult{Session: parser.ParsedSession{
				ID: issue1717SessionID(index), Agent: req.Source.Provider,
				Project: project, Machine: "local", Cwd: p.cwd,
				StartedAt: started, EndedAt: started,
				File: parser.FileInfo{Path: req.Source.FingerprintKey},
			}},
			DataVersion: parser.DataVersionCurrent,
		}},
		ResultSetComplete: true,
	}, nil
}

func (p *issue1717Provider) resetForOperation() {
	p.mu.Lock()
	p.next = 0
	p.mu.Unlock()
}

type issue1717Factory struct {
	provider *issue1717Provider
}

func (f issue1717Factory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f issue1717Factory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f issue1717Factory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	return issue1717ScopedProvider{
		issue1717Provider: f.provider,
		scopes: parser.ProviderBase{
			Def: f.provider.Def, Caps: f.provider.Caps, Config: cfg.Clone(),
		},
	}
}

type issue1717ScopedProvider struct {
	*issue1717Provider
	scopes parser.ProviderBase
}

func (p issue1717ScopedProvider) ResolveReconciliationScopes(
	ctx context.Context, req parser.ReconciliationScopeRequest,
) (parser.ReconciliationScopePlan, error) {
	return p.scopes.ResolveReconciliationScopes(ctx, req)
}

func newIssue1717Provider(
	t *testing.T, sourceCount, mutateAt int,
) (*issue1717Provider, string) {
	t.Helper()
	root := t.TempDir()
	worktreeRoot := filepath.Join(root, "worktrees")
	mainRepo := filepath.Join(worktreeRoot, "main_repo")
	makeIssue1717Repo(t, mainRepo)
	newRepo := filepath.Join(worktreeRoot, "new_repo")
	cwd := filepath.Join(worktreeRoot, "deleted-child", "src")
	sourceRoot := filepath.Join(root, "sources")
	require.NoError(t, os.MkdirAll(sourceRoot, 0o755))

	sources := make([]parser.SourceRef, sourceCount)
	indices := make(map[string]int, sourceCount)
	for i := range sources {
		path := filepath.Join(sourceRoot, fmt.Sprintf("session-%03d.jsonl", i))
		require.NoError(t, os.WriteFile(path, []byte("source\n"), 0o600))
		sources[i] = parser.SourceRef{
			Provider: parser.AgentType("issue1717"), Key: path,
			DisplayPath: path, FingerprintKey: path,
		}
		indices[path] = i
	}
	provider := &issue1717Provider{
		sources: sources, indices: indices, cwd: cwd, mutateAt: mutateAt,
		mainRepo: mainRepo, newRepo: newRepo,
	}
	provider.ProviderBase = parser.ProviderBase{
		Def: parser.AgentDef{Type: parser.AgentType("issue1717"), FileBased: true},
		Caps: parser.Capabilities{Source: parser.SourceCapabilities{
			DiscoverSources:    parser.CapabilitySupported,
			StreamingDiscovery: parser.CapabilitySupported,
			WatchSources:       parser.CapabilitySupported,
		}},
	}
	provider.cond = stdsync.NewCond(&provider.mu)
	provider.mutate = func() {
		if err := os.RemoveAll(mainRepo); err != nil {
			assert.Failf(t, "test failed", "remove initial repository: %v", err)
		}
		if err := os.MkdirAll(
			filepath.Join(newRepo, ".git", "worktrees", "deleted-child"),
			0o755,
		); err != nil {
			assert.Failf(t, "test failed", "create replacement repository: %v", err)
		}
	}
	return provider, root
}

func newIssue1717Engine(
	t *testing.T, provider *issue1717Provider, root string,
) *Engine {
	t.Helper()
	engine := NewEngine(t.Context(), openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			provider.Def.Type: {root},
		},
		Machine: "local",
		ProviderFactories: []parser.ProviderFactory{
			issue1717Factory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			provider.Def.Type: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	return engine
}

func makeIssue1717Repo(t *testing.T, repo string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(
		filepath.Join(repo, ".git", "worktrees", "deleted-child"),
		0o755,
	))
}

func issue1717SessionID(index int) string {
	return fmt.Sprintf("issue1717:%03d", index)
}
