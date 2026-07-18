package service

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

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
	subagentContributing := false
	allPriced := true
	totalCostUSD := 0.0
	addUsage := func(usage *db.SessionUsage, isSubagent bool) {
		if usage.BreakdownCount == 0 {
			return
		}
		if isSubagent {
			subagentContributing = true
		}
		if usage.HasCost {
			totalCostUSD += usage.CostUSD
		} else {
			allPriced = false
		}
	}
	addUsage(root, false)

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
			if child.RelationshipType != "subagent" {
				continue
			}
			out.SubagentCount++
			usage, err := store.GetSessionUsage(ctx, child.ID, false)
			if err != nil {
				return nil, err
			}
			if usage != nil {
				addUsage(usage, true)
			}
			queue = append(queue, child.ID)
		}
	}
	out.HasCost = subagentContributing && allPriced
	if out.HasCost {
		out.CostUSD = totalCostUSD
	}
	return out, nil
}
