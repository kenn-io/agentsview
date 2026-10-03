package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// Two writes for one excluded session id in the same bulk batch must
// skip-cache both sources, not only whichever write used the id last.
func TestWriteBatchBulkSkipCachesEachExcludedSource(t *testing.T) {
	database := openTestDB(t)
	dir := t.TempDir()
	const sessionID = "gemini:shared"
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: sessionID, Project: "p", Machine: "local", Agent: string(parser.AgentGemini),
	}))
	require.NoError(t, database.DeleteSession(t.Context(), sessionID))

	startedAt := time.Unix(1_700_000_000, 0)
	makeWrite := func(name string) (pendingWrite, string) {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
		return pendingWrite{
			sess: parser.ParsedSession{
				ID: sessionID, Project: "p", Machine: "local",
				Agent: parser.AgentGemini, StartedAt: startedAt, MessageCount: 1,
				File: parser.FileInfo{Path: path, Mtime: 1},
			},
			msgs: []parser.ParsedMessage{{Role: parser.RoleUser, Content: "m", Timestamp: startedAt}},
		}, path
	}
	first, firstPath := makeWrite("first.json")
	second, secondPath := makeWrite("second.json")

	e := &Engine{db: database, skipCache: map[string]int64{}, skipHashKeys: map[string]string{}}
	outcome := e.writeBatchBulkWithOutcome([]pendingWrite{first, second}, true)

	assert.Zero(t, outcome.writtenSessions)
	assert.Equal(t, []bool{true, true}, outcome.resolved)
	assert.Contains(t, e.skipCache, firstPath)
	assert.Contains(t, e.skipCache, secondPath)
}
