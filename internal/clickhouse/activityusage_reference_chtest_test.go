//go:build chtest

package clickhouse

import (
	"cmp"
	"context"
	"iter"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
)

// This file keeps the selection Activity reports used before ClickHouse
// summed the survivors: read the candidates' rows and their Claude peers,
// then select and price the survivors among them in Go. The tests compare
// the SQL sums with it.

// claudeUsageKey is a Claude message and request ID pair.
type claudeUsageKey struct{ message, request string }

// activityUsageSelection returns the rows the Go path read for the
// candidate sessions, in report order, and how many there are: every row of
// a candidate session, and every message row of another session that
// carries the Claude message and request IDs of a candidate row. Those
// peers are what complete-snapshot selection compares the candidates with.
// Report order is time, session, and ordinal, with rows that tie in the
// order they arrive.
func activityUsageSelection(rows []clickSessionUsageOrderedRow, candidates []string) (iter.Seq[*clickActivityReportUsageRow], int) {
	selected := make(map[string]bool, len(candidates))
	for _, id := range candidates {
		selected[id] = true
	}
	held := map[claudeUsageKey]bool{}
	for _, r := range rows {
		if selected[r.scan.sessionID] && r.scan.claudeMessageID != "" {
			held[claudeUsageKey{r.scan.claudeMessageID, r.scan.claudeRequestID}] = true
		}
	}
	var read []clickSessionUsageOrderedRow
	for _, r := range rows {
		if selected[r.scan.sessionID] || (r.scan.source == "message" &&
			held[claudeUsageKey{r.scan.claudeMessageID, r.scan.claudeRequestID}]) {
			read = append(read, r)
		}
	}
	slices.SortStableFunc(read, func(a, b clickSessionUsageOrderedRow) int {
		return cmp.Or(cmp.Compare(a.ts.UnixMicro(), b.ts.UnixMicro()),
			cmp.Compare(a.scan.sessionID, b.scan.sessionID), cmp.Compare(a.ordinal, b.ordinal))
	})
	return func(yield func(*clickActivityReportUsageRow) bool) {
		for i := range read {
			if !yield(&read[i].scan) {
				return
			}
		}
	}, len(read)
}

// selectActivityUsage selects and prices the usage survivors of the
// candidate sessions ids from ordered, the count rows a read of them and
// their Claude peers returns, in report order.
func selectActivityUsage(
	ctx context.Context, ordered iter.Seq[*clickActivityReportUsageRow], count int,
	ids []string, q activity.Query, rateResolver *export.PricingResolver,
) ([]activity.UsageRow, error) {
	selection := make([]activity.UsageSelectionRow, 0, count)
	for r := range ordered {
		ts, err := time.Parse(time.RFC3339Nano, r.ts)
		selection = append(selection, activity.UsageSelectionRow{SessionID: r.sessionID, Timestamp: ts, TimestampValid: err == nil, OutputTokens: r.outputTok, WebSearchRequests: r.webSearchRequests, Agent: r.agent, ClaudeMessageID: r.claudeMessageID, ClaudeRequestID: r.claudeRequestID, SourceUUID: r.sourceUUID, UsageDedupKey: r.usageDedupKey})
	}
	mask, attribution, webSearchRequests, err := activity.SelectUsageSurvivorsForSessions(ctx, q.RangeStart, q.RangeEnd, q.EffectiveEnd, selection, ids)
	if err != nil {
		return nil, err
	}
	out := make([]activity.UsageRow, 0, count)
	i := -1
	for r := range ordered {
		i++
		if !mask[i] {
			continue
		}
		costRow := *r
		costRow.webSearchRequests = webSearchRequests[i]
		cost, costSource, priced, contributes, sessionCost, priceErr := clickActivityUsageCost(costRow, rateResolver)
		if priceErr != nil {
			return nil, priceErr
		}
		out = append(out, activity.UsageRow{
			SessionID:         attribution[i],
			Model:             costRow.model,
			Timestamp:         costRow.ts,
			InputTokens:       costRow.inputTok,
			OutputTokens:      costRow.outputTok,
			WebSearchRequests: webSearchRequests[i],
			Cost:              cost,
			CostSource:        costSource,
			SessionCost:       sessionCost,
			Priced:            priced,
			Contributes:       contributes,
			Agent:             costRow.agent,
			ClaudeMessageID:   costRow.claudeMessageID,
			ClaudeRequestID:   costRow.claudeRequestID,
			SourceUUID:        costRow.sourceUUID,
			UsageDedupKey:     costRow.usageDedupKey,
		})
	}
	return out, nil
}
