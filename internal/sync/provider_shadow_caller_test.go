package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestClassifyProviderChangedPathPassesStoredHintsToShadowProvider(
	t *testing.T,
) {
	root := t.TempDir()
	eventPath := filepath.Join(root, "state.sqlite3-wal")
	storedPath := filepath.Join(root, "state.sqlite3") + "#session-a"
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.UpsertSession(db.Session{
		ID:       "claude:session-a",
		Project:  "demo",
		Machine:  "devbox",
		Agent:    string(parser.AgentClaude),
		FilePath: &storedPath,
	}))
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentClaude,
					DisplayName: "Claude Code",
				},
			},
		},
		watchPlan: parser.WatchPlan{Roots: []parser.WatchRoot{{
			Path: root,
		}}},
		changedSources: []parser.SourceRef{{
			Provider:       parser.AgentClaude,
			Key:            storedPath,
			DisplayPath:    storedPath,
			FingerprintKey: storedPath,
			ProjectHint:    "demo",
		}},
	}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationShadowCompare,
		},
	})

	files := engine.classifyPaths([]string{eventPath})

	require.Len(t, provider.changedRequests, 1)
	assert.Equal(t, eventPath, provider.changedRequests[0].Path)
	assert.Equal(t, root, provider.changedRequests[0].WatchRoot)
	assert.Equal(t, []string{storedPath}, provider.changedRequests[0].StoredSourcePaths)
	require.Len(t, files, 1)
	assert.Equal(t, storedPath, files[0].Path)
	assert.Equal(t, "demo", files[0].Project)
	assert.True(t, files[0].ForceParse)
	assert.False(t, files[0].ProviderProcess)
	require.NotNil(t, files[0].ProviderSource)
	assert.Equal(t, storedPath, files[0].ProviderSource.DisplayPath)
}

func TestClassifyProviderChangedPathRunsAlongsideLegacyClassifier(
	t *testing.T,
) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-dev-code-demo", "shadow-recognized.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	require.NoError(t, os.WriteFile(
		sourcePath,
		[]byte(testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON(
				"legacy already recognizes this",
				"2026-06-01T10:00:00Z",
				"/Users/dev/code/demo",
			),
		)),
		0o644,
	))
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentClaude,
					DisplayName: "Claude Code",
				},
			},
		},
		watchPlan: parser.WatchPlan{Roots: []parser.WatchRoot{{
			Path: root,
		}}},
		changedSources: []parser.SourceRef{{
			Provider:       parser.AgentClaude,
			Key:            sourcePath,
			DisplayPath:    sourcePath,
			FingerprintKey: sourcePath,
			ProjectHint:    "provider-project",
		}},
	}
	engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationShadowCompare,
		},
	})

	files := engine.classifyPaths([]string{sourcePath})

	require.Len(t, provider.changedRequests, 1)
	assert.Equal(t, sourcePath, provider.changedRequests[0].Path)
	require.Len(t, files, 1)
	assert.Equal(t, sourcePath, files[0].Path)
	assert.True(t, files[0].ForceParse)
	assert.False(t, files[0].ProviderProcess)
	require.NotNil(t, files[0].ProviderSource)
	assert.Equal(t, sourcePath, files[0].ProviderSource.DisplayPath)
}

func TestClassifyProviderChangedPathMarksAuthoritativeProviderProcess(
	t *testing.T,
) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-dev-code-demo", "auth-recognized.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	require.NoError(t, os.WriteFile(
		sourcePath,
		[]byte(testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON(
				"authoritative provider owns this",
				"2026-06-01T10:00:00Z",
				"/Users/dev/code/demo",
			),
		)),
		0o644,
	))
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentClaude,
					DisplayName: "Claude Code",
				},
			},
		},
		watchPlan: parser.WatchPlan{Roots: []parser.WatchRoot{{
			Path: root,
		}}},
		changedSources: []parser.SourceRef{{
			Provider:       parser.AgentClaude,
			Key:            sourcePath,
			DisplayPath:    sourcePath,
			FingerprintKey: sourcePath,
		}},
	}
	engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	files := engine.classifyPaths([]string{sourcePath})

	require.Len(t, provider.changedRequests, 1)
	assert.Equal(t, sourcePath, provider.changedRequests[0].Path)
	require.Len(t, files, 1)
	assert.Equal(t, sourcePath, files[0].Path)
	assert.True(t, files[0].ProviderProcess)
	assert.False(t, files[0].ForceParse)
	require.NotNil(t, files[0].ProviderSource)
	assert.Equal(t, sourcePath, files[0].ProviderSource.DisplayPath)
}

func TestDiscoverProviderSourcesOnlyRunsAuthoritativeProviders(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "provider-only.jsonl")
	source := parser.SourceRef{
		Provider:       parser.AgentClaude,
		Key:            sourcePath,
		DisplayPath:    sourcePath,
		FingerprintKey: sourcePath,
		ProjectHint:    "provider-project",
	}
	makeEngine := func(mode parser.ProviderMigrationMode) (*Engine, *shadowCallerProvider) {
		t.Helper()
		provider := &shadowCallerProvider{
			shadowTestProvider: shadowTestProvider{
				ProviderBase: parser.ProviderBase{
					Def: parser.AgentDef{
						Type:        parser.AgentClaude,
						DisplayName: "Claude Code",
					},
				},
			},
			discoverSources: []parser.SourceRef{source},
		}
		engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentClaude: {root},
			},
			Machine: "devbox",
			ProviderFactories: []parser.ProviderFactory{
				shadowCallerFactory{provider: provider},
			},
			ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
				parser.AgentClaude: mode,
			},
		})
		return engine, provider
	}

	shadowEngine, shadowProvider := makeEngine(parser.ProviderMigrationShadowCompare)
	files, failures := shadowEngine.discoverProviderSources(context.Background(), nil)
	assert.Empty(t, files)
	assert.Zero(t, failures)
	assert.Empty(t, shadowProvider.calls)

	authoritativeEngine, authoritativeProvider := makeEngine(
		parser.ProviderMigrationProviderAuthoritative,
	)
	files, failures = authoritativeEngine.discoverProviderSources(context.Background(), nil)
	require.Len(t, files, 1)
	assert.Zero(t, failures)
	assert.Equal(t, []string{"discover"}, authoritativeProvider.calls)
	assert.Equal(t, sourcePath, files[0].Path)
	assert.Equal(t, "provider-project", files[0].Project)
	assert.True(t, files[0].ProviderProcess)
	require.NotNil(t, files[0].ProviderSource)
	assert.Equal(t, source, *files[0].ProviderSource)
}

func TestSyncAllProviderDiscoveryFailureSkipsFinishedWatermark(t *testing.T) {
	root := t.TempDir()
	discoverErr := errors.New("provider discovery failed")
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentClaude,
					DisplayName: "Claude Code",
				},
			},
		},
		discoverErr: discoverErr,
	}
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	stats := engine.SyncAll(context.Background(), nil)

	assert.Equal(t, []string{"discover"}, provider.calls)
	assert.Equal(t, 1, stats.Failed)
	started, err := database.GetSyncState(syncStateStartedAt)
	require.NoError(t, err)
	assert.NotEmpty(t, started)
	finished, err := database.GetSyncState(syncStateFinishedAt)
	require.NoError(t, err)
	assert.Empty(t, finished)
}

func TestFindSourceFileFallsBackToAuthoritativeNonFileProvider(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "forge.db") + "#session-a"
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentForge,
					DisplayName: "Forge",
				},
			},
		},
		source: parser.SourceRef{
			Provider:       parser.AgentForge,
			Key:            sourcePath,
			DisplayPath:    sourcePath,
			FingerprintKey: sourcePath,
		},
	}
	engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentForge: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentForge: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	found := engine.FindSourceFile("forge:session-a")

	assert.Equal(t, sourcePath, found)
	assert.Equal(t, "session-a", provider.findRequest.RawSessionID)
	assert.Equal(t, "forge:session-a", provider.findRequest.FullSessionID)
}

func TestProviderVirtualSourceBackedByEventPreservesHashInDBPath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state#prod", "sessions.db")
	sourcePath := dbPath + "#session-a"

	assert.True(t, providerVirtualSourceBackedByEvent(sourcePath, dbPath))
	assert.True(t, providerVirtualSourceBackedByEvent(sourcePath, dbPath+"-wal"))
	assert.True(t, providerVirtualSourceBackedByEvent(sourcePath, dbPath+"-shm"))
	assert.False(t, providerVirtualSourceBackedByEvent(sourcePath, filepath.Dir(dbPath)))
}

func TestParseDiffProviderDiscoveryErrorFails(t *testing.T) {
	root := t.TempDir()
	discoverErr := errors.New("discover failed")
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentCodex,
					DisplayName: "Codex",
				},
			},
		},
		discoverErr: discoverErr,
	}
	engine := NewDiffEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCodex: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	report, err := engine.ParseDiff(context.Background(), ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentCodex},
	})

	require.Error(t, err)
	assert.Nil(t, report)
	assert.ErrorContains(t, err, "parse-diff codex provider discovery")
	assert.ErrorIs(t, err, discoverErr)
}

func TestProcessFileShadowRecordsCachedSkipAsNotComparable(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-dev-code-demo", "shadow-skip.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	require.NoError(t, os.WriteFile(
		sourcePath,
		[]byte(testjsonl.JoinJSONL(
			testjsonl.ClaudeUserJSON(
				"already cached",
				"2026-06-01T10:00:00Z",
				"/Users/dev/code/demo",
			),
		)),
		0o644,
	))
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)

	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentCowork,
					DisplayName: "Claude Cowork",
				},
			},
		},
		source: parser.SourceRef{
			Provider: parser.AgentCowork,
			Key:      sourcePath,
		},
	}
	var comparisons []ProviderShadowComparison
	engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCowork: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCowork: parser.ProviderMigrationShadowCompare,
		},
		ProviderShadowRecorder: func(comparison ProviderShadowComparison) {
			comparisons = append(comparisons, comparison)
		},
	})
	engine.InjectSkipCache(map[string]int64{
		sourcePath: info.ModTime().UnixNano(),
	})

	result := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentCowork,
	})

	require.True(t, result.skip)
	require.Len(t, comparisons, 1)
	assert.Equal(t, "legacy skip", comparisons[0].NotComparableReason)
	assert.Empty(t, comparisons[0].Mismatches)
	assert.Empty(t, provider.calls)
}

func TestProcessFileProviderAuthoritativeUsesInjectedProvider(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "provider-owned.jsonl")
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o644))
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)

	source := parser.SourceRef{
		Provider:       parser.AgentClaude,
		Key:            sourcePath,
		DisplayPath:    sourcePath,
		FingerprintKey: sourcePath,
		ProjectHint:    "provider-project",
	}
	providerResult := parser.ParseResult{
		Session: parser.ParsedSession{
			ID:      "provider-owned",
			Project: "provider-project",
			Agent:   parser.AgentClaude,
			Machine: "devbox",
			File: parser.FileInfo{
				Path:  sourcePath,
				Mtime: info.ModTime().UnixNano(),
			},
		},
		Messages: []parser.ParsedMessage{{
			Role:      parser.RoleUser,
			Content:   "parsed through provider",
			Timestamp: info.ModTime(),
			Ordinal:   0,
		}},
	}
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentClaude,
					DisplayName: "Claude Code",
				},
			},
			fingerprint: parser.SourceFingerprint{
				Key:     sourcePath + "#fingerprint",
				Size:    info.Size(),
				MTimeNS: info.ModTime().UnixNano(),
			},
			outcome: parser.ParseOutcome{
				Results: []parser.ParseResultOutcome{{
					Result:      providerResult,
					DataVersion: parser.DataVersionCurrent,
				}},
				ResultSetComplete: true,
				ForceReplace:      true,
			},
		},
		source: source,
	}
	engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	result := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentClaude,
	})

	require.NoError(t, result.err)
	require.Len(t, result.results, 1)
	assert.Equal(t, "provider-owned", result.results[0].Session.ID)
	assert.Equal(t, "provider-project", result.results[0].Session.Project)
	assert.Equal(t, info.ModTime().UnixNano(), result.mtime)
	assert.True(t, result.forceReplace)
	assert.Equal(t, []string{"fingerprint", "parse"}, provider.calls)
}

func TestProcessFileProviderAuthoritativeSkipsFreshClaudeBeforeFingerprint(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "-Users-dev-code-demo", "fresh.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o644))
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)

	source := parser.SourceRef{
		Provider:       parser.AgentClaude,
		Key:            sourcePath,
		DisplayPath:    sourcePath,
		FingerprintKey: sourcePath,
		ProjectHint:    "demo",
	}

	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentClaude,
					DisplayName: "Claude Code",
				},
				Caps: parser.Capabilities{
					Source: parser.SourceCapabilities{
						IncrementalAppend: parser.CapabilitySupported,
					},
				},
			},
		},
		source: source,
	}
	database := dbtest.OpenTestDB(t)
	filePath := sourcePath
	fileSize := info.Size()
	fileMtime := info.ModTime().UnixNano()
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        "fresh",
		Project:   "demo",
		Machine:   "devbox",
		Agent:     string(parser.AgentClaude),
		FilePath:  &filePath,
		FileSize:  &fileSize,
		FileMtime: &fileMtime,
	}))
	require.NoError(t, database.SetSessionDataVersion("fresh", db.CurrentDataVersion()))
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	result := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentClaude,
	})

	require.NoError(t, result.err)
	assert.True(t, result.skip)
	assert.Equal(t, fileMtime, result.mtime)
	assert.Empty(t, provider.calls)
	assert.Equal(t, sourcePath, provider.findRequest.StoredFilePath)
}

func TestProcessFileProviderAuthoritativeSkipsFreshCoworkBeforeFingerprint(t *testing.T) {
	root := t.TempDir()
	database := dbtest.OpenTestDB(t)
	sourcePath, sourceMtime := writeFreshCoworkProviderSource(
		t, root, database, "fresh-session",
	)
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentCowork,
					DisplayName: "Claude Cowork",
				},
			},
		},
		source: parser.SourceRef{
			Provider:       parser.AgentCowork,
			Key:            sourcePath,
			DisplayPath:    sourcePath,
			FingerprintKey: sourcePath,
		},
	}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCowork: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	result := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentCowork,
	})

	require.NoError(t, result.err)
	assert.True(t, result.skip)
	assert.Equal(t, sourceMtime, result.mtime)
	assert.Empty(t, provider.calls)
}

func TestProcessFileProviderAuthoritativeSkipsFreshGeminiBeforeFingerprint(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(
		root, "tmp", "alias", "chats", "session-001.json",
	)
	sourceMtime := writeFreshProviderDBSession(
		t, sourcePath, nil,
	)
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentGemini,
					DisplayName: "Gemini CLI",
				},
			},
		},
		source: parser.SourceRef{
			Provider:       parser.AgentGemini,
			Key:            sourcePath,
			DisplayPath:    sourcePath,
			FingerprintKey: sourcePath,
		},
	}
	engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGemini: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentGemini: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	requireFreshProviderSession(t, engine.db, parser.AgentGemini, sourcePath, sourceMtime)

	result := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentGemini,
	})

	require.NoError(t, result.err)
	assert.True(t, result.skip)
	assert.Equal(t, sourceMtime, result.mtime)
	assert.Empty(t, provider.calls)
}

func TestProcessFileProviderAuthoritativeForceParseBypassesFreshCoworkSkip(t *testing.T) {
	root := t.TempDir()
	database := dbtest.OpenTestDB(t)
	sourcePath, sourceMtime := writeFreshCoworkProviderSource(
		t, root, database, "force-session",
	)
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentCowork,
					DisplayName: "Claude Cowork",
				},
			},
			fingerprint: parser.SourceFingerprint{
				Key:     sourcePath,
				MTimeNS: sourceMtime,
			},
			outcome: parser.ParseOutcome{
				ResultSetComplete: true,
			},
		},
		source: parser.SourceRef{
			Provider:       parser.AgentCowork,
			Key:            sourcePath,
			DisplayPath:    sourcePath,
			FingerprintKey: sourcePath,
		},
	}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCowork: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	result := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:       sourcePath,
		Agent:      parser.AgentCowork,
		ForceParse: true,
	})

	require.NoError(t, result.err)
	assert.False(t, result.skip)
	assert.Equal(t, []string{"fingerprint", "parse"}, provider.calls)
	assert.True(t, provider.parseRequest.ForceParse)
}

func TestProcessFileProviderAuthoritativeSkipsFreshCopilotBeforeFingerprint(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(
		root, "session-state", "copilot-fresh", "events.jsonl",
	)
	workspacePath := filepath.Join(filepath.Dir(sourcePath), "workspace.yaml")
	sourceMtime := writeFreshProviderDBSession(
		t, sourcePath, &workspacePath,
	)
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentCopilot,
					DisplayName: "Copilot CLI",
				},
			},
		},
		source: parser.SourceRef{
			Provider:       parser.AgentCopilot,
			Key:            sourcePath,
			DisplayPath:    sourcePath,
			FingerprintKey: sourcePath,
		},
	}
	engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCopilot: {root},
		},
		Machine: "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCopilot: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	requireFreshProviderSession(t, engine.db, parser.AgentCopilot, sourcePath, sourceMtime)

	result := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentCopilot,
	})

	require.NoError(t, result.err)
	assert.True(t, result.skip)
	assert.Equal(t, sourceMtime, result.mtime)
	assert.Empty(t, provider.calls)
}

func TestProcessFileProviderAuthoritativeKeepsRetryStatePerResult(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "multi-provider-owned.jsonl")
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o644))
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)

	source := parser.SourceRef{
		Provider:       parser.AgentClaude,
		Key:            sourcePath,
		DisplayPath:    sourcePath,
		FingerprintKey: sourcePath,
	}
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentClaude,
					DisplayName: "Claude Code",
				},
			},
			fingerprint: parser.SourceFingerprint{
				Key:     sourcePath,
				Size:    info.Size(),
				MTimeNS: info.ModTime().UnixNano(),
			},
			outcome: parser.ParseOutcome{
				Results: []parser.ParseResultOutcome{
					{
						Result: parser.ParseResult{Session: parser.ParsedSession{
							ID: "provider-current", Agent: parser.AgentClaude,
						}},
						DataVersion: parser.DataVersionCurrent,
					},
					{
						Result: parser.ParseResult{Session: parser.ParsedSession{
							ID: "provider-retry", Agent: parser.AgentClaude,
						}},
						DataVersion: parser.DataVersionNeedsRetry,
					},
				},
				ResultSetComplete: true,
			},
		},
		source: source,
	}
	engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	result := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentClaude,
	})

	require.NoError(t, result.err)
	require.Len(t, result.results, 2)
	assert.False(t, result.needsRetryForSession("provider-current"))
	assert.True(t, result.needsRetryForSession("provider-retry"))
}

func TestProcessFileProviderAuthoritativeSuppressesUncleanSkipCache(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "unclean-provider-owned.jsonl")
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o644))
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)

	source := parser.SourceRef{
		Provider:       parser.AgentClaude,
		Key:            sourcePath,
		DisplayPath:    sourcePath,
		FingerprintKey: sourcePath + "#source-key",
	}
	makeEngine := func(outcome parser.ParseOutcome, parseErr error) *Engine {
		t.Helper()
		provider := &shadowCallerProvider{
			shadowTestProvider: shadowTestProvider{
				ProviderBase: parser.ProviderBase{
					Def: parser.AgentDef{
						Type:        parser.AgentClaude,
						DisplayName: "Claude Code",
					},
				},
				fingerprint: parser.SourceFingerprint{
					Key:     sourcePath + "#fingerprint",
					Size:    info.Size(),
					MTimeNS: info.ModTime().UnixNano(),
				},
				outcome:  outcome,
				parseErr: parseErr,
			},
			source: source,
		}
		return NewEngine(dbtest.OpenTestDB(t), EngineConfig{
			AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
			Machine:   "devbox",
			ProviderFactories: []parser.ProviderFactory{
				shadowCallerFactory{provider: provider},
			},
			ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
				parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
			},
		})
	}

	tests := []struct {
		name     string
		outcome  parser.ParseOutcome
		parseErr error
		wantErr  bool
	}{
		{
			name:    "whole source parse error",
			wantErr: true,
			parseErr: errors.New(
				"provider source failed",
			),
		},
		{
			name: "incomplete empty result set",
			outcome: parser.ParseOutcome{
				ResultSetComplete: false,
			},
		},
		{
			name: "source error",
			outcome: parser.ParseOutcome{
				ResultSetComplete: true,
				SourceErrors: []parser.SourceError{{
					SourceKey: sourcePath,
					Err:       errors.New("session failed"),
				}},
			},
		},
		{
			name: "retry result",
			outcome: parser.ParseOutcome{
				ResultSetComplete: true,
				Results: []parser.ParseResultOutcome{{
					Result: parser.ParseResult{Session: parser.ParsedSession{
						ID: "provider-retry", Agent: parser.AgentClaude,
					}},
					DataVersion: parser.DataVersionNeedsRetry,
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := makeEngine(tt.outcome, tt.parseErr)

			result := engine.processFile(context.Background(), parser.DiscoveredFile{
				Path:  sourcePath,
				Agent: parser.AgentClaude,
			})

			if tt.wantErr {
				require.Error(t, result.err)
			} else {
				require.NoError(t, result.err)
			}
			assert.True(t, result.cacheSkip)
			assert.True(t, result.noCacheSkip)

			stats := engine.collectAndBatch(
				context.Background(),
				singleSyncJob(syncJob{processResult: result, path: sourcePath}),
				1,
				1,
				nil,
				syncWriteDefault,
			)
			if tt.wantErr {
				assert.Equal(t, 1, stats.Failed)
			}
			cache := engine.SnapshotSkipCache()
			assert.NotContains(t, cache, sourcePath+"#source-key")
			assert.NotContains(t, cache, sourcePath)
		})
	}
}

func TestSyncSingleSessionProviderAuthoritativeBypassesProviderSkipCache(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "single-provider-owned.jsonl")
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o644))
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)

	sourceKey := sourcePath + "#source-key"
	providerResult := parser.ParseResult{
		Session: parser.ParsedSession{
			ID:           "provider-owned",
			Project:      "provider-project",
			Agent:        parser.AgentClaude,
			Machine:      "devbox",
			StartedAt:    info.ModTime(),
			EndedAt:      info.ModTime(),
			MessageCount: 1,
			File: parser.FileInfo{
				Path:  sourcePath,
				Mtime: info.ModTime().UnixNano(),
			},
		},
		Messages: []parser.ParsedMessage{{
			Role:      parser.RoleUser,
			Content:   "explicit provider resync",
			Timestamp: info.ModTime(),
			Ordinal:   0,
		}},
	}
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentClaude,
					DisplayName: "Claude Code",
				},
			},
			fingerprint: parser.SourceFingerprint{
				Key:     sourcePath + "#fingerprint",
				Size:    info.Size(),
				MTimeNS: info.ModTime().UnixNano(),
			},
			outcome: parser.ParseOutcome{
				Results: []parser.ParseResultOutcome{{
					Result:      providerResult,
					DataVersion: parser.DataVersionCurrent,
				}},
				ResultSetComplete: true,
			},
		},
		source: parser.SourceRef{
			Provider:       parser.AgentClaude,
			Key:            sourcePath,
			DisplayPath:    sourcePath,
			FingerprintKey: sourceKey,
			ProjectHint:    "provider-project",
		},
	}
	database := dbtest.OpenTestDB(t)
	filePath := sourcePath
	fileSize := info.Size()
	fileMtime := info.ModTime().UnixNano()
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        "provider-owned",
		Project:   "old-project",
		Machine:   "devbox",
		Agent:     string(parser.AgentClaude),
		FilePath:  &filePath,
		FileSize:  &fileSize,
		FileMtime: &fileMtime,
	}))
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	engine.InjectSkipCache(map[string]int64{
		sourceKey: info.ModTime().UnixNano(),
	})

	require.NoError(t, engine.SyncSingleSession("provider-owned"))

	assert.Equal(t, []string{"fingerprint", "parse"}, provider.calls)
	assert.True(t, provider.parseRequest.ForceParse)
	cache := engine.SnapshotSkipCache()
	assert.NotContains(t, cache, sourceKey)
}

func singleSyncJob(job syncJob) <-chan syncJob {
	results := make(chan syncJob, 1)
	results <- job
	close(results)
	return results
}

func TestProcessFileProviderAuthoritativeForceParseAllowsStaleSourceLookup(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "force-provider-owned.jsonl")
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o644))
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)

	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentClaude,
					DisplayName: "Claude Code",
				},
			},
			fingerprint: parser.SourceFingerprint{
				Key:     sourcePath,
				Size:    info.Size(),
				MTimeNS: info.ModTime().UnixNano(),
			},
			outcome: parser.ParseOutcome{ResultSetComplete: true},
		},
		source: parser.SourceRef{
			Provider:       parser.AgentClaude,
			Key:            sourcePath,
			DisplayPath:    sourcePath,
			FingerprintKey: sourcePath + "#source-key",
		},
	}
	engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	engine.forceParse = true

	result := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentClaude,
	})

	require.NoError(t, result.err)
	assert.False(t, provider.findRequest.RequireFreshSource)
	assert.True(t, provider.parseRequest.ForceParse)
}

func TestProcessFileProviderAuthoritativeNotFoundFails(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "missing-provider-owned.jsonl")
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o644))
	found := false
	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentClaude,
					DisplayName: "Claude Code",
				},
			},
		},
		findFound: &found,
	}
	engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	result := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentClaude,
	})

	require.Error(t, result.err)
	assert.Contains(t, result.err.Error(), "provider source not found")
	assert.Empty(t, provider.calls)
}

func TestProcessFileProviderAuthoritativeTranslatesSkipReason(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "skip-provider-owned.jsonl")
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o644))
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)

	provider := &shadowCallerProvider{
		shadowTestProvider: shadowTestProvider{
			ProviderBase: parser.ProviderBase{
				Def: parser.AgentDef{
					Type:        parser.AgentClaude,
					DisplayName: "Claude Code",
				},
			},
			fingerprint: parser.SourceFingerprint{
				Key:     sourcePath,
				Size:    info.Size(),
				MTimeNS: info.ModTime().UnixNano(),
			},
			outcome: parser.ParseOutcome{
				ResultSetComplete: true,
				SkipReason:        parser.SkipNoSession,
			},
		},
		source: parser.SourceRef{
			Provider:       parser.AgentClaude,
			Key:            sourcePath,
			DisplayPath:    sourcePath,
			FingerprintKey: sourcePath + "#source-key",
		},
	}
	engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "devbox",
		ProviderFactories: []parser.ProviderFactory{
			shadowCallerFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentClaude: parser.ProviderMigrationProviderAuthoritative,
		},
	})

	result := engine.processFile(context.Background(), parser.DiscoveredFile{
		Path:  sourcePath,
		Agent: parser.AgentClaude,
	})

	require.NoError(t, result.err)
	assert.True(t, result.skip)
	assert.True(t, result.cacheSkip)
	assert.Equal(t, sourcePath+"#source-key", result.cacheKey)
	assert.Equal(t, info.ModTime().UnixNano(), result.mtime)
	assert.Empty(t, result.results)

	results := make(chan syncJob, 1)
	results <- syncJob{
		processResult: result,
		path:          sourcePath,
	}
	close(results)
	stats := engine.collectAndBatch(context.Background(), results, 1, 1, nil, syncWriteDefault)

	assert.Equal(t, 1, stats.Skipped)
	cache := engine.SnapshotSkipCache()
	assert.Equal(t, info.ModTime().UnixNano(), cache[sourcePath+"#source-key"])
	_, cachedByPath := cache[sourcePath]
	assert.False(t, cachedByPath)

	cleanResult := processResult{
		results: []parser.ParseResult{{
			Session: parser.ParsedSession{
				ID:        "provider-clean",
				Project:   "provider-project",
				Agent:     parser.AgentClaude,
				Machine:   "devbox",
				StartedAt: info.ModTime(),
				EndedAt:   info.ModTime(),
				File: parser.FileInfo{
					Path:  sourcePath,
					Mtime: info.ModTime().UnixNano(),
				},
			},
		}},
		mtime:     info.ModTime().UnixNano(),
		cacheSkip: true,
		cacheKey:  sourcePath + "#source-key",
	}
	stats = engine.collectAndBatch(
		context.Background(),
		singleSyncJob(syncJob{processResult: cleanResult, path: sourcePath}),
		1,
		1,
		nil,
		syncWriteDefault,
	)

	assert.Equal(t, 1, stats.Synced)
	cache = engine.SnapshotSkipCache()
	assert.NotContains(t, cache, sourcePath+"#source-key")
	assert.NotContains(t, cache, sourcePath)
}

type shadowCallerProvider struct {
	shadowTestProvider
	source          parser.SourceRef
	findRequest     parser.FindSourceRequest
	findFound       *bool
	watchPlan       parser.WatchPlan
	changedSources  []parser.SourceRef
	changedRequests []parser.ChangedPathRequest
	changedErr      error
	discoverSources []parser.SourceRef
	discoverErr     error
}

func (p *shadowCallerProvider) Discover(
	context.Context,
) ([]parser.SourceRef, error) {
	p.calls = append(p.calls, "discover")
	if p.discoverErr != nil {
		return nil, p.discoverErr
	}
	return append([]parser.SourceRef(nil), p.discoverSources...), nil
}

func (p *shadowCallerProvider) FindSource(
	_ context.Context,
	req parser.FindSourceRequest,
) (parser.SourceRef, bool, error) {
	p.findRequest = req
	if p.findFound != nil && !*p.findFound {
		return parser.SourceRef{}, false, nil
	}
	return p.source, true, nil
}

func (p *shadowCallerProvider) WatchPlan(
	context.Context,
) (parser.WatchPlan, error) {
	return p.watchPlan, nil
}

func (p *shadowCallerProvider) SourcesForChangedPath(
	_ context.Context,
	req parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	p.changedRequests = append(p.changedRequests, req)
	if p.changedErr != nil {
		return nil, p.changedErr
	}
	return append([]parser.SourceRef(nil), p.changedSources...), nil
}

type shadowCallerFactory struct {
	provider *shadowCallerProvider
}

func (f shadowCallerFactory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f shadowCallerFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f shadowCallerFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

func writeFreshCoworkProviderSource(
	t *testing.T,
	root string,
	database *db.DB,
	rawSessionID string,
) (string, int64) {
	t.Helper()

	sessionDir := filepath.Join(root, "org", "workspace", "local_fresh")
	projectDir := filepath.Join(sessionDir, ".claude", "projects", "-demo")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	metaPath := sessionDir + ".json"
	sourcePath := filepath.Join(projectDir, rawSessionID+".jsonl")
	require.NoError(t, os.WriteFile(metaPath, []byte(`{"title":"Fresh"}`), 0o644))
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o644))

	transcriptTime := time.Unix(1_781_475_210, 0)
	metaTime := transcriptTime.Add(time.Second)
	require.NoError(t, os.Chtimes(sourcePath, transcriptTime, transcriptTime))
	require.NoError(t, os.Chtimes(metaPath, metaTime, metaTime))
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)
	sourceSize := info.Size()
	sourceMtime := parser.CoworkSessionMtime(sourcePath, info.ModTime().UnixNano())
	require.Equal(t, metaTime.UnixNano(), sourceMtime)

	fullSessionID := "cowork:" + rawSessionID
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        fullSessionID,
		Project:   "cowork-project",
		Machine:   "devbox",
		Agent:     string(parser.AgentCowork),
		FilePath:  &sourcePath,
		FileSize:  &sourceSize,
		FileMtime: &sourceMtime,
	}))
	require.NoError(t, database.SetSessionDataVersion(
		fullSessionID, db.CurrentDataVersion(),
	))

	return sourcePath, sourceMtime
}

func writeFreshProviderDBSession(
	t *testing.T,
	sourcePath string,
	mtimeSidecar *string,
) int64 {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	require.NoError(t, os.WriteFile(sourcePath, []byte("{}\n"), 0o644))
	sourceTime := time.Unix(1_781_475_210, 0)
	require.NoError(t, os.Chtimes(sourcePath, sourceTime, sourceTime))
	mtime := sourceTime.UnixNano()
	if mtimeSidecar != nil {
		sidecarTime := sourceTime.Add(time.Second)
		require.NoError(t, os.WriteFile(*mtimeSidecar, []byte("name: Fresh\n"), 0o644))
		require.NoError(t, os.Chtimes(*mtimeSidecar, sidecarTime, sidecarTime))
		mtime = sidecarTime.UnixNano()
	}

	return mtime
}

func requireFreshProviderSession(
	t *testing.T,
	database *db.DB,
	agent parser.AgentType,
	sourcePath string,
	sourceMtime int64,
) {
	t.Helper()

	info, err := os.Stat(sourcePath)
	require.NoError(t, err)
	sourceSize := info.Size()
	fullSessionID := string(agent) + ":fresh"
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        fullSessionID,
		Project:   "provider-project",
		Machine:   "devbox",
		Agent:     string(agent),
		FilePath:  &sourcePath,
		FileSize:  &sourceSize,
		FileMtime: &sourceMtime,
	}))
	require.NoError(t, database.SetSessionDataVersion(
		fullSessionID, db.CurrentDataVersion(),
	))
}
