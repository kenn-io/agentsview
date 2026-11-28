package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// A transcript that changes while its session is in the trash is skipped by
// fallback polling, and restoring changes no file. The restore route must make
// the next polling pass read the transcript again.
func TestRestoreRouteMakesPollingRereadTheSession(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	root := filepath.Join(dir, "projects")
	path := filepath.Join(root, "project", "restored.jsonl")
	write := func(prompts ...string) {
		t.Helper()
		builder := testjsonl.NewSessionBuilder()
		for _, prompt := range prompts {
			builder.AddClaudeUser("2024-01-01T00:00:00Z", prompt)
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o644))
	}
	write("first")
	database := dbtest.OpenTestDBAt(t, dbPath)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	s := New(config.Config{Host: "127.0.0.1", Port: 0, DataDir: dir, DBPath: dbPath}, database, engine)
	groups := []sync.ProviderRootsGroup{{Agent: parser.AgentClaude, Roots: []string{root}}}
	poll := func() {
		t.Helper()
		require.NoError(t, engine.PollProviderRootsGrouped(t.Context(), groups, false))
	}
	post := func(path, body string) {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	}
	poll()

	post("/api/v1/sessions/batch-delete", `{"session_ids":["restored"]}`)
	write("first", "second")
	poll()
	post("/api/v1/sessions/restored/restore", "")
	poll()

	session, err := database.GetSessionFull(t.Context(), "restored")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 2, session.UserMessageCount)
}
