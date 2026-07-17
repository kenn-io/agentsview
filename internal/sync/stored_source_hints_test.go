package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	gosync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type hintRecordingFactory struct {
	agent parser.AgentType
	caps  parser.Capabilities
	seen  *[][]string
}

func (f hintRecordingFactory) Definition() parser.AgentDef {
	return parser.AgentDef{Type: f.agent, DisplayName: string(f.agent)}
}

func (f hintRecordingFactory) Capabilities() parser.Capabilities { return f.caps }

func (f hintRecordingFactory) NewProvider(cfg parser.ProviderConfig) parser.Provider {
	return &hintRecordingProvider{ProviderBase: parser.ProviderBase{
		Def: f.Definition(), Caps: f.caps, Config: cfg.Clone(),
	}, seen: f.seen}
}

type hintRecordingProvider struct {
	parser.ProviderBase
	seen *[][]string
}

func (p *hintRecordingProvider) WatchPlan(context.Context) (parser.WatchPlan, error) {
	roots := make([]parser.WatchRoot, 0, len(p.Config.Roots))
	for _, root := range p.Config.Roots {
		roots = append(roots, parser.WatchRoot{Path: root})
	}
	return parser.WatchPlan{Roots: roots}, nil
}

type retryRecordingProvider struct {
	parser.ProviderBase
	seen []parser.FindSourceRequest
}

func (p *retryRecordingProvider) FindSource(
	_ context.Context, req parser.FindSourceRequest,
) (parser.SourceRef, bool, error) {
	p.seen = append(p.seen, req)
	return parser.SourceRef{
		Provider: parser.AgentOmnigent, Key: req.StoredFilePath,
		DisplayPath: req.StoredFilePath, FingerprintKey: req.StoredFilePath,
	}, true, nil
}

func (p *retryRecordingProvider) Parse(
	context.Context, parser.ParseRequest,
) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, nil
}

func (p *hintRecordingProvider) SourcesForChangedPath(
	_ context.Context,
	req parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	*p.seen = append(*p.seen, append([]string(nil), req.StoredSourcePaths...))
	return []parser.SourceRef{{
		Provider: p.Definition().Type, Key: req.Path,
		DisplayPath: req.Path, FingerprintKey: req.Path,
	}}, nil
}

func (p *hintRecordingProvider) Parse(context.Context, parser.ParseRequest) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, nil
}

func TestClassifyProviderChangedPathSchedulesStoredSourceHintsByCapability(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps parser.CapabilitySupport
		want bool
	}{
		{name: "supported", caps: parser.CapabilitySupported, want: true},
		{name: "unsupported", caps: parser.CapabilityUnsupported, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			changedPath := filepath.Join(root, "container.db")
			require.NoError(t, os.WriteFile(changedPath, []byte("fixture"), 0o600))
			persistedPath := filepath.Join(root, "archive", "stored#member")
			database := dbtest.OpenTestDB(t)
			require.NoError(t, database.UpsertSession(db.Session{
				ID: "hint-agent:stored", Agent: "hint-agent", Project: "fixture",
				Machine: "local", FilePath: strPtr(persistedPath),
			}))

			var seen [][]string
			caps := parser.Capabilities{Source: parser.SourceCapabilities{
				ClassifyChangedPath: parser.CapabilitySupported,
				StoredSourceHints:   tc.caps,
			}}
			factory := hintRecordingFactory{agent: "hint-agent", caps: caps, seen: &seen}
			engine := &Engine{
				db: database, machine: "local",
				agentDirs:         map[parser.AgentType][]string{"hint-agent": {root}},
				providerFactories: map[parser.AgentType]parser.ProviderFactory{"hint-agent": factory},
				providerMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
					"hint-agent": parser.ProviderMigrationProviderAuthoritative,
				},
			}

			files := engine.classifyProviderChangedPath(changedPath)

			require.Len(t, files, 1)
			require.Len(t, seen, 1)
			if tc.want {
				assert.Equal(t, []string{persistedPath}, seen[0])
			} else {
				assert.Empty(t, seen[0])
			}
		})
	}
}

func TestOmnigentChangedPathClaimsHintsOnlyForOwningWatchRoot(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	changedPath := filepath.Join(rootA, "chat.db")
	require.NoError(t, os.WriteFile(changedPath, []byte("fixture"), 0o600))
	database := dbtest.OpenTestDB(t)
	pathA := filepath.Join(rootA, "chat.db#member-a")
	pathB := filepath.Join(rootB, "chat.db#member-b")
	for id, path := range map[string]string{"member-a": pathA, "member-b": pathB} {
		require.NoError(t, database.UpsertSession(db.Session{
			ID: "omnigent:" + id, Agent: string(parser.AgentOmnigent),
			Project: "fixture", Machine: "local", FilePath: strPtr(path),
		}))
	}

	var seen [][]string
	caps := parser.Capabilities{Source: parser.SourceCapabilities{
		ClassifyChangedPath: parser.CapabilitySupported,
		StoredSourceHints:   parser.CapabilitySupported,
	}}
	factory := hintRecordingFactory{
		agent: parser.AgentOmnigent, caps: caps, seen: &seen,
	}
	engine := &Engine{
		db: database, machine: "local",
		agentDirs: map[parser.AgentType][]string{
			parser.AgentOmnigent: {rootA, rootB},
		},
		providerFactories: map[parser.AgentType]parser.ProviderFactory{
			parser.AgentOmnigent: factory,
		},
		providerMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentOmnigent: parser.ProviderMigrationProviderAuthoritative,
		},
	}

	files := engine.classifyProviderChangedPath(changedPath)

	require.Len(t, files, 1)
	require.Equal(t, [][]string{{pathA}}, seen)
	unrelatedKey := string(parser.AgentOmnigent) + "\x00" + filepath.Clean(rootB)
	engine.omnigentHintMu.Lock()
	_, unrelatedActivated := engine.omnigentHintCursors[unrelatedKey]
	engine.omnigentHintMu.Unlock()
	assert.False(t, unrelatedActivated,
		"the event must not consume the unrelated root's archived hints")
}

func TestOmnigentStoredHintCursorSerializesConcurrentPages(t *testing.T) {
	root := t.TempDir()
	database := dbtest.OpenTestDB(t)
	for i := range 2 * omnigentStoredHintBatchSize {
		path := filepath.Join(root, fmt.Sprintf("chat.db#member-%03d", i))
		require.NoError(t, database.UpsertSession(db.Session{
			ID: fmt.Sprintf("omnigent:member-%03d", i), Agent: string(parser.AgentOmnigent),
			Project: "fixture", Machine: "local", FilePath: strPtr(path),
		}))
	}
	engine := &Engine{db: database}
	type pageResult struct {
		paths []string
		err   error
	}
	results := make(chan pageResult, 2)
	var wg gosync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for {
				paths, claimed, err := engine.changedPathStoredSourcePaths(
					parser.AgentOmnigent, root,
				)
				if err != nil || claimed {
					engine.finishOmnigentStoredHintPage(root, err == nil)
					results <- pageResult{paths: paths, err: err}
					return
				}
				runtime.Gosched()
			}
		})
	}
	wg.Wait()
	close(results)

	seen := make(map[string]struct{}, 2*omnigentStoredHintBatchSize)
	for result := range results {
		require.NoError(t, result.err)
		require.Len(t, result.paths, omnigentStoredHintBatchSize)
		for _, path := range result.paths {
			_, duplicate := seen[path]
			assert.False(t, duplicate, "concurrent page claims must not overlap")
			seen[path] = struct{}{}
		}
	}
	assert.Len(t, seen, 2*omnigentStoredHintBatchSize)
}

func TestOmnigentStoredHintPageRetriesFailureAndDeactivatesAfterCompletion(t *testing.T) {
	root := t.TempDir()
	database := dbtest.OpenTestDB(t)
	path := filepath.Join(root, "chat.db#member")
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "omnigent:member", Agent: string(parser.AgentOmnigent),
		Project: "fixture", Machine: "local", FilePath: strPtr(path),
	}))
	engine := &Engine{db: database}

	first, claimed, err := engine.changedPathStoredSourcePaths(parser.AgentOmnigent, root)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, []string{path}, first)
	engine.finishOmnigentStoredHintPage(root, false)

	retry, claimed, err := engine.nextOmnigentStoredHintPage(root, false)
	require.NoError(t, err)
	require.True(t, claimed)
	assert.Equal(t, first, retry)
	engine.finishOmnigentStoredHintPage(root, true)

	remaining, claimed, err := engine.nextOmnigentStoredHintPage(root, false)
	require.NoError(t, err)
	assert.False(t, claimed)
	assert.Empty(t, remaining)
}

func TestOmnigentStoredHintActivationSurvivesInitialQueryFailure(t *testing.T) {
	root := t.TempDir()
	database := dbtest.OpenTestDB(t)
	engine := &Engine{db: database}
	require.NoError(t, database.Close())

	paths, claimed, err := engine.changedPathStoredSourcePaths(parser.AgentOmnigent, root)
	require.Error(t, err)
	assert.False(t, claimed)
	assert.Empty(t, paths)

	key := string(parser.AgentOmnigent) + "\x00" + filepath.Clean(root)
	engine.omnigentHintMu.Lock()
	cursor := engine.omnigentHintCursors[key]
	engine.omnigentHintMu.Unlock()
	assert.True(t, cursor.active)
	assert.False(t, cursor.inFlight)
}

func TestOmnigentStoredHintActivationDuringFinalPageRestartsSweep(t *testing.T) {
	root := t.TempDir()
	database := dbtest.OpenTestDB(t)
	path := filepath.Join(root, "chat.db#member")
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "omnigent:member", Agent: string(parser.AgentOmnigent),
		Project: "fixture", Machine: "local", FilePath: strPtr(path),
	}))
	engine := &Engine{db: database}

	first, claimed, err := engine.changedPathStoredSourcePaths(parser.AgentOmnigent, root)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, []string{path}, first)

	concurrent, claimed, err := engine.changedPathStoredSourcePaths(
		parser.AgentOmnigent, root,
	)
	require.NoError(t, err)
	assert.False(t, claimed)
	assert.Empty(t, concurrent)
	engine.finishOmnigentStoredHintPage(root, true)

	restarted, claimed, err := engine.nextOmnigentStoredHintPage(root, false)
	require.NoError(t, err)
	require.True(t, claimed)
	assert.Equal(t, first, restarted)
	engine.finishOmnigentStoredHintPage(root, true)

	remaining, claimed, err := engine.nextOmnigentStoredHintPage(root, false)
	require.NoError(t, err)
	assert.False(t, claimed)
	assert.Empty(t, remaining)
}

func TestOmnigentRetryDiscoveryProcessesBoundedRotatingPages(t *testing.T) {
	engine := &Engine{}
	for i := range 3 * omnigentRetryBatchSize {
		engine.storeOmnigentRetry(omnigentRetrySource{
			filePath: fmt.Sprintf("/retry/container-%03d.db", i),
		})
	}
	provider := &retryRecordingProvider{ProviderBase: parser.ProviderBase{
		Def: parser.AgentDef{Type: parser.AgentOmnigent},
	}}

	first, failures := engine.discoverOmnigentRetrySources(
		context.Background(), provider, map[string]struct{}{},
	)
	require.Zero(t, failures)
	require.Len(t, first, omnigentRetryBatchSize)
	second, failures := engine.discoverOmnigentRetrySources(
		context.Background(), provider, map[string]struct{}{},
	)
	require.Zero(t, failures)
	require.Len(t, second, omnigentRetryBatchSize)
	require.Len(t, provider.seen, 2*omnigentRetryBatchSize)

	seen := make(map[string]struct{}, 2*omnigentRetryBatchSize)
	for _, req := range provider.seen {
		_, duplicate := seen[req.StoredFilePath]
		assert.False(t, duplicate, "successive retry pages must advance")
		seen[req.StoredFilePath] = struct{}{}
	}
}

func TestOmnigentRetryDiscoveryDoesNotWrapShortQueue(t *testing.T) {
	engine := &Engine{}
	for i := range 3 {
		engine.storeOmnigentRetry(omnigentRetrySource{
			filePath: fmt.Sprintf("/retry/short-%03d.db", i),
		})
	}
	provider := &retryRecordingProvider{ProviderBase: parser.ProviderBase{
		Def: parser.AgentDef{Type: parser.AgentOmnigent},
	}}

	sources, failures := engine.discoverOmnigentRetrySources(
		context.Background(), provider, map[string]struct{}{},
	)
	require.Zero(t, failures)
	require.Len(t, sources, 3)
	assert.Len(t, provider.seen, 3)
}

func TestOmnigentMemberRetryOverflowActivatesBoundedHintSweep(t *testing.T) {
	root := t.TempDir()
	container := filepath.Join(root, "chat.db")
	database := dbtest.OpenTestDB(t)
	engine := &Engine{db: database}
	for i := range 4 * omnigentRetryBatchSize {
		member := fmt.Sprintf("member-%03d", i)
		path := parser.VirtualSourcePath(container, member)
		require.NoError(t, database.UpsertSession(db.Session{
			ID: "omnigent:" + member, Agent: string(parser.AgentOmnigent),
			Project: "fixture", Machine: "local", FilePath: strPtr(path),
		}))
		engine.storeOmnigentRetry(omnigentRetrySource{
			sessionID: "omnigent:" + member,
			filePath:  path,
		})
	}

	engine.omnigentRetryMu.Lock()
	assert.Empty(t, engine.omnigentRetrySources)
	assert.Nil(t, engine.omnigentRetryHead)
	assert.Nil(t, engine.omnigentRetryTail)
	engine.omnigentRetryMu.Unlock()

	hints, claimed, err := engine.nextOmnigentStoredHintPage(root, false)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Len(t, hints, omnigentStoredHintBatchSize)
	for _, path := range hints {
		assert.True(t, strings.HasPrefix(path, container+"#"))
	}
}

func TestClassifyCodexChangedPathAllocationsStayBounded(t *testing.T) {
	measure := func(t *testing.T, hintCount int) float64 {
		t.Helper()
		root := t.TempDir()
		path := filepath.Join(root, "rollout-2026-07-11T10-00-00-alloc.jsonl")
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(
				"alloc", "/workspace/agentsview", "codex_cli_rs", "2026-07-11T10:00:00Z",
			),
			testjsonl.CodexMsgJSON("user", "measure allocations", "2026-07-11T10:00:01Z"),
		)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		database := dbtest.OpenTestDB(t)
		for i := range hintCount {
			hint := filepath.Join(root, "archive", fmt.Sprintf("%04d.jsonl", i))
			require.NoError(t, database.UpsertSession(db.Session{
				ID: fmt.Sprintf("codex:hint-%04d", i), Agent: string(parser.AgentCodex),
				Project: "archive", Machine: "local", FilePath: strPtr(hint),
			}))
		}
		engine := &Engine{
			db: database, machine: "local",
			agentDirs:         map[parser.AgentType][]string{parser.AgentCodex: {root}},
			providerFactories: providerFactoryMap(parser.ProviderFactories()),
			providerMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
				parser.AgentCodex: parser.ProviderMigrationProviderAuthoritative,
			},
		}
		warm := engine.classifyProviderChangedPath(path)
		require.Len(t, warm, 1)
		assert.Equal(t, path, warm[0].Path)
		assert.Equal(t, parser.AgentCodex, warm[0].Agent)

		var got []parser.DiscoveredFile
		allocs := testing.AllocsPerRun(5, func() {
			got = engine.classifyProviderChangedPath(path)
		})
		require.Len(t, got, 1)
		assert.Equal(t, path, got[0].Path)
		assert.Equal(t, parser.AgentCodex, got[0].Agent)
		return allocs
	}

	smallAllocs := measure(t, 10)
	largeAllocs := measure(t, 2000)
	assert.LessOrEqual(t, largeAllocs, smallAllocs*2,
		"stored archives must not scale Codex changed-path allocations")
}

func TestClassifyProviderChangedPathPreservesHintDependentTombstones(t *testing.T) {
	tests := []struct {
		name  string
		agent parser.AgentType
		setup func(t *testing.T) (root, changedPath, deletedPath string)
	}{
		{
			name: "Forge dbBackedSourceSet", agent: parser.AgentForge,
			setup: func(t *testing.T) (string, string, string) {
				root := t.TempDir()
				path := writeProcessProviderForgeDB(t, root)
				conn, err := sql.Open("sqlite3", path)
				require.NoError(t, err)
				_, err = conn.Exec(`DELETE FROM conversations WHERE conversation_id = 'conv-001'`)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
				return root, path, path + "#conv-001"
			},
		},
		{
			name: "Zed multiSessionContainerSourceSet", agent: parser.AgentZed,
			setup: func(t *testing.T) (string, string, string) {
				root := t.TempDir()
				path := filepath.Join(root, "threads", "threads.db")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				store, err := sql.Open("sqlite3", path)
				require.NoError(t, err)
				_, err = store.Exec(`CREATE TABLE threads (
					id TEXT PRIMARY KEY, summary TEXT NOT NULL, updated_at TEXT NOT NULL,
					data_type TEXT NOT NULL, data BLOB NOT NULL, parent_id TEXT,
					folder_paths TEXT, folder_paths_order TEXT, created_at TEXT)`)
				require.NoError(t, err)
				require.NoError(t, store.Close())
				return root, path, parser.ZedSQLiteVirtualPath(path, "deleted")
			},
		},
		{
			name: "Devin direct DB reader", agent: parser.AgentDevin,
			setup: func(t *testing.T) (string, string, string) {
				root := t.TempDir()
				path, _ := writeProcessProviderDevinFixture(
					t, root, "deleted", "reply", 1710000000000, 1710000005000,
				)
				conn, err := sql.Open("sqlite3", path)
				require.NoError(t, err)
				_, err = conn.Exec(`DELETE FROM sessions WHERE id = 'deleted'`)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
				return root, path, path + "#deleted"
			},
		},
		{
			name: "Kiro SQLite helper", agent: parser.AgentKiro,
			setup: func(t *testing.T) (string, string, string) {
				root := t.TempDir()
				path := filepath.Join(root, "data.sqlite3")
				store, err := sql.Open("sqlite3", path)
				require.NoError(t, err)
				_, err = store.Exec(`CREATE TABLE conversations_v2 (
					key TEXT NOT NULL, conversation_id TEXT NOT NULL, value TEXT NOT NULL,
					created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
					PRIMARY KEY (key, conversation_id))`)
				require.NoError(t, err)
				payload, err := os.ReadFile(filepath.Join(
					"..", "parser", "testdata", "kiro_sqlite", "standard_payload.json",
				))
				require.NoError(t, err)
				_, err = store.Exec(`INSERT INTO conversations_v2
					(key, conversation_id, value, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
					"/workspace/agentsview", "deleted", string(payload),
					1710000000000, 1710000005000)
				require.NoError(t, err)
				_, err = store.Exec(`DELETE FROM conversations_v2 WHERE conversation_id = 'deleted'`)
				require.NoError(t, err)
				require.NoError(t, store.Close())
				return root, path, parser.KiroSQLiteVirtualPath(path, "deleted")
			},
		},
		{
			name: "Windsurf direct state DB reader", agent: parser.AgentWindsurf,
			setup: func(t *testing.T) (string, string, string) {
				root := filepath.Join(t.TempDir(), "Windsurf", "User")
				path := filepath.Join(root, "workspaceStorage", "fixture", "state.vscdb")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				writeSyncWindsurfStateDB(t, path, windsurfSyncPayload("deleted", "reply"))
				updateSyncWindsurfStateDB(t, path, `{}`)
				return root, path, path + "#deleted"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, changedPath, deletedPath := tc.setup(t)
			database := dbtest.OpenTestDB(t)
			require.NoError(t, database.UpsertSession(db.Session{
				ID: string(tc.agent) + ":deleted", Agent: string(tc.agent),
				Project: "fixture", Machine: "local", FilePath: strPtr(deletedPath),
			}))
			engine := &Engine{
				db: database, machine: "local", skipCache: make(map[string]int64),
				agentDirs:         map[parser.AgentType][]string{tc.agent: {root}},
				providerFactories: providerFactoryMap(parser.ProviderFactories()),
				providerMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
					tc.agent: parser.ProviderMigrationProviderAuthoritative,
				},
			}

			files := engine.classifyProviderChangedPath(changedPath)
			var tombstone parser.DiscoveredFile
			for _, file := range files {
				if file.Path == deletedPath {
					tombstone = file
				}
			}
			require.Equal(t, deletedPath, tombstone.Path)
			assert.Equal(t, tc.agent, tombstone.Agent)

			result := engine.processFile(context.Background(), tombstone)
			require.NoError(t, result.err)
			assert.True(t, result.forceReplace)
			assert.Empty(t, result.results)
		})
	}
}
