package activity

import (
	"context"
	"maps"
	"slices"
)

// Launcher describes an ancestor and whether it passes the report filters.
type Launcher struct {
	ID              string
	IsAutomated     bool
	IsSubagent      bool
	ParentSessionID string
	Counted         bool
}

type Launchers struct {
	Rows     map[string]Launcher
	Examined []string
}

// ResolveLaunchers loads ancestors once per level, including missing IDs in Examined.
func ResolveLaunchers(ctx context.Context, parentIDs []string, load func(context.Context, []string) ([]Launcher, error)) (Launchers, error) {
	result := Launchers{Rows: make(map[string]Launcher)}
	seen := make(map[string]struct{})
	for len(parentIDs) > 0 {
		var ids []string
		for _, id := range parentIDs {
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			break
		}
		if err := ctx.Err(); err != nil {
			return Launchers{}, err
		}
		slices.Sort(ids)
		rows, err := load(ctx, ids)
		if err != nil {
			return Launchers{}, err
		}
		parentIDs = nil
		for _, row := range rows {
			result.Rows[row.ID] = row
			if row.IsSubagent {
				parentIDs = append(parentIDs, row.ParentSessionID)
			}
		}
	}
	result.Examined = slices.Sorted(maps.Keys(seen))
	return result, nil
}

// LauncherBackend loads ancestors outside the candidate date range.
type LauncherBackend interface {
	ActivityReportLaunchers(context.Context, []SessionMeta, Query) (Launchers, error)
}
