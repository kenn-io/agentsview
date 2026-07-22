package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconcileWatchRootsCommandCodeCompanionOnlyChangeReparsesOwner(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	transcript := filepath.Join(project, "sess_123.jsonl")
	meta := filepath.Join(project, "sess_123.meta.json")
	require.NoError(t, os.WriteFile(transcript, []byte(
		`{"id":"m1","timestamp":"2026-06-01T10:00:00Z","sessionId":"sess_123","role":"user","content":[{"type":"text","text":"Inspect logs"}],"metadata":{"version":2,"cwd":"/workspace/project"}}`+"\n"+
			`{"id":"m2","timestamp":"2026-06-01T10:00:01Z","sessionId":"sess_123","role":"assistant","content":[{"type":"text","text":"Done"}],"metadata":{"version":2}}`+"\n",
	), 0o644))
	require.NoError(t, os.WriteFile(meta, []byte(`{"title":"Original title"}`), 0o644))
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCommandCode: {root}},
		Machine:   "test-machine",
	})
	defer engine.Close()

	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))
	before, err := database.GetSessionFull(t.Context(), "commandcode:sess_123")
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NotNil(t, before.SessionName)
	assert.Equal(t, "Original title", *before.SessionName)

	require.NoError(t, os.WriteFile(meta, []byte(`{"title":"Companion-only rename"}`), 0o644))
	future := time.Now().Add(time.Second)
	require.NoError(t, os.Chtimes(meta, future, future))
	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))

	after, err := database.GetSessionFull(t.Context(), "commandcode:sess_123")
	require.NoError(t, err)
	require.NotNil(t, after)
	require.NotNil(t, after.SessionName)
	assert.Equal(t, "Companion-only rename", *after.SessionName)
	active, err := database.GetSession(t.Context(), "commandcode:sess_123")
	require.NoError(t, err)
	assert.NotNil(t, active, "companion-only reconciliation must not tombstone its owner")
}
