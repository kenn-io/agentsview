package insight

import (
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/money"
)

// RangeSummary is the deterministic, prompt-input contract derived from
// activity.Report for multi-day ranges. Field order and formatting are
// fixed so the same report always yields the same prompt text.
type RangeSummary struct {
	ActiveMinutes    float64
	IdleMinutes      float64
	AgentMinutes     float64
	Sessions         int
	DistinctProjects int
	DistinctModels   int
	OutputTokens     int
	Cost             money.Money
	PeakAgents       int
	PeakAt           string       // "" when nil
	TopProjects      []KeyMinutes // top-N by agent-minutes, deterministic order
	TopModels        []KeyMinutes
	TopAgents        []KeyMinutes
}

// KeyMinutes pairs a breakdown key with its agent-minutes total.
type KeyMinutes struct {
	Key          string
	AgentMinutes float64
}

// SummarizeReport builds a RangeSummary from an activity.Report, taking the
// top topN entries of each breakdown (already minutes-desc, key-asc from the
// aggregator) so the result is deterministic.
func SummarizeReport(r activity.Report, topN int) RangeSummary {
	peakAt := ""
	if r.Peak.At != nil {
		peakAt = *r.Peak.At
	}
	return RangeSummary{
		ActiveMinutes:    r.Totals.ActiveMinutes,
		IdleMinutes:      r.Totals.IdleMinutes,
		AgentMinutes:     r.Totals.AgentMinutes,
		Sessions:         r.Totals.Sessions,
		DistinctProjects: r.Totals.DistinctProjects,
		DistinctModels:   r.Totals.DistinctModels,
		OutputTokens:     r.Totals.OutputTokens,
		Cost:             r.Totals.Cost,
		PeakAgents:       r.Peak.Agents,
		PeakAt:           peakAt,
		TopProjects:      topKeyMinutes(r.ByProject, topN),
		TopModels:        topKeyMinutes(r.ByModel, topN),
		TopAgents:        topKeyMinutes(r.ByAgent, topN),
	}
}

// topKeyMinutes copies up to topN entries from src verbatim, preserving the
// aggregator's deterministic minutes-desc, key-asc order.
func topKeyMinutes(src []activity.KeyMinutes, topN int) []KeyMinutes {
	if topN < 0 {
		topN = 0
	}
	out := make([]KeyMinutes, 0, min(topN, len(src)))
	for i, km := range src {
		if i >= topN {
			break
		}
		out = append(out, KeyMinutes{Key: km.Key, AgentMinutes: km.AgentMinutes})
	}
	return out
}

// WriteTo renders the summary into the prompt builder deterministically. The
// section and field order are fixed so the same RangeSummary always produces
// the same text.
func (s RangeSummary) WriteTo(b *strings.Builder) {
	b.WriteString("## Range Summary\n\n")
	fmt.Fprintf(b, "- Sessions: %d\n", s.Sessions)
	fmt.Fprintf(b, "- Active minutes: %.1f\n", s.ActiveMinutes)
	fmt.Fprintf(b, "- Idle minutes: %.1f\n", s.IdleMinutes)
	fmt.Fprintf(b, "- Agent minutes: %.1f\n", s.AgentMinutes)
	fmt.Fprintf(b, "- Distinct projects: %d\n", s.DistinctProjects)
	fmt.Fprintf(b, "- Distinct models: %d\n", s.DistinctModels)
	fmt.Fprintf(b, "- Output tokens: %d\n", s.OutputTokens)
	fmt.Fprintf(b, "- Cost: %s\n", money.FormatUSD(s.Cost, money.DisplayCents))
	fmt.Fprintf(b, "- Peak concurrency: %d", s.PeakAgents)
	if s.PeakAt != "" {
		fmt.Fprintf(b, " at %s", s.PeakAt)
	}
	b.WriteString("\n\n")

	writeKeyMinutes(b, "### Top Projects", s.TopProjects)
	writeKeyMinutes(b, "### Top Models", s.TopModels)
	writeKeyMinutes(b, "### Top Agents", s.TopAgents)
}

// writeKeyMinutes renders a titled breakdown list, or a placeholder line when
// the breakdown is empty, keeping the section present for determinism.
func writeKeyMinutes(b *strings.Builder, title string, items []KeyMinutes) {
	b.WriteString(title)
	b.WriteString("\n\n")
	if len(items) == 0 {
		b.WriteString("- (none)\n\n")
		return
	}
	for _, km := range items {
		fmt.Fprintf(b, "- %s: %.1f agent-minutes\n", km.Key, km.AgentMinutes)
	}
	b.WriteString("\n")
}
