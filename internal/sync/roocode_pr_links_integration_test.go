package sync_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestRooCodeChildPromptPRFilter(t *testing.T) {
	root := t.TempDir()
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentRooCode: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	messages, err := json.Marshal([]map[string]any{
		{"ts": 1688836851000, "type": "say", "say": "text", "text": "Delegate the review"},
		{"ts": 1688836860000, "type": "ask", "ask": "tool", "text": `{"tool":"newTask","mode":"Code","content":"Review https://github.com/example/repo/pull/42"}`},
	})
	require.NoError(t, err)
	writeRooCodeSyncFixture(t, root, "selected", string(messages), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	assert.Equal(t, []string{"https://github.com/example/repo/pull/42"}, storedPRURLs(t, archive, "roocode:selected"))
}
