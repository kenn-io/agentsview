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

func traeFixtureValue(t *testing.T) string {
	t.Helper()
	value := map[string]any{"list": []any{map[string]any{
		"sessionId": "session-1", "createdAt": 1715340600000, "updatedAt": 1715340900000, "model": "trae-model",
		"messages": []any{
			map[string]any{"role": "user", "content": "first", "turnIndex": 0},
			map[string]any{"role": "assistant", "content": "", "agentTaskContent": map[string]any{"content": "fallback", "guideline": map[string]any{"planItems": []any{map[string]any{"content": "ignored after direct"}}}}, "turnIndex": 1},
		},
	}}}
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
		assert.Contains(t, source.Key, "#session-1")
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
	db, id, ok := SplitTraeVirtualPath(sources[0].Key)
	assert.True(t, ok)
	assert.Equal(t, dbPath, db)
	assert.Equal(t, "session-1", id)
	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{StoredFilePath: sources[0].Key})
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, sources[0].Key, found.Key)
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
	_, id, ok := SplitTraeVirtualPath(changed[0].Key)
	require.True(t, ok)
	require.NoError(t, WriteTraeSessionJSON(&exported, path, id))
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
