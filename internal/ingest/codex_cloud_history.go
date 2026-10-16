package ingest

import (
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// CodexCloudPrior is the saved copy of a Codex cloud chat that a new parse
// of its mirror file may replace. Sync reads it from the archive; the hub
// reads it from the previous raw projection.
type CodexCloudPrior struct {
	Session     db.Session
	UsageEvents []db.UsageEvent
	PlanUsage   *db.SessionPlanUsage
	// Current reports that the saved copy came from the current parser
	// version.
	Current bool
}

// CodexCloudKeepReason returns why the saved chat must stay as it is, or
// "" when the parse may replace it. A chat's history only grows, so a
// mirror with fewer messages lost part of the chat. A saved copy from an
// older parser version is replaced, so a parser change can still reparse
// it, unless the mirror holds no messages at all: no parser change empties
// a chat, but a deleted mirror rebuilt around a usage record does.
func CodexCloudKeepReason(messages int, prior CodexCloudPrior) string {
	saved := prior.Session.MessageCount
	if messages == 0 && saved > 0 || prior.Current && messages < saved {
		return fmt.Sprintf("the local copy of the chat has fewer messages (%d saved, %d found)", saved, messages)
	}
	return ""
}

// CarryCodexCloudUsage copies onto prepared the usage its mirror file lost.
// Token counts and plan usage reach the mirror only from AgentsView's own
// writes, and server history carries neither, so a parse without them
// where the saved copy has them was rebuilt after the mirror was lost.
//
//   - With no token counts, prepared takes the saved token counts, usage
//     events, peak context, and capture coverage. Turns that ran while the
//     mirror was missing are not counted as uncaptured.
//   - With token counts, prepared keeps the larger peak context of the two
//     when the saved copy is current: a rebuilt mirror holds only the
//     newest records.
//   - With no plan records, prepared takes the saved plan figures.
//
// It reports whether prepared took the saved token counts.
func CarryCodexCloudUsage(prepared *PreparedSession, prior CodexCloudPrior) bool {
	saved := prior.Session
	tokens := !prepared.Session.HasTotalOutputTokens && saved.HasTotalOutputTokens
	if tokens {
		prepared.UsageEvents = prior.UsageEvents
		prepared.Session.TotalOutputTokens = saved.TotalOutputTokens
		prepared.Session.HasTotalOutputTokens = true
		prepared.Session.PeakContextTokens = saved.PeakContextTokens
		prepared.Session.HasPeakContextTokens = saved.HasPeakContextTokens
	} else if prior.Current && saved.HasPeakContextTokens &&
		prepared.Session.HasPeakContextTokens && saved.PeakContextTokens > prepared.Session.PeakContextTokens {
		prepared.Session.PeakContextTokens = saved.PeakContextTokens
	}

	var row db.SessionPlanUsage
	if prepared.PlanUsage != nil {
		row = *prepared.PlanUsage
	}
	switch {
	case hasPlanRecords(prepared.PlanUsage):
	case hasPlanRecords(prior.PlanUsage):
		row = withCoverage(*prior.PlanUsage, row)
	case tokens:
		// A parse with token counts reports no plan usage, not the "not
		// checked yet" placeholder of a chat with neither.
		row = withCoverage(db.SessionPlanUsage{}, row)
	}
	if tokens {
		var coverage db.SessionPlanUsage
		if prior.PlanUsage != nil {
			coverage = *prior.PlanUsage
		}
		row = withCoverage(row, coverage)
	}
	if row.HasPlanUsage || row.HasCoverage {
		prepared.PlanUsage = &row
	} else {
		prepared.PlanUsage = nil
	}
	return tokens
}

// hasPlanRecords reports whether a plan usage row holds a report or an
// outcome. The parser reports a chat with neither plan usage nor token
// records as an empty row, meaning plan usage is not checked yet.
func hasPlanRecords(row *db.SessionPlanUsage) bool {
	return row != nil && row.HasPlanUsage &&
		(row.DataAsOf != "" || row.OutcomeKind != "" || row.CountedUnder != "")
}

// withCoverage returns plan with the capture coverage of coverage.
func withCoverage(plan, coverage db.SessionPlanUsage) db.SessionPlanUsage {
	plan.HasCoverage = coverage.HasCoverage
	plan.LastCaptureAt = coverage.LastCaptureAt
	plan.UncapturedTurns = coverage.UncapturedTurns
	return plan
}
