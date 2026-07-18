package service

import (
	"context"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

type sessionUsageRowsProvider interface {
	GetSessionUsageRows(context.Context, []string) ([]activity.UsageRow, error)
}

// SessionUsageRollup combines a root session's usage with explicit subagent
// descendants. SubagentCount includes descendants without usage rows.
type SessionUsageRollup struct {
	Usage         *db.SessionUsage
	CostUSD       float64
	HasCost       bool
	CostSource    export.CostSource
	SubagentCount int
}

// GetSessionUsageRollup returns the root usage and the complete priced cost of
// every reachable session whose stored relationship_type is "subagent".
func GetSessionUsageRollup(
	ctx context.Context, store db.Store, rootID string, includeBreakdown bool,
) (*SessionUsageRollup, error) {
	root, err := store.GetSessionUsage(ctx, rootID, includeBreakdown)
	if err != nil || root == nil {
		return nil, err
	}

	out := &SessionUsageRollup{Usage: root}
	visited := map[string]struct{}{rootID: {}}
	queue := []string{rootID}
	usageIDs := []string{rootID}

	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]
		children, err := store.GetChildSessions(ctx, parentID)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if _, ok := visited[child.ID]; ok {
				continue
			}
			visited[child.ID] = struct{}{}
			if child.RelationshipType == "subagent" {
				out.SubagentCount++
				usageIDs = append(usageIDs, child.ID)
			}
			queue = append(queue, child.ID)
		}
	}
	subagentContributing := false
	allPriced := true
	totalCostUSD := 0.0
	var hasComputedCost, hasReportedCost bool
	if provider, ok := store.(sessionUsageRowsProvider); ok {
		rows, err := provider.GetSessionUsageRows(ctx, usageIDs)
		if err != nil {
			return nil, err
		}
		if rows != nil {
			allocated := activity.AllocateUsageCosts(rows)
			for i, row := range rows {
				cost := allocated[i]
				if !cost.Contributes {
					continue
				}
				if row.SessionID != rootID {
					subagentContributing = true
				}
				if !cost.Priced {
					allPriced = false
					continue
				}
				totalCostUSD += cost.Cost
				recordRollupCostSource(
					cost.CostSource, &hasComputedCost, &hasReportedCost)
			}
		} else {
			subagentContributing, totalCostUSD, allPriced,
				hasComputedCost, hasReportedCost, err =
				sumRollupUsageFallback(ctx, store, root, usageIDs)
			if err != nil {
				return nil, err
			}
		}
	} else {
		subagentContributing, totalCostUSD, allPriced,
			hasComputedCost, hasReportedCost, err =
			sumRollupUsageFallback(ctx, store, root, usageIDs)
		if err != nil {
			return nil, err
		}
	}
	out.HasCost = subagentContributing && allPriced
	if out.HasCost {
		out.CostUSD = totalCostUSD
		out.CostSource = export.CombinedCostSource(
			hasComputedCost, hasReportedCost)
	}
	return out, nil
}

func sumRollupUsageFallback(
	ctx context.Context,
	store db.Store,
	root *db.SessionUsage,
	usageIDs []string,
) (subagentContributing bool, totalCostUSD float64, allPriced,
	hasComputedCost, hasReportedCost bool, err error) {
	allPriced = true
	if root.BreakdownCount > 0 && !root.HasCost {
		allPriced = false
	}
	if root.HasCost {
		recordRollupCostSource(
			root.CostSource, &hasComputedCost, &hasReportedCost)
	}
	for _, id := range usageIDs[1:] {
		usage, getErr := store.GetSessionUsage(ctx, id, false)
		if getErr != nil {
			return false, 0, false, false, false, getErr
		}
		if usage == nil || usage.BreakdownCount == 0 {
			continue
		}
		subagentContributing = true
		if usage.HasCost {
			totalCostUSD += usage.CostUSD
			recordRollupCostSource(
				usage.CostSource, &hasComputedCost, &hasReportedCost)
		} else {
			allPriced = false
		}
	}
	totalCostUSD += root.CostUSD
	return subagentContributing, totalCostUSD, allPriced,
		hasComputedCost, hasReportedCost, nil
}

func recordRollupCostSource(
	source export.CostSource, hasComputed, hasReported *bool,
) {
	switch source {
	case export.CostSourceComputed:
		*hasComputed = true
	case export.CostSourceReported:
		*hasReported = true
	case export.CostSourceMixed:
		*hasComputed = true
		*hasReported = true
	}
}
