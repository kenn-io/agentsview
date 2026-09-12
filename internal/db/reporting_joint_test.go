package db

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestReportingJointCellsPreserveTimeAndDimensions(t *testing.T) {
	d := testDB(t)
	seedJointReporting(t, d)
	opts := ReportingExportOptions{Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now: time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC), SchemaVersion: 4}
	day, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	hour := day.Hours[12]
	require.NotNil(t, hour.Joint)
	require.Len(t, hour.Joint.Cells, 4)
	assert.Equal(t, 2, hour.Activity.Buckets[0].MaxAgents)
	assert.Equal(t, int64(600), hour.Usage.Totals.OutputTokens)
	assert.Equal(t, int64(6_000_000), hour.Usage.Totals.Cost.Microdollars)

	var projectAKey string
	for _, cell := range hour.Joint.Cells {
		require.NotEmpty(t, cell.ProjectKey)
		if cell.Project == "project-a" {
			projectAKey = cell.ProjectKey
		}
		switch {
		case cell.Project == "project-b":
			assert.Equal(t, "agent-b", cell.Agent)
			assert.Equal(t, "automated", cell.Automation)
			assert.Equal(t, 3.0, cell.AgentMinutes)
			assert.Equal(t, int64(300), cell.Usage.OutputTokens)
		case cell.Model == "model-a":
			assert.Equal(t, "2026-07-28T12:00:00Z", cell.BucketStart)
			assert.Equal(t, 2.0, cell.AgentMinutes)
			assert.Equal(t, int64(100), cell.Usage.OutputTokens)
		case cell.BucketStart == "2026-07-28T12:00:00Z":
			assert.Equal(t, 3.0, cell.AgentMinutes)
			assert.Zero(t, cell.Usage.OutputTokens)
		default:
			assert.Equal(t, "2026-07-28T12:05:00Z", cell.BucketStart)
			assert.Zero(t, cell.AgentMinutes)
			assert.Equal(t, int64(200), cell.Usage.OutputTokens)
		}
		assert.Equal(t, cell.Usage.Cost, cell.Pricing.ComputedCost)
	}

	opts.ProjectKeys = []string{projectAKey, projectAKey}
	scoped, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, []string{projectAKey}, scoped.Hours[12].Joint.ProjectKeys)
	assert.Equal(t, 1, scoped.Hours[12].Activity.Buckets[0].MaxAgents)
	assert.Equal(t, int64(300), scoped.Hours[12].Usage.Totals.OutputTokens)
	assert.NotEqual(t, hour.Digest, scoped.Hours[12].Digest)
	assert.NotEqual(t, day.Hours[0].Digest, scoped.Hours[0].Digest, "quiet hours still bind their scope")
	for _, cell := range scoped.Hours[12].Joint.Cells {
		assert.Equal(t, "project-a", cell.Project)
	}

	oracle, err := d.GetActivityReport(t.Context(), AnalyticsFilter{Timezone: "UTC"}, dayQuery(t, "2026-07-28", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 8.0, oracle.Totals.AgentMinutes)
	assert.Equal(t, oracle.Totals.AgentMinutes, hour.Activity.Totals.AgentMinutes)
	assert.Equal(t, oracle.Peak.Agents, hour.Activity.Peak.Agents)
}

func TestReportingJointSubagentsTakePrecedenceOverAutomation(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "model-a", OutputPerMTok: money.MustParseDollars("1"),
	}}))
	for _, session := range []struct {
		id                  string
		subagent, automated bool
	}{
		{"root", false, false},
		{"child", true, false},
		{"automated-child", true, true},
		{"automated", false, true},
	} {
		insertSession(t, d, session.id, "project-a", func(s *Session) {
			s.Agent = "agent-a"
			s.StartedAt, s.EndedAt = new("2026-07-28T12:00:00Z"), new("2026-07-28T12:01:00Z")
			s.IsAutomated = session.automated
			if session.subagent {
				s.ParentSessionID = new("root")
				s.RelationshipType = "subagent"
			}
		})
		insertMessages(t, d,
			Message{SessionID: session.id, Ordinal: 0, Role: "user", Timestamp: "2026-07-28T12:00:00Z"},
			Message{SessionID: session.id, Ordinal: 1, Role: "assistant", Timestamp: "2026-07-28T12:01:00Z",
				Model: "model-a", TokenUsage: jsontext.Value(`{"output_tokens":10}`)},
		)
	}
	day, err := d.ExportReportingDay(t.Context(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC), SchemaVersion: 4,
	})
	require.NoError(t, err)
	hour := day.Hours[12]
	require.NotNil(t, hour.Joint)
	require.Len(t, hour.Joint.Cells, 3)
	for i, want := range []struct {
		category string
		minutes  float64
		peak     int
		tokens   int64
	}{
		{"automated", 1, 1, 10},
		{"interactive", 1, 1, 10},
		{"subagent", 2, 2, 20},
	} {
		cell := hour.Joint.Cells[i]
		assert.Equal(t, want.category, cell.Automation)
		assert.Equal(t, want.minutes, cell.AgentMinutes)
		assert.Equal(t, want.peak, cell.MaxAgents)
		assert.Equal(t, want.tokens, cell.Usage.OutputTokens)
		assert.Equal(t, money.Money{Microdollars: want.tokens}, cell.Usage.Cost)
		assert.Equal(t, cell.Usage.Cost, cell.Pricing.ComputedCost)
	}
	assert.Equal(t, 1.0, hour.Activity.Totals.InteractiveAgentMinutes)
	assert.Equal(t, 2.0, hour.Activity.Totals.SubagentAgentMinutes)
	assert.Equal(t, 1.0, hour.Activity.Totals.AutomatedAgentMinutes)
	assert.Equal(t, 2, hour.Activity.Buckets[0].MaxSubagentAgents)
	assert.Equal(t, money.Money{Microdollars: 20}, hour.Activity.Totals.SubagentCost)
}

func TestReportingJointCorrectionsReplaceCellsWithinSnapshot(t *testing.T) {
	d := testDB(t)
	seedJointReporting(t, d)
	opts := ReportingExportOptions{Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now: time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC), SchemaVersion: 4}
	before, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	opts.afterSnapshot = func() {
		_, writeErr := d.getWriter().Exec(`UPDATE messages SET model = 'model-c' WHERE session_id = 'joint-a' AND model = 'model-a'`)
		require.NoError(t, writeErr)
	}
	during, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, before.Digest, during.Digest)
	opts.afterSnapshot = nil
	after, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	assert.NotEqual(t, before.Hours[12].Digest, after.Hours[12].Digest)
	for _, cell := range after.Hours[12].Joint.Cells {
		assert.NotEqual(t, "model-a", cell.Model)
		if cell.Model == "model-c" {
			assert.Equal(t, int64(1), cell.Pricing.UnpricedRows)
		}
	}
	_, err = d.getWriter().Exec(`DELETE FROM sessions WHERE id IN ('joint-a', 'joint-b')`)
	require.NoError(t, err)
	empty, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	assert.False(t, empty.Hours[12].HasData)
	assert.Empty(t, empty.Hours[12].Joint.Cells)
	assert.Zero(t, empty.Hours[12].Activity.Buckets[0].MaxAgents)
	assert.NotEqual(t, after.Hours[12].Digest, empty.Hours[12].Digest)
}

func seedJointReporting(t *testing.T, d *DB) {
	t.Helper()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "model-a", OutputPerMTok: money.MustParseDollars("10000")},
		{ModelPattern: "model-b", OutputPerMTok: money.MustParseDollars("10000")},
	}))
	insertSession(t, d, "joint-a", "project-a", func(s *Session) {
		s.Agent, s.StartedAt, s.EndedAt = "agent-a", Ptr("2026-07-28T12:00:00Z"), Ptr("2026-07-28T12:05:00Z")
	})
	insertSession(t, d, "joint-b", "project-b", func(s *Session) {
		s.Agent, s.StartedAt, s.EndedAt = "agent-b", Ptr("2026-07-28T12:01:00Z"), Ptr("2026-07-28T12:04:00Z")
		s.IsAutomated = true
	})
	insertMessages(t, d,
		Message{SessionID: "joint-a", Ordinal: 0, Role: "user", Timestamp: "2026-07-28T12:00:00Z", Content: "synthetic private prose"},
		Message{SessionID: "joint-a", Ordinal: 1, Role: "assistant", Timestamp: "2026-07-28T12:02:00Z", Model: "model-a", TokenUsage: jsontext.Value(`{"output_tokens":100}`)},
		Message{SessionID: "joint-a", Ordinal: 2, Role: "assistant", Timestamp: "2026-07-28T12:05:00Z", Model: "model-b", TokenUsage: jsontext.Value(`{"output_tokens":200}`)},
		Message{SessionID: "joint-b", Ordinal: 0, Role: "user", Timestamp: "2026-07-28T12:01:00Z"},
		Message{SessionID: "joint-b", Ordinal: 1, Role: "assistant", Timestamp: "2026-07-28T12:04:00Z", Model: "model-b", TokenUsage: jsontext.Value(`{"output_tokens":300}`)},
	)
}

func TestReportingJointProjectScopeRequiresNewVersion(t *testing.T) {
	d := testDB(t)
	_, err := d.ExportReportingDay(t.Context(), ReportingExportOptions{
		Date:          time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:           time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC),
		SchemaVersion: export.ReportingSchemaVersion, ProjectKeys: []string{"project-key"},
	})
	assert.ErrorContains(t, err, "project scope requires reporting schema 4")
}

func TestReportingJointScopeDoesNotRechargeDuplicateUsage(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "shared", "project-a", func(s *Session) {
		s.StartedAt, s.EndedAt = Ptr("2026-07-28T09:00:00Z"), Ptr("2026-07-28T09:01:00Z")
	})
	reported := money.MustParseDollars("0.002")
	require.NoError(t, d.ReplaceSessionUsageEvents("shared", []UsageEvent{{
		Source: "fixture", Model: "model-a", InputTokens: 41, Cost: &reported,
		CostStatus: "exact", CostSource: "reported", OccurredAt: "2026-07-28T09:05:00Z", DedupKey: "same",
	}}))
	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt: "2026-07-28T09:05:00Z", Model: "standalone", Kind: "usage",
		InputTokens: 17, Charged: money.MustParseDollars("0.007"), DedupKey: "shared:fixture:same",
	}}))
	// Give this project a real activity cell so its exported key is discoverable.
	insertMessages(t, d,
		Message{SessionID: "shared", Ordinal: 0, Role: "user", Timestamp: "2026-07-28T09:00:00Z"},
		Message{SessionID: "shared", Ordinal: 1, Role: "assistant", Timestamp: "2026-07-28T09:01:00Z"},
	)
	opts := ReportingExportOptions{Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now: time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC), SchemaVersion: 4}
	all, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, int64(17), all.Hours[9].Usage.Totals.InputTokens)
	var key string
	for _, cell := range all.Hours[9].Joint.Cells {
		if cell.Project == "project-a" {
			key = cell.ProjectKey
		}
	}
	require.NotEmpty(t, key)
	opts.ProjectKeys = []string{key}
	scoped, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, 1.0, scoped.Hours[9].Activity.Totals.AgentMinutes)
	assert.Zero(t, scoped.Hours[9].Usage.Totals.InputTokens)
	assert.Zero(t, scoped.Hours[9].Usage.Totals.Cost.Microdollars)

	opts.ProjectKeys = []string{"absent-project-key"}
	empty, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	assert.False(t, empty.HasData)
	assert.Empty(t, empty.Hours[9].Joint.Cells)
	assert.Equal(t, opts.ProjectKeys, empty.Hours[9].Joint.ProjectKeys)
}

func TestReportingJointUnpricedTokensRetainKnownSearchFees(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "partial-price", "project-a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt, s.EndedAt = Ptr("2026-07-28T12:00:00Z"), Ptr("2026-07-28T12:01:00Z")
	})
	insertMessages(t, d, Message{SessionID: "partial-price", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-07-28T12:01:00Z", Model: "synthetic-unpriced-model",
		TokenUsage: jsontext.Value(`{"input_tokens":100,"server_tool_use":{"web_search_requests":2}}`),
	})
	day, err := d.ExportReportingDay(t.Context(), ReportingExportOptions{
		Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now:  time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC), SchemaVersion: 4,
	})
	require.NoError(t, err)
	require.Len(t, day.Hours[12].Joint.Cells, 1)
	cell := day.Hours[12].Joint.Cells[0]
	assert.Equal(t, int64(20_000), cell.Usage.Cost.Microdollars)
	assert.Equal(t, int64(20_000), cell.Pricing.ComputedCost.Microdollars)
	assert.Equal(t, int64(1), cell.Pricing.UnpricedRows)
}

func TestReportingJointStandaloneUsageDoesNotInheritEmptyLabelProject(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "empty-label", "", func(s *Session) {
		s.StartedAt, s.EndedAt = Ptr("2026-07-28T09:00:00Z"), Ptr("2026-07-28T09:01:00Z")
	})
	insertMessages(t, d,
		Message{SessionID: "empty-label", Ordinal: 0, Role: "user", Timestamp: "2026-07-28T09:00:00Z"},
		Message{SessionID: "empty-label", Ordinal: 1, Role: "assistant", Timestamp: "2026-07-28T09:01:00Z"},
	)
	require.NoError(t, d.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt: "2026-07-28T09:01:00Z", Model: "standalone", Kind: "usage",
		InputTokens: 17, Charged: money.MustParseDollars("0.007"), DedupKey: "standalone",
	}}))
	opts := ReportingExportOptions{Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		Now: time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC), SchemaVersion: 4}
	all, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	require.Len(t, all.Hours[9].Joint.Cells, 2)
	var sessionProjectKey string
	for _, cell := range all.Hours[9].Joint.Cells {
		if cell.Model == "standalone" {
			assert.Empty(t, cell.ProjectKey, "standalone cost has no session project")
			assert.Equal(t, int64(17), cell.Usage.InputTokens)
			assert.Equal(t, int64(7_000), cell.Usage.Cost.Microdollars)
		} else {
			sessionProjectKey = cell.ProjectKey
			assert.Equal(t, 1.0, cell.AgentMinutes)
		}
	}
	require.NotEmpty(t, sessionProjectKey, "an empty label still has a real session project identity")
	opts.ProjectKeys = []string{sessionProjectKey}
	scoped, err := d.ExportReportingDay(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, 1.0, scoped.Hours[9].Activity.Totals.AgentMinutes)
	assert.Zero(t, scoped.Hours[9].Usage.Totals.InputTokens)
	assert.Zero(t, scoped.Hours[9].Usage.Totals.Cost.Microdollars)
	require.Len(t, scoped.Hours[9].Joint.Cells, 1)
	assert.Equal(t, sessionProjectKey, scoped.Hours[9].Joint.Cells[0].ProjectKey)
}
