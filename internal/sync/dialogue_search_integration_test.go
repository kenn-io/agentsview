package sync_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestPiDialogueSearch(t *testing.T) {
	env := setupTestEnv(t)
	const content = `{"type":"session","version":3,"id":"dialogue","timestamp":"2026-01-01T10:00:00Z","cwd":"/tmp/project"}
{"type":"message","id":"u","timestamp":"2026-01-01T10:00:01Z","message":{"role":"user","content":"Please inspect this."}}
{"type":"message","id":"a","timestamp":"2026-01-01T10:00:02Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"reasoningneedle"},{"type":"text","text":"An answer."},{"type":"toolCall","id":"tool-1","name":"bash","arguments":{"command":"echo commandneedle"}}]}}
{"type":"message","id":"u2","timestamp":"2026-01-01T10:00:03Z","message":{"role":"user","content":"Thank you."}}`
	env.writeSession(t, env.piDir, filepath.Join("encoded-cwd", "dialogue.jsonl"), content)
	stats := env.engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)
	page, err := env.db.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "An answer.", Sources: []string{"messages"}})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "An answer.", page.Matches[0].Snippet)
	for _, needle := range []string{"reasoningneedle", "commandneedle"} {
		page, err := env.db.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: needle, Sources: []string{"messages"}})
		require.NoError(t, err)
		assert.Empty(t, page.Matches, "%s must stay outside message search", needle)
	}
}
