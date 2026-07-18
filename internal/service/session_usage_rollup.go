package service

import (
	"context"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
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
	if provider, ok := store.(sessionUsageRowsProvider); ok {
		rows, err := provider.GetSessionUsageRows(ctx, usageIDs)
		if err != nil {
			return nil, err
		}
		if rows != nil {
			rows = activity.SubstituteAuthoritativeSessionCosts(rows)
			for _, row := range rows {
				if !row.Contributes {
					continue
				}
				if row.SessionID != rootID {
					subagentContributing = true
				}
				if !row.Priced {
					allPriced = false
					continue
				}
				totalCostUSD += row.Cost
			}
		} else {
			subagentContributing, totalCostUSD, allPriced, err =
				sumRollupUsageFallback(ctx, store, root, usageIDs)
			if err != nil {
				return nil, err
			}
		}
	} else {
		subagentContributing, totalCostUSD, allPriced, err =
			sumRollupUsageFallback(ctx, store, root, usageIDs)
		if err != nil {
			return nil, err
		}
	}
	out.HasCost = subagentContributing && allPriced
	if out.HasCost {
		out.CostUSD = totalCostUSD
	}
	return out, nil
}

func sumRollupUsageFallback(
	ctx context.Context,
	store db.Store,
	root *db.SessionUsage,
	usageIDs []string,
) (subagentContributing bool, totalCostUSD float64, allPriced bool, err error) {
	allPriced = true
	if root.BreakdownCount > 0 && !root.HasCost {
		allPriced = false
	}
	for _, id := range usageIDs[1:] {
		usage, getErr := store.GetSessionUsage(ctx, id, false)
		if getErr != nil {
			return false, 0, false, getErr
		}
		if usage == nil || usage.BreakdownCount == 0 {
			continue
		}
		subagentContributing = true
		if usage.HasCost {
			totalCostUSD += usage.CostUSD
		} else {
			allPriced = false
		}
	}
	totalCostUSD += root.CostUSD
	return subagentContributing, totalCostUSD, allPriced, nil
}
