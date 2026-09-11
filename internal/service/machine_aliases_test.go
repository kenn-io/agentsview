package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestDirectMachineAliases(t *testing.T) {
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
	require.NoError(t, d.SetSyncState("machine_alias:old-owner", "installation-a"))
	require.NoError(t, d.SetSyncState("artifact_local_installation_id", "installation-a"))
	require.NoError(t, d.SetSyncState("machine_label:installation-a", "Laptop"))
	backend := service.NewDirectBackend(d, nil)
	for _, tt := range []struct {
		machine string
		count   int
		tokens  int
	}{
		{"old-owner", 1, 100},
		{"old-owner,installation-a", 1, 100},
		{"local", 1, 100},
		{"Laptop", 0, 0},
	} {
		t.Run(tt.machine, func(t *testing.T) {
			list, err := backend.List(t.Context(), service.ListFilter{Machine: tt.machine})
			require.NoError(t, err)
			assert.Len(t, list.Sessions, tt.count)
			search, err := backend.SearchContent(t.Context(), service.ContentSearchRequest{
				Machine: tt.machine, Pattern: "needle", Mode: "substring",
			})
			require.NoError(t, err)
			assert.Len(t, search.Matches, tt.count)
			req := service.UsageRequest{From: "2024-06-01", To: "2024-06-01", Machine: tt.machine}
			summary, err := backend.UsageSummary(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, tt.tokens, summary.Totals.InputTokens)
			comparison, err := backend.UsagePairwiseComparison(t.Context(), service.UsagePairwiseComparisonRequest{
				UsageRequest:  req,
				LeftDimension: "project", LeftValue: "project",
				RightDimension: "project", RightValue: "project",
			})
			require.NoError(t, err)
			assert.Equal(t, tt.tokens, comparison.Left.InputTokens)
			assert.Equal(t, tt.tokens, comparison.Right.InputTokens)
		})
	}
}
