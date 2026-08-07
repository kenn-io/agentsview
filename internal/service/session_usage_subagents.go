// ABOUTME: presentation-time combination of a session's own usage with
// ABOUTME: the usage of every subagent transcript spawned beneath it.
package service

import (
	"context"
	"fmt"
	"sort"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// SessionUsageWithSubagents returns rootID's usage with every reachable
// subagent and the forks inside those subagent subtrees folded in.
//
// Claude Code writes each Task-tool subagent to its own transcript under
// <session>/subagents/, which agentsview ingests as a separate session. A
// parent's own rows therefore omit whatever its subagents spent, which
// understates the cost of the work the caller asked for. This combines the
// two at read time only: nothing is persisted, no child row is duplicated
// under the parent, and day aggregates (which already count subagent
// sessions as first-class spend) are untouched.
//
// Traversal matches GetSessionUsageRollup: breadth-first through all children,
// including subagents plus forks created inside a subagent subtree, cycle-safe.
// Root-level forks are traversed but not included. When the root has no
// delegated descendants the store's own-session result is returned unchanged.
//
// Rows come from GetSessionUsageRows, which dedups across the whole id set,
// so a message recorded in both the parent and a child transcript is
// counted once. Breakdown entries stay in that call's global order
// (timestamp, then root-before-descendants, then message ordinal) and are
// renumbered from 1; entries from a child carry its id in
// SubagentSessionID while keeping their real Source.
func SessionUsageWithSubagents(
	ctx context.Context, store db.Store, rootID string, includeBreakdown bool,
) (*db.SessionUsage, error) {
	root, err := store.GetSessionUsage(ctx, rootID, includeBreakdown)
	if err != nil || root == nil {
		return nil, err
	}
	descendants, err := delegatedDescendants(ctx, store, rootID)
	if err != nil {
		return nil, err
	}
	if len(descendants) == 0 {
		return root, nil
	}

	var rowSet *activity.SessionUsageRows
	if provider, ok := store.(sessionUsageRowsProvider); ok {
		rowSet, err = provider.GetSessionUsageRows(
			ctx, sessionUsageIDs(rootID, descendants))
		if err != nil {
			return nil, err
		}
	}
	if rowSet == nil {
		return combineSubagentUsageFromSessions(
			ctx, store, root, descendants, includeBreakdown)
	}
	rootStoredOutputTokens := root.TotalOutputTokens
	storedRoot, err := store.GetSession(ctx, rootID)
	if err != nil {
		return nil, err
	}
	if storedRoot != nil {
		rootStoredOutputTokens = storedRoot.TotalOutputTokens
	}
	return combineSubagentUsageFromRows(
		rootID, root, rootStoredOutputTokens, descendants, rowSet.Rows,
		rowSet.RawOutputTokensBySession,
		rowSet.DiscardedContributingSessions, includeBreakdown)
}

// combineSubagentUsageFromRows builds the combined result from one deduped,
// globally ordered usage-row set. rootID is the id the rows were queried
// under, which is what decides whether a row belongs to the parent.
func combineSubagentUsageFromRows(
	rootID string,
	root *db.SessionUsage,
	rootStoredOutputTokens int,
	descendants []db.Session,
	rows []activity.UsageRow,
	rawOutputTokensBySession map[string]int,
	discardedContributingSessions map[string]struct{},
	includeBreakdown bool,
) (*db.SessionUsage, error) {
	out := newCombinedSessionUsage(root, descendants)
	allocated := activity.AllocateUsageCosts(rows)

	var cost money.Money
	var hasComputedCost, hasReportedCost bool
	hasCostSettlement := false
	allPriced := true
	models := make(map[string]struct{})
	unpriced := make(map[string]struct{})
	breakdown := make([]db.SessionUsageBreakdownEntry, 0, len(rows))
	outputBySession := make(map[string]int)
	usageRowsBySession := make(map[string]struct{})

	for i, row := range rows {
		alloc := allocated[i]
		if !alloc.Contributes {
			continue
		}
		hasCostSettlement = true
		recordRollupCostSource(
			alloc.CostSource, &hasComputedCost, &hasReportedCost)
		if alloc.Priced {
			sum, addErr := money.Add(cost, alloc.Cost)
			if addErr != nil {
				return nil, fmt.Errorf(
					"summing session usage with subagents: %w", addErr)
			}
			cost = sum
		} else {
			allPriced = false
			if row.Contributes {
				unpriced[row.Model] = struct{}{}
			}
		}
		// Allocation promotes a cost-only session-total carrier so its
		// settlement is retained. The original row still decides whether
		// user-visible usage metadata and breakdown membership exist.
		if !row.Contributes {
			continue
		}
		usageRowsBySession[usageRowSourceSessionID(row)] = struct{}{}
		outputBySession[row.SessionID] += row.OutputTokens
		models[row.Model] = struct{}{}
		out.BreakdownCount++
		if includeBreakdown {
			breakdown = append(breakdown, usageRowBreakdownEntry(
				row, rootID, out.BreakdownCount,
				alloc.Cost, alloc.Priced))
		}
	}

	out.Breakdown = breakdown
	out.Models = sortedKeys(models)
	var outputCostCovered bool
	out.TotalOutputTokens, outputCostCovered = combinedOutputTokens(
		rootID, rootStoredOutputTokens, descendants, outputBySession,
		rawOutputTokensBySession)
	if out.TotalOutputTokens > 0 {
		out.HasTokenData = true
	}
	usageRowsCovered := sessionUsageRowsCoverTokens(
		rootID,
		rootStoredOutputTokens > 0 || root.PeakContextTokens > 0,
		descendants, usageRowsBySession,
		discardedContributingSessions)
	out.HasCost = hasCostSettlement && allPriced && outputCostCovered &&
		usageRowsCovered
	if out.HasCost {
		out.Cost = cost
		out.CostSource = export.CombinedCostSource(
			hasComputedCost, hasReportedCost)
		out.AICredits = db.AICreditsFromCost(out.Agent, out.Cost)
	}
	out.CostUSD = db.CostUSDFromCost(out.HasCost, out.Cost)
	if len(unpriced) > 0 {
		out.UnpricedModels = sortedKeys(unpriced)
	}
	return out, nil
}

// combinedOutputTokens totals output tokens over the included sessions
// without double-counting a message that appears in more than one
// transcript.
//
// A session's stored total_output_tokens includes output-bearing messages
// even when they lack the raw usage payload required for cost rows. Preserve
// that rowless residual while counting usage-row output only from the globally
// deduplicated survivors. Survivors remain a fallback for legacy sessions
// whose stored total is incomplete.
func combinedOutputTokens(
	rootID string,
	rootStoredOutputTokens int,
	descendants []db.Session,
	outputBySession map[string]int,
	rawOutputTokensBySession map[string]int,
) (total int, costCovered bool) {
	costCovered = true
	for _, output := range outputBySession {
		total += output
	}
	addSession := func(id string, stored int) {
		residual := stored - rawOutputTokensBySession[id]
		if residual > 0 {
			total += residual
			costCovered = false
		}
	}
	addSession(rootID, rootStoredOutputTokens)
	for _, descendant := range descendants {
		addSession(descendant.ID, descendant.TotalOutputTokens)
	}
	return total, costCovered
}

// sessionUsageRowsCoverTokens reports whether every token-bearing included
// session is represented by at least one surviving or deduplicated usage row.
// Session-level context data can exist without output tokens, so output-token
// reconciliation alone cannot establish complete cost coverage.
func sessionUsageRowsCoverTokens(
	rootID string,
	rootHasPositiveTokens bool,
	descendants []db.Session,
	usageRowsBySession map[string]struct{},
	discardedContributingSessions map[string]struct{},
) bool {
	hasRows := func(id string) bool {
		if _, ok := usageRowsBySession[id]; ok {
			return true
		}
		_, ok := discardedContributingSessions[id]
		return ok
	}
	if rootHasPositiveTokens && !hasRows(rootID) {
		return false
	}
	for _, descendant := range descendants {
		hasPositiveTokens := descendant.TotalOutputTokens > 0 ||
			descendant.PeakContextTokens > 0
		if hasPositiveTokens && !hasRows(descendant.ID) {
			return false
		}
	}
	return true
}

func usageRowSourceSessionID(row activity.UsageRow) string {
	if row.SourceSessionID != "" {
		return row.SourceSessionID
	}
	return row.SessionID
}

// combineSubagentUsageFromSessions is the fallback for stores that expose no
// usage-row provider. It merges per-session results instead, which cannot
// dedup rows shared between a parent and a child transcript; every store in
// this repo implements GetSessionUsageRows, so the primary path is what
// production takes.
func combineSubagentUsageFromSessions(
	ctx context.Context,
	store db.Store,
	root *db.SessionUsage,
	descendants []db.Session,
	includeBreakdown bool,
) (*db.SessionUsage, error) {
	out := newCombinedSessionUsage(root, descendants)
	models := make(map[string]struct{})
	unpriced := make(map[string]struct{})
	breakdown := make([]db.SessionUsageBreakdownEntry, 0, len(root.Breakdown))

	var cost money.Money
	var hasComputedCost, hasReportedCost bool
	contributing := false
	allPriced := true

	accumulate := func(usage *db.SessionUsage, subagentID string) error {
		if usage == nil {
			return nil
		}
		if usage.HasTokenData && !usage.HasCost {
			allPriced = false
		}
		for _, model := range usage.Models {
			models[model] = struct{}{}
		}
		for _, model := range usage.UnpricedModels {
			unpriced[model] = struct{}{}
		}
		if usage.BreakdownCount > 0 {
			contributing = true
			if usage.HasCost {
				sum, err := money.Add(cost, usage.Cost)
				if err != nil {
					return fmt.Errorf(
						"summing subagent session usage: %w", err)
				}
				cost = sum
				recordRollupCostSource(
					usage.CostSource, &hasComputedCost, &hasReportedCost)
			} else {
				allPriced = false
			}
		}
		out.BreakdownCount += usage.BreakdownCount
		for _, entry := range usage.Breakdown {
			entry.Ordinal = len(breakdown) + 1
			entry.SubagentSessionID = subagentID
			breakdown = append(breakdown, entry)
		}
		return nil
	}

	if err := accumulate(root, ""); err != nil {
		return nil, err
	}
	for _, descendant := range descendants {
		usage, err := store.GetSessionUsage(
			ctx, descendant.ID, includeBreakdown)
		if err != nil {
			return nil, err
		}
		if err := accumulate(usage, descendant.ID); err != nil {
			return nil, err
		}
	}

	out.Breakdown = breakdown
	out.Models = sortedKeys(models)
	out.HasCost = contributing && allPriced && len(unpriced) == 0
	if out.HasCost {
		out.Cost = cost
		out.CostSource = export.CombinedCostSource(
			hasComputedCost, hasReportedCost)
		out.AICredits = db.AICreditsFromCost(out.Agent, out.Cost)
	}
	out.CostUSD = db.CostUSDFromCost(out.HasCost, out.Cost)
	if len(unpriced) > 0 {
		out.UnpricedModels = sortedKeys(unpriced)
	}
	return out, nil
}

// newCombinedSessionUsage seeds the combined result with the root's identity
// and the session-level token aggregates of everything included. Peak context
// is the maximum rather than a sum, because each session's peak is an
// independent high-water mark.
//
// The output-token total here sums stored per-session aggregates, which
// double-counts a message echoed across transcripts. That is the best the
// row-less fallback path can do; combineSubagentUsageFromRows overwrites it
// with a deduplicated total from the rows themselves.
func newCombinedSessionUsage(
	root *db.SessionUsage, descendants []db.Session,
) *db.SessionUsage {
	out := &db.SessionUsage{
		SessionID:         root.SessionID,
		Agent:             root.Agent,
		Project:           root.Project,
		TotalOutputTokens: root.TotalOutputTokens,
		PeakContextTokens: root.PeakContextTokens,
		HasTokenData:      root.HasTokenData,
		SubagentCount:     explicitSubagentCount(descendants),
	}
	for _, descendant := range descendants {
		out.TotalOutputTokens += descendant.TotalOutputTokens
		if descendant.PeakContextTokens > out.PeakContextTokens {
			out.PeakContextTokens = descendant.PeakContextTokens
		}
		if descendant.HasTotalOutputTokens ||
			descendant.HasPeakContextTokens {
			out.HasTokenData = true
		}
	}
	return out
}

// usageRowBreakdownEntry renders one deduped usage row as a breakdown entry,
// tagging it with its session id when it did not come from the root.
func usageRowBreakdownEntry(
	row activity.UsageRow,
	rootID string,
	ordinal int,
	cost money.Money,
	priced bool,
) db.SessionUsageBreakdownEntry {
	// UsageRow carries the ordinal in its COALESCE(message_ordinal, -1)
	// convention; the breakdown entry uses a pointer with nil for "not
	// tied to a message".
	var messageOrdinal *int
	if row.MessageOrdinal >= 0 {
		v := int(row.MessageOrdinal)
		messageOrdinal = &v
	}
	label := db.SessionUsageBreakdownLabel(messageOrdinal, row.UsageSource)
	entry := db.SessionUsageBreakdownEntry{
		Ordinal:                  ordinal,
		MessageOrdinal:           messageOrdinal,
		Source:                   row.UsageSource,
		Label:                    label,
		Timestamp:                row.Timestamp,
		Model:                    row.Model,
		InputTokens:              row.InputTokens,
		OutputTokens:             row.OutputTokens,
		CacheCreationInputTokens: row.CacheCreationTokens,
		CacheReadInputTokens:     row.CacheReadTokens,
		WebSearchRequests:        row.WebSearchRequests,
		Cost:                     cost,
		HasCost:                  priced,
	}
	sourceSessionID := usageRowSourceSessionID(row)
	if sourceSessionID != rootID {
		entry.SubagentSessionID = sourceSessionID
	}
	return entry
}

// sortedKeys returns the set's keys sorted; never nil, so JSON renders "[]"
// rather than "null" (matching the per-backend session usage paths).
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
