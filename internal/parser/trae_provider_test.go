package parser

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
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
	_, err = db.ExecContext(t.Context(), `CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO ItemTable(key, value) VALUES (?, ?), (?, ?)`, traeStorageKey, value, extraKey, `{"list":[{"sessionId":"ignored","messages":[{"role":"user","content":"wrong"}]}]`)
	require.NoError(t, err)
}

func writeTraeDBWithoutStorageKey(t *testing.T, path string, extraKey string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO ItemTable(key, value) VALUES (?, ?)`, extraKey, `{"list":[{"sessionId":"ignored","messages":[{"role":"user","content":"wrong"}]}]}`)
	require.NoError(t, err)
}

func setTraeDBValue(t *testing.T, path, value string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `UPDATE ItemTable SET value = ? WHERE key = ?`, value, traeStorageKey)
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
	assert := assert.New(t)

	def, ok := AgentByType(AgentTrae)
	require.True(t, ok)
	assert.Equal("Trae", def.DisplayName)
	assert.Equal("TRAE_DIR", def.EnvVar)
	assert.Equal("trae_dirs", def.ConfigKey)
	assert.Equal("trae:", def.IDPrefix)
	assert.Equal([]string{"workspaceStorage", "globalStorage"}, def.WatchSubdirs)
	assert.True(def.Usage.NoPerMessageTokenData)
	assert.False(def.Usage.AICreditsDenominated)
	assert.Contains(def.DefaultDirs, "AppData/Roaming/TRAE SOLO CN/User")
}

func TestTraeWorkspaceGlobalDiscoveryAndParsing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	workspaceDB := filepath.Join(root, "workspaceStorage", "hash", traeStateDBName)
	globalDB := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, workspaceDB, traeFixtureValue(t), "memento/unrelated-chat-storage")
	writeTraeDB(t, globalDB, traeFixtureValue(t), "memento/unrelated-chat-storage")
	require.NoError(os.WriteFile(filepath.Join(filepath.Dir(workspaceDB), "workspace.json"), []byte(`{"folder":"file:///tmp/project"}`), 0o644))

	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 2)

	for _, source := range sources {
		if strings.Contains(source.Key, "workspaceStorage") {
			assert.Equal("project", source.ProjectHint)
		}
		assert.NotContains(source.Key, "#session-1")
		outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
		require.NoError(err)
		require.Len(outcome.Results, 1)
		result := outcome.Results[0].Result
		assert.Equal(AgentTrae, result.Session.Agent)
		assert.Equal("trae:session-1", result.Session.ID)
		assert.Equal([]string{"first", "fallback"}, []string{result.Messages[0].Content, result.Messages[1].Content})
		assert.Equal("trae-model", result.Messages[1].Model)
		assert.Empty(result.UsageEvents)
		assert.False(result.Messages[1].HasOutputTokens)
		assert.Equal(RelationshipType(""), result.Session.RelationshipType)
	}
}

func TestTraeStreamingDiscoveryBoundsWorkspaceAndStopsEarly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const workspaces = streamingDirectoryBatchSize*2 + 3
	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "workspaceStorage")
	for i := range workspaces {
		path := filepath.Join(
			workspaceRoot, fmt.Sprintf("workspace-%03d", i), traeStateDBName,
		)
		require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(os.WriteFile(path, nil, 0o600))
	}
	provider, ok := NewProvider(AgentTrae, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	streaming, ok := provider.(StreamingDiscoverer)
	require.True(ok)
	maxBuffered := 0
	ctx := WithStreamingDiscoveryBufferObserver(t.Context(), func(buffered int) {
		maxBuffered = max(maxBuffered, buffered)
	})
	count := 0

	err := streaming.DiscoverEach(ctx, func(SourceRef) error {
		count++
		return nil
	})

	require.NoError(err)
	assert.Equal(workspaces, count)
	assert.Positive(maxBuffered,
		"Trae discovery must report its bounded directory batches")
	assert.LessOrEqual(maxBuffered, streamingDirectoryBatchSize)

	stop := errors.New("stop after first source")
	visited := 0
	ctx = withStreamingDirectoryReader(t.Context(), func(
		ctx context.Context, dir string, yield func(os.DirEntry) error,
	) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			visited++
			if err := yield(entry); err != nil {
				return err
			}
		}
		return nil
	})

	err = streaming.DiscoverEach(ctx, func(SourceRef) error { return stop })

	assert.ErrorIs(err, stop)
	assert.Equal(1, visited,
		"consumer stop must halt the workspace traversal immediately")
}

func TestTraeWatchChangedPathAndVirtualLookup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	dbPath := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, dbPath, traeFixtureValue(t), "memento/unrelated-chat-storage")
	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 2)
	sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{WatchRoot: filepath.Join(root, "globalStorage"), Path: dbPath})
	require.NoError(err)
	require.Len(sources, 1)
	_, _, ok = SplitTraeVirtualPath(sources[0].Key)
	assert.False(ok)
	virtual := traeVirtualPath(dbPath, "session-1")
	var found SourceRef
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{StoredFilePath: sources[0].Key})
	require.NoError(err)
	assert.True(ok)
	assert.Equal(sources[0].Key, found.Key)
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{StoredFilePath: virtual, RawSessionID: "session-1", RequireFreshSource: true})
	require.NoError(err)
	assert.True(ok)
	assert.Equal(virtual, found.Key)
	for _, name := range []string{traeStateDBName + "-wal"} {
		changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{WatchRoot: filepath.Join(root, "globalStorage"), Path: filepath.Join(root, "globalStorage", name)})
		require.NoError(err)
		assert.Len(changed, 1)
	}
}

// Watcher sidecar events (workspace.json, -wal) must scope stored-source
// hints to the owning state.vscdb container, not the whole watch root, so
// changed-path hint queries stay bounded by the affected container.
func TestTraeStoredSourceHintScopesResolveEventToContainer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	workspaceDB := filepath.Join(
		root, "workspaceStorage", "hash-a", traeStateDBName,
	)
	globalDB := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, workspaceDB, traeFixtureValue(t), "")
	writeTraeDB(t, globalDB, traeFixtureValue(t), "")
	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	resolver, ok := provider.(StoredSourceHintScopeProvider)
	require.True(ok, "trae provider must resolve stored-source hint scopes")

	workspaceScopes := resolver.StoredSourceHintScopes(ChangedPathRequest{
		Path: filepath.Join(
			root, "workspaceStorage", "hash-a", "workspace.json",
		),
		WatchRoot: filepath.Join(root, "workspaceStorage"),
	})
	require.Len(workspaceScopes, 1)
	assert.Equal(workspaceDB, workspaceScopes[0].Path)
	assert.True(workspaceScopes[0].IncludeVirtualMembers)

	globalScopes := resolver.StoredSourceHintScopes(ChangedPathRequest{
		Path:      globalDB + "-wal",
		WatchRoot: filepath.Join(root, "globalStorage"),
	})
	require.Len(globalScopes, 1)
	assert.Equal(globalDB, globalScopes[0].Path)
	assert.True(globalScopes[0].IncludeVirtualMembers)
}

func TestTraeWorkspaceChangedPathAndRawExport(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "workspaceStorage", "hash", traeStateDBName)
	writeTraeDB(t, path, traeFixtureValue(t), "memento/unrelated-chat-storage")
	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{WatchRoot: filepath.Join(root, "workspaceStorage"), Path: filepath.Join(root, "workspaceStorage", "hash", "workspace.json")})
	require.NoError(err)
	require.Len(changed, 1)
	var exported bytes.Buffer
	require.NoError(WriteTraeSessionJSON(t.Context(), &exported, path, "session-1"))
	assert.Contains(t, exported.String(), `"sessionId":"session-1"`)
}

func TestTraeUnsupportedKeyNegativeSpace(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "workspaceStorage", "hash", traeStateDBName)
	writeTraeDB(t, path, traeFixtureValue(t), "memento/unrelated-chat-storage")
	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(ok)
	sources, err := factory.NewProvider(ProviderConfig{Roots: []string{root}}).Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	assert.NotContains(t, sources[0].Key, "ignored")
}

func TestTraeMalformedSessionEntryDoesNotBlockSiblingDiscovery(t *testing.T) {
	require := require.New(t)

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
	require.True(ok)
	sources, err := factory.NewProvider(ProviderConfig{Roots: []string{root}}).Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 2)
	for _, source := range sources {
		outcome, err := factory.NewProvider(ProviderConfig{Roots: []string{root}}).Parse(t.Context(), ParseRequest{Source: source})
		require.NoError(err)
		require.Len(outcome.Results, 1)
	}
}

func TestTraeMalformedStorageDoesNotBlockSiblingDiscovery(t *testing.T) {
	require := require.New(t)

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
	require.True(ok)
	sources, err := factory.NewProvider(ProviderConfig{Roots: []string{root}}).Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 2)
	for _, source := range sources {
		outcome, err := factory.NewProvider(ProviderConfig{Roots: []string{root}}).Parse(t.Context(), ParseRequest{Source: source})
		if strings.Contains(source.Key, "globalStorage") {
			require.NoError(err)
			require.Len(outcome.Results, 1)
		} else {
			require.Error(err)
		}
	}
}

func TestTraeValidEmptyStoreReturnsCompleteNoSessionOutcome(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, path, `{"list":[]}`, "memento/unrelated-chat-storage")

	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(err)
	assert.Empty(outcome.Results)
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.True(outcome.ForceReplace)
	assert.True(outcome.ResultSetComplete)
}

func TestTraeMissingContainerReturnsCompleteNoSessionOutcome(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, path, traeStoreValue(t, []any{
		map[string]any{
			"sessionId": "session-1",
			"createdAt": 1715340600000,
			"messages":  []any{map[string]any{"role": "user", "content": "archived"}},
		},
	}), "memento/unrelated-chat-storage")

	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	require.NoError(os.Remove(path))

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(err)
	assert.Empty(outcome.Results)
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.True(outcome.ResultSetComplete)
	assert.False(outcome.ForceReplace)
}

func TestTraeUnknownStoragePreservesArchiveUntilExplicitList(t *testing.T) {
	for _, value := range []string{`{}`, `{"list":null}`} {
		t.Run(value, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			root := t.TempDir()
			path := filepath.Join(root, "globalStorage", traeStateDBName)
			writeTraeDB(t, path, value, "memento/unrelated-chat-storage")

			factory, ok := ProviderFactoryByType(AgentTrae)
			require.True(ok)
			provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
			sources, err := provider.Discover(t.Context())
			require.NoError(err)
			require.Len(sources, 1)

			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
			require.NoError(err)
			assert.Empty(outcome.Results)
			assert.Equal(SkipNoSession, outcome.SkipReason)
			assert.False(outcome.ForceReplace)
			assert.False(outcome.ResultSetComplete)

			virtual := traeVirtualPath(path, "session-1")
			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				WatchRoot:         filepath.Join(root, "globalStorage"),
				Path:              path,
				StoredSourcePaths: []string{virtual},
			})
			require.NoError(err)
			require.Len(changed, 1)
			assert.Equal(path, changed[0].Key)
		})
	}
}

func TestTraeRequireFreshSourceFallsBackToRawIDAfterStoredVirtualPathRelocates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     traeVirtualPath(oldDB, "session-1"),
		RawSessionID:       "session-1",
		RequireFreshSource: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(traeVirtualPath(oldDB, "session-1"), found.Key)

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

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     traeVirtualPath(oldDB, "session-1"),
		RawSessionID:       "session-1",
		RequireFreshSource: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(traeVirtualPath(newDB, "session-1"), found.Key)
}

func TestTraeMalformedSessionEntryKeepsContainerIncomplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal("trae:good", outcome.Results[0].Result.Session.ID)
	assert.False(outcome.ResultSetComplete)

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		WatchRoot:         filepath.Join(root, "globalStorage"),
		Path:              path,
		StoredSourcePaths: []string{traeVirtualPath(path, "broken")},
	})
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(path, changed[0].Key)
}

func TestTraeEncryptedLayoutOutcomeUnsupported(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "empty stub",
			setup: func(t *testing.T, path string) {
				writeTraeDB(t, path, traeStoreValue(t, []any{
					map[string]any{
						"sessionId": "stub",
						"messages":  []any{},
					},
				}), "memento/unrelated-chat-storage")
			},
		},
		{
			name: "missing storage key",
			setup: func(t *testing.T, path string) {
				writeTraeDBWithoutStorageKey(t, path, "memento/unrelated-chat-storage")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			root := traeProfileRoot(t)
			path := filepath.Join(root, "globalStorage", traeStateDBName)
			test.setup(t, path)
			writeTraeModularData(t, root, "encrypted header")

			factory, ok := ProviderFactoryByType(AgentTrae)
			require.True(ok)
			provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
			sources, err := provider.Discover(t.Context())
			require.NoError(err)
			require.Len(sources, 1)

			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
			require.NoError(err)
			assert.Equal(SkipUnsupportedSource, outcome.SkipReason)
			assert.True(outcome.ResultSetComplete)
			assert.Empty(outcome.Results)
			assert.Empty(outcome.SourceErrors)
			assert.False(outcome.ForceReplace)
		})
	}
}

func TestTraeMixedLegacyAndEmptyStubPreservesInlineSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := traeProfileRoot(t)
	path := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, path, traeStoreValue(t, []any{
		map[string]any{
			"sessionId": "real",
			"messages":  []any{map[string]any{"role": "user", "content": "legacy"}},
		},
		map[string]any{
			"sessionId": "stub",
			"messages":  []any{map[string]any{"role": "assistant", "content": ""}},
		},
	}), "memento/unrelated-chat-storage")
	writeTraeModularData(t, root, "encrypted header")

	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal("trae:real", outcome.Results[0].Result.Session.ID)
	assert.Equal(SkipNone, outcome.SkipReason)
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
			assert := assert.New(t)
			require := require.New(t)

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
			require.True(ok)
			provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
			sources, err := provider.Discover(t.Context())
			require.NoError(err)
			require.Len(sources, 1)

			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
			require.NoError(err)
			require.Len(outcome.Results, 1)
			assert.Equal("trae:good", outcome.Results[0].Result.Session.ID)
			assert.False(outcome.ResultSetComplete)

			virtual := traeVirtualPath(path, tc.session["sessionId"].(string))
			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
				WatchRoot:         filepath.Join(root, "globalStorage"),
				Path:              path,
				StoredSourcePaths: []string{virtual},
			})
			require.NoError(err)
			require.Len(changed, 1)
			assert.Equal(path, changed[0].Key)

			found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
				StoredFilePath:     virtual,
				RawSessionID:       tc.session["sessionId"].(string),
				RequireFreshSource: true,
			})
			require.NoError(err)
			assert.True(ok)
			_, err = provider.Parse(t.Context(), ParseRequest{Source: found})
			require.Error(err)
		})
	}
}

func TestTraeUnparseableEncryptedSessionsStayIncomplete(t *testing.T) {
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
			assert := assert.New(t)
			require := require.New(t)

			root := traeProfileRoot(t)
			path := filepath.Join(root, "globalStorage", traeStateDBName)
			writeTraeDB(t, path, traeStoreValue(t, []any{tc.session}), "memento/unrelated-chat-storage")
			writeTraeModularData(t, root, "encrypted header")

			factory, ok := ProviderFactoryByType(AgentTrae)
			require.True(ok)
			provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
			sources, err := provider.Discover(t.Context())
			require.NoError(err)
			require.Len(sources, 1)

			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
			require.NoError(err)
			assert.Equal(SkipNoSession, outcome.SkipReason)
			assert.False(outcome.ResultSetComplete)
			assert.False(outcome.ForceReplace)
		})
	}
}

func TestTraeChangedPathTombstonesRefreshWarmMemberPresenceCache(t *testing.T) {
	require := require.New(t)

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
	require.True(ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	virtual := traeVirtualPath(dbPath, "session-1")
	_, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     virtual,
		RawSessionID:       "session-1",
		RequireFreshSource: true,
	})
	require.NoError(err)
	require.True(ok)

	setTraeDBValue(t, dbPath, `{"list":[]}`)
	sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		WatchRoot:         filepath.Join(root, "globalStorage"),
		Path:              dbPath,
		StoredSourcePaths: []string{virtual},
	})
	require.NoError(err)
	var tombstone SourceRef
	for _, source := range sources {
		if source.Key == virtual {
			tombstone = source
		}
	}
	assert.Equal(t, virtual, tombstone.Key)
}

func TestTraeChangedPathTombstonesDecodeSnapshotOncePerContainer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	traeLoadSessionSnapshot = func(ctx context.Context, path string) (traeSessionSnapshot, error) {
		decodes++
		return orig(ctx, path)
	}

	factory, ok := ProviderFactoryByType(AgentTrae)
	require.True(ok)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}})
	stored := []string{
		traeVirtualPath(dbPath, "session-1"),
		traeVirtualPath(dbPath, "session-2"),
		traeVirtualPath(dbPath, "session-3"),
	}

	sources, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		WatchRoot:         filepath.Join(root, "globalStorage"),
		Path:              dbPath,
		StoredSourcePaths: stored,
	})
	require.NoError(err)
	assert.Equal(1, decodes)
	require.Len(sources, 3)
	assert.Equal(dbPath, sources[0].Key)
	assert.ElementsMatch([]string{
		dbPath,
		traeVirtualPath(dbPath, "session-2"),
		traeVirtualPath(dbPath, "session-3"),
	}, []string{sources[0].Key, sources[1].Key, sources[2].Key})
}

func TestTraeAssistantFallbackVariants(t *testing.T) {
	assert := assert.New(t)

	assert.Equal("plain text", traeAssistantFallback(jsontext.Value(`"plain text"`)))
	assert.Equal("text field", traeAssistantFallback(jsontext.Value(`{"text":"text field"}`)))
	assert.Equal("proposal field", traeAssistantFallback(jsontext.Value(`{"proposal":"proposal field"}`)))
	assert.Equal("step one\nstep two", traeAssistantFallback(jsontext.Value(`{"guideline":{"planItems":[{"content":"step one"},{"content":"step two"}]}}`)))
}

func TestTraeTimeUnmarshalVariants(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var seconds traeTime
	require.NoError(json.Unmarshal([]byte(`1715340600`), &seconds))
	assert.Equal(int64(1715340600000), seconds.UnixMilli())

	var rfc3339 traeTime
	require.NoError(json.Unmarshal([]byte(`"2024-05-10T08:10:00Z"`), &rfc3339))
	assert.Equal("2024-05-10T08:10:00Z", rfc3339.UTC().Format(time.RFC3339))
}
