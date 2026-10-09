package db

import (
	"fmt"
	"sort"
	"strings"
)

// ToolEffectivenessCounts retains integer counts so aggregation precedes division.
type ToolEffectivenessCounts struct {
	AnalyzedCalls      int `json:"analyzed_calls"`
	KnownOutcomeCalls  int `json:"known_outcome_calls"`
	EmptyCalls         int `json:"empty_calls"`
	RepeatedCalls      int `json:"repeated_calls"`
	RecoveredSequences int `json:"recovered_sequences"`
	AbandonedSequences int `json:"abandoned_sequences"`
	OpenSequences      int `json:"open_sequences"`
	UnknownSequences   int `json:"unknown_sequences"`
}

func (c *ToolEffectivenessCounts) Add(other ToolEffectivenessCounts) {
	c.AnalyzedCalls += other.AnalyzedCalls
	c.KnownOutcomeCalls += other.KnownOutcomeCalls
	c.EmptyCalls += other.EmptyCalls
	c.RepeatedCalls += other.RepeatedCalls
	c.RecoveredSequences += other.RecoveredSequences
	c.AbandonedSequences += other.AbandonedSequences
	c.OpenSequences += other.OpenSequences
	c.UnknownSequences += other.UnknownSequences
}

func (c *ToolEffectivenessCounts) ScanTargets() []any {
	return []any{&c.AnalyzedCalls, &c.KnownOutcomeCalls, &c.EmptyCalls, &c.RepeatedCalls, &c.RecoveredSequences, &c.AbandonedSequences, &c.OpenSequences, &c.UnknownSequences}
}

func toolRate(numerator, denominator int) *float64 {
	if denominator == 0 {
		return nil
	}
	rate := float64(numerator) / float64(denominator)
	return &rate
}

// ToolEffectivenessSQL extends a store's existing grouped call query.
func ToolEffectivenessSQL(alias, versionColumn string) string {
	fresh := toolObservationFreshSQL(versionColumn)
	predicates := []string{
		alias + ".observed_outcome IS NOT NULL",
		alias + ".observed_outcome IN ('empty','content','errored')",
		ToolMetricPredicate(alias, "tool_empty_rate"),
		ToolMetricPredicate(alias, "tool_repeat_rate"),
		ToolMetricPredicate(alias, "tool_recovery_rate"),
		alias + ".sequence_ending = 'abandoned'",
		alias + ".sequence_ending = 'open'",
		alias + ".sequence_ending = 'unknown'",
	}
	var sql strings.Builder
	for _, predicate := range predicates {
		fmt.Fprintf(&sql, ", SUM(CASE WHEN %s AND %s THEN 1 ELSE 0 END)", fresh, predicate)
	}
	return sql.String()
}

func IsToolMetric(signal string) bool {
	return signal == "tool_empty_rate" || signal == "tool_repeat_rate" || signal == "tool_recovery_rate"
}

// ToolMetricPredicate selects the calls behind a rate's numerator.
func ToolMetricPredicate(alias, signal string) string {
	switch signal {
	case "tool_empty_rate":
		return alias + ".observed_outcome = 'empty'"
	case "tool_repeat_rate":
		return alias + ".observed_repeat IN ('identical','near_identical')"
	case "tool_recovery_rate":
		return alias + ".sequence_ending = 'recovered'"
	default:
		return ""
	}
}

// ToolMetricCall returns the index of the first call in message that counts
// toward signal for the selected tool, so evidence links can target the call
// rather than only its message.
func ToolMetricCall(message Message, signal, toolName, category string) (int, bool) {
	for i, call := range message.ToolCalls {
		if toolName != "" && normalizeToolName(call.ToolName) != normalizeToolName(toolName) {
			continue
		}
		if category != "" && call.Category != category {
			continue
		}
		if toolMetricMatches(call, signal) {
			return i, true
		}
	}
	return 0, false
}

func toolMetricMatches(call ToolCall, signal string) bool {
	switch signal {
	case "tool_empty_rate":
		return call.ObservedOutcome != nil && *call.ObservedOutcome == "empty"
	case "tool_repeat_rate":
		return call.ObservedRepeat != nil && (*call.ObservedRepeat == "identical" || *call.ObservedRepeat == "near_identical")
	case "tool_recovery_rate":
		return call.SequenceEnding != nil && *call.SequenceEnding == "recovered"
	default:
		return false
	}
}

// ToolSelectionPredicates shares row identity and freshness while each store binds values.
func ToolSelectionPredicates(alias, versionColumn string, f AnalyticsFilter, bind func(string) string) []string {
	predicates := []string{}
	if f.ToolCategory != "" {
		predicates = append(predicates, alias+".category = "+bind(f.ToolCategory))
	}
	if predicate := ToolMetricPredicate(alias, f.ToolMetric); predicate != "" {
		predicates = append(predicates, predicate, toolObservationFreshSQL(versionColumn))
	}
	return predicates
}

func toolObservationFreshSQL(versionColumn string) string {
	return fmt.Sprintf("%s = %d", versionColumn, CurrentQualitySignalVersion)
}

// BuildToolMetricEvidence returns distinct contributing sessions in stable order.
func BuildToolMetricEvidence(rows []ToolAnalyticsRow, signal, toolName string, offset, limit int) SignalSessionsResponse {
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	examples := make(map[string]SignalSessionExample)
	for _, row := range rows {
		if toolName != "" && normalizeToolName(row.ToolName) != normalizeToolName(toolName) {
			continue
		}
		count := row.RepeatedCalls
		if signal == "tool_empty_rate" {
			count = row.EmptyCalls
		}
		if signal == "tool_recovery_rate" {
			count = row.RecoveredSequences
		}
		if count == 0 {
			continue
		}
		example, ok := examples[row.SessionID]
		if !ok {
			ordinal := row.Ordinal
			example = SignalSessionExample{SessionID: row.SessionID, Project: row.Project, Agent: row.Agent, Date: row.Date, ReasonCode: signal, MessageOrdinal: &ordinal}
		}
		example.SignalTotal += count
		if row.Ordinal < *example.MessageOrdinal {
			ordinal := row.Ordinal
			example.MessageOrdinal = &ordinal
		}
		if row.Date > example.Date {
			example.Date = row.Date
		}
		examples[row.SessionID] = example
	}
	all := make([]SignalSessionExample, 0, len(examples))
	for _, example := range examples {
		all = append(all, example)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Date != all[j].Date {
			return all[i].Date > all[j].Date
		}
		return all[i].SessionID < all[j].SessionID
	})
	offset = min(max(offset, 0), len(all))
	end := min(offset+limit, len(all))
	result := SignalSessionsResponse{Signal: signal, Sessions: all[offset:end], Total: new(len(all))}
	if end < len(all) {
		result.NextOffset = &end
	}
	return result
}

func normalizeToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Unknown"
	}
	return name
}
