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

func TestHeadlessAutomationSurvivesSyncAppends(t *testing.T) {
	for _, tc := range []struct {
		name, id, initial, reply, followup string
		agent                              parser.AgentType
	}{
		{
			name: "claude", id: "worker", agent: parser.AgentClaude,
			initial: `{"type":"agent-setting","entrypoint":"sdk-cli"}` + "\n" +
				`{"type":"user","uuid":"u1","turnOrigin":"sdk","promptSource":"sdk","timestamp":"2026-10-01T10:00:00Z","message":{"content":"Plan a settings change."}}` + "\n",
			reply:    testjsonl.NewSessionBuilder().AddClaudeAssistantWithUUID("2026-10-01T10:01:00Z", "The plan is ready.", "a1", "u1").String(),
			followup: `{"type":"user","uuid":"u2","parentUuid":"a1","turnOrigin":"sdk","timestamp":"2026-10-01T10:02:00Z","message":{"content":"Explain the plan."}}` + "\n",
		},
		{
			name: "codex", id: "codex:019eb791-cf7d-75c1-8439-9ed74c122c80", agent: parser.AgentCodex,
			initial: testjsonl.CodexSessionMetaJSON("019eb791-cf7d-75c1-8439-9ed74c122c80", "/workspace/project", "codex_exec", "2026-10-01T10:00:00Z") + "\n" +
				testjsonl.CodexMsgJSON("user", "Plan a settings change.", "2026-10-01T10:00:01Z") + "\n",
			reply:    testjsonl.CodexMsgJSON("assistant", "The plan is ready.", "2026-10-01T10:01:00Z") + "\n",
			followup: testjsonl.CodexMsgJSON("user", "Explain the plan.", "2026-10-01T10:02:00Z") + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "project", tc.id+".jsonl")
			if tc.agent == parser.AgentCodex {
				path = filepath.Join(root, "rollout-2026-10-01T10-00-00-019eb791-cf7d-75c1-8439-9ed74c122c80.jsonl")
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			database := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{tc.agent: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			content := tc.initial
			for i, tail := range []string{"", tc.reply, tc.followup} {
				content += tail
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
				require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
				stored, err := database.GetSession(t.Context(), tc.id)
				require.NoError(t, err)
				require.NotNil(t, stored)
				assert.Equal(t, parser.SessionKindNonInteractive, stored.SessionKind)
				assert.True(t, stored.IsAutomated)
				if i == 0 {
					raw, err := sql.Open("sqlite3", database.Path())
					require.NoError(t, err)
					_, err = raw.ExecContext(t.Context(), `UPDATE sessions SET session_kind = '', is_automated = 0, data_version = 126 WHERE id = ?`, tc.id)
					require.NoError(t, err)
					require.NoError(t, raw.Close())
					require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
					assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), tc.id))
					stored, err = database.GetSession(t.Context(), tc.id)
					require.NoError(t, err)
					assert.Equal(t, parser.SessionKindNonInteractive, stored.SessionKind)
					assert.True(t, stored.IsAutomated)
				}
			}
		})
	}
}
