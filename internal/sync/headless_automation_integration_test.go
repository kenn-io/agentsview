package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestHeadlessSubagentsSurviveSyncAppends(t *testing.T) {
	root := t.TempDir()
	claudeRoot, codexRoot := filepath.Join(root, "claude"), filepath.Join(root, "codex")
	sessions := []struct {
		id, path, content, reply, followup, kind string
		relationship                             parser.RelationshipType
		automated                                bool
	}{
		{
			id: "scripted-worker", path: filepath.Join(claudeRoot, "project", "scripted-worker.jsonl"), relationship: parser.RelSubagent, automated: true,
			content:  `{"type":"user","entrypoint":"sdk-cli","uuid":"u1","turnOrigin":"sdk","promptSource":"sdk","timestamp":"2026-10-01T10:00:00Z","message":{"content":"You are a code reviewer. Review the settings change."}}` + "\n",
			reply:    testjsonl.NewSessionBuilder().AddClaudeAssistantWithUUID("2026-10-01T10:01:00Z", "The plan is ready.", "a1", "u1").String(),
			followup: testjsonl.NewSessionBuilder().AddClaudeAssistantWithUUID("2026-10-01T10:02:00Z", "More detail.", "a2", "a1").String(),
		},
		{
			id: "codex:019eb791-cf7d-75c1-8439-9ed74c122c80", path: filepath.Join(codexRoot, "rollout-2026-10-01T10-00-00-019eb791-cf7d-75c1-8439-9ed74c122c80.jsonl"),
			kind: parser.SessionKindNonInteractive, relationship: parser.RelSubagent,
			content: testjsonl.CodexSessionMetaJSON("019eb791-cf7d-75c1-8439-9ed74c122c80", "/workspace/project", "codex_exec", "2026-10-01T10:00:00Z") + "\n" +
				testjsonl.CodexMsgJSON("user", "Plan a settings change.", "2026-10-01T10:00:00Z") + "\n",
			reply:    testjsonl.CodexMsgJSON("assistant", "The plan is ready.", "2026-10-01T10:01:00Z") + "\n",
			followup: testjsonl.CodexMsgJSON("user", "Explain the plan.", "2026-10-01T10:02:00Z") + "\n",
		},
	}
	database := dbtest.OpenTestDB(t)
	engineConfig := sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {claudeRoot}, parser.AgentCodex: {codexRoot}}, Machine: "local",
	}
	engine := sync.NewEngine(t.Context(), database, engineConfig)
	t.Cleanup(func() { engine.Close() })
	for stage := range 3 {
		for i := range sessions {
			session := &sessions[i]
			if stage == 1 {
				session.content += session.reply
			}
			if stage == 2 {
				session.content += session.followup
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(session.path), 0o700))
			require.NoError(t, os.WriteFile(session.path, []byte(session.content), 0o600))
		}
		require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
		for _, session := range sessions {
			stored, err := database.GetSession(t.Context(), session.id)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Equal(t, session.kind, stored.SessionKind)
			assert.Equal(t, string(session.relationship), stored.RelationshipType)
			assert.Nil(t, stored.ParentSessionID)
			assert.Equal(t, session.automated, stored.IsAutomated)
		}
	}
	engine.Close()
	path := database.Path()
	require.NoError(t, database.Close())
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `UPDATE sessions SET session_kind = '', relationship_type = '', is_automated = 0, data_version = 126 WHERE id = 'scripted-worker'`)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `UPDATE sessions SET session_kind = 'non-interactive', relationship_type = '', is_automated = 1, data_version = 126 WHERE agent = 'codex'`)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `PRAGMA user_version = 126`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	reopened := dbtest.OpenTestDBAt(t, path)
	engine = sync.NewEngine(t.Context(), reopened, engineConfig)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	worker, err := reopened.GetSession(t.Context(), "scripted-worker")
	require.NoError(t, err)
	require.NotNil(t, worker)
	assert.Empty(t, worker.SessionKind)
	assert.Equal(t, "subagent", worker.RelationshipType)
	assert.Equal(t, db.CurrentDataVersion(), reopened.GetSessionDataVersion(t.Context(), "scripted-worker"))
	engine.Close()
	require.NoError(t, reopened.Close())
}
