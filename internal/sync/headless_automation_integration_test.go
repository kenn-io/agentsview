package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
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
			id: "human", path: filepath.Join(claudeRoot, "project", "human.jsonl"),
			content:  `{"type":"user","entrypoint":"sdk-cli","uuid":"u1","turnOrigin":"human","origin":{"kind":"human"},"promptSource":"sdk","timestamp":"2026-10-01T10:00:00Z","message":{"content":"Plan a settings change."}}` + "\n",
			reply:    testjsonl.NewSessionBuilder().AddClaudeAssistantWithUUID("2026-10-01T10:01:00Z", "The plan is ready.", "a1", "u1").String(),
			followup: `{"type":"user","uuid":"u2","parentUuid":"a1","turnOrigin":"human","timestamp":"2026-10-01T10:02:00Z","message":{"content":"Explain the plan."}}` + "\n",
		},
		{
			id: "worker", path: filepath.Join(claudeRoot, "project", "worker.jsonl"), relationship: parser.RelSubagent,
			content:  `{"type":"user","entrypoint":"sdk-cli","uuid":"u1","turnOrigin":"sdk","promptSource":"sdk","timestamp":"2026-10-01T10:00:00Z","message":{"content":"Delegate a settings change."}}` + "\n",
			reply:    testjsonl.NewSessionBuilder().AddClaudeAssistantWithUUID("2026-10-01T10:01:00Z", "The plan is ready.", "a1", "u1").String(),
			followup: testjsonl.NewSessionBuilder().AddClaudeAssistantWithUUID("2026-10-01T10:02:00Z", "More detail.", "a2", "a1").String(),
		},
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
	query, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2026-10-01", Timezone: "UTC",
	}, time.Date(2026, time.October, 2, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
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
		require.Equal(t, 4, engine.SyncAll(t.Context(), nil).Synced)
		for _, session := range sessions {
			stored, err := database.GetSession(t.Context(), session.id)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Equal(t, session.kind, stored.SessionKind)
			assert.Equal(t, string(session.relationship), stored.RelationshipType)
			assert.Nil(t, stored.ParentSessionID)
			assert.Equal(t, session.automated, stored.IsAutomated)
		}
		if stage > 0 {
			report, err := database.GetActivityReport(t.Context(), db.AnalyticsFilter{Timezone: "UTC"}, query)
			require.NoError(t, err)
			assert.Equal(t, 1, report.Totals.InteractiveSessions)
			assert.Equal(t, 2, report.Totals.SubagentSessions)
			assert.Equal(t, 1, report.Totals.AutomatedSessions)
			assert.Equal(t, 1, report.InteractivePeak.Agents)
			assert.Equal(t, 2, report.SubagentPeak.Agents)
			assert.Equal(t, 4, report.Peak.Agents)
		}
	}
	engine.Close()
	path := database.Path()
	require.NoError(t, database.Close())
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `UPDATE sessions SET session_kind = '', relationship_type = '', is_automated = 0, data_version = 126 WHERE id = 'worker'`)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `UPDATE sessions SET session_kind = 'non-interactive', relationship_type = '', is_automated = 1, data_version = 126 WHERE agent = 'codex'`)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `PRAGMA user_version = 126`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	reopened := dbtest.OpenTestDBAt(t, path)
	engine = sync.NewEngine(t.Context(), reopened, engineConfig)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	report, err := reopened.GetActivityReport(t.Context(), db.AnalyticsFilter{Timezone: "UTC"}, query)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Totals.InteractiveSessions)
	assert.Equal(t, 2, report.Totals.SubagentSessions)
	assert.Equal(t, 1, report.Totals.AutomatedSessions)
	worker, err := reopened.GetSession(t.Context(), "worker")
	require.NoError(t, err)
	require.NotNil(t, worker)
	assert.Empty(t, worker.SessionKind)
	assert.Equal(t, "subagent", worker.RelationshipType)
	assert.Equal(t, db.CurrentDataVersion(), reopened.GetSessionDataVersion(t.Context(), "worker"))
	engine.Close()
	require.NoError(t, reopened.Close())
	db.SetUserAutomationPrefixes([]string{"Delegate a settings change."})
	t.Cleanup(func() { db.SetUserAutomationPrefixes(nil) })
	reclassified := dbtest.OpenTestDBAt(t, path)
	worker, err = reclassified.GetSession(t.Context(), "worker")
	require.NoError(t, err)
	require.NotNil(t, worker)
	assert.True(t, worker.IsAutomated)
	assert.Equal(t, "subagent", worker.RelationshipType)
	report, err = reclassified.GetActivityReport(t.Context(), db.AnalyticsFilter{Timezone: "UTC"}, query)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Totals.InteractiveSessions)
	assert.Equal(t, 1, report.Totals.SubagentSessions)
	assert.Equal(t, 2, report.Totals.AutomatedSessions)
}
