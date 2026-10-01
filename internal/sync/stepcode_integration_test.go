package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestStepCodeSyncIndexesSessionsAndTagsSpawnedChildren(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	project := filepath.Join(root, "--Users-alice-code-step-project--")
	require.NoError(t, os.MkdirAll(project, 0o755))
	write := func(name, id string) string {
		path := filepath.Join(project, name)
		require.NoError(t, os.WriteFile(path, []byte(`{"type":"session","version":3,"id":"`+id+`","timestamp":"2026-09-01T12:00:00Z","cwd":"/Users/alice/code/step-project"}
{"type":"message","id":"m1","timestamp":"2026-09-01T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Inspect the source."}]}}
{"type":"message","id":"m2","timestamp":"2026-09-01T12:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"Looks ready."}],"model":"step-3"}}
`), 0o644))
		return path
	}
	write("2026-09-01T12-00-00-000Z_step-parent.jsonl", "step-parent")
	write("2026-09-01T12-00-05-000Z_subagent-0199e4c2.jsonl", "subagent-0199e4c2")
	write("2026-09-01T12-00-06-000Z_workflow-run1-agent1.jsonl", "workflow-run1-agent1")

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentStepCode: {root},
		},
		Machine: "local",
	})

	for range 2 {
		stats := engine.SyncAll(t.Context(), nil)
		require.Equal(t, 0, stats.Failed)
		require.Equal(t, 3, countAgentSessions(t, database, string(parser.AgentStepCode)))
		require.Equal(t, 0, countAgentSessions(t, database, string(parser.AgentPi)))
	}

	for id, want := range map[string]string{
		"stepcode:step-parent":          "",
		"stepcode:subagent-0199e4c2":    string(parser.RelSubagent),
		"stepcode:workflow-run1-agent1": string(parser.RelSubagent),
	} {
		sess, err := database.GetSession(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, sess, id)
		assert.Equal(t, want, sess.RelationshipType, id)
		assert.Nil(t, sess.ParentSessionID, id)
	}
}
