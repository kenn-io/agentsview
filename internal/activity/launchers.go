package activity

import (
	"context"
	"maps"
	"slices"
)

// Launcher describes an ancestor and whether it passes the report filters.
type Launcher struct {
	ID               string
	IsAutomated      bool
	IsSubagent       bool
	ParentSessionIDs []string
	MatchesFilter    bool
}

type Launchers struct {
	Waiting  map[string][]string
	Examined []string
}

// ResolveLaunchers loads unknown ancestors and finds interactive launchers for each run.
func ResolveLaunchers(ctx context.Context, runs []SessionMeta, selected map[string]bool, load func(context.Context, []string) ([]Launcher, error)) (Launchers, error) {
	rows := make(map[string]Launcher, len(runs))
	var pending []string
	for _, run := range runs {
		rows[run.SessionID] = Launcher{ID: run.SessionID, IsAutomated: run.IsAutomated, IsSubagent: run.IsSubagent, ParentSessionIDs: run.ParentSessionIDs, MatchesFilter: selected[run.SessionID]}
		if run.IsSubagent {
			pending = append(pending, run.ParentSessionIDs...)
		}
	}
	examined := make(map[string]struct{})
	for len(pending) > 0 {
		var ids []string
		for _, id := range pending {
			if id == "" {
				continue
			}
			if _, ok := examined[id]; ok {
				continue
			}
			examined[id] = struct{}{}
			ids = append(ids, id)
		}
		pending = nil
		var missing []string
		for _, id := range ids {
			if row, ok := rows[id]; ok {
				if row.IsSubagent {
					pending = append(pending, row.ParentSessionIDs...)
				}
			} else {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			continue
		}
		loaded, err := load(ctx, missing)
		if err != nil {
			return Launchers{}, err
		}
		for _, row := range loaded {
			rows[row.ID] = row
			if row.IsSubagent {
				pending = append(pending, row.ParentSessionIDs...)
			}
		}
	}
	result := Launchers{Waiting: make(map[string][]string), Examined: slices.Sorted(maps.Keys(examined))}
	for _, run := range runs {
		pending := []string{run.SessionID}
		seen := map[string]bool{}
		for len(pending) > 0 {
			id := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if seen[id] {
				continue
			}
			seen[id] = true
			current, ok := rows[id]
			if !ok {
				continue
			}
			if id != run.SessionID && current.MatchesFilter && !current.IsSubagent && !current.IsAutomated {
				result.Waiting[run.SessionID] = append(result.Waiting[run.SessionID], id)
			}
			if current.IsSubagent {
				pending = append(pending, current.ParentSessionIDs...)
			}
		}
		slices.Sort(result.Waiting[run.SessionID])
	}
	return result, nil
}

// LauncherBackend loads ancestors outside the candidate date range.
type LauncherBackend interface {
	ActivityReportLaunchers(context.Context, []SessionMeta, map[string]bool, Query) (Launchers, error)
}
