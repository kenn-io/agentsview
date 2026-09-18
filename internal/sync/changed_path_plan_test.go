package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type changedPathPlanFactory struct {
	agent    parser.AgentType
	caps     parser.Capabilities
	provider func(parser.ProviderConfig) *changedPathPlanProvider
}

func (f changedPathPlanFactory) Definition() parser.AgentDef {
	return parser.AgentDef{Type: f.agent, DisplayName: string(f.agent)}
}

func (f changedPathPlanFactory) Capabilities() parser.Capabilities { return f.caps }

func (f changedPathPlanFactory) NewProvider(cfg parser.ProviderConfig) parser.Provider {
	p := f.provider(cfg.Clone())
	p.ProviderBase = parser.ProviderBase{Def: f.Definition(), Caps: f.caps, Config: cfg.Clone()}
	return p
}

type changedPathPlanProvider struct {
	parser.ProviderBase
	relevance     parser.ChangedPathRelevance
	relevanceErr  error
	sources       []parser.SourceRef
	sourcesErr    error
	discovered    []parser.SourceRef
	discoverErr   error
	discoveries   *int
	hintScopes    []parser.StoredSourceHintScope
	requests      *[]parser.ChangedPathRequest
	reconciled    map[string]parser.SourceRef
	fingerprints  map[string]parser.SourceFingerprint
	outcomes      map[string]parser.ParseOutcome
	parseRequests *[]parser.ParseRequest
}

func (p *changedPathPlanProvider) WatchPlan(context.Context) (parser.WatchPlan, error) {
	roots := make([]parser.WatchRoot, 0, len(p.Config.Roots))
	for _, root := range p.Config.Roots {
		roots = append(roots, parser.WatchRoot{Path: root})
	}
	return parser.WatchPlan{Roots: roots}, nil
}

func (p *changedPathPlanProvider) ChangedPathRelevance(
	context.Context, parser.ChangedPathRequest,
) (parser.ChangedPathRelevance, error) {
	return p.relevance, p.relevanceErr
}

func (p *changedPathPlanProvider) SourcesForChangedPath(
	_ context.Context, req parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	if p.requests != nil {
		*p.requests = append(*p.requests, req)
	}
	return append([]parser.SourceRef(nil), p.sources...), p.sourcesErr
}

func (p *changedPathPlanProvider) StoredSourceHintScopes(
	parser.ChangedPathRequest,
) []parser.StoredSourceHintScope {
	return append([]parser.StoredSourceHintScope(nil), p.hintScopes...)
}

func (p *changedPathPlanProvider) Discover(context.Context) ([]parser.SourceRef, error) {
	if p.discoveries != nil {
		*p.discoveries++
	}
	return append([]parser.SourceRef(nil), p.discovered...), p.discoverErr
}

func (p *changedPathPlanProvider) SourceForReconciliation(
	_ context.Context, path, _ string,
) (parser.SourceRef, bool, error) {
	source, ok := p.reconciled[path]
	return source, ok, nil
}

func (p *changedPathPlanProvider) Fingerprint(
	_ context.Context, source parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return p.fingerprints[source.Key], nil
}

func (p *changedPathPlanProvider) Parse(
	_ context.Context, request parser.ParseRequest,
) (parser.ParseOutcome, error) {
	if p.parseRequests != nil {
		*p.parseRequests = append(*p.parseRequests, request)
	}
	return p.outcomes[request.Source.Key], nil
}

func changedPathPlanCapabilities() parser.Capabilities {
	return parser.Capabilities{Source: parser.SourceCapabilities{
		ClassifyChangedPath:  parser.CapabilitySupported,
		ChangedPathRelevance: parser.CapabilitySupported,
		DiscoverSources:      parser.CapabilitySupported,
	}}
}

func newChangedPathPlanEngine(
	t *testing.T,
	roots map[parser.AgentType][]string,
	factories ...changedPathPlanFactory,
) *Engine {
	t.Helper()
	modes := make(map[parser.AgentType]parser.ProviderMigrationMode, len(factories))
	providerFactories := make([]parser.ProviderFactory, 0, len(factories))
	for _, factory := range factories {
		modes[factory.agent] = parser.ProviderMigrationProviderAuthoritative
		providerFactories = append(providerFactories, factory)
	}
	return NewEngine(t.Context(), dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs:              roots,
		Machine:                "remote",
		ProviderFactories:      providerFactories,
		ProviderMigrationModes: modes,
		StoredPathResolver: func(path string) (string, bool) {
			return path, true
		},
	})
}

func TestPlanChangedPathsExactIrrelevantAndOutsideRoot(t *testing.T) {
	root := t.TempDir()
	exactPath := filepath.Join(root, "changed.jsonl")
	nonDataPath := filepath.Join(root, "metadata.json")
	sourcePath := filepath.Join(root, "session.jsonl")
	caps := changedPathPlanCapabilities()
	factory := changedPathPlanFactory{agent: "plan-exact", caps: caps}
	factory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{
			relevance: parser.ChangedPathDataBearing,
			sources: []parser.SourceRef{{
				Provider: factory.agent, Key: sourcePath,
				DisplayPath: sourcePath, FingerprintKey: sourcePath,
			}},
		}
	}
	engine := newChangedPathPlanEngine(t, map[parser.AgentType][]string{
		factory.agent: {root},
	}, factory)

	plan, err := engine.PlanChangedPathsContext(t.Context(), []string{exactPath, exactPath})
	require.NoError(t, err)
	require.Len(t, plan.Files, 1)
	assert.Equal(t, sourcePath, plan.Files[0].Path)
	assert.Empty(t, plan.FallbackProviders)

	factory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{relevance: parser.ChangedPathNonData}
	}
	engine = newChangedPathPlanEngine(t, map[parser.AgentType][]string{
		factory.agent: {root},
	}, factory)
	plan, err = engine.PlanChangedPathsContext(t.Context(), []string{
		nonDataPath, filepath.Join(t.TempDir(), "outside.jsonl"),
	})
	require.NoError(t, err)
	assert.Empty(t, plan.Files)
	assert.Empty(t, plan.FallbackProviders)
}

func TestPlanChangedPathsProviderErrorFallsBackWithoutDroppingExactWork(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	caps := changedPathPlanCapabilities()
	exactSource := filepath.Join(rootA, "session.jsonl")
	exactFactory := changedPathPlanFactory{agent: "plan-exact", caps: caps}
	exactFactory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{
			relevance: parser.ChangedPathDataBearing,
			sources: []parser.SourceRef{{
				Provider: exactFactory.agent, Key: exactSource,
				DisplayPath: exactSource, FingerprintKey: exactSource,
			}},
		}
	}
	fallbackFactory := changedPathPlanFactory{agent: "plan-fallback", caps: caps}
	fallbackFactory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{
			relevance:  parser.ChangedPathDataBearing,
			sourcesErr: errors.New("classification unavailable"),
		}
	}
	engine := newChangedPathPlanEngine(t, map[parser.AgentType][]string{
		exactFactory.agent:    {rootA},
		fallbackFactory.agent: {rootB},
	}, exactFactory, fallbackFactory)

	plan, err := engine.PlanChangedPathsContext(t.Context(), []string{
		filepath.Join(rootA, "changed.jsonl"),
		filepath.Join(rootB, "changed.jsonl"),
	})
	require.NoError(t, err)
	require.Len(t, plan.Files, 1)
	assert.Equal(t, exactSource, plan.Files[0].Path)
	assert.Equal(t, []parser.AgentType{fallbackFactory.agent}, plan.FallbackProviders)
}

func TestPlanChangedPathsCancellationAbortsInsteadOfFallingBack(t *testing.T) {
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			root := t.TempDir()
			factory := changedPathPlanFactory{
				agent: "plan-cancel", caps: changedPathPlanCapabilities(),
			}
			factory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
				return &changedPathPlanProvider{relevanceErr: sentinel}
			}
			engine := newChangedPathPlanEngine(t, map[parser.AgentType][]string{
				factory.agent: {root},
			}, factory)

			plan, err := engine.PlanChangedPathsContext(
				t.Context(), []string{filepath.Join(root, "changed.jsonl")},
			)
			require.ErrorIs(t, err, sentinel)
			assert.Empty(t, plan.Files)
			assert.Empty(t, plan.FallbackProviders)
		})
	}
}

func TestPlanChangedPathsUnprovenPathsUseOwningProviderFallback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		relevance parser.ChangedPathRelevance
	}{
		{name: "unclassified", relevance: parser.ChangedPathUnclassified},
		{name: "data-bearing without source", relevance: parser.ChangedPathDataBearing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			caps := changedPathPlanCapabilities()
			factory := changedPathPlanFactory{agent: "plan-fallback", caps: caps}
			factory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
				return &changedPathPlanProvider{relevance: tc.relevance}
			}
			engine := newChangedPathPlanEngine(t, map[parser.AgentType][]string{
				factory.agent: {root},
			}, factory)

			plan, err := engine.PlanChangedPathsContext(
				t.Context(), []string{filepath.Join(root, "changed.jsonl")},
			)
			require.NoError(t, err)
			assert.Empty(t, plan.Files)
			assert.Equal(t, []parser.AgentType{factory.agent}, plan.FallbackProviders)
		})
	}
}

func TestPlanChangedPathsClaudeProcessesOnlyChangedSession(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	var changedPath string
	for i := range 12 {
		path := filepath.Join(projectDir, fmt.Sprintf("session-%02d.jsonl", i))
		body := testjsonl.NewSessionBuilder().AddClaudeUser(
			"2026-08-14T10:00:00Z", fmt.Sprintf("message %d", i),
		).String()
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		if i == 5 {
			changedPath = path
		}
	}
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "remote",
	})
	t.Cleanup(engine.Close)

	plan, err := engine.PlanChangedPathsContext(
		t.Context(), []string{changedPath},
	)
	require.NoError(t, err)
	require.Len(t, plan.Files, 1)
	assert.Equal(t, changedPath, plan.Files[0].Path)
	assert.Empty(t, plan.FallbackProviders)

	result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.FilesDiscovered)
	assert.Equal(t, 1, result.FilesProcessed)
	assert.Equal(t, 1, result.Stats.Synced)
	page, err := database.ListSessions(t.Context(), db.SessionFilter{Limit: 20})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1,
		"bounded processing must not import unrelated Claude sessions")
	assert.Equal(t, "session-05", page.Sessions[0].ID)
}

func TestPlanChangedPathsCodexIndexUsesStoredArchivedDuplicate(t *testing.T) {
	root := t.TempDir()
	liveRoot := filepath.Join(root, "sessions")
	archiveRoot := filepath.Join(root, "archived_sessions")
	require.NoError(t, os.MkdirAll(liveRoot, 0o755))
	require.NoError(t, os.MkdirAll(archiveRoot, 0o755))
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122abc"
	livePath := filepath.Join(
		liveRoot, "2026", "08", "15",
		"rollout-2026-08-15T10-00-00-"+uuid+".jsonl",
	)
	archivePath := filepath.Join(
		archiveRoot, "rollout-2026-08-14T10-00-00-"+uuid+".jsonl",
	)
	for path, content := range map[string]string{
		livePath:    "live stale copy",
		archivePath: "archived tracked copy",
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		body := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(
				uuid, "/workspace/project", "codex_cli_rs", "2026-08-15T10:00:00Z",
			),
			testjsonl.CodexMsgJSON("user", content, "2026-08-15T10:00:01Z"),
		)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}
	indexPath := filepath.Join(root, parser.CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"id":"`+uuid+`","thread_name":"Renamed title"}`+"\n",
	), 0o600))
	database := dbtest.OpenTestDB(t)
	toStoredPath := func(path string) string {
		rel, err := filepath.Rel(root, path)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return "remote:/codex/" + filepath.ToSlash(rel)
		}
		return path
	}
	storedPath := toStoredPath(archivePath)
	oldName := "Old title"
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "remote~codex:" + uuid, Agent: string(parser.AgentCodex),
		Machine: "remote", FilePath: &storedPath, SessionName: &oldName,
	}))
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {liveRoot, archiveRoot},
		},
		Machine: "remote", IDPrefix: "remote~", PathRewriter: toStoredPath,
	})
	t.Cleanup(engine.Close)

	plan, err := engine.PlanChangedPathsContext(t.Context(), []string{indexPath})

	require.NoError(t, err)
	require.Len(t, plan.Files, 1)
	assert.Equal(t, archivePath, plan.Files[0].Path)
	assert.Empty(t, plan.FallbackProviders)
}

func TestSyncChangedPathPlanPrefersLiveCodexDuplicate(t *testing.T) {
	root := t.TempDir()
	liveRoot := filepath.Join(root, "sessions")
	archiveRoot := filepath.Join(root, "archived_sessions")
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c123def"
	livePath := filepath.Join(
		liveRoot, "2026", "08", "15",
		"rollout-2026-08-15T10-00-00-"+uuid+".jsonl",
	)
	archivePath := filepath.Join(
		archiveRoot, "rollout-2026-08-14T10-00-00-"+uuid+".jsonl",
	)
	for path, content := range map[string]string{
		livePath:    "preferred live content",
		archivePath: "stale archived content",
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		body := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(
				uuid, "/workspace/project", "codex_cli_rs", "2026-08-15T10:00:00Z",
			),
			testjsonl.CodexMsgJSON("user", content, "2026-08-15T10:00:01Z"),
		)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {liveRoot, archiveRoot},
		},
		Machine: "remote",
	})
	t.Cleanup(engine.Close)

	plan, err := engine.PlanChangedPathsContext(
		t.Context(), []string{archivePath, livePath},
	)
	require.NoError(t, err)
	require.Len(t, plan.Files, 1)
	assert.Equal(t, livePath, plan.Files[0].Path)
	prune := plan.PruneScope(map[string]struct{}{
		archivePath: {},
		livePath:    {},
	})
	require.Len(t, prune.Files, 2)
	assert.ElementsMatch(t, []string{archivePath, livePath}, []string{
		prune.Files[0].Path,
		prune.Files[1].Path,
	})

	result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.FilesProcessed)
	assert.Equal(t, 1, result.Stats.Synced)
	messages, err := database.GetMessages(
		t.Context(), "codex:"+uuid, 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "preferred live content", messages[0].Content)
}

func TestPlanChangedPathsOmnigentIncludesStoredDescendants(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	physicalRoot := t.TempDir()
	physicalContainer := filepath.Join(physicalRoot, "chat.db")
	physicalParent := parser.VirtualSourcePath(physicalContainer, "parent")
	physicalChild := parser.VirtualSourcePath(physicalContainer, "child")
	storedContainer := "/remote/omnigent/chat.db"
	storedParent := parser.VirtualSourcePath(storedContainer, "parent")
	storedChild := parser.VirtualSourcePath(storedContainer, "child")
	parentID := "remote~omnigent:parent"
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: parentID, Agent: string(parser.AgentOmnigent), Machine: "remote",
		FilePath: &storedParent,
	}))
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "remote~omnigent:child", Agent: string(parser.AgentOmnigent),
		Machine: "remote", ParentSessionID: &parentID, FilePath: &storedChild,
	}))
	parentSource := parser.SourceRef{
		Provider: parser.AgentOmnigent, Key: physicalParent,
		DisplayPath: physicalParent, FingerprintKey: physicalParent,
	}
	childSource := parser.SourceRef{
		Provider: parser.AgentOmnigent, Key: physicalChild,
		DisplayPath: physicalChild, FingerprintKey: physicalChild,
	}
	parentResult := processFixtureResult(
		"omnigent:parent", parser.AgentOmnigent, "project",
		physicalParent, parser.SourceFingerprint{Key: physicalParent, MTimeNS: 2},
	)
	parentResult.Session.Cwd = "/workspace/new"
	parentResult.Session.GitBranch = "feature"
	childResult := processFixtureResult(
		"omnigent:child", parser.AgentOmnigent, "project",
		physicalChild, parser.SourceFingerprint{Key: physicalChild, MTimeNS: 2},
	)
	childResult.Session.Cwd = "/workspace/new"
	childResult.Session.GitBranch = "feature"
	childResult.Session.ParentSessionID = "omnigent:parent"
	var parseRequests []parser.ParseRequest
	factory := changedPathPlanFactory{
		agent: parser.AgentOmnigent, caps: changedPathPlanCapabilities(),
	}
	factory.caps.Source.CompositeFingerprint = parser.CapabilitySupported
	factory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{
			relevance: parser.ChangedPathDataBearing,
			sources:   []parser.SourceRef{parentSource},
			reconciled: map[string]parser.SourceRef{
				physicalChild: childSource,
			},
			fingerprints: map[string]parser.SourceFingerprint{
				physicalParent: {Key: physicalParent, MTimeNS: 2},
				physicalChild:  {Key: physicalChild, MTimeNS: 2},
			},
			outcomes: map[string]parser.ParseOutcome{
				physicalParent: {
					Results: []parser.ParseResultOutcome{{
						Result: parentResult, DataVersion: parser.DataVersionCurrent,
					}},
					ResultSetComplete: true,
				},
				physicalChild: {
					Results: []parser.ParseResultOutcome{{
						Result: childResult, DataVersion: parser.DataVersionCurrent,
					}},
					ResultSetComplete: true,
				},
			},
			parseRequests: &parseRequests,
		}
	}
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {physicalRoot},
		},
		Machine: "remote", IDPrefix: "remote~",
		ProviderFactories: []parser.ProviderFactory{factory},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentOmnigent: parser.ProviderMigrationProviderAuthoritative,
		},
		StoredPathResolver: func(path string) (string, bool) {
			if path == storedChild {
				return physicalChild, true
			}
			return "", false
		},
	})
	t.Cleanup(engine.Close)

	plan, err := engine.PlanChangedPathsContext(
		t.Context(), []string{physicalContainer},
	)

	require.NoError(t, err)
	require.Len(t, plan.Files, 2)
	assert.ElementsMatch(t, []string{physicalParent, physicalChild}, []string{
		plan.Files[0].Path, plan.Files[1].Path,
	})
	assert.Empty(t, plan.FallbackProviders)

	result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, result.FilesProcessed)
	require.Len(t, parseRequests, 2)
	child, err := database.GetSession(t.Context(), "remote~omnigent:child")
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Equal(t, "/workspace/new", child.Cwd)
	assert.Equal(t, "feature", child.GitBranch)
}

func TestPlanChangedPathsDeletionClassifiesWithoutPhysicalStat(t *testing.T) {
	root := t.TempDir()
	deletedPath := filepath.Join(root, "deleted.jsonl")
	ownerPath := filepath.Join(root, "owner.jsonl")
	caps := changedPathPlanCapabilities()
	factory := changedPathPlanFactory{agent: "plan-delete", caps: caps}
	factory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{
			relevance: parser.ChangedPathDataBearing,
			sources: []parser.SourceRef{{
				Provider: factory.agent, Key: ownerPath,
				DisplayPath: ownerPath, FingerprintKey: ownerPath,
			}},
		}
	}
	engine := newChangedPathPlanEngine(t, map[parser.AgentType][]string{
		factory.agent: {root},
	}, factory)
	require.NoFileExists(t, deletedPath)

	plan, err := engine.PlanChangedPathsContext(t.Context(), []string{deletedPath})
	require.NoError(t, err)
	require.Len(t, plan.Files, 1)
	assert.Equal(t, ownerPath, plan.Files[0].Path)
}

func TestPlanChangedPathsExactUsesPreferredClaudeDuplicate(t *testing.T) {
	liveRoot := t.TempDir()
	archiveRoot := t.TempDir()
	livePath := filepath.Join(liveRoot, "proj-live", "exact-duplicate.jsonl")
	archivePath := filepath.Join(archiveRoot, "proj-archive", "exact-duplicate.jsonl")
	for _, path := range []string{livePath, archivePath} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	}
	older := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Second)
	require.NoError(t, os.Chtimes(archivePath, older, older))
	require.NoError(t, os.Chtimes(livePath, newer, newer))

	database := dbtest.OpenTestDB(t)
	liveInfo, err := os.Stat(livePath)
	require.NoError(t, err)
	liveSize := liveInfo.Size()
	liveMtime := liveInfo.ModTime().UnixNano()
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "exact-duplicate", Agent: string(parser.AgentClaude), Machine: "remote",
		FilePath: &livePath, FileSize: &liveSize, FileMtime: &liveMtime,
	}))
	require.NoError(t, database.SetSessionDataVersion(t.Context(),
		"exact-duplicate", db.CurrentDataVersion(),
	))
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {liveRoot, archiveRoot},
		},
		Machine: "remote",
	})
	t.Cleanup(engine.Close)
	scanCalls := 0
	engine.claudeProjectSessionFiles = func(string) []parser.DiscoveredFile {
		scanCalls++
		return nil
	}

	plan, err := engine.PlanChangedPathsContext(t.Context(), []string{archivePath})
	require.NoError(t, err)
	require.Len(t, plan.Files, 1)
	assert.Equal(t, livePath, plan.Files[0].Path)
	assert.Zero(t, scanCalls,
		"exact duplicate lookup must not enumerate the full Claude corpus")
}

func TestPlanChangedPathsDoesNotScanClaudeCorpusForExactBatch(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "project", "shared-session.jsonl")
	caps := changedPathPlanCapabilities()
	factory := changedPathPlanFactory{agent: parser.AgentClaude, caps: caps}
	factory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{
			relevance: parser.ChangedPathDataBearing,
			sources: []parser.SourceRef{{
				Provider: parser.AgentClaude, Key: sourcePath,
				DisplayPath: sourcePath, FingerprintKey: sourcePath,
			}},
		}
	}
	engine := newChangedPathPlanEngine(t, map[parser.AgentType][]string{
		parser.AgentClaude: {root},
	}, factory)
	scanCalls := 0
	engine.claudeProjectSessionFiles = func(string) []parser.DiscoveredFile {
		scanCalls++
		return nil
	}
	changedPaths := make([]string, 16)
	for i := range changedPaths {
		changedPaths[i] = filepath.Join(root, fmt.Sprintf("changed-%02d.jsonl", i))
	}

	plan, err := engine.PlanChangedPathsContext(t.Context(), changedPaths)

	require.NoError(t, err)
	assert.Zero(t, scanCalls,
		"exact planning must not enumerate the full Claude corpus")
	require.Len(t, plan.Files, 1)
	assert.Equal(t, sourcePath, plan.Files[0].Path)
	for _, changedPath := range changedPaths {
		attribution, ok := plan.attribution[changedPath]
		require.True(t, ok)
		require.Len(t, attribution.files, 1)
		assert.Equal(t, sourcePath, attribution.files[0].Path)
	}
}

func TestPlanChangedPathsTranslatesStoredHintsOrFallsBack(t *testing.T) {
	root := t.TempDir()
	changedPath := filepath.Join(root, "container.db")
	require.NoError(t, os.WriteFile(changedPath, []byte("fixture"), 0o600))
	storedPath := "remote:/sessions/container.db#member"
	physicalHint := changedPath + "#member"
	storedContainer := "remote:/sessions/container.db"
	caps := changedPathPlanCapabilities()
	caps.Source.StoredSourceHints = parser.CapabilitySupported
	var requests []parser.ChangedPathRequest
	factory := changedPathPlanFactory{agent: "plan-hints", caps: caps}
	factory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{
			relevance: parser.ChangedPathDataBearing,
			hintScopes: []parser.StoredSourceHintScope{{
				Path: changedPath, IncludeVirtualMembers: true,
			}},
			requests: &requests,
			sources: []parser.SourceRef{{
				Provider: factory.agent, Key: physicalHint,
				DisplayPath: physicalHint, FingerprintKey: physicalHint,
			}},
		}
	}
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "plan-hints:member", Agent: string(factory.agent), Machine: "remote",
		Project: "fixture", FilePath: &storedPath,
	}))
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{factory.agent: {root}},
		Machine:   "remote", ProviderFactories: []parser.ProviderFactory{factory},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			factory.agent: parser.ProviderMigrationProviderAuthoritative,
		},
		StoredPathResolver: func(path string) (string, bool) {
			return physicalHint, path == storedPath
		},
		PathRewriter: func(path string) string {
			if path == changedPath {
				return storedContainer
			}
			return path
		},
	})

	plan, err := engine.PlanChangedPathsContext(t.Context(), []string{changedPath})
	require.NoError(t, err)
	assert.Empty(t, plan.FallbackProviders)
	require.Len(t, requests, 1)
	assert.Equal(t, []string{physicalHint}, requests[0].StoredSourcePaths)

	engine.storedPathResolver = func(string) (string, bool) { return "", false }
	requests = nil
	plan, err = engine.PlanChangedPathsContext(t.Context(), []string{changedPath})
	require.NoError(t, err)
	assert.Empty(t, plan.Files)
	assert.Equal(t, []parser.AgentType{factory.agent}, plan.FallbackProviders)
	assert.Empty(t, requests, "untranslated hints must prevent bounded classification")
}

func TestPlanChangedPathsRejectsUntrustedInputAndOwnership(t *testing.T) {
	root := t.TempDir()
	caps := changedPathPlanCapabilities()
	factory := changedPathPlanFactory{agent: "configured-owner", caps: caps}
	factory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{relevance: parser.ChangedPathNonData}
	}
	engine := newChangedPathPlanEngine(t, map[parser.AgentType][]string{
		factory.agent: {root},
	}, factory)

	plan, err := engine.PlanChangedPathsContext(t.Context(), []string{"relative/path"})
	require.Error(t, err)
	assert.Empty(t, plan.Files)
	assert.Empty(t, plan.FallbackProviders)

	mismatched := factory
	mismatched.agent = "different-owner"
	engine.providerFactories[factory.agent] = mismatched
	plan, err = engine.PlanChangedPathsContext(
		t.Context(), []string{filepath.Join(root, "changed.jsonl")},
	)
	require.Error(t, err)
	assert.Empty(t, plan.Files)
	assert.Empty(t, plan.FallbackProviders)
}

func TestChangedPathPlanPruneScopeUsesOnlyArmedAttribution(t *testing.T) {
	root := t.TempDir()
	armedPath := filepath.Join(root, "armed.jsonl")
	disarmedPath := filepath.Join(root, "disarmed.jsonl")
	armedSource := filepath.Join(root, "armed-session.jsonl")
	disarmedSource := filepath.Join(root, "disarmed-session.jsonl")
	agent := parser.AgentType("plan-projection")
	plan := ChangedPathPlan{
		Files: []parser.DiscoveredFile{
			{Agent: agent, Path: armedSource},
			{Agent: agent, Path: disarmedSource},
		},
		FallbackProviders: []parser.AgentType{agent},
		attribution: map[string]changedPathAttribution{
			armedPath: {
				files: []parser.DiscoveredFile{{Agent: agent, Path: armedSource}},
			},
			disarmedPath: {
				files:             []parser.DiscoveredFile{{Agent: agent, Path: disarmedSource}},
				fallbackProviders: []parser.AgentType{agent},
			},
		},
	}

	nonCanonicalArmedPath := filepath.Join(root, "nested", "..", filepath.Base(armedPath))
	prune := plan.PruneScope(map[string]struct{}{nonCanonicalArmedPath: {}})
	require.Len(t, prune.Files, 1)
	assert.Equal(t, armedSource, prune.Files[0].Path)
	assert.Empty(t, prune.FallbackProviders)
	assert.Len(t, plan.Files, 2)
	assert.Equal(t, []parser.AgentType{agent}, plan.FallbackProviders)
}

func TestChangedPathPlanCountsEveryDisarmedFallbackInput(t *testing.T) {
	root := t.TempDir()
	agent := parser.AgentType("plan-fallback")
	first := filepath.Join(root, "first.jsonl")
	second := filepath.Join(root, "second.jsonl")
	plan := ChangedPathPlan{attribution: map[string]changedPathAttribution{
		first:  {fallbackProviders: []parser.AgentType{agent}},
		second: {fallbackProviders: []parser.AgentType{agent}},
	}}

	assert.Equal(t, 2, plan.CountCachedSuppressedInputs(
		map[string]struct{}{}, nil, map[parser.AgentType]int{agent: 1},
	))
	assert.Equal(t, 1, plan.CountCachedSuppressedInputs(
		map[string]struct{}{filepath.Join(root, "nested", "..", "first.jsonl"): {}},
		nil, map[parser.AgentType]int{agent: 1},
	))
}

func TestDiscoverChangedPathFallbackIsProviderBounded(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	var discoveriesA, discoveriesB int
	caps := changedPathPlanCapabilities()
	source := filepath.Join(rootA, "session.jsonl")
	factoryA := changedPathPlanFactory{agent: "fallback-a", caps: caps}
	factoryA.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{
			discoveries: &discoveriesA,
			discovered: []parser.SourceRef{
				{Provider: factoryA.agent, Key: source, DisplayPath: source},
				{Provider: factoryA.agent, Key: source, DisplayPath: source},
			},
		}
	}
	factoryB := changedPathPlanFactory{agent: "fallback-b", caps: caps}
	factoryB.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{discoveries: &discoveriesB}
	}
	engine := newChangedPathPlanEngine(t, map[parser.AgentType][]string{
		factoryA.agent: {rootA}, factoryB.agent: {rootB},
	}, factoryA, factoryB)

	files, counts, err := engine.discoverChangedPathFallbackProviders(
		t.Context(), []parser.AgentType{factoryA.agent},
	)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, source, files[0].Path)
	assert.Equal(t, map[parser.AgentType]int{factoryA.agent: 1}, counts)
	assert.Equal(t, 1, discoveriesA)
	assert.Zero(t, discoveriesB)
}
