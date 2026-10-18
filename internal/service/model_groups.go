package service

import (
	"maps"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
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

// Configured reports whether model aliases are configured.
func (g ModelGroups) Configured() bool {
	return len(g.names) > 0
}

// Aliases returns a copy of the configured model-to-group map.
func (g ModelGroups) Aliases() map[string]string {
	if len(g.names) == 0 {
		return nil
	}
	return maps.Clone(g.names)
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

// GroupDaily merges each day's model breakdowns and sorts by cost, then name.
func (g ModelGroups) GroupDaily(daily []db.DailyUsageEntry) error {
	for i := range daily {
		day := &daily[i]
		rows := slices.Clone(day.ModelBreakdowns)
		for j := range rows {
			rows[j].ModelName = g.Name(rows[j].ModelName)
		}
		totals, err := foldModelTotals([]db.DailyUsageEntry{{ModelBreakdowns: rows}}, nil)
		if err != nil {
			return err
		}
		day.ModelBreakdowns = make([]db.ModelBreakdown, 0, len(totals))
		day.ModelsUsed = make([]string, 0, len(totals))
		for _, total := range totals {
			day.ModelBreakdowns = append(day.ModelBreakdowns, db.ModelBreakdown{
				ModelName:           total.Model,
				InputTokens:         total.InputTokens,
				OutputTokens:        total.OutputTokens,
				CacheCreationTokens: total.CacheCreationTokens,
				CacheReadTokens:     total.CacheReadTokens,
				Cost:                total.Cost,
			})
			day.ModelsUsed = append(day.ModelsUsed, total.Model)
		}
	}
	return nil
}
