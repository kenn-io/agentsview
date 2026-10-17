package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestDirectMachineDisplayName(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	for _, machine := range []string{"installation-a", "source-a"} {
		msg := dbtest.AsstMsg(machine, 1, "needle")
		msg.Timestamp = "2024-06-01T09:00:00Z"
		msg.Model = "gpt-4o"
		msg.TokenUsage = []byte(`{"input_tokens":100}`)
		dbtest.SeedSessionWithMessages(t, d, machine, "project", []db.Message{
			dbtest.UserMsg(machine, 0, "question"), msg,
		}, dbtest.WithMessageCounts(2, 2), func(s *db.Session) {
			s.Machine = machine
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.EndedAt = new("2024-06-01T09:00:00Z")
		})
	}
	require.NoError(t, d.SetSyncState(t.Context(), "machine_label:installation-a", "host-a.example"))
	backend := service.NewDirectBackend(d, nil)
	list, err := backend.List(t.Context(), service.ListFilter{Machine: "host-a.example"})
	require.NoError(t, err)
	assert.Len(t, list.Sessions, 1)
	search, err := backend.SearchContent(t.Context(), service.ContentSearchRequest{
		Machine: "host-a.example", Pattern: "needle", Mode: "substring",
	})
	require.NoError(t, err)
	assert.Len(t, search.Matches, 1)
	req := service.UsageRequest{From: "2024-06-01", To: "2024-06-01", Machine: "host-a.example"}
	summary, err := backend.UsageSummary(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, 100, summary.Totals.InputTokens)
	comparison, err := backend.UsagePairwiseComparison(t.Context(), service.UsagePairwiseComparisonRequest{
		UsageRequest:  req,
		LeftDimension: "project", LeftValue: "project",
		RightDimension: "project", RightValue: "project",
	})
	require.NoError(t, err)
	assert.Equal(t, 100, comparison.Left.InputTokens)
	assert.Equal(t, 100, comparison.Right.InputTokens)
}
