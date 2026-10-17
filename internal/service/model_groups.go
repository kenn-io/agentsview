package service

import (
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

// ModelGroups maps reported model names to configured display groups.
// Its zero value preserves raw names and rows.
type ModelGroups struct {
	names   map[string]string
	members map[string][]string
}

// NewModelGroups copies aliases and indexes each group's members.
func NewModelGroups(aliases map[string]string) ModelGroups {
	g := ModelGroups{names: make(map[string]string, len(aliases)), members: make(map[string][]string)}
	for model, name := range aliases {
		g.names[model] = name
		g.members[name] = append(g.members[name], model)
	}
	for _, members := range g.members {
		slices.Sort(members)
	}
	return g
}

// Name returns a model's configured group, or its raw name.
func (g ModelGroups) Name(model string) string {
	if name, ok := g.names[model]; ok {
		return name
	}
	return model
}

// Expand includes group members while retaining raw member selections.
func (g ModelGroups) Expand(csv string) string {
	if len(g.names) == 0 {
		return csv
	}
	var out []string
	seen := make(map[string]bool)
	for _, token := range splitCSVTokens(csv) {
		for _, model := range append([]string{token}, g.members[token]...) {
			if !seen[model] {
				out = append(out, model)
				seen[model] = true
			}
		}
	}
	return strings.Join(out, ",")
}

// GroupDaily merges model breakdowns within each day, preserving absent slices.
func (g ModelGroups) GroupDaily(daily []db.DailyUsageEntry) error {
	if len(g.names) == 0 {
		return nil
	}
	for i := range daily {
		day := &daily[i]
		if day.ModelBreakdowns == nil {
			continue
		}
		rows := make([]db.ModelBreakdown, 0, len(day.ModelBreakdowns))
		names := make([]string, 0, len(day.ModelBreakdowns))
		indexes := make(map[string]int)
		for _, row := range day.ModelBreakdowns {
			name := g.Name(row.ModelName)
			index, ok := indexes[name]
			if !ok {
				index = len(rows)
				indexes[name] = index
				rows = append(rows, db.ModelBreakdown{ModelName: name})
				names = append(names, name)
			}
			target := &rows[index]
			target.InputTokens += row.InputTokens
			target.OutputTokens += row.OutputTokens
			target.CacheCreationTokens += row.CacheCreationTokens
			target.CacheReadTokens += row.CacheReadTokens
			var err error
			target.Cost, err = money.Add(target.Cost, row.Cost)
			if err != nil {
				return fmt.Errorf("summing model group cost: %w", err)
			}
		}
		day.ModelBreakdowns = rows
		day.ModelsUsed = names
	}
	return nil
}
