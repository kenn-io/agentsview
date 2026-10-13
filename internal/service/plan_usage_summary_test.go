package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

// TestPlanOnlySessionsLeftOutOfTotals checks that a codex-cloud session
// with only plan usage adds nothing to token and cost totals, is counted
// as left out, and marks its model in the model totals, both in process
// and through the HTTP API.
func TestPlanOnlySessionsLeftOutOfTotals(t *testing.T) {
	env := newHTTPBackendEnv(t)
	d := env.DB
	started := "2026-09-10T10:00:00Z"
	session := func(id, agent string) db.Session {
		return db.Session{
			ID: id, Project: "alpha", Machine: "local", Agent: agent,
			StartedAt: &started, EndedAt: &started, CreatedAt: started,
			MessageCount: 3, UserMessageCount: 2, DataVersion: 1,
		}
	}
	messages := func(id, model, tokenUsage string) []db.Message {
		return []db.Message{
			{SessionID: id, Ordinal: 0, Role: "user", Content: "a", Timestamp: started},
			{SessionID: id, Ordinal: 1, Role: "user", Content: "b", Timestamp: started},
			{
				SessionID: id, Ordinal: 2, Role: "assistant", Content: "c", Timestamp: started,
				Model: model, TokenUsage: []byte(tokenUsage),
			},
		}
	}
	_, err := d.WriteSessionBatch([]db.SessionBatchWrite{
		{
			Session:         session("claude-a", "claude"),
			Messages:        messages("claude-a", "claude-sonnet-4", `{"input_tokens":40,"output_tokens":7}`),
			ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session:  session("codex-cloud:plan-a", "codex-cloud"),
			Messages: messages("codex-cloud:plan-a", "gpt-5-codex", ""),
			PlanUsage: &db.SessionPlanUsage{
				HasPlanUsage: true, DataAsOf: started, DataStatus: "available",
				WeeklyPercent: new(1.5), Credits: new("0"),
			},
			ReplaceMessages: true, DataVersion: 1,
		},
	})
	require.NoError(t, err)
	for name, be := range map[string]service.SessionService{
		"direct": service.NewReadOnlyBackend(d),
		"http":   env.Backend("", false),
	} {
		t.Run(name, func(t *testing.T) { assertPlanOnlySummary(t, be) })
	}
}

func assertPlanOnlySummary(t *testing.T, be service.SessionService) {
	t.Helper()
	res, err := be.UsageSummary(t.Context(), service.UsageRequest{
		From: "2026-09-10", To: "2026-09-10", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 40, res.Totals.InputTokens)
	assert.Equal(t, 7, res.Totals.OutputTokens)
	assert.Equal(t, 1, res.SessionCounts.Total, "only the token session counts as billed")
	assert.Equal(t, 1, res.PlanOnlySessions)
	assert.Zero(t, res.PartlyCapturedSessions)
	planOnlyByModel := map[string]int{}
	for _, total := range res.ModelTotals {
		planOnlyByModel[total.Model] = total.PlanOnlySessions
		if total.Model == "gpt-5-codex" {
			assert.Zero(t, total.InputTokens+total.OutputTokens, "plan-only model has no tokens")
			assert.Zero(t, total.Cost.Microdollars, "plan-only model has no cost")
		}
	}
	assert.Equal(t, map[string]int{"claude-sonnet-4": 0, "gpt-5-codex": 1}, planOnlyByModel)

	// codex-cloud records per-message token data when live capture sees a
	// turn, so a codex-cloud report never gets the "no token data" notice;
	// the plan-only count explains the empty totals instead.
	res, err = be.UsageSummary(t.Context(), service.UsageRequest{
		From: "2026-09-10", To: "2026-09-10", Timezone: "UTC", Agent: "codex-cloud",
	})
	require.NoError(t, err)
	assert.Nil(t, res.UnsupportedUsage)
	assert.Zero(t, res.Totals.InputTokens)
	assert.Equal(t, 1, res.PlanOnlySessions)
}

// TestSessionDetailIncludesPlanUsage checks that session details carry a
// codex-cloud session's plan usage and capture coverage, in process and
// through the HTTP API, and leave both out for a session without them.
func TestSessionDetailIncludesPlanUsage(t *testing.T) {
	env := newHTTPBackendEnv(t)
	started := "2026-09-10T10:00:00Z"
	session := func(id, agent string) db.Session {
		return db.Session{
			ID: id, Project: "alpha", Machine: "local", Agent: agent,
			StartedAt: &started, CreatedAt: started, DataVersion: 1,
		}
	}
	_, err := env.DB.WriteSessionBatch([]db.SessionBatchWrite{
		{
			Session: session("codex-cloud:thread-a", "codex-cloud"),
			PlanUsage: &db.SessionPlanUsage{
				HasPlanUsage: true, OutcomeKind: "counted_under",
				OutcomeAt: "2026-09-11T08:00:00Z", CountedUnder: "codex-cloud:thread-root",
				HasCoverage: true, LastCaptureAt: "2026-09-10T12:00:00Z", UncapturedTurns: 1,
			},
			DataVersion: 1,
		},
		{
			Session:     session("codex-cloud:thread-b", "codex-cloud"),
			PlanUsage:   &db.SessionPlanUsage{HasPlanUsage: true, WeeklyPercent: new(0.5)},
			DataVersion: 1,
		},
		{Session: session("claude-a", "claude"), DataVersion: 1},
	})
	require.NoError(t, err)

	for name, be := range map[string]service.SessionService{
		"direct": service.NewReadOnlyBackend(env.DB),
		"http":   env.Backend("", false),
	} {
		t.Run(name, func(t *testing.T) {
			detail, err := be.Get(t.Context(), "codex-cloud:thread-a")
			require.NoError(t, err)
			require.NotNil(t, detail)
			assert.Equal(t, &db.PlanUsageView{
				Groups: []db.SessionPlanUsageGroup{}, OutcomeKind: "counted_under",
				OutcomeAt: "2026-09-11T08:00:00Z", CountedUnder: "codex-cloud:thread-root",
			}, detail.PlanUsage)
			assert.Equal(t, &db.CaptureCoverageView{
				LastCaptureAt: "2026-09-10T12:00:00Z", UncapturedTurns: 1,
			}, detail.CaptureCoverage)

			unseen, err := be.Get(t.Context(), "codex-cloud:thread-b")
			require.NoError(t, err)
			require.NotNil(t, unseen)
			require.NotNil(t, unseen.PlanUsage)
			assert.Nil(t, unseen.CaptureCoverage, "no live capture saw this thread")

			other, err := be.Get(t.Context(), "claude-a")
			require.NoError(t, err)
			require.NotNil(t, other)
			assert.Nil(t, other.PlanUsage)
			assert.Nil(t, other.CaptureCoverage)
		})
	}
}
