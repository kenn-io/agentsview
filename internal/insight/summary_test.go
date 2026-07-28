package insight

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/money"
)

func sampleReport() activity.Report {
	at := "2026-06-16T10:00:00Z"
	return activity.Report{
		Peak: activity.Peak{Agents: 3, At: &at},
		Totals: activity.Totals{
			ActiveMinutes: 120, IdleMinutes: 60, AgentMinutes: 200,
			Sessions: 9, DistinctProjects: 2, DistinctModels: 2,
			OutputTokens: 5000, Cost: money.MustParseDollars("4.25"),
		},
		ByProject: []activity.KeyMinutes{{Key: "alpha", AgentMinutes: 150}, {Key: "beta", AgentMinutes: 50}},
		ByModel:   []activity.KeyMinutes{{Key: "model-x", AgentMinutes: 120}, {Key: "model-y", AgentMinutes: 80}},
		ByAgent:   []activity.KeyMinutes{{Key: "claude", AgentMinutes: 200}},
	}
}

func TestSummarizeReport_Deterministic(t *testing.T) {
	r := sampleReport()
	a := SummarizeReport(r, 10)
	b := SummarizeReport(r, 10)
	assert.Equal(t, a, b, "same report yields identical summary")
	assert.Equal(t, 3, a.PeakAgents)
	assert.Equal(t, "2026-06-16T10:00:00Z", a.PeakAt)
	require.Len(t, a.TopProjects, 2)
	assert.Equal(t, "alpha", a.TopProjects[0].Key)
}

func TestSummarizeReport_TopNCap(t *testing.T) {
	r := sampleReport()
	s := SummarizeReport(r, 1)
	require.Len(t, s.TopProjects, 1, "topN caps the breakdown length")
	assert.Equal(t, "alpha", s.TopProjects[0].Key)
}

func TestRangeSummary_WriteToDeterministic(t *testing.T) {
	s := SummarizeReport(sampleReport(), 10)
	var b1, b2 strings.Builder
	s.WriteTo(&b1)
	s.WriteTo(&b2)
	assert.Equal(t, b1.String(), b2.String())
	assert.Contains(t, b1.String(), "Peak concurrency: 3")
	assert.Contains(t, b1.String(), "alpha")
}
