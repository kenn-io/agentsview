package rawparity

import (
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"hegel.dev/go/hegel"
)

func planUsageSample() Snapshot {
	return Snapshot{PlanUsage: &db.SessionPlanUsage{
		HasPlanUsage: true, DataAsOf: "2026-01-01T00:00:00Z", DataStatus: "partial", UsageSource: "report",
		WeeklyPercent: new(1.5), FiveHourPercent: new(2.5), Credits: new("0.000001"),
		Groups: []db.SessionPlanUsageGroup{{Model: "model-a", WeeklyPercent: new(1.5)}, {Model: "model-b", Credits: new("2")}},
		Final:  true, OutcomeKind: "counted_under", OutcomeAt: "2026-01-01T00:00:00Z", CountedUnder: "root",
		HasCoverage: true, LastCaptureAt: "2026-01-01T00:00:00Z", UncapturedTurns: 3,
	}}
}

func TestPlanUsageSemanticDifferences(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*db.SessionPlanUsage)
	}{
		{"has_plan_usage", func(p *db.SessionPlanUsage) { p.HasPlanUsage = false }},
		{"data_as_of", func(p *db.SessionPlanUsage) { p.DataAsOf = "2026-01-02T00:00:00Z" }},
		{"data_status", func(p *db.SessionPlanUsage) { p.DataStatus = "complete" }},
		{"usage_source", func(p *db.SessionPlanUsage) { p.UsageSource = "other" }},
		{"weekly_percent", func(p *db.SessionPlanUsage) { p.WeeklyPercent = new(0.0) }},
		{"five_hour_percent", func(p *db.SessionPlanUsage) { p.FiveHourPercent = nil }},
		{"credits", func(p *db.SessionPlanUsage) { p.Credits = new("0.000002") }},
		{"groups", func(p *db.SessionPlanUsage) { p.Groups[0].WeeklyPercent = new(2.5) }},
		{"final", func(p *db.SessionPlanUsage) { p.Final = false }},
		{"outcome_kind", func(p *db.SessionPlanUsage) { p.OutcomeKind = "left_out" }},
		{"outcome_at", func(p *db.SessionPlanUsage) { p.OutcomeAt = "2026-01-02T00:00:00Z" }},
		{"counted_under", func(p *db.SessionPlanUsage) { p.CountedUnder = "other-root" }},
		{"has_coverage", func(p *db.SessionPlanUsage) { p.HasCoverage = false }},
		{"last_capture_at", func(p *db.SessionPlanUsage) { p.LastCaptureAt = "2026-01-02T00:00:00Z" }},
		{"uncaptured_turns", func(p *db.SessionPlanUsage) { p.UncapturedTurns++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := planUsageSample(), planUsageSample()
			tc.change(b.PlanUsage)
			got, err := Compare(a, b)
			require.NoError(t, err)
			assert.Equal(t, []string{"usage"}, got.Differing)
		})
	}
	for _, p := range []*db.SessionPlanUsage{{}, planUsageSample().PlanUsage} {
		got, err := Compare(Snapshot{}, Snapshot{PlanUsage: p})
		require.NoError(t, err)
		assert.Equal(t, []string{"usage"}, got.Differing, "a missing row differs from a present row")
	}
}

func TestPlanUsageRepresentationAndMatchIDs(t *testing.T) {
	a, b := planUsageSample(), planUsageSample()
	b.PlanUsage.DataAsOf = "2025-12-31T19:00:00-05:00"
	b.PlanUsage.OutcomeAt = "2026-01-01 00:00:00"
	b.PlanUsage.LastCaptureAt = "2026-01-01T00:00:00.000000001Z"
	b.PlanUsage.Groups[0], b.PlanUsage.Groups[1] = b.PlanUsage.Groups[1], b.PlanUsage.Groups[0]
	b.PlanUsage.CountedUnder = "raw-root"
	b.MatchIDs = map[string]string{"raw-root": "root"}
	want := planUsageSample()
	got, err := Compare(a, b)
	require.NoError(t, err)
	assert.Empty(t, got.Differing)
	assert.Equal(t, want, a)
	assert.Equal(t, "raw-root", b.PlanUsage.CountedUnder)
	assert.Equal(t, "model-b", b.PlanUsage.Groups[0].Model)
	got, err = Compare(Snapshot{PlanUsage: &db.SessionPlanUsage{}}, Snapshot{PlanUsage: &db.SessionPlanUsage{Groups: []db.SessionPlanUsageGroup{}}})
	require.NoError(t, err)
	assert.Empty(t, got.Differing)
}

// Coverage-only rows retain nullable accounting even without a plan report.
func TestPlanUsageNullableValuesProperty(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		value := float64(hegel.Draw(ht, hegel.Integers(math.MinInt32, math.MaxInt32)))
		credits := hegel.Draw(ht, hegel.Text())
		for _, plan := range []*db.SessionPlanUsage{
			{WeeklyPercent: &value},
			{FiveHourPercent: &value},
			{Credits: &credits},
			{Groups: []db.SessionPlanUsageGroup{{WeeklyPercent: &value}}},
			{Groups: []db.SessionPlanUsageGroup{{FiveHourPercent: &value}}},
			{Groups: []db.SessionPlanUsageGroup{{Credits: &credits}}},
		} {
			absent := Snapshot{PlanUsage: &db.SessionPlanUsage{}}
			if len(plan.Groups) > 0 {
				absent.PlanUsage.Groups = []db.SessionPlanUsageGroup{{}}
			}
			got, err := Compare(absent, Snapshot{PlanUsage: plan})
			require.NoError(ht, err)
			require.Equal(ht, []string{"usage"}, got.Differing)
		}
	}, hegel.WithTestCases(1000))
}

func TestPlanUsageGroupFieldsProperty(t *testing.T) {
	for _, field := range []string{"model", "reasoning", "speed", "product"} {
		t.Run(field, func(t *testing.T) {
			propertyTest(t, func(ht *hegel.T) {
				a := Snapshot{PlanUsage: &db.SessionPlanUsage{Groups: []db.SessionPlanUsageGroup{{}}}}
				b := Snapshot{PlanUsage: &db.SessionPlanUsage{Groups: []db.SessionPlanUsageGroup{{}}}}
				g := &b.PlanUsage.Groups[0]
				text := "value:" + hegel.Draw(ht, hegel.Text())
				switch field {
				case "model":
					g.Model = text
				case "reasoning":
					g.ReasoningEffort = text
				case "speed":
					g.Speed = text
				case "product":
					g.ProductExperience = text
				}
				got, err := Compare(a, b)
				require.NoError(ht, err)
				require.Equal(ht, []string{"usage"}, got.Differing)
			}, hegel.WithTestCases(1000))
		})
	}
}

func TestPlanUsageGroupOrderProperty(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		names := hegel.Draw(ht, hegel.Lists(hegel.Text()))
		groups := make([]db.SessionPlanUsageGroup, 0, min(len(names), 32))
		for _, name := range names[:min(len(names), 32)] {
			groups = append(groups, db.SessionPlanUsageGroup{Model: name})
		}
		reversed := slices.Clone(groups)
		slices.Reverse(reversed)
		got, err := Compare(Snapshot{PlanUsage: &db.SessionPlanUsage{Groups: groups}}, Snapshot{PlanUsage: &db.SessionPlanUsage{Groups: reversed}})
		require.NoError(ht, err)
		require.Empty(ht, got.Differing)
	}, hegel.WithTestCases(1000))
}

func TestPlanUsageGroupMultiplicityProperty(t *testing.T) {
	propertyTest(t, func(ht *hegel.T) {
		group := db.SessionPlanUsageGroup{Model: hegel.Draw(ht, hegel.Text())}
		got, err := Compare(Snapshot{PlanUsage: &db.SessionPlanUsage{Groups: []db.SessionPlanUsageGroup{group}}},
			Snapshot{PlanUsage: &db.SessionPlanUsage{Groups: []db.SessionPlanUsageGroup{group, group}}})
		require.NoError(ht, err)
		require.Equal(ht, []string{"usage"}, got.Differing)
	}, hegel.WithTestCases(1000))
}
