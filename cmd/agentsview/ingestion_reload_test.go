package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func writeReloadGeminiSession(t *testing.T, root, id string) {
	t.Helper()
	path := filepath.Join(root, "tmp", "project", "chats", "session-"+id+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.GeminiSessionJSON(
		id, "project", "2026-08-09T10:00:00Z", "2026-08-09T10:01:00Z",
		[]map[string]any{testjsonl.GeminiUserMsg(
			"user", "2026-08-09T10:00:00Z", "hello from "+id,
		)},
	)), 0o644))
}

func sessionImported(t *testing.T, database *db.DB, id string) bool {
	stored, err := database.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	return stored != nil && stored.DeletedAt == nil
}

func TestDaemonIngestionReloadAppliesProviderSettings(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	base := t.TempDir()
	primary := filepath.Join(base, "gemini")
	alternate := filepath.Join(base, "gemini-work")
	writeReloadGeminiSession(t, primary, "primary")
	writeReloadGeminiSession(t, alternate, "alternate")

	startup := config.Config{
		AgentDirs:      map[parser.AgentType][]string{parser.AgentGemini: {primary}},
		DisabledAgents: []parser.AgentType{parser.AgentGemini},
	}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{
		AgentDirs:      startup.AgentDirs,
		DisabledAgents: startup.DisabledAgents,
		Machine:        "test-machine",
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	require.False(t, sessionImported(t, database, "gemini:primary"))

	var mu sync.Mutex
	saved := startup
	load := func() (config.Config, error) {
		mu.Lock()
		defer mu.Unlock()
		return saved, nil
	}
	ingestion := newDaemonIngestion(
		t.Context(), startup, engine, database, nil, load,
	)
	t.Cleanup(ingestion.Stop)
	ingestion.OpenWatcherDispatch()

	// The user enables Gemini and adds a second home in one saved change.
	mu.Lock()
	saved = config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGemini: {primary, alternate},
		},
	}
	mu.Unlock()
	reloaded, err := ingestion.Reload(t.Context())
	require.NoError(t, err)
	assert.Empty(t, reloaded.DisabledAgents)

	assert.Eventually(t, func() bool {
		return sessionImported(t, database, "gemini:primary") &&
			sessionImported(t, database, "gemini:alternate")
	}, 10*time.Second, 20*time.Millisecond,
		"existing sessions in the enabled provider's roots are synced")

	// A session written after the change is picked up by the replacement
	// watcher, which covers the new root.
	writeReloadGeminiSession(t, alternate, "later")
	assert.Eventually(t, func() bool {
		return sessionImported(t, database, "gemini:later")
	}, 20*time.Second, 50*time.Millisecond,
		"the watcher follows the new provider settings")
}

func TestAddedReconcileScopes(t *testing.T) {
	base := t.TempDir()
	primary := filepath.Join(base, "gemini")
	added := filepath.Join(base, "gemini-work")
	missing := filepath.Join(base, "gemini-missing")
	for _, root := range []string{primary, added} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "tmp"), 0o755))
	}
	dirs := func(roots ...string) map[parser.AgentType][]string {
		return map[parser.AgentType][]string{parser.AgentGemini: roots}
	}

	tests := []struct {
		name       string
		prev, next config.Config
		want       []agentsync.ProviderRootsGroup
	}{
		{
			name: "enabled provider syncs every present root",
			prev: config.Config{
				AgentDirs:      dirs(primary, added),
				DisabledAgents: []parser.AgentType{parser.AgentGemini},
			},
			next: config.Config{AgentDirs: dirs(primary, added)},
			want: []agentsync.ProviderRootsGroup{{
				Agent: parser.AgentGemini, Roots: []string{primary, added},
			}},
		},
		{
			name: "added root syncs only that root",
			prev: config.Config{AgentDirs: dirs(primary)},
			next: config.Config{AgentDirs: dirs(primary, added)},
			want: []agentsync.ProviderRootsGroup{{
				Agent: parser.AgentGemini, Roots: []string{added},
			}},
		},
		{
			name: "missing added root waits for the watcher",
			prev: config.Config{AgentDirs: dirs(primary)},
			next: config.Config{AgentDirs: dirs(primary, missing)},
		},
		{
			name: "disabling a provider syncs nothing",
			prev: config.Config{AgentDirs: dirs(primary)},
			next: config.Config{
				AgentDirs:      dirs(primary),
				DisabledAgents: []parser.AgentType{parser.AgentGemini},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, addedReconcileScopes(tt.prev, tt.next))
		})
	}
}
