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

func TestHeadlessAutomationSurvivesSyncAppends(t *testing.T) {
	for _, tc := range []struct {
		name, id, initial, reply, followup string
		agent                              parser.AgentType
		interactive                        bool
		nativeKind                         string
	}{
		{
			name: "claude", id: "worker", agent: parser.AgentClaude,
			initial: `{"type":"agent-setting","entrypoint":"sdk-cli"}` + "\n" +
				`{"type":"user","uuid":"u1","turnOrigin":"sdk","promptSource":"sdk","timestamp":"2026-10-01T10:00:00Z","message":{"content":"Plan a settings change."}}` + "\n",
			reply:    testjsonl.NewSessionBuilder().AddClaudeAssistantWithUUID("2026-10-01T10:01:00Z", "The plan is ready.", "a1", "u1").String(),
			followup: `{"type":"user","uuid":"u2","parentUuid":"a1","turnOrigin":"sdk","timestamp":"2026-10-01T10:02:00Z","message":{"content":"Explain the plan."}}` + "\n",
		},
		{
			name: "human", id: "human", agent: parser.AgentClaude, interactive: true,
			initial: `{"type":"agent-setting","entrypoint":"sdk-cli"}` + "\n" +
				`{"type":"user","uuid":"u1","turnOrigin":"human","origin":{"kind":"human"},"promptSource":"sdk","timestamp":"2026-10-01T10:00:00Z","message":{"content":"Plan a settings change."}}` + "\n",
			reply:    testjsonl.NewSessionBuilder().AddClaudeAssistantWithUUID("2026-10-01T10:01:00Z", "The plan is ready.", "a1", "u1").String(),
			followup: `{"type":"user","uuid":"u2","parentUuid":"a1","turnOrigin":"sdk","timestamp":"2026-10-01T10:02:00Z","message":{"content":"Explain the plan."}}` + "\n",
		},
		{
			name: "queued missing origin", id: "queued-missing", agent: parser.AgentClaude, interactive: true,
			initial: `{"type":"agent-setting","entrypoint":"sdk-cli"}` + "\n" +
				`{"type":"attachment","timestamp":"2026-10-01T10:00:00Z","attachment":{"type":"queued_command","prompt":"Plan a settings change."}}` + "\n",
			reply:    testjsonl.NewSessionBuilder().AddClaudeAssistantWithUUID("2026-10-01T10:01:00Z", "The plan is ready.", "a1", "").String(),
			followup: `{"type":"user","uuid":"u2","parentUuid":"a1","turnOrigin":"sdk","timestamp":"2026-10-01T10:02:00Z","message":{"content":"Explain the plan."}}` + "\n",
		},
		{
			name: "queued human", id: "queued-human", agent: parser.AgentClaude, interactive: true,
			initial: `{"type":"agent-setting","entrypoint":"sdk-cli"}` + "\n" +
				`{"type":"attachment","timestamp":"2026-10-01T10:00:00Z","attachment":{"type":"queued_command","origin":{"kind":"human"},"prompt":"Plan a settings change."}}` + "\n",
			reply:    testjsonl.NewSessionBuilder().AddClaudeAssistantWithUUID("2026-10-01T10:01:00Z", "The plan is ready.", "a1", "").String(),
			followup: `{"type":"user","uuid":"u2","parentUuid":"a1","turnOrigin":"sdk","timestamp":"2026-10-01T10:02:00Z","message":{"content":"Explain the plan."}}` + "\n",
		},
		{
			name: "unknown", id: "unknown", agent: parser.AgentClaude, interactive: true,
			initial: `{"type":"agent-setting","entrypoint":"sdk-cli"}` + "\n" +
				`{"type":"user","uuid":"u1","timestamp":"2026-10-01T10:00:00Z","message":{"content":"Plan a settings change."}}` + "\n",
			reply:    testjsonl.NewSessionBuilder().AddClaudeAssistantWithUUID("2026-10-01T10:01:00Z", "The plan is ready.", "a1", "u1").String(),
			followup: `{"type":"user","uuid":"u2","parentUuid":"a1","turnOrigin":"sdk","timestamp":"2026-10-01T10:02:00Z","message":{"content":"Explain the plan."}}` + "\n",
		},
		{
			name: "native kind after worker", id: "native-worker", agent: parser.AgentClaude, nativeKind: "bg",
			initial: `{"type":"agent-setting","entrypoint":"sdk-cli"}` + "\n" +
				`{"type":"user","uuid":"u1","turnOrigin":"sdk","timestamp":"2026-10-01T10:00:00Z","message":{"content":"Plan a settings change."}}` + "\n",
			reply: testjsonl.NewSessionBuilder().AddClaudeAssistantWithUUID("2026-10-01T10:01:00Z", "The plan is ready.", "a1", "u1").String(),
			followup: `{"type":"agent-setting","sessionKind":"bg"}` + "\n" +
				`{"type":"user","uuid":"u2","parentUuid":"a1","turnOrigin":"sdk","timestamp":"2026-10-01T10:02:00Z","message":{"content":"Explain the plan."}}` + "\n",
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
			query, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-10-01", Timezone: "UTC"}, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
			require.NoError(t, err)
			wantAutomated := 1
			if tc.interactive {
				wantAutomated = 0
			}
			content := tc.initial
			for i, tail := range []string{"", tc.reply, tc.followup} {
				content += tail
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
				require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
				stored, err := database.GetSession(t.Context(), tc.id)
				require.NoError(t, err)
				require.NotNil(t, stored)
				if tc.nativeKind != "" {
					if i == 2 {
						wantAutomated = 0
						assert.Equal(t, tc.nativeKind, stored.SessionKind)
					} else {
						assert.Equal(t, parser.SessionKindNonInteractive, stored.SessionKind)
					}
				}
				assert.Equal(t, wantAutomated == 1, stored.IsAutomated)
				if i == 0 {
					raw, err := sql.Open("sqlite3", database.Path())
					require.NoError(t, err)
					_, err = raw.ExecContext(t.Context(), `UPDATE sessions SET session_kind = '', is_automated = 0, data_version = 126 WHERE id = ?`, tc.id)
					require.NoError(t, err)
					require.NoError(t, raw.Close())
					require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
					assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), tc.id))
				}
				report, err := database.BuildActivityReportArtifacts(t.Context(), db.AnalyticsFilter{}, query, nil)
				require.NoError(t, err)
				assert.Equal(t, wantAutomated, report.Report.Totals.AutomatedSessions)
				assert.Equal(t, 1-wantAutomated, report.Report.Totals.InteractiveSessions)
			}
			engine.Close()
			require.NoError(t, database.Close())
			reopened, err := db.Open(t.Context(), database.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			require.NoError(t, os.Remove(path))
			restarted := sync.NewEngine(t.Context(), reopened, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{tc.agent: {root}}, Machine: "local"})
			t.Cleanup(restarted.Close)
			restarted.SyncAll(t.Context(), nil)
			stored, err := reopened.GetSession(t.Context(), tc.id)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Equal(t, wantAutomated == 1, stored.IsAutomated, "source-less archive after restart")
			report, err := reopened.BuildActivityReportArtifacts(t.Context(), db.AnalyticsFilter{}, query, nil)
			require.NoError(t, err)
			assert.Equal(t, wantAutomated, report.Report.Totals.AutomatedSessions)
			assert.Equal(t, 1-wantAutomated, report.Report.Totals.InteractiveSessions)
		})
	}
}
