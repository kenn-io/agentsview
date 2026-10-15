//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

// seedBranchUsageFixture writes sessions on several branches. Every token
// of branch-test costs $0.001, so a row's cost is its token count in
// thousandths of a dollar.
//
//   - proj/main: 1 input token on 2026-03-01 and 2 on 2026-03-02
//   - proj/feature: 3 input tokens on 2026-03-01
//   - other/main: 4 input tokens on 2026-03-01, a same-named branch in
//     another project
//   - proj/snap-first and proj/snap-second: one Claude request recorded by
//     both sessions on 2026-03-01. snap-first records it first, so the
//     request is attributed to snap-first, while snap-second holds the
//     largest snapshot (5 input, 10 output), whose counters survive.
//   - cursor: sessionless Cursor usage charged $0.05 on 2026-03-01
func seedBranchUsageFixture(t *testing.T) (*db.DB, Target) {
	t.Helper()
	local := dbtest.OpenTestDB(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "branch-test",
		InputPerMTok:  money.MustParseDollars("1000"),
		OutputPerMTok: money.MustParseDollars("1000"),
	}}))
	session := func(id, project, branch, ts string, messages ...db.Message) db.SessionBatchWrite {
		s := fixtureSession(id, project, id+" first", ts, len(messages))
		s.GitBranch = branch
		return db.SessionBatchWrite{
			Session: s, Messages: messages, DataVersion: 1, ReplaceMessages: true,
		}
	}
	message := func(sessionID string, ordinal int, ts, tokenUsage string) db.Message {
		return usagePriceMessage(sessionID, ordinal, ts, "branch-test", tokenUsage)
	}
	snapshot := func(sessionID, ts, tokenUsage string) db.Message {
		m := message(sessionID, 0, ts, tokenUsage)
		m.ClaudeMessageID = "msg_branch"
		m.ClaudeRequestID = "req_branch"
		return m
	}
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{
		session("branch-main", "proj", "main", "2026-03-01T10:00:00.000Z",
			message("branch-main", 0, "2026-03-01T10:00:00.000Z", `{"input_tokens":1}`),
			message("branch-main", 1, "2026-03-02T10:00:00.000Z", `{"input_tokens":2}`)),
		session("branch-feature", "proj", "feature", "2026-03-01T10:05:00.000Z",
			message("branch-feature", 0, "2026-03-01T10:05:00.000Z", `{"input_tokens":3}`)),
		session("branch-other", "other", "main", "2026-03-01T10:10:00.000Z",
			message("branch-other", 0, "2026-03-01T10:10:00.000Z", `{"input_tokens":4}`)),
		session("branch-snap-first", "proj", "snap-first", "2026-03-01T10:20:00.000Z",
			snapshot("branch-snap-first", "2026-03-01T10:20:00.000Z", `{"input_tokens":5,"output_tokens":1}`)),
		session("branch-snap-second", "proj", "snap-second", "2026-03-01T10:21:00.000Z",
			snapshot("branch-snap-second", "2026-03-01T10:21:00.000Z", `{"input_tokens":5,"output_tokens":10}`)),
	})
	require.NoError(t, err)
	require.NoError(t, local.InsertCursorUsageEvents(t.Context(), []db.CursorUsageEvent{{
		OccurredAt: "2026-03-01T10:30:00.000Z", Model: "branch-test", Kind: "usage",
		InputTokens: 7, Charged: money.MustParseDollars("0.05"), DedupKey: "branch-cursor",
	}}))
	dsn, database := chtest.FreshDatabase(t)
	return local, Target{URL: dsn, Database: database}
}

type branchCost struct {
	project, branch string
	input, output   int
	cost            money.Money
}

// branchCosts flattens a day's breakdowns. The project keys are checked
// separately against the day's project breakdowns.
func branchCosts(breakdowns []db.BranchBreakdown) []branchCost {
	out := make([]branchCost, 0, len(breakdowns))
	for _, b := range breakdowns {
		out = append(out, branchCost{
			project: b.Project, branch: b.Branch,
			input: b.InputTokens, output: b.OutputTokens, cost: b.Cost,
		})
	}
	return out
}

func TestDailyUsageBranchBreakdowns(t *testing.T) {
	ctx := t.Context()
	local, target := seedBranchUsageFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store := NewStoreFromDB(syncer.conn)
	// The stopped view holds the push's refresh so the first read uses the
	// raw rows and the second the prepared rows.
	_, err := store.DB().ExecContext(ctx, "SYSTEM STOP VIEW prepare_usage")
	require.NoError(t, err)
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)

	dollars := money.MustParseDollars
	wantDays := map[string][]branchCost{
		"2026-03-01": {
			{project: "proj", branch: "snap-first", input: 5, output: 10, cost: dollars("0.015")},
			{project: "other", branch: "main", input: 4, cost: dollars("0.004")},
			{project: "proj", branch: "feature", input: 3, cost: dollars("0.003")},
			{project: "proj", branch: "main", input: 1, cost: dollars("0.001")},
		},
		"2026-03-02": {
			{project: "proj", branch: "main", input: 2, cost: dollars("0.002")},
		},
	}
	wantDayTotals := map[string]money.Money{
		"2026-03-01": dollars("0.073"),
		"2026-03-02": dollars("0.002"),
	}
	wantRange := map[[2]string]money.Money{
		{"proj", "main"}:       dollars("0.003"),
		{"proj", "feature"}:    dollars("0.003"),
		{"other", "main"}:      dollars("0.004"),
		{"proj", "snap-first"}: dollars("0.015"),
	}

	check := func(name string) {
		t.Helper()
		daily, err := store.GetDailyUsage(ctx, db.UsageFilter{
			Timezone: "UTC", Breakdowns: true, BranchBreakdowns: true,
		})
		require.NoError(t, err, name)
		gotDays := map[string][]branchCost{}
		gotDayTotals := map[string]money.Money{}
		gotRange := map[[2]string]money.Money{}
		for _, day := range daily.Daily {
			gotDays[day.Date] = branchCosts(day.BranchBreakdowns)
			gotDayTotals[day.Date] = day.TotalCost
			projectKeys := map[string]string{}
			for _, p := range day.ProjectBreakdowns {
				projectKeys[p.Project] = p.ProjectKey
			}
			for _, b := range day.BranchBreakdowns {
				assert.NotEmpty(t, b.ProjectKey, "%s: %s/%s", name, b.Project, b.Branch)
				assert.Equal(t, projectKeys[b.Project], b.ProjectKey,
					"%s: %s/%s", name, b.Project, b.Branch)
				key := [2]string{b.Project, b.Branch}
				gotRange[key], err = money.Add(gotRange[key], b.Cost)
				require.NoError(t, err)
			}
		}
		assert.Equal(t, wantDays, gotDays, name)
		assert.Equal(t, wantDayTotals, gotDayTotals, name)
		assert.Equal(t, wantRange, gotRange, name)
		// The Cursor charge counts toward the total but no branch.
		assert.Equal(t, dollars("0.075"), daily.Totals.TotalCost, name)
	}

	ready, err := store.preparedUsageReady(ctx)
	require.NoError(t, err)
	require.False(t, ready)
	check("raw")
	for _, stmt := range []string{
		"SYSTEM START VIEW prepare_usage", "SYSTEM REFRESH VIEW prepare_usage", "SYSTEM WAIT VIEW prepare_usage",
	} {
		_, err := store.DB().ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	ready, err = store.preparedUsageReady(ctx)
	require.NoError(t, err)
	require.True(t, ready)
	check("prepared")
}
