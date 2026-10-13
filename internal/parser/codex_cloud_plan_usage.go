package parser

import (
	"time"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

// planUsage resolves the file's plan usage from the newest row and the
// newest outcome record. Order is the records' position in the file, never
// their times. An outcome counts only when it was written after the row;
// a newer "counted under" outcome clears the row's values, since the
// thread's usage belongs to its root. It returns nil for a file with no
// plan usage records.
func (t *codexCloudThread) planUsage() *ParsedPlanUsage {
	var usage ParsedPlanUsage
	if t.planRow != nil {
		usage = codexCloudPlanRow(t.planRow)
	}
	outcome := t.planOutcome
	if outcome != nil && t.planRow != nil && t.planOutcomeAt < t.planRowAt {
		outcome = nil
	}
	if t.planRow == nil && outcome == nil {
		return nil
	}
	if outcome != nil {
		if outcome.Kind == mirror.OutcomeCountedUnder {
			usage = ParsedPlanUsage{CountedUnder: codexCloudSessionPrefix + outcome.Root}
		}
		usage.OutcomeKind = outcome.Kind
		usage.OutcomeAt = codexCloudTimePtr(outcome.At)
	}
	return &usage
}

func codexCloudPlanRow(data *mirror.PlanUsageRowData) ParsedPlanUsage {
	row := data.Row
	usage := ParsedPlanUsage{
		DataAsOf:        codexCloudTimePtr(data.DataAsOf),
		DataStatus:      row.DataStatus,
		UsageSource:     row.UsageSource,
		WeeklyPercent:   row.WeeklyPercent,
		FiveHourPercent: row.FiveHourPercent,
		Credits:         row.Credits,
		Final:           data.Final,
	}
	for _, g := range row.Groups {
		usage.Groups = append(usage.Groups, ParsedPlanUsageGroup{
			Model:             g.Model,
			ReasoningEffort:   g.ReasoningEffort,
			Speed:             g.Speed,
			ProductExperience: g.ProductExperience,
			WeeklyPercent:     g.WeeklyPercent,
			FiveHourPercent:   g.FiveHourPercent,
			Credits:           g.Credits,
		})
	}
	return usage
}

func codexCloudTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}
