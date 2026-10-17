package artifact

import "go.kenn.io/agentsview/internal/db"

// manifestPlanUsageFromDB splits a session's plan usage row into its wire
// plan usage and coverage. Each is nil when the row lacks that part.
func manifestPlanUsageFromDB(
	p *db.SessionPlanUsage,
) (*artifactPlanUsage, *artifactCaptureCoverage) {
	if p == nil {
		return nil, nil
	}
	var usage *artifactPlanUsage
	if p.HasPlanUsage {
		usage = &artifactPlanUsage{
			DataAsOf: p.DataAsOf, DataStatus: p.DataStatus,
			UsageSource: p.UsageSource, WeeklyPercent: p.WeeklyPercent,
			FiveHourPercent: p.FiveHourPercent, Credits: p.Credits,
			Groups: p.Groups, Final: p.Final, OutcomeKind: p.OutcomeKind,
			OutcomeAt: p.OutcomeAt, CountedUnder: p.CountedUnder,
		}
	}
	var coverage *artifactCaptureCoverage
	if p.HasCoverage {
		coverage = &artifactCaptureCoverage{
			LastCaptureAt: p.LastCaptureAt, UncapturedTurns: p.UncapturedTurns,
		}
	}
	return usage, coverage
}

// importedPlanUsage rebuilds the manifest's plan usage row, or nil when the
// manifest carries neither part. The usage root gets the origin prefix that
// every imported session ID gets.
func importedPlanUsage(m manifest) *db.SessionPlanUsage {
	if m.PlanUsage == nil && m.CaptureCoverage == nil {
		return nil
	}
	var p db.SessionPlanUsage
	if u := m.PlanUsage; u != nil {
		p.HasPlanUsage = true
		p.DataAsOf, p.DataStatus, p.UsageSource = u.DataAsOf, u.DataStatus, u.UsageSource
		p.WeeklyPercent, p.FiveHourPercent, p.Credits = u.WeeklyPercent, u.FiveHourPercent, u.Credits
		p.Groups, p.Final = u.Groups, u.Final
		p.OutcomeKind, p.OutcomeAt = u.OutcomeKind, u.OutcomeAt
		p.CountedUnder = prefixImportedSessionID(m.Origin, u.CountedUnder)
	}
	if c := m.CaptureCoverage; c != nil {
		p.HasCoverage = true
		p.LastCaptureAt, p.UncapturedTurns = c.LastCaptureAt, c.UncapturedTurns
	}
	return &p
}
