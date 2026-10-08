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
				`{"type":"user","uuid":"u1","timestamp":"2026-10-01T10:00:00Z","message":{"content":"Plan a settings change."}}` + "\n",
			reply:    `{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-10-01T10:01:00Z","message":{"content":[{"type":"text","text":"The plan is ready."}]}}` + "\n",
			followup: `{"type":"user","uuid":"u2","parentUuid":"a1","timestamp":"2026-10-01T10:02:00Z","message":{"content":"Explain the plan."}}` + "\n",
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
			path := filepath.Join(root, "project", "worker.jsonl")
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
				assert.Equal(t, i < 2 || tc.agent == parser.AgentCodex, stored.IsAutomated)
				if i == 1 {
					query, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-10-01", Timezone: "UTC"}, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
					require.NoError(t, err)
					report, err := database.BuildActivityReportArtifacts(t.Context(), db.AnalyticsFilter{}, query, nil)
					require.NoError(t, err)
					assert.Equal(t, 1, report.Report.Totals.AutomatedSessions)
					assert.Zero(t, report.Report.Totals.InteractiveSessions)
				}
			}
		})
	}
}
