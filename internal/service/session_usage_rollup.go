package service

import (
	"context"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

type sessionUsageRowsProvider interface {
	GetSessionUsageRows(context.Context, []string) ([]activity.UsageRow, error)
}

// SessionUsageRollup combines a root session's usage with explicit subagent
// descendants. SubagentCount includes descendants without usage rows.
type SessionUsageRollup struct {
	Usage         *db.SessionUsage
	Cost          money.Money
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
	var totalCost money.Money
	if provider, ok := store.(sessionUsageRowsProvider); ok {
		rows, err := provider.GetSessionUsageRows(ctx, usageIDs)
		if err != nil {
			return nil, err
		}
		if rows != nil {
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
				totalCost = money.MustAdd(totalCost, row.Cost)
			}
		} else {
			subagentContributing, totalCost, allPriced, err =
				sumRollupUsageFallback(ctx, store, root, usageIDs)
			if err != nil {
				return nil, err
			}
		}
	} else {
		subagentContributing, totalCost, allPriced, err =
			sumRollupUsageFallback(ctx, store, root, usageIDs)
		if err != nil {
			return nil, err
		}
	}
	out.HasCost = subagentContributing && allPriced
	if out.HasCost {
		out.Cost = totalCost
	}
	return out, nil
}

func sumRollupUsageFallback(
	ctx context.Context,
	store db.Store,
	root *db.SessionUsage,
	usageIDs []string,
) (subagentContributing bool, totalCost money.Money, allPriced bool, err error) {
	allPriced = true
	if root.BreakdownCount > 0 && !root.HasCost {
		allPriced = false
	}
	for _, id := range usageIDs[1:] {
		usage, getErr := store.GetSessionUsage(ctx, id, false)
		if getErr != nil {
			return false, money.Money{}, false, getErr
		}
		if usage == nil || usage.BreakdownCount == 0 {
			continue
		}
		subagentContributing = true
		if usage.HasCost {
			totalCost = money.MustAdd(totalCost, usage.Cost)
		} else {
			allPriced = false
		}
	}
	totalCost = money.MustAdd(totalCost, root.Cost)
	return subagentContributing, totalCost, allPriced, nil
}
