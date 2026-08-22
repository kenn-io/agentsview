package service

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

type sessionUsageRowsProvider interface {
	GetSessionUsageRows(
		context.Context, []string,
	) (*activity.SessionUsageRows, error)
}

// SessionUsageRollup combines a root session's usage with explicit subagent
// descendants and their forks. SubagentCount counts only explicit subagents,
// including subagents without usage rows.
type SessionUsageRollup struct {
	Usage         *db.SessionUsage
	Cost          money.Money
	HasCost       bool
	CostSource    export.CostSource
	SubagentCount int
}

// GetSessionUsageRollup returns the root usage and the complete priced cost of
// every reachable subagent plus forks created inside a subagent subtree.
func GetSessionUsageRollup(
	ctx context.Context, store db.Store, rootID string, includeBreakdown bool,
) (*SessionUsageRollup, error) {
	root, err := store.GetSessionUsage(ctx, rootID, includeBreakdown)
	if err != nil || root == nil {
		return nil, err
	}

	descendants, err := delegatedDescendants(ctx, store, rootID)
	if err != nil {
		return nil, err
	}
	out := &SessionUsageRollup{
		Usage:         root,
		SubagentCount: explicitSubagentCount(descendants),
	}
	usageIDs := sessionUsageIDs(rootID, descendants)

	subagentContributing := false
	allPriced := true
	var totalCost money.Money
	var hasComputedCost, hasReportedCost bool
	if provider, ok := store.(sessionUsageRowsProvider); ok {
		rowSet, err := provider.GetSessionUsageRows(ctx, usageIDs)
		if err != nil {
			return nil, err
		}
		if rowSet != nil {
			rows := rowSet.Rows
			allocated := activity.AllocateUsageCosts(rows)
			for i, row := range rows {
				cost := allocated[i]
				if !cost.Contributes {
					continue
				}
				if usageRowSourceSessionID(row) != rootID {
					subagentContributing = true
				}
				if !cost.Priced {
					allPriced = false
					continue
				}
				totalCost, err = money.Add(totalCost, cost.Cost)
				if err != nil {
					return nil, fmt.Errorf("summing session usage rollup: %w", err)
				}
				recordRollupCostSource(
					cost.CostSource, &hasComputedCost, &hasReportedCost)
			}
		} else {
			subagentContributing, totalCost, allPriced,
				hasComputedCost, hasReportedCost, err =
				sumRollupUsageFallback(ctx, store, root, usageIDs)
			if err != nil {
				return nil, err
			}
		}
	} else {
		subagentContributing, totalCost, allPriced,
			hasComputedCost, hasReportedCost, err =
			sumRollupUsageFallback(ctx, store, root, usageIDs)
		if err != nil {
			return nil, err
		}
	}
	out.HasCost = subagentContributing && allPriced
	if out.HasCost {
		out.Cost = totalCost
		out.CostSource = export.CombinedCostSource(
			hasComputedCost, hasReportedCost)
	}
	return out, nil
}

// delegatedDescendants walks the session graph breadth-first from rootID and
// returns every reachable subagent plus forks created inside a subagent
// subtree, in discovery order. Traversal descends through every relationship
// so a subagent nested under a root-level fork or continuation is still found.
// The path state distinguishes root-level forks, which are traversed but not
// included, from forks that belong to delegated work.
func delegatedDescendants(
	ctx context.Context, store db.Store, rootID string,
) ([]db.Session, error) {
	return delegatedDescendantsFrom(ctx, store, rootID, false)
}

func delegatedDescendantsFrom(
	ctx context.Context, store db.Store, rootID string, rootDelegated bool,
) ([]db.Session, error) {
	type walkState struct {
		id        string
		delegated bool
	}
	visited := map[walkState]struct{}{
		{id: rootID, delegated: rootDelegated}: {},
	}
	included := make(map[string]struct{})
	queue := []walkState{{id: rootID, delegated: rootDelegated}}
	var out []db.Session
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parent := queue[0]
		queue = queue[1:]
		children, err := store.GetChildSessions(ctx, parent.id)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if child.ID == rootID {
				continue
			}
			childDelegated := parent.delegated ||
				child.RelationshipType == "subagent"
			include := child.RelationshipType == "subagent" ||
				(parent.delegated && child.RelationshipType == "fork")
			if include {
				if _, ok := included[child.ID]; !ok {
					included[child.ID] = struct{}{}
					out = append(out, child)
				}
			}
			next := walkState{id: child.ID, delegated: childDelegated}
			if _, ok := visited[next]; ok {
				continue
			}
			visited[next] = struct{}{}
			queue = append(queue, next)
		}
	}
	return out, nil
}

// DelegatedUsageSessions returns the canonical session set included by the
// session-usage subagent rollup. The root itself is not returned.
func DelegatedUsageSessions(
	ctx context.Context, store db.Store, rootID string,
) ([]db.Session, error) {
	return delegatedDescendants(ctx, store, rootID)
}

func explicitSubagentCount(descendants []db.Session) int {
	count := 0
	for _, descendant := range descendants {
		if descendant.RelationshipType == "subagent" {
			count++
		}
	}
	return count
}

// sessionUsageIDs returns rootID followed by each descendant's id, the
// order GetSessionUsageRows uses to break ties between rows sharing a
// timestamp.
func sessionUsageIDs(rootID string, descendants []db.Session) []string {
	ids := make([]string, 0, len(descendants)+1)
	ids = append(ids, rootID)
	for _, d := range descendants {
		ids = append(ids, d.ID)
	}
	return ids
}

func sumRollupUsageFallback(
	ctx context.Context,
	store db.Store,
	root *db.SessionUsage,
	usageIDs []string,
) (subagentContributing bool, totalCost money.Money, allPriced,
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
			return false, money.Money{}, false, false, false, getErr
		}
		if usage == nil || usage.BreakdownCount == 0 {
			continue
		}
		subagentContributing = true
		if usage.HasCost {
			totalCost, err = money.Add(totalCost, usage.Cost)
			if err != nil {
				return false, money.Money{}, false, false, false,
					fmt.Errorf("summing subagent usage rollup: %w", err)
			}
			recordRollupCostSource(
				usage.CostSource, &hasComputedCost, &hasReportedCost)
		} else {
			allPriced = false
		}
	}
	totalCost, err = money.Add(totalCost, root.Cost)
	if err != nil {
		return false, money.Money{}, false, false, false,
			fmt.Errorf("summing root usage rollup: %w", err)
	}
	return subagentContributing, totalCost, allPriced,
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
