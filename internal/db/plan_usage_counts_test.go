package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedPlanUsageCountSessions writes sessions that cover each plan usage
// count rule:
//
//   - codex-cloud:plan-a (project alpha, machine m1): plan-only, assistant
//     messages gpt-5-codex at 2026-09-10T10:01Z and gpt-5-mini at
//     2026-09-12T23:30Z; capture missed turns, which must not make it
//     partly captured.
//   - codex-cloud:plan-nomsg (project beta, machine m2): plan-only, no
//     assistant message, started 2026-09-11T05:00Z.
//   - codex-cloud:partly (project alpha): an assistant message and a
//     gpt-5-codex usage event at 2026-09-10T12:00Z, 2 turns not captured.
//   - codex-cloud:full (project alpha): an assistant message and a usage
//     event, every turn captured.
//   - claude-a (project alpha): token messages, no plan usage row.
//   - claude-idle (project alpha): no assistant message, no plan usage row.
func seedPlanUsageCountSessions(t *testing.T, d *DB) {
	t.Helper()
	session := func(id, agent, project, machine, started string) Session {
		return Session{
			ID: id, Project: project, Machine: machine, Agent: agent,
			StartedAt: &started, EndedAt: &started, CreatedAt: started,
			MessageCount: 4, UserMessageCount: 2, DataVersion: 1,
		}
	}
	msg := func(id string, ordinal int, role, model, ts string) Message {
		return Message{SessionID: id, Ordinal: ordinal, Role: role, Content: role, Model: model, Timestamp: ts}
	}
	userTurns := func(id, ts string) []Message {
		return []Message{msg(id, 0, "user", "", ts), msg(id, 1, "user", "", ts)}
	}
	captured := func(id string) []Message {
		return append(userTurns(id, "2026-09-10T11:00:00Z"),
			msg(id, 2, "assistant", "gpt-5-codex", "2026-09-10T12:00:00Z"))
	}
	event := func(id, model, ts string) UsageEvent {
		return UsageEvent{
			SessionID: id, Source: "codex-cloud-request", Model: model,
			InputTokens: 100, OutputTokens: 10, OccurredAt: ts, DedupKey: id + ts,
		}
	}
	coverage := func(uncaptured int) *SessionPlanUsage {
		return &SessionPlanUsage{HasCoverage: true, LastCaptureAt: "2026-09-12T00:00:00Z", UncapturedTurns: uncaptured}
	}
	planA := append(userTurns("codex-cloud:plan-a", "2026-09-10T10:00:00Z"),
		msg("codex-cloud:plan-a", 2, "assistant", "gpt-5-codex", "2026-09-10T10:01:00Z"),
		msg("codex-cloud:plan-a", 3, "assistant", "gpt-5-mini", "2026-09-12T23:30:00Z"))
	planAUsage := rootPlanUsage()
	planAUsage.UncapturedTurns = 3
	claudeMessages := append(userTurns("claude-a", "2026-09-10T09:00:00Z"),
		msg("claude-a", 2, "assistant", "claude-sonnet-4", "2026-09-10T09:01:00Z"))
	claudeMessages[2].TokenUsage = []byte(`{"input_tokens":5,"output_tokens":7}`)

	_, err := d.WriteSessionBatch([]SessionBatchWrite{
		{
			Session:  session("codex-cloud:plan-a", "codex-cloud", "alpha", "m1", "2026-09-10T10:00:00Z"),
			Messages: planA, PlanUsage: planAUsage, ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session:   session("codex-cloud:plan-nomsg", "codex-cloud", "beta", "m2", "2026-09-11T05:00:00Z"),
			Messages:  userTurns("codex-cloud:plan-nomsg", "2026-09-11T05:00:00Z"),
			PlanUsage: rootPlanUsage(), ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session:     session("codex-cloud:partly", "codex-cloud", "alpha", "m1", "2026-09-10T11:00:00Z"),
			Messages:    captured("codex-cloud:partly"),
			UsageEvents: []UsageEvent{event("codex-cloud:partly", "gpt-5-codex", "2026-09-10T12:00:00Z")},
			PlanUsage:   coverage(2), ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session:     session("codex-cloud:full", "codex-cloud", "alpha", "m1", "2026-09-10T11:00:00Z"),
			Messages:    captured("codex-cloud:full"),
			UsageEvents: []UsageEvent{event("codex-cloud:full", "gpt-5-codex", "2026-09-10T12:00:00Z")},
			PlanUsage:   coverage(0), ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session:  session("claude-a", "claude", "alpha", "m1", "2026-09-10T09:00:00Z"),
			Messages: claudeMessages, ReplaceMessages: true, DataVersion: 1,
		},
		{
			Session:  session("claude-idle", "claude", "alpha", "m1", "2026-09-10T09:00:00Z"),
			Messages: userTurns("claude-idle", "2026-09-10T09:00:00Z"), ReplaceMessages: true, DataVersion: 1,
		},
	})
	require.NoError(t, err)
}

func TestPlanOnlyCountFilters(t *testing.T) {
	d := testDB(t)
	seedPlanUsageCountSessions(t, d)

	for _, tc := range []struct {
		name    string
		filter  UsageFilter
		count   int
		byModel map[string]int
	}{
		{
			name:    "no filter",
			count:   2,
			byModel: map[string]int{"gpt-5-codex": 1, "gpt-5-mini": 1},
		},
		{name: "other agent", filter: UsageFilter{Agent: "claude"}, byModel: map[string]int{}},
		{
			name:    "excluded agent",
			filter:  UsageFilter{ExcludeAgent: "codex-cloud"},
			byModel: map[string]int{},
		},
		{
			name:    "project",
			filter:  UsageFilter{Project: "alpha"},
			count:   1,
			byModel: map[string]int{"gpt-5-codex": 1, "gpt-5-mini": 1},
		},
		{name: "machine", filter: UsageFilter{Machine: "m2"}, count: 1, byModel: map[string]int{}},
		{
			name:    "include model drops the session with no assistant message",
			filter:  UsageFilter{Model: "gpt-5-mini"},
			count:   1,
			byModel: map[string]int{"gpt-5-mini": 1},
		},
		{
			name:    "exclude model keeps the session with no assistant message",
			filter:  UsageFilter{ExcludeModel: "gpt-5-codex"},
			count:   2,
			byModel: map[string]int{"gpt-5-mini": 1},
		},
		{
			name:    "window matches an assistant message",
			filter:  UsageFilter{From: "2026-09-12", To: "2026-09-12", Timezone: "UTC"},
			count:   1,
			byModel: map[string]int{"gpt-5-mini": 1},
		},
		{
			name:    "window matches started_at of a session with no assistant message",
			filter:  UsageFilter{From: "2026-09-11", To: "2026-09-11", Timezone: "UTC"},
			count:   1,
			byModel: map[string]int{},
		},
		{
			name: "a session whose assistant messages are all excluded is not matched by started_at",
			filter: UsageFilter{
				From: "2026-09-10", To: "2026-09-10", Timezone: "UTC",
				ExcludeModel: "gpt-5-codex,gpt-5-mini",
			},
			byModel: map[string]int{},
		},
		{
			name:    "window outside every session",
			filter:  UsageFilter{From: "2026-09-13", To: "2026-09-20", Timezone: "UTC"},
			byModel: map[string]int{},
		},
		{
			name:    "timezone moves a message into the window",
			filter:  UsageFilter{From: "2026-09-13", To: "2026-09-13", Timezone: "Asia/Tokyo"},
			count:   1,
			byModel: map[string]int{"gpt-5-mini": 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d.GetPlanUsageSessionCounts(t.Context(), tc.filter)
			require.NoError(t, err)
			assert.Equal(t, tc.count, got.PlanOnly, "plan-only sessions")
			assert.Equal(t, tc.byModel, got.PlanOnlyByModel, "plan-only sessions by model")
		})
	}
}

func TestPartlyCapturedCount(t *testing.T) {
	d := testDB(t)
	seedPlanUsageCountSessions(t, d)

	for _, tc := range []struct {
		name   string
		filter UsageFilter
		want   int
	}{
		// Only codex-cloud:partly counts: codex-cloud:full captured every
		// turn and codex-cloud:plan-a has no usage events.
		{name: "no filter", want: 1},
		{name: "event in window", filter: UsageFilter{From: "2026-09-10", To: "2026-09-10", Timezone: "UTC"}, want: 1},
		{name: "event outside window", filter: UsageFilter{From: "2026-09-11", To: "2026-09-12", Timezone: "UTC"}},
		{name: "event model included", filter: UsageFilter{Model: "gpt-5-codex"}, want: 1},
		{name: "event model excluded", filter: UsageFilter{ExcludeModel: "gpt-5-codex"}},
		{name: "other project", filter: UsageFilter{Project: "beta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d.GetPlanUsageSessionCounts(t.Context(), tc.filter)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.PartlyCaptured)
		})
	}
}

func TestExportPlanOnlyEmptyCost(t *testing.T) {
	d := testDB(t)
	seedPlanUsageCountSessions(t, d)
	started := "2026-09-10T10:00:00Z"
	_, err := d.WriteSessionBatch([]SessionBatchWrite{{
		Session: Session{
			ID: "codex-cloud:unchecked", Project: "alpha", Machine: "m1", Agent: "codex-cloud",
			StartedAt: &started, CreatedAt: started, MessageCount: 1, UserMessageCount: 1, DataVersion: 1,
		},
		Messages: []Message{{
			SessionID: "codex-cloud:unchecked", Role: "user", Content: "user", Timestamp: started,
		}},
		ReplaceMessages: true, DataVersion: 1,
	}})
	require.NoError(t, err)

	result, err := d.ExportSessionSummaries(t.Context(), SessionExportOptions{Limit: 10})
	require.NoError(t, err)
	rows := map[string]SessionSummaryRow{}
	for _, row := range result.Rows {
		rows[row.ID] = row
	}

	planOnly := rows["codex-cloud:plan-a"]
	assert.Nil(t, planOnly.ModelUsage, "a plan-only session exports no token or cost figures")
	require.NotNil(t, planOnly.PlanUsage)
	assert.Equal(t, new(2.25), planOnly.PlanUsage.WeeklyPercent)
	assert.Equal(t, &CaptureCoverageView{LastCaptureAt: "2026-10-01T12:30:00Z", UncapturedTurns: 3},
		planOnly.CaptureCoverage)

	partly := rows["codex-cloud:partly"]
	require.NotNil(t, partly.ModelUsage, "captured tokens still export")
	assert.Equal(t, 100, partly.ModelUsage.InputTokens)
	assert.Nil(t, partly.PlanUsage, "a coverage-only row has no plan usage")
	assert.Equal(t, &CaptureCoverageView{LastCaptureAt: "2026-09-12T00:00:00Z", UncapturedTurns: 2},
		partly.CaptureCoverage)

	claude := rows["claude-a"]
	assert.Nil(t, claude.PlanUsage)
	assert.Nil(t, claude.CaptureCoverage)

	unchecked := rows["codex-cloud:unchecked"]
	assert.Nil(t, unchecked.ModelUsage,
		"a codex-cloud session with no plan usage row yet has unknown, not zero, usage")
	assert.Nil(t, unchecked.PlanUsage)

	// Other sessions without usage keep the zero model usage they always had.
	idle := rows["claude-idle"]
	require.NotNil(t, idle.ModelUsage)
	assert.Empty(t, idle.ModelUsage.Models)
}
