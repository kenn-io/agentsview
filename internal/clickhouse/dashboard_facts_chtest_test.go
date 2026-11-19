//go:build chtest

package clickhouse

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/storage"
)

// dashboardPanels are the panel responses dashboard facts feed.
type dashboardPanels struct {
	Activity db.ActivityResponse
	Shape    db.SessionShapeResponse
	Velocity db.VelocityResponse
	Tools    db.ToolsAnalyticsResponse
	Skills   db.SkillsAnalyticsResponse
	Signals  db.SignalsAnalyticsResponse
}

// Panels read from the facts push stores must equal panels that scan the
// stored messages and tool calls, for every session or for only some, and
// must not need the messages at all once every row carries facts.
func TestDashboardFactsMatchMessageScans(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	hook := parser.ClaudeHookSubtypes()[0]
	msg := func(sessionID string, ordinal int, role, content, ts string, calls ...db.ToolCall) db.Message {
		m := fixtureMessage(sessionID, ordinal, role, content, ts, calls...)
		m.Model = "claude-test"
		return m
	}
	rich := []db.Message{
		// Sub-microsecond parts: the scan reads stored microseconds, so the
		// first turn cycle is exactly 30 s on both paths.
		msg("facts-rich", 0, "user", "please add the export feature", "2026-03-02T15:00:00.000000400Z"),
		msg("facts-rich", 1, "user", "hook output", "2026-03-02T15:00:05Z"),
		msg("facts-rich", 2, "assistant", "reading the code", "2026-03-02T15:00:30.000000900Z",
			db.ToolCall{ToolName: "Read", Category: "Read", ToolUseID: "r1", InputJSON: `{"path":"a.go"}`}),
		msg("facts-rich", 3, "user", "tool output", "2026-03-02T15:00:31Z"),
		msg("facts-rich", 4, "user", "this is broken!!! come on", "2026-03-02T15:01:00Z"),
		msg("facts-rich", 5, "user", "this is still broken!!! why won't it work", "2026-03-02T15:02:00Z"),
		msg("facts-rich", 6, "assistant", "fixed the export", "2026-03-02T15:02:10Z",
			db.ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "b1", InputJSON: `{"command":"go test"}`},
			// Whole seconds: SQLite keeps nanoseconds in a skill's last use
			// and ClickHouse microseconds.
			db.ToolCall{ToolName: "Skill", Category: "Skill", SkillName: "review", ToolUseID: "s1", InputJSON: `{}`}),
		// No timestamp: tool analytics date this call by its session.
		msg("facts-rich", 7, "assistant", "done", "",
			db.ToolCall{ToolName: "Skill", Category: "Skill", SkillName: "review", ToolUseID: "s2", InputJSON: `{}`}),
	}
	rich[1].SourceSubtype = hook
	rich[1].IsSystem = true
	rich[2].HasThinking = true
	rich[3].SourceSubtype = string(parser.SourceSubtypeToolResult)
	rich[4].IsSystem = true
	other := []db.Message{
		msg("facts-other", 0, "user", "run the linter", "2026-03-02T16:00:00Z"),
		msg("facts-other", 1, "assistant", "running it", "2026-03-02T16:00:45Z",
			db.ToolCall{ToolName: "Grep", Category: "Grep", ToolUseID: "g1", InputJSON: `{"pattern":"x"}`}),
	}
	// Both calls fall on March 3 in UTC but on March 2 and March 3 in New
	// York, so a read must group them by local day.
	late := []db.Message{
		msg("facts-late", 0, "user", "hello there friend", "2026-03-03T03:00:00Z",
			db.ToolCall{ToolName: "Skill", Category: "Skill", SkillName: "review", ToolUseID: "l1", InputJSON: `{}`}),
		msg("facts-late", 1, "assistant", "hello", "2026-03-03T06:00:00Z",
			db.ToolCall{ToolName: "Skill", Category: "Skill", SkillName: "review", ToolUseID: "l2", InputJSON: `{}`}),
	}
	session := func(id, agent, ts string, msgs []db.Message) db.SessionBatchWrite {
		s := fixtureSession(id, "facts", msgs[0].Content, ts, len(msgs))
		s.Agent = agent
		return db.SessionBatchWrite{Session: s, Messages: msgs, DataVersion: 1, ReplaceMessages: true}
	}
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{
		session("facts-rich", "claude", "2026-03-02T15:00:00Z", rich),
		session("facts-other", "codex", "2026-03-02T16:00:00Z", other),
		session("facts-late", "claude", "2026-03-03T03:00:00Z", late),
	})
	require.NoError(t, err)
	for _, id := range []string{"facts-rich", "facts-other", "facts-late"} {
		// Quality totals, frustration markers included, count only
		// sessions with computed quality signals.
		require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{
			Outcome: "success", OutcomeConfidence: "high",
			QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion},
		}))
	}

	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}
	_, err = newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	conn := chtest.Open(t, dsn, database)
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := conn.ExecContext(ctx, query+" SETTINGS mutations_sync = 2", args...)
		require.NoError(t, err)
	}

	filter := db.AnalyticsFilter{From: "2026-03-01", To: "2026-03-03", Timezone: "America/New_York"}
	// An hour filter makes tool and skill reads keep per-minute rows.
	hourFilter := filter
	hourFilter.Hour = new(10)
	type panelReader interface {
		GetAnalyticsActivity(context.Context, db.AnalyticsFilter, string) (db.ActivityResponse, error)
		GetAnalyticsSessionShape(context.Context, db.AnalyticsFilter) (db.SessionShapeResponse, error)
		GetAnalyticsVelocity(context.Context, db.AnalyticsFilter) (db.VelocityResponse, error)
		GetAnalyticsTools(context.Context, db.AnalyticsFilter) (db.ToolsAnalyticsResponse, error)
		GetAnalyticsSkills(context.Context, db.AnalyticsFilter, string) (db.SkillsAnalyticsResponse, error)
		GetAnalyticsSignals(context.Context, db.AnalyticsFilter) (db.SignalsAnalyticsResponse, error)
	}
	readFrom := func(store panelReader, filter db.AnalyticsFilter) dashboardPanels {
		t.Helper()
		var p dashboardPanels
		var err error
		p.Activity, err = store.GetAnalyticsActivity(ctx, filter, "day")
		require.NoError(t, err)
		p.Shape, err = store.GetAnalyticsSessionShape(ctx, filter)
		require.NoError(t, err)
		p.Velocity, err = store.GetAnalyticsVelocity(ctx, filter)
		require.NoError(t, err)
		p.Tools, err = store.GetAnalyticsTools(ctx, filter)
		require.NoError(t, err)
		p.Skills, err = store.GetAnalyticsSkills(ctx, filter, "day")
		require.NoError(t, err)
		p.Signals, err = store.GetAnalyticsSignals(ctx, filter)
		require.NoError(t, err)
		return p
	}
	read := func() [2]dashboardPanels {
		t.Helper()
		// A fresh store per read, so no memo answers from an earlier phase.
		return [2]dashboardPanels{readFrom(newTestStore(t, conn, target), filter), readFrom(newTestStore(t, conn, target), hourFilter)}
	}

	both := read()
	withFacts := both[0]
	assert.Equal(t, readFrom(local, filter), both[0], "ClickHouse matches the SQLite archive it mirrors")
	assert.Equal(t, readFrom(local, hourFilter), both[1], "ClickHouse matches the SQLite archive it mirrors")
	// The fixture reaches every fact: only the non-system marker prompt
	// counts, the hook and tool output rows are messages but not prompts.
	assert.Equal(t, 1, withFacts.Signals.QualityHealth.Totals.FrustrationMarkerCount)
	var day *db.ActivityEntry
	for i := range withFacts.Activity.Series {
		if withFacts.Activity.Series[i].Date == "2026-03-02" {
			day = &withFacts.Activity.Series[i]
		}
	}
	require.NotNil(t, day)
	assert.Equal(t, 12, day.Messages)
	assert.Equal(t, 4, day.UserMessages)
	assert.Equal(t, 1, day.ThinkingMessages)
	assert.Equal(t, 7, day.ToolCalls)
	assert.Equal(t, 7, withFacts.Tools.TotalCalls)
	assert.Equal(t, 4, withFacts.Skills.TotalSkillCalls)
	skillDays := map[string]int{}
	for _, entry := range withFacts.Skills.Trend {
		skillDays[entry.Date] = entry.BySkill["review"]
	}
	assert.Equal(t, map[string]int{"2026-03-01": 0, "2026-03-02": 3, "2026-03-03": 1}, skillDays)
	// Turn cycles skip the hook row and use stored microseconds: 30 s, not
	// 30.0000005 s, then 10 s.
	var cycles []float64
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT fact_velocity_turn_cycles FROM sessions WHERE id = 'facts-rich'").Scan(&cycles))
	assert.Equal(t, []float64{30, 10}, cycles)
	assert.Equal(t, 30.0, withFacts.Velocity.Overall.TurnCycleSec.P50)

	exec("ALTER TABLE sessions UPDATE dashboard_facts_version = 0 WHERE id = 'facts-rich'")
	exec("ALTER TABLE tool_calls UPDATE message_facts = false WHERE session_id = 'facts-rich'")
	assert.Equal(t, both, read(), "one session scanned, the others from facts")

	exec("ALTER TABLE sessions UPDATE dashboard_facts_version = 0 WHERE 1")
	exec("ALTER TABLE tool_calls UPDATE message_facts = false WHERE 1")
	assert.Equal(t, both, read(), "every session scanned")

	exec("ALTER TABLE sessions UPDATE dashboard_facts_version = ? WHERE 1", dashboardFactsVersion)
	exec("ALTER TABLE tool_calls UPDATE message_facts = true WHERE 1")
	exec("DELETE FROM messages WHERE 1")
	// An hour filter selects sessions by message time, so only the
	// unfiltered panels can do without messages.
	assert.Equal(t, withFacts, read()[0], "facts alone answer every panel")
}

// A push can publish a session's facts after the Velocity read finds the
// session without them and before it scans the sessions it did not find.
// The session must still reach the Velocity response.
func TestVelocityKeepsSessionPublishedBetweenReads(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	msgs := []db.Message{
		fixtureMessage("velocity-race", 0, "user", "add the export", "2026-03-02T15:00:00Z"),
		fixtureMessage("velocity-race", 1, "assistant", "added it", "2026-03-02T15:00:30Z"),
	}
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session:  fixtureSession("velocity-race", "facts", msgs[0].Content, "2026-03-02T15:00:00Z", len(msgs)),
		Messages: msgs, DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	dsn, database := chtest.FreshDatabase(t)
	_, err = newTestSync(t, local, Target{URL: dsn, Database: database}, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	conn := chtest.Open(t, dsn, database)
	setFactsVersion := func(version int) {
		t.Helper()
		_, err := conn.ExecContext(ctx,
			"ALTER TABLE sessions UPDATE dashboard_facts_version = ? WHERE id = 'velocity-race' SETTINGS mutations_sync = 2",
			version)
		require.NoError(t, err)
	}

	reads := analyticsSQL{newTestStore(t, conn, Target{URL: dsn, Database: database})}
	known, knownArgs, _ := chAnalyticsFactSets(db.AnalyticsFilter{From: "2026-03-01", To: "2026-03-03"})
	setFactsVersion(0)
	velocities, err := reads.knownVelocities(ctx, known, knownArgs)
	require.NoError(t, err)
	require.NotContains(t, velocities, "velocity-race", "the facts read runs before the push publishes")
	setFactsVersion(dashboardFactsVersion)
	require.NoError(t, reads.scanMissingVelocities(ctx, []string{"velocity-race"}, velocities, time.UTC))
	got := velocities["velocity-race"]
	assert.Equal(t, 2, got.Messages)
	assert.Equal(t, []float64{30}, got.TurnCycles)
}
