package sync_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestRooCodeChildPromptPRFilter(t *testing.T) {
	root := t.TempDir()
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentRooCode: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	for _, tc := range []struct{ id, input string }{
		{"selected", `{"tool":"newTask","mode":"Code","content":"Review https://github.com/example/repo/pull/42"}`},
		{"other", `{"tool":"newTask","mode":"Code","content":"Review https://github.com/example/other/pull/42"}`},
	} {
		messages, err := json.Marshal([]map[string]any{
			{"ts": 1688836851000, "type": "say", "say": "text", "text": "Delegate the review"},
			{"ts": 1688836860000, "type": "ask", "ask": "tool", "text": tc.input},
		})
		require.NoError(t, err)
		writeRooCodeSyncFixture(t, root, tc.id, string(messages), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	}
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	filter, err := db.ParsePRFilter("example/repo#42")
	require.NoError(t, err)
	page, err := archive.ListSessions(t.Context(), db.SessionFilter{PR: filter})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "roocode:selected", page.Sessions[0].ID)
}
