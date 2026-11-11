package sync

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestCodexOrdinaryForkLinkSurvivesSyncAndUpgrade(t *testing.T) {
	const (
		parent = "a1000000-0000-4000-8000-000000000001"
		child  = "a1000000-0000-4000-8000-000000000002"
	)
	root := t.TempDir()
	writeCodexRollout(t, root, "10", "rollout-2026-09-10T10-00-00-"+parent+".jsonl",
		`{"timestamp":"2026-09-10T10:00:00Z","type":"session_meta","payload":{"id":"`+parent+`","cwd":"/synthetic/project","originator":"codex_cli_rs","timestamp":"2026-09-10T10:00:00Z"}}
{"timestamp":"2026-09-10T10:00:01Z","type":"turn_context","payload":{"turn_id":"synthetic-parent-turn","model":"gpt-5"}}
{"timestamp":"2026-09-10T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic parent prompt"}]}}
{"timestamp":"2026-09-10T10:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Synthetic parent answer"}]}}
`)
	writeCodexRollout(t, root, "10", "rollout-2026-09-10T10-00-00-"+child+".jsonl",
		`{"timestamp":"2026-09-10T10:00:00Z","type":"session_meta","payload":{"id":"`+child+`","cwd":"/synthetic/project","originator":"codex_cli_rs","timestamp":"2026-09-10T10:00:00Z","forked_from_id":"`+parent+`","history_mode":"paginated","history_base":{"thread_id":"`+parent+`","end_ordinal_exclusive":4}}}
{"timestamp":"2026-09-10T10:00:01Z","type":"turn_context","payload":{"turn_id":"synthetic-child-turn","model":"gpt-5"}}
{"timestamp":"2026-09-10T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic child prompt"}]}}
{"timestamp":"2026-09-10T10:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Synthetic child answer"}]}}
`)
	childID := "codex:" + child
	assertForkLink := func(t *testing.T, database *db.DB) {
		t.Helper()
		sess, err := database.GetSession(t.Context(), childID)
		require.NoError(t, err)
		require.NotNil(t, sess)
		require.NotNil(t, sess.ParentSessionID)
		assert.Equal(t, "codex:"+parent, *sess.ParentSessionID)
		assert.Equal(t, "fork", sess.RelationshipType)
		msgs, err := database.GetAllMessages(t.Context(), childID)
		require.NoError(t, err)
		var contents []string
		for _, msg := range msgs {
			contents = append(contents, msg.Content)
		}
		assert.Equal(t, []string{"Synthetic child prompt", "Synthetic child answer"}, contents)
	}

	database := dbtest.OpenTestDB(t)
	agentDirs := map[parser.AgentType][]string{parser.AgentCodex: {root}}
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: agentDirs, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	assertForkLink(t, database)

	// Model an archive that imported the fork without its parent before data
	// version 148.
	const previousVersion = 147
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `UPDATE sessions
			SET parent_session_id = NULL, parser_parent_session_id = NULL, relationship_type = ''
			WHERE id = ?`, childID); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version = %d", previousVersion))
		return err
	}))
	require.NoError(t, database.SetSessionDataVersion(t.Context(), childID, previousVersion))
	engine.Close()
	dbPath := database.Path()
	require.NoError(t, database.Close())

	database, err := db.OpenIsolated(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	assert.False(t, database.NeedsResync(), "the archive upgrades in place")
	engine = NewEngine(t.Context(), database, EngineConfig{AgentDirs: agentDirs, Machine: "local"})
	t.Cleanup(engine.Close)
	// The version upgrade must reparse even when the source is unchanged.
	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))
	assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), childID))
	assertForkLink(t, database)
}
