package db

import (
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/export"
)

// ProjectInventoryRow is one project's aggregated session inventory plus
// worktree-mapping-rule attribution, keyed by opaque project identity.
type ProjectInventoryRow struct {
	Label                 string     `json:"label"`
	ProjectKey            string     `json:"project_key"`
	Sessions              int        `json:"sessions"`
	Machines              int        `json:"machines"`
	Agents                int        `json:"agents"`
	DistinctCwds          int        `json:"distinct_cwds"`
	FirstActivity         *time.Time `json:"first_activity,omitempty"`
	LastActivity          *time.Time `json:"last_activity,omitempty"`
	EnabledRulesTargeting int        `json:"enabled_rules_targeting"`
	RecordedAsOriginal    bool       `json:"recorded_as_original"`
}

// ProjectInventory is the full project inventory: one row per opaque project
// identity, plus archive-wide totals.
type ProjectInventory struct {
	Projects         []ProjectInventoryRow `json:"projects"`
	TotalProjects    int                   `json:"total_projects"`
	TotalSessions    int                   `json:"total_sessions"`
	GovernedSessions int                   `json:"governed_sessions"`
}

// projectInventoryAgg is one raw project's aggregate over visible
// (non-deleted) sessions, before display-label sanitization.
type projectInventoryAgg struct {
	sessions     int
	machines     int
	agents       int
	distinctCwds int
	first        *time.Time
	last         *time.Time
}

// buildProjectInventoryRows groups raw project labels by opaque project key.
// Display-label sanitization is presentation-only: distinct absolute-path
// projects may both display as empty without losing either row or key.
func buildProjectInventoryRows(
	agg map[string]projectInventoryAgg,
	rawProjects []string,
	projects map[string]export.ProjectMapEntry,
) ([]ProjectInventoryRow, int) {
	sort.Strings(rawProjects)

	byKey := map[string]*ProjectInventoryRow{}
	totalSessions := 0
	for _, project := range rawProjects {
		a := agg[project]
		totalSessions += a.sessions
		label := export.SafeProjectDisplayLabel(project)
		projectKey := export.ProjectKeyForEntry(projects[project])
		row, ok := byKey[projectKey]
		if !ok {
			row = &ProjectInventoryRow{
				Label:      label,
				ProjectKey: projectKey,
			}
			byKey[projectKey] = row
		} else if row.Label == "" && label != "" {
			row.Label = label
		}
		row.Sessions += a.sessions
		row.Machines += a.machines
		row.Agents += a.agents
		row.DistinctCwds += a.distinctCwds
		row.FirstActivity = minTimePtr(row.FirstActivity, a.first)
		row.LastActivity = maxTimePtr(row.LastActivity, a.last)
	}

	rowList := make([]ProjectInventoryRow, 0, len(byKey))
	for _, row := range byKey {
		rowList = append(rowList, *row)
	}
	sort.Slice(rowList, func(i, j int) bool {
		if rowList[i].Label != rowList[j].Label {
			return rowList[i].Label < rowList[j].Label
		}
		return rowList[i].ProjectKey < rowList[j].ProjectKey
	})
	return rowList, totalSessions
}

// annotateProjectInventoryRows sets EnabledRulesTargeting and
// RecordedAsOriginal on rows in place, keyed by opaque project identity.
//
// Static attribution counts every enabled explicit-layout rule whose
// target project resolves to a row's key, regardless of whether
// that rule currently governs any session. Dynamic attribution adds, per
// project key, the number of distinct repo_dot_worktrees rules the evaluator
// found currently resolving at least one row to it (eval.DynamicLabelRules).
// RecordedAsOriginal scans every mapping, including disabled ones, since
// original_project is the historical display label the user renamed away
// from. A duplicate display label is intentionally left unattributed because
// the historical field cannot identify which opaque project it represented.
func annotateProjectInventoryRows(
	rows []ProjectInventoryRow,
	mappings []WorktreeProjectMapping,
	eval GovernedEvaluation,
	projects map[string]export.ProjectMapEntry,
) {
	byKey := make(map[string]*ProjectInventoryRow, len(rows))
	byLabel := make(map[string][]*ProjectInventoryRow, len(rows))
	for i := range rows {
		row := &rows[i]
		byKey[row.ProjectKey] = row
		byLabel[row.Label] = append(byLabel[row.Label], row)
	}
	rowForProject := func(project string) *ProjectInventoryRow {
		key := export.ProjectKeyForEntry(projects[project])
		return byKey[key]
	}

	for _, m := range mappings {
		if m.Enabled && m.Layout != WorktreeMappingLayoutRepoDotWorktrees && m.Project != "" {
			if row := rowForProject(m.Project); row != nil {
				row.EnabledRulesTargeting++
			}
		}
		if m.OriginalProject != "" {
			if matches := byLabel[m.OriginalProject]; len(matches) == 1 {
				matches[0].RecordedAsOriginal = true
			}
		}
	}
	for project, rules := range eval.DynamicLabelRules {
		if row := rowForProject(project); row != nil {
			row.EnabledRulesTargeting += len(rules)
		}
	}
}

// minTimePtr returns the earlier of a and b, treating nil as "no bound".
func minTimePtr(a, b *time.Time) *time.Time {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if b.Before(*a) {
		return b
	}
	return a
}

// maxTimePtr returns the later of a and b, treating nil as "no bound".
func maxTimePtr(a, b *time.Time) *time.Time {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if b.After(*a) {
		return b
	}
	return a
}
