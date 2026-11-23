package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestFirstImportSelectsNewestPage(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	for i := range 101 {
		writeFirstImportClaudeSession(t, project, i, time.Unix(1700000000+int64(i), 0))
	}
	zcode := filepath.Join(t.TempDir(), ".zcode", "cli")
	writeProcessProviderZCodeDB(t, zcode)
	archive := openTestDB(t)
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}, parser.AgentZCode: {zcode}}, Machine: "local", DeferStartupMaintenance: true,
	})
	t.Cleanup(engine.Close)

	engine.syncNewest(t.Context(), nil, nil)
	require.True(t, engine.LastSyncFinishedAt(t.Context()).IsZero())
	zcodeSession, err := archive.GetSession(t.Context(), "zcode:session-001")
	require.NoError(t, err)
	require.NotNil(t, zcodeSession)
	page, err := archive.ListSessions(t.Context(), db.SessionFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 100)
	oldest, err := archive.GetSession(t.Context(), "session-000")
	require.NoError(t, err)
	require.Nil(t, oldest)
	secondOldest, err := archive.GetSession(t.Context(), "session-001")
	require.NoError(t, err)
	require.Nil(t, secondOldest)
	newest, err := archive.GetSession(t.Context(), "session-100")
	require.NoError(t, err)
	require.NotNil(t, newest)
}
