package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/export"
)

// ProjectInventoryRow is one project's aggregated session inventory plus
// worktree-mapping-rule attribution, keyed by display label.
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

// ProjectInventory is the full project inventory: one row per display
// label, plus archive-wide totals.
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

// GetProjectInventory aggregates every visible session into a per-project
// inventory: session/machine/agent/cwd counts and activity bounds, plus
// worktree-mapping-rule attribution (which enabled rules target each
// project, and whether it was ever recorded as a rule's original_project).
func (db *DB) GetProjectInventory(ctx context.Context) (ProjectInventory, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	agg, err := db.projectInventoryAggregate(ctx)
	if err != nil {
		return ProjectInventory{}, err
	}

	rawProjects := make([]string, 0, len(agg))
	for project := range agg {
		rawProjects = append(rawProjects, project)
	}
	projects, err := db.BuildProjectIdentityMap(ctx, rawProjects)
	if err != nil {
		return ProjectInventory{}, err
	}
	rows, totalSessions := buildProjectInventoryRows(agg, rawProjects, projects)

	mappings, eval, err := db.projectInventoryGovernance(ctx)
	if err != nil {
		return ProjectInventory{}, err
	}
	annotateProjectInventoryRows(rows, mappings, eval)

	return ProjectInventory{
		Projects:         rows,
		TotalProjects:    len(rows),
		TotalSessions:    totalSessions,
		GovernedSessions: eval.GovernedSessions,
	}, nil
}

// projectInventoryAggregate runs the one-pass aggregation over visible
// sessions, grouped by raw (unsanitized) project label. cwd distinctness
// normalizes backslashes to forward slashes so a Windows-style cwd and its
// POSIX-style equivalent collapse to one entry; this is the dominant
// cross-platform duplicate case and is expressible in all three storage
// dialects.
func (db *DB) projectInventoryAggregate(
	ctx context.Context,
) (map[string]projectInventoryAgg, error) {
	rows, err := db.getReader().QueryContext(ctx, projectInventoryAggregateQuery())
	if err != nil {
		return nil, fmt.Errorf("aggregating project inventory: %w", err)
	}
	defer rows.Close()

	out := map[string]projectInventoryAgg{}
	for rows.Next() {
		var project string
		var agg projectInventoryAgg
		var first, last sql.NullString
		if err := rows.Scan(
			&project, &agg.sessions, &agg.machines, &agg.agents,
			&agg.distinctCwds, &first, &last,
		); err != nil {
			return nil, fmt.Errorf("scanning project inventory row: %w", err)
		}
		if first.Valid && first.String != "" {
			if t, err := parseTimestamp(first.String); err == nil {
				agg.first = &t
			}
		}
		if last.Valid && last.String != "" {
			if t, err := parseTimestamp(last.String); err == nil {
				agg.last = &t
			}
		}
		out[project] = agg
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating project inventory rows: %w", err)
	}
	return out, nil
}

// projectInventoryAggregateQuery returns the one-pass aggregation SQL
// grouped by raw project label. Factored out so tests can EXPLAIN QUERY
// PLAN it directly and assert it stays a single sessions scan. Timestamps
// are stored as TEXT and legacy rows may hold empty strings instead of
// NULL; NULLIF keeps such rows from corrupting MIN (an empty string sorts
// before every real timestamp).
func projectInventoryAggregateQuery() string {
	return `
		SELECT project,
		       COUNT(*),
		       COUNT(DISTINCT machine),
		       COUNT(DISTINCT agent),
		       COUNT(DISTINCT CASE WHEN cwd IS NOT NULL AND cwd != ''
		             THEN replace(cwd, '\', '/') END),
		       MIN(NULLIF(started_at, '')),
		       MAX(COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, '')))
		FROM sessions
		WHERE deleted_at IS NULL
		GROUP BY project
		ORDER BY project`
}

// buildProjectInventoryRows sanitizes each raw project into its display
// label and merges rows that collide onto the same label. The sanitizer
// (export.SafeProjectDisplayLabel) only collides for private-path labels
// (e.g. two different absolute-path projects both sanitize to ""), so a
// collision is rare; when it happens, distinct counts (machines, agents,
// cwds) merge conservatively by summing rather than true deduplication,
// since the raw per-project counts don't carry enough information to
// dedupe across projects.
func buildProjectInventoryRows(
	agg map[string]projectInventoryAgg,
	rawProjects []string,
	projects map[string]export.ProjectMapEntry,
) ([]ProjectInventoryRow, int) {
	sort.Strings(rawProjects)

	byLabel := map[string]*ProjectInventoryRow{}
	var order []string
	totalSessions := 0
	for _, project := range rawProjects {
		a := agg[project]
		totalSessions += a.sessions
		label := export.SafeProjectDisplayLabel(project)
		row, ok := byLabel[label]
		if !ok {
			row = &ProjectInventoryRow{
				Label:      label,
				ProjectKey: export.ProjectKeyForEntry(projects[project]),
			}
			byLabel[label] = row
			order = append(order, label)
		}
		row.Sessions += a.sessions
		row.Machines += a.machines
		row.Agents += a.agents
		row.DistinctCwds += a.distinctCwds
		row.FirstActivity = minTimePtr(row.FirstActivity, a.first)
		row.LastActivity = maxTimePtr(row.LastActivity, a.last)
	}

	sort.Strings(order)
	rowList := make([]ProjectInventoryRow, len(order))
	for i, label := range order {
		if row := byLabel[label]; row != nil {
			rowList[i] = *row
		}
	}
	return rowList, totalSessions
}

// projectInventoryGovernance loads every worktree mapping (enabled and
// disabled) plus the candidate session rows for machines with at least one
// enabled mapping, then runs the shared governed-session evaluator.
func (db *DB) projectInventoryGovernance(
	ctx context.Context,
) ([]WorktreeProjectMapping, GovernedEvaluation, error) {
	mappings, err := db.ListAllWorktreeProjectMappings(ctx)
	if err != nil {
		return nil, GovernedEvaluation{}, fmt.Errorf(
			"listing worktree mappings for project inventory: %w", err)
	}
	archiveID, err := db.GetArchiveID(ctx)
	if err != nil {
		return nil, GovernedEvaluation{}, fmt.Errorf(
			"resolving archive id for project inventory: %w", err)
	}

	machines := governedCandidateMachines(mappings)
	candidates, err := db.projectInventoryCandidateRows(ctx, archiveID, machines)
	if err != nil {
		return nil, GovernedEvaluation{}, err
	}

	eval := EvaluateGovernedSessions(
		[]ArchiveMappings{{SourceArchiveID: archiveID, Mappings: mappings}},
		candidates,
	)
	return mappings, eval, nil
}

// governedCandidateMachines returns the set of machines carrying at least
// one enabled worktree mapping. Only these machines' sessions are fetched
// as governed-evaluation candidates; a machine whose mappings are all
// disabled (or that has no mapping at all) contributes no candidate rows,
// regardless of its session count.
func governedCandidateMachines(
	mappings []WorktreeProjectMapping,
) map[string]struct{} {
	machines := map[string]struct{}{}
	for _, m := range mappings {
		if m.Enabled {
			machines[m.Machine] = struct{}{}
		}
	}
	return machines
}

// projectInventoryCandidateRows returns the prefiltered session rows the
// evaluator needs: every visible session on a machine with at least one
// enabled worktree mapping. SourceArchiveID is set to the local archive ID
// for every row, matching the ArchiveMappings entry built by the caller.
func (db *DB) projectInventoryCandidateRows(
	ctx context.Context, archiveID string, machines map[string]struct{},
) ([]MappingEvaluationRow, error) {
	if len(machines) == 0 {
		return nil, nil
	}
	machineList := sortedSetKeys(machines)
	query, args := projectInventoryCandidateQuery(machineList)
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"querying project inventory candidate sessions: %w", err)
	}
	defer rows.Close()

	var out []MappingEvaluationRow
	for rows.Next() {
		var row MappingEvaluationRow
		if err := rows.Scan(
			&row.SessionID, &row.Machine, &row.Project, &row.Cwd, &row.FilePath,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning project inventory candidate session: %w", err)
		}
		row.SourceArchiveID = archiveID
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating project inventory candidate sessions: %w", err)
	}
	return out, nil
}

// projectInventoryCandidateQuery returns the candidate-row SQL and its bind
// args for the given machine list. Factored out so tests can EXPLAIN QUERY
// PLAN it directly and assert it uses the sessions machine index.
func projectInventoryCandidateQuery(machineList []string) (string, []any) {
	placeholders := make([]string, len(machineList))
	args := make([]any, len(machineList))
	for i, m := range machineList {
		placeholders[i] = "?"
		args[i] = m
	}
	query := `
		SELECT id, machine, project, cwd, COALESCE(file_path, '')
		FROM sessions
		WHERE deleted_at IS NULL
		  AND machine IN (` + strings.Join(placeholders, ",") + `)`
	return query, args
}

// annotateProjectInventoryRows sets EnabledRulesTargeting and
// RecordedAsOriginal on rows in place, keyed by display label.
//
// Static attribution counts every enabled explicit-layout rule whose
// (sanitized) target project matches a row's label, regardless of whether
// that rule currently governs any session. Dynamic attribution adds, per
// label, the number of distinct repo_dot_worktrees rules the evaluator
// found currently resolving at least one row to it (eval.DynamicLabelRules).
// RecordedAsOriginal scans every mapping, including disabled ones, since
// original_project is a historical record of what the user renamed away
// from; original_project stores the display label the user saw, so it's
// compared directly against Label rather than re-sanitized.
func annotateProjectInventoryRows(
	rows []ProjectInventoryRow,
	mappings []WorktreeProjectMapping,
	eval GovernedEvaluation,
) {
	byLabel := make(map[string]*ProjectInventoryRow, len(rows))
	for i := range rows {
		byLabel[rows[i].Label] = &rows[i]
	}

	for _, m := range mappings {
		if m.Enabled && m.Layout != WorktreeMappingLayoutRepoDotWorktrees && m.Project != "" {
			if row, ok := byLabel[export.SafeProjectDisplayLabel(m.Project)]; ok {
				row.EnabledRulesTargeting++
			}
		}
		if m.OriginalProject != "" {
			if row, ok := byLabel[m.OriginalProject]; ok {
				row.RecordedAsOriginal = true
			}
		}
	}
	for project, rules := range eval.DynamicLabelRules {
		if row, ok := byLabel[export.SafeProjectDisplayLabel(project)]; ok {
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
