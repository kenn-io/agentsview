package ingest

import (
	"fmt"
	"slices"
	"time"

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
		return fmt.Sprintf("the mirror file has fewer messages than the saved copy (%d saved, %d found)", saved, messages)
	}
	return ""
}

// CarryCodexCloudUsage copies onto prepared the usage its mirror file lost.
// Token counts and plan usage reach the mirror only from AgentsView's own
// writes, and server history carries neither, so a parse without them
// where the saved copy has them was rebuilt after the mirror was lost.
//
//   - With no token counts, prepared takes the saved token counts, usage
//     events, peak context, and capture coverage. It takes them too when
//     the saved copy is current and the parse has a smaller output total
//     or only some of the saved usage events: records only accumulate, so
//     the file was cut short. Turns that ran while the mirror was missing
//     are not counted as uncaptured.
//   - With token counts, prepared keeps the larger peak context of the two
//     when the saved copy is current: a rebuilt mirror holds only the
//     newest records.
//   - With no plan records, prepared takes the saved plan figures. With
//     only an outcome other than "counted under", it keeps the saved
//     report figures under the new outcome, as the parser does for a
//     whole mirror. With a report no newer than a current saved copy's, it
//     keeps what the cut took; see newerSavedPlan.
//
// It reports whether prepared took anything from the saved copy.
func CarryCodexCloudUsage(prepared *PreparedSession, prior CodexCloudPrior) bool {
	saved := prior.Session
	cut := prior.Current && (prepared.Session.TotalOutputTokens < saved.TotalOutputTokens ||
		dropsEvents(prepared.UsageEvents, prior.UsageEvents))
	tokens := saved.HasTotalOutputTokens && (!prepared.Session.HasTotalOutputTokens || cut)
	carried := tokens
	if tokens {
		prepared.UsageEvents = prior.UsageEvents
		prepared.Session.TotalOutputTokens = saved.TotalOutputTokens
		prepared.Session.HasTotalOutputTokens = true
		prepared.Session.PeakContextTokens = saved.PeakContextTokens
		prepared.Session.HasPeakContextTokens = saved.HasPeakContextTokens
	} else if prior.Current && saved.HasPeakContextTokens &&
		prepared.Session.HasPeakContextTokens && saved.PeakContextTokens > prepared.Session.PeakContextTokens {
		prepared.Session.PeakContextTokens = saved.PeakContextTokens
		carried = true
	}

	var row db.SessionPlanUsage
	if prepared.PlanUsage != nil {
		row = *prepared.PlanUsage
	}
	switch {
	case row.CountedUnder != "":
	case hasPlanReport(prepared.PlanUsage):
		if merged, ok := newerSavedPlan(row, prior); ok {
			row = withCoverage(merged, row)
			carried = true
		}
	case hasPlanRecords(prepared.PlanUsage):
		if hasPlanReport(prior.PlanUsage) {
			merged := *prior.PlanUsage
			merged.OutcomeKind, merged.OutcomeAt = row.OutcomeKind, row.OutcomeAt
			row = withCoverage(merged, row)
			carried = true
		}
	case hasPlanRecords(prior.PlanUsage):
		row = withCoverage(*prior.PlanUsage, row)
		carried = true
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
	return carried
}

// dropsEvents reports whether parsed holds only some of the saved usage
// events: a file cut short keeps the keys of its earlier events, while a
// rebuilt file starts a new running total under new keys.
func dropsEvents(parsed, saved []db.UsageEvent) bool {
	if len(parsed) >= len(saved) {
		return false
	}
	keys := make(map[string]bool, len(saved))
	for _, ev := range saved {
		keys[ev.DedupKey] = true
	}
	return !slices.ContainsFunc(parsed, func(ev db.UsageEvent) bool { return !keys[ev.DedupKey] })
}

// newerSavedPlan returns the plan row to keep when a current saved copy's
// report is not older than the parse's, which happens when the file was
// cut short. With a newer saved report, the saved figures stay, and the
// parse's outcome replaces the saved one only when it is newer than both
// the saved report and the saved outcome. With the same report time, the
// cut may have taken the copy of the report that settling writes again
// marked final, and the outcome written after it, so the parse keeps the
// final mark of either copy, and the newer saved outcome when only the
// saved copy is final. A later answer can repeat a report time after a
// left_out outcome; its row follows the outcome and clears it, and the
// saved outcome must not return. The parser orders records by their place
// in the file, but two copies share no file, so this falls back on times.
// It reports whether the saved copy changed the row.
func newerSavedPlan(row db.SessionPlanUsage, prior CodexCloudPrior) (db.SessionPlanUsage, bool) {
	if !prior.Current || !hasPlanReport(prior.PlanUsage) {
		return row, false
	}
	saved := *prior.PlanUsage
	savedAsOf, asOf := planTime(saved.DataAsOf), planTime(row.DataAsOf)
	switch {
	case savedAsOf.Before(asOf):
		return row, false
	case savedAsOf.Equal(asOf):
		merged := row
		merged.Final = row.Final || saved.Final
		if saved.Final && !row.Final && planTime(saved.OutcomeAt).After(planTime(row.OutcomeAt)) {
			merged.OutcomeKind, merged.OutcomeAt = saved.OutcomeKind, saved.OutcomeAt
		}
		return merged, merged.Final != row.Final || merged.OutcomeAt != row.OutcomeAt
	}
	outcomeAt := planTime(row.OutcomeAt)
	if outcomeAt.After(savedAsOf) && outcomeAt.After(planTime(saved.OutcomeAt)) {
		saved.OutcomeKind, saved.OutcomeAt = row.OutcomeKind, row.OutcomeAt
	}
	return saved, true
}

// planTime parses a stored plan usage time. An empty or malformed time is
// the zero time, which is older than any real one.
func planTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// hasPlanReport reports whether a plan usage row holds OpenAI's report
// figures.
func hasPlanReport(row *db.SessionPlanUsage) bool {
	return row != nil && row.HasPlanUsage && row.DataAsOf != ""
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
