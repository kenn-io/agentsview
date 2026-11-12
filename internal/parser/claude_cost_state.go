package parser

import (
	"cmp"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
)

// ClaudeCostStateSource names the session total read from Claude Code's
// cost-state records.
const ClaudeCostStateSource = "claude-cost-state"

// parseClaudeCostState decodes one Claude Code cost-state record. Claude
// Code writes these as cumulative snapshots of its in-process cost tracker,
// so the last record in a transcript is the session's total. The tracker
// also counts subagent calls and side calls, such as title generation,
// that never reach the transcript, so the total includes subagents.
func parseClaudeCostState(line []byte) (*ParsedSessionTotal, bool) {
	record := gjson.ParseBytes(line)
	if record.Get("type").Str != "cost-state" {
		return nil, false
	}
	total := &ParsedSessionTotal{
		Source:        ClaudeCostStateSource,
		SubagentScope: SubagentScopeIncluded,
		Incomplete:    record.Get("hasUnknownModelCost").Bool(),
	}
	if cost, ok := recordedCostDollars(record.Get("totalCostUSD")); ok {
		total.Cost = &cost
	}
	record.Get("modelUsage").ForEach(func(name, usage gjson.Result) bool {
		model := strings.TrimSpace(name.Str)
		if model == "" || !usage.IsObject() {
			return true
		}
		entry := ParsedSessionTotalModel{
			Model:                    strings.Clone(model),
			InputTokens:              max(usage.Get("inputTokens").Int(), 0),
			OutputTokens:             max(usage.Get("outputTokens").Int(), 0),
			ReasoningTokens:          max(usage.Get("thinkingTokens").Int(), 0),
			CacheCreationInputTokens: max(usage.Get("cacheCreationInputTokens").Int(), 0),
			CacheReadInputTokens:     max(usage.Get("cacheReadInputTokens").Int(), 0),
			WebSearchRequests:        max(usage.Get("webSearchRequests").Int(), 0),
		}
		if cost, ok := recordedCostDollars(usage.Get("costUSD")); ok {
			entry.Cost = &cost
		}
		total.Models = append(total.Models, entry)
		return true
	})
	slices.SortFunc(total.Models, func(a, b ParsedSessionTotalModel) int {
		return cmp.Compare(a.Model, b.Model)
	})
	if total.Cost == nil && len(total.Models) == 0 {
		return nil, false
	}
	return total, true
}

// claudeCostStateHasUsage reports whether a cost-state total records any
// spend. Claude Code writes empty records for sessions that made no call.
func claudeCostStateHasUsage(total *ParsedSessionTotal) bool {
	if total == nil {
		return false
	}
	if total.Cost != nil && total.Cost.Microdollars > 0 {
		return true
	}
	for _, model := range total.Models {
		if model.InputTokens+model.OutputTokens+model.CacheCreationInputTokens+
			model.CacheReadInputTokens > 0 {
			return true
		}
	}
	return false
}
