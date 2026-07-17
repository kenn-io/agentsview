package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
	return parser.WatchPlan{Roots: []parser.WatchRoot{{Path: p.Config.Roots[0]}}}, nil
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
			paths, err := engine.changedPathStoredSourcePaths(parser.AgentOmnigent, root)
			results <- pageResult{paths: paths, err: err}
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
