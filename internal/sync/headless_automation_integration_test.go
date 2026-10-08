package sync_test

import (
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
			reply:    `{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-10-01T10:01:00Z","message":{"content":[{"type":"text","text":"The plan is ready."}]}}` + "\n",
			followup: `{"type":"user","uuid":"u2","parentUuid":"a1","turnOrigin":"sdk","timestamp":"2026-10-01T10:02:00Z","message":{"content":"Explain the plan."}}` + "\n",
		},
		{
			name: "human", id: "human", agent: parser.AgentClaude,
			initial: `{"type":"agent-setting","entrypoint":"sdk-cli"}` + "\n" +
				`{"type":"user","uuid":"u1","turnOrigin":"human","origin":{"kind":"human"},"promptSource":"sdk","timestamp":"2026-10-01T10:00:00Z","message":{"content":"Plan a settings change."}}` + "\n",
			reply:    `{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-10-01T10:01:00Z","message":{"content":[{"type":"text","text":"The plan is ready."}]}}` + "\n",
			followup: `{"type":"user","uuid":"u2","parentUuid":"a1","turnOrigin":"sdk","timestamp":"2026-10-01T10:02:00Z","message":{"content":"Explain the plan."}}` + "\n",
		},
		{
			name: "codex", id: "codex:019eb791-cf7d-75c1-8439-9ed74c122c80", agent: parser.AgentCodex,
			initial: `{"type":"session_meta","timestamp":"2026-10-01T10:00:00Z","payload":{"id":"019eb791-cf7d-75c1-8439-9ed74c122c80","originator":"codex_exec","source":"exec"}}` + "\n" +
				`{"type":"response_item","timestamp":"2026-10-01T10:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Plan a settings change."}]}}` + "\n",
			reply:    `{"type":"response_item","timestamp":"2026-10-01T10:01:00Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The plan is ready."}]}}` + "\n",
			followup: `{"type":"response_item","timestamp":"2026-10-01T10:02:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Explain the plan."}]}}` + "\n",
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
			if tc.name == "human" {
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
				assert.Equal(t, tc.name != "human", stored.IsAutomated)
				if i == 0 {
					require.NoError(t, database.SetSessionDataVersion(t.Context(), tc.id, 126))
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
			assert.Equal(t, tc.name != "human", stored.IsAutomated, "source-less archive after restart")
			report, err := reopened.BuildActivityReportArtifacts(t.Context(), db.AnalyticsFilter{}, query, nil)
			require.NoError(t, err)
			assert.Equal(t, wantAutomated, report.Report.Totals.AutomatedSessions)
			assert.Equal(t, 1-wantAutomated, report.Report.Totals.InteractiveSessions)
		})
	}
}
