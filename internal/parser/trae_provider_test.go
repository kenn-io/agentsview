package parser

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTraeDB(t *testing.T, path, value string, extraKey string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO ItemTable(key, value) VALUES (?, ?), (?, ?)`, traeStorageKey, value, extraKey, `{"list":[{"sessionId":"ignored","messages":[{"role":"user","content":"wrong"}]}]`)
	require.NoError(t, err)
}

func setTraeDBValue(t *testing.T, path, value string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`UPDATE ItemTable SET value = ? WHERE key = ?`, value, traeStorageKey)
	require.NoError(t, err)
}

func traeFixtureValue(t *testing.T) string {
	t.Helper()
	value := map[string]any{"list": []any{map[string]any{
		"sessionId": "session-1", "createdAt": 1715340600000, "updatedAt": 1715340900000, "model": "trae-model",
		"messages": []any{
			map[string]any{"role": "user", "content": "first", "turnIndex": 0},
			map[string]any{"role": "assistant", "content": "", "agentTaskContent": map[string]any{"content": "fallback", "guideline": map[string]any{"planItems": []any{map[string]any{"content": "ignored after direct"}}}}, "turnIndex": 1},
		},
	}}}
	return traeStoreValue(t, value["list"].([]any))
}

func traeStoreValue(t *testing.T, list []any) string {
	t.Helper()
	value := map[string]any{"list": list}
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return string(data)
}

func TestTraeRegistryMetadata(t *testing.T) {
	def, ok := AgentByType(AgentTrae)
	require.True(t, ok)
	assert.Equal(t, "Trae", def.DisplayName)
	assert.Equal(t, "TRAE_DIR", def.EnvVar)
	assert.Equal(t, "trae_dirs", def.ConfigKey)
	assert.Equal(t, "trae:", def.IDPrefix)
	assert.Equal(t, []string{"workspaceStorage", "globalStorage"}, def.WatchSubdirs)
	assert.True(t, def.Usage.NoPerMessageTokenData)
	assert.False(t, def.Usage.AICreditsDenominated)
	assert.Contains(t, def.DefaultDirs, "AppData/Roaming/TRAE SOLO CN/User")
}

func TestTraeWorkspaceGlobalDiscoveryAndParsing(t *testing.T) {
	root := t.TempDir()
	workspaceDB := filepath.Join(root, "workspaceStorage", "hash", traeStateDBName)
	globalDB := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, workspaceDB, traeFixtureValue(t), "memento/unrelated-chat-storage")
	writeTraeDB(t, globalDB, traeFixtureValue(t), "memento/unrelated-chat-storage")
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(workspaceDB), "workspace.json"), []byte(`{"folder":"file:///tmp/project"}`), 0o644))

	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(t, ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)

	for _, source := range sources {
		if strings.Contains(source.Key, "workspaceStorage") {
			assert.Equal(t, "project", source.ProjectHint)
		}
		assert.NotContains(t, source.Key, "#session-1")
		outcome, err := provider.Parse(context.Background(), ParseRequest{Source: source})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		result := outcome.Results[0].Result
		assert.Equal(t, AgentTrae, result.Session.Agent)
		assert.Equal(t, "trae:session-1", result.Session.ID)
		assert.Equal(t, []string{"first", "fallback"}, []string{result.Messages[0].Content, result.Messages[1].Content})
		assert.Equal(t, "trae-model", result.Messages[1].Model)
		assert.Empty(t, result.UsageEvents)
		assert.False(t, result.Messages[1].HasOutputTokens)
		assert.Equal(t, RelationshipType(""), result.Session.RelationshipType)
	}
}

func TestTraeWatchChangedPathAndVirtualLookup(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, dbPath, traeFixtureValue(t), "memento/unrelated-chat-storage")
	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(t, ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 2)
	sources, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{WatchRoot: filepath.Join(root, "globalStorage"), Path: dbPath})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	_, _, ok = SplitTraeVirtualPath(sources[0].Key)
	assert.False(t, ok)
	virtual := traeVirtualPath(dbPath, "session-1")
	var found SourceRef
	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{StoredFilePath: sources[0].Key})
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, sources[0].Key, found.Key)
	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{StoredFilePath: virtual, RawSessionID: "session-1", RequireFreshSource: true})
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, virtual, found.Key)
	for _, name := range []string{traeStateDBName + "-wal"} {
		changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{WatchRoot: filepath.Join(root, "globalStorage"), Path: filepath.Join(root, "globalStorage", name)})
		require.NoError(t, err)
		assert.Len(t, changed, 1)
	}
}

func TestTraeWorkspaceChangedPathAndRawExport(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workspaceStorage", "hash", traeStateDBName)
	writeTraeDB(t, path, traeFixtureValue(t), "memento/unrelated-chat-storage")
	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(t, ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{WatchRoot: filepath.Join(root, "workspaceStorage"), Path: filepath.Join(root, "workspaceStorage", "hash", "workspace.json")})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	var exported bytes.Buffer
	require.NoError(t, WriteTraeSessionJSON(&exported, path, "session-1"))
	assert.Contains(t, exported.String(), `"sessionId":"session-1"`)
}

func TestTraeUnsupportedKeyNegativeSpace(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workspaceStorage", "hash", traeStateDBName)
	writeTraeDB(t, path, traeFixtureValue(t), "memento/unrelated-chat-storage")
	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(t, ok)
	sources, err := factory.NewProvider(ProviderConfig{Roots: []string{root}}).Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.NotContains(t, sources[0].Key, "ignored")
}

func TestTraeMalformedSessionEntryDoesNotBlockSiblingDiscovery(t *testing.T) {
	root := t.TempDir()
	workspaceDB := filepath.Join(root, "workspaceStorage", "hash", traeStateDBName)
	globalDB := filepath.Join(root, "globalStorage", traeStateDBName)
	workspaceValue := traeStoreValue(t, []any{
		map[string]any{
			"sessionId": "broken",
			"createdAt": "not-a-time",
			"messages":  []any{map[string]any{"role": "user", "content": "bad"}},
		},
		map[string]any{
			"sessionId": "workspace-good",
			"createdAt": 1715340600000,
			"messages":  []any{map[string]any{"role": "user", "content": "workspace"}},
		},
	})
	globalValue := traeStoreValue(t, []any{
		map[string]any{
			"sessionId": "global-good",
			"createdAt": 1715340600000,
			"messages":  []any{map[string]any{"role": "user", "content": "global"}},
		},
	})
	writeTraeDB(t, workspaceDB, workspaceValue, "memento/unrelated-chat-storage")
	writeTraeDB(t, globalDB, globalValue, "memento/unrelated-chat-storage")

	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(t, ok)
	sources, err := factory.NewProvider(ProviderConfig{Roots: []string{root}}).Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	for _, source := range sources {
		outcome, err := factory.NewProvider(ProviderConfig{Roots: []string{root}}).Parse(context.Background(), ParseRequest{Source: source})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
	}
}

func TestTraeMalformedStorageDoesNotBlockSiblingDiscovery(t *testing.T) {
	root := t.TempDir()
	workspaceDB := filepath.Join(root, "workspaceStorage", "hash", traeStateDBName)
	globalDB := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, workspaceDB, `{"list":[`, "memento/unrelated-chat-storage")
	writeTraeDB(t, globalDB, traeStoreValue(t, []any{
		map[string]any{
			"sessionId": "global-good",
			"createdAt": 1715340600000,
			"messages":  []any{map[string]any{"role": "user", "content": "global"}},
		},
	}), "memento/unrelated-chat-storage")

	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(t, ok)
	sources, err := factory.NewProvider(ProviderConfig{Roots: []string{root}}).Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 2)
	for _, source := range sources {
		outcome, err := factory.NewProvider(ProviderConfig{Roots: []string{root}}).Parse(context.Background(), ParseRequest{Source: source})
		if strings.Contains(source.Key, "globalStorage") {
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
		} else {
			require.Error(t, err)
		}
	}
}

func TestTraeValidEmptyStoreReturnsCompleteNoSessionOutcome(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, path, `{"list":[]}`, "memento/unrelated-chat-storage")

	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(t, ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	assert.Empty(t, outcome.Results)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.True(t, outcome.ForceReplace)
	assert.True(t, outcome.ResultSetComplete)
}

func TestTraeUnknownStoragePreservesArchiveUntilExplicitList(t *testing.T) {
	for _, value := range []string{`{}`, `{"list":null}`} {
		t.Run(value, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "globalStorage", traeStateDBName)
			writeTraeDB(t, path, value, "memento/unrelated-chat-storage")

			factory, ok := ProviderFactoryByType(AgentTrae)
			require.True(t, ok)
			provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
			sources, err := provider.Discover(context.Background())
			require.NoError(t, err)
			require.Len(t, sources, 1)

			outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
			require.NoError(t, err)
			assert.Empty(t, outcome.Results)
			assert.Equal(t, SkipNoSession, outcome.SkipReason)
			assert.False(t, outcome.ForceReplace)
			assert.False(t, outcome.ResultSetComplete)

			virtual := traeVirtualPath(path, "session-1")
			changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
				WatchRoot:         filepath.Join(root, "globalStorage"),
				Path:              path,
				StoredSourcePaths: []string{virtual},
			})
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, path, changed[0].Key)
		})
	}
}

func TestTraeRequireFreshSourceFallsBackToRawIDAfterStoredVirtualPathRelocates(t *testing.T) {
	root := t.TempDir()
	oldDB := filepath.Join(root, "globalStorage", traeStateDBName)
	newDB := filepath.Join(root, "workspaceStorage", "hash", traeStateDBName)
	writeTraeDB(t, oldDB, traeStoreValue(t, []any{
		map[string]any{
			"sessionId": "session-1",
			"createdAt": 1715340600000,
			"messages":  []any{map[string]any{"role": "user", "content": "old container"}},
		},
	}), "memento/unrelated-chat-storage")
	writeTraeDB(t, newDB, `{"list":[]}`, "memento/unrelated-chat-storage")

	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(t, ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     traeVirtualPath(oldDB, "session-1"),
		RawSessionID:       "session-1",
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, traeVirtualPath(oldDB, "session-1"), found.Key)

	setTraeDBValue(t, oldDB, traeStoreValue(t, []any{
		map[string]any{
			"sessionId": "other",
			"createdAt": 1715340600000,
			"messages":  []any{map[string]any{"role": "user", "content": "old container moved"}},
		},
	}))
	setTraeDBValue(t, newDB, traeStoreValue(t, []any{
		map[string]any{
			"sessionId": "session-1",
			"createdAt": 1715340600000,
			"messages":  []any{map[string]any{"role": "user", "content": "new container"}},
		},
	}))

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     traeVirtualPath(oldDB, "session-1"),
		RawSessionID:       "session-1",
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, traeVirtualPath(newDB, "session-1"), found.Key)
}

func TestTraeMalformedSessionEntryKeepsContainerIncomplete(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, path, traeStoreValue(t, []any{
		map[string]any{
			"sessionId": "broken",
			"createdAt": "not-a-time",
			"messages":  []any{map[string]any{"role": "user", "content": "bad"}},
		},
		map[string]any{
			"sessionId": "good",
			"createdAt": 1715340600000,
			"messages":  []any{map[string]any{"role": "user", "content": "good"}},
		},
	}), "memento/unrelated-chat-storage")

	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(t, ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "trae:good", outcome.Results[0].Result.Session.ID)
	assert.False(t, outcome.ResultSetComplete)

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		WatchRoot:         filepath.Join(root, "globalStorage"),
		Path:              path,
		StoredSourcePaths: []string{traeVirtualPath(path, "broken")},
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, path, changed[0].Key)
}

func TestTraeUnparseableSessionStatesKeepContainerIncomplete(t *testing.T) {
	cases := []struct {
		name    string
		session map[string]any
	}{
		{
			name: "empty content",
			session: map[string]any{
				"sessionId": "empty-content",
				"createdAt": 1715340600000,
				"messages":  []any{map[string]any{"role": "user", "content": "   "}},
			},
		},
		{
			name: "unknown role",
			session: map[string]any{
				"sessionId": "unknown-role",
				"createdAt": 1715340600000,
				"messages":  []any{map[string]any{"role": "system", "content": "ignored"}},
			},
		},
		{
			name: "partial init",
			session: map[string]any{
				"sessionId": "partial-init",
				"createdAt": 1715340600000,
				"messages":  []any{map[string]any{"role": "assistant", "content": "", "agentTaskContent": map[string]any{}}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "globalStorage", traeStateDBName)
			writeTraeDB(t, path, traeStoreValue(t, []any{
				tc.session,
				map[string]any{
					"sessionId": "good",
					"createdAt": 1715340600000,
					"messages":  []any{map[string]any{"role": "user", "content": "good"}},
				},
			}), "memento/unrelated-chat-storage")

			factory, ok := ProviderFactoryByType(AgentTrae)
			require.True(t, ok)
			provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
			sources, err := provider.Discover(context.Background())
			require.NoError(t, err)
			require.Len(t, sources, 1)

			outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			assert.Equal(t, "trae:good", outcome.Results[0].Result.Session.ID)
			assert.False(t, outcome.ResultSetComplete)

			virtual := traeVirtualPath(path, tc.session["sessionId"].(string))
			changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
				WatchRoot:         filepath.Join(root, "globalStorage"),
				Path:              path,
				StoredSourcePaths: []string{virtual},
			})
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, path, changed[0].Key)

			found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
				StoredFilePath:     virtual,
				RawSessionID:       tc.session["sessionId"].(string),
				RequireFreshSource: true,
			})
			require.NoError(t, err)
			assert.True(t, ok)
			_, err = provider.Parse(context.Background(), ParseRequest{Source: found})
			require.Error(t, err)
		})
	}
}

func TestTraeChangedPathTombstonesRefreshWarmMemberPresenceCache(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, dbPath, traeStoreValue(t, []any{
		map[string]any{
			"sessionId": "session-1",
			"createdAt": 1715340600000,
			"messages":  []any{map[string]any{"role": "user", "content": "cached"}},
		},
	}), "memento/unrelated-chat-storage")

	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(t, ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	virtual := traeVirtualPath(dbPath, "session-1")
	_, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     virtual,
		RawSessionID:       "session-1",
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)

	setTraeDBValue(t, dbPath, `{"list":[]}`)
	sources, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		WatchRoot:         filepath.Join(root, "globalStorage"),
		Path:              dbPath,
		StoredSourcePaths: []string{virtual},
	})
	require.NoError(t, err)
	var tombstone SourceRef
	for _, source := range sources {
		if source.Key == virtual {
			tombstone = source
		}
	}
	assert.Equal(t, virtual, tombstone.Key)
}

func TestTraeChangedPathTombstonesDecodeSnapshotOncePerContainer(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, dbPath, traeStoreValue(t, []any{
		map[string]any{
			"sessionId": "session-1",
			"createdAt": 1715340600000,
			"messages":  []any{map[string]any{"role": "user", "content": "one"}},
		},
		map[string]any{
			"sessionId": "session-2",
			"createdAt": 1715340600000,
			"messages":  []any{map[string]any{"role": "user", "content": "two"}},
		},
		map[string]any{
			"sessionId": "session-3",
			"createdAt": 1715340600000,
			"messages":  []any{map[string]any{"role": "user", "content": "three"}},
		},
	}), "memento/unrelated-chat-storage")

	setTraeDBValue(t, dbPath, traeStoreValue(t, []any{
		map[string]any{
			"sessionId": "session-1",
			"createdAt": 1715340600000,
			"messages":  []any{map[string]any{"role": "user", "content": "one"}},
		},
	}))

	orig := traeLoadSessionSnapshot
	defer func() { traeLoadSessionSnapshot = orig }()
	var decodes int
	traeLoadSessionSnapshot = func(path string) (traeSessionSnapshot, error) {
		decodes++
		return orig(path)
	}

	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(t, ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	stored := []string{
		traeVirtualPath(dbPath, "session-1"),
		traeVirtualPath(dbPath, "session-2"),
		traeVirtualPath(dbPath, "session-3"),
	}

	sources, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		WatchRoot:         filepath.Join(root, "globalStorage"),
		Path:              dbPath,
		StoredSourcePaths: stored,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, decodes)
	require.Len(t, sources, 3)
	assert.Equal(t, dbPath, sources[0].Key)
	assert.ElementsMatch(t, []string{
		dbPath,
		traeVirtualPath(dbPath, "session-2"),
		traeVirtualPath(dbPath, "session-3"),
	}, []string{sources[0].Key, sources[1].Key, sources[2].Key})
}

func TestTraeAssistantFallbackVariants(t *testing.T) {
	assert.Equal(t, "plain text", traeAssistantFallback(json.RawMessage(`"plain text"`)))
	assert.Equal(t, "text field", traeAssistantFallback(json.RawMessage(`{"text":"text field"}`)))
	assert.Equal(t, "proposal field", traeAssistantFallback(json.RawMessage(`{"proposal":"proposal field"}`)))
	assert.Equal(t, "step one\nstep two", traeAssistantFallback(json.RawMessage(`{"guideline":{"planItems":[{"content":"step one"},{"content":"step two"}]}}`)))
}

func TestTraeTimeUnmarshalVariants(t *testing.T) {
	var seconds traeTime
	require.NoError(t, json.Unmarshal([]byte(`1715340600`), &seconds))
	assert.Equal(t, int64(1715340600000), seconds.UnixMilli())

	var rfc3339 traeTime
	require.NoError(t, json.Unmarshal([]byte(`"2024-05-10T08:10:00Z"`), &rfc3339))
	assert.Equal(t, "2024-05-10T08:10:00Z", rfc3339.UTC().Format(time.RFC3339))
}
