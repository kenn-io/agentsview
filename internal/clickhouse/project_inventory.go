package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// GetProjectInventory aggregates every visible session across every source
// archive mirrored into this ClickHouse store into a per-project inventory,
// plus worktree-mapping-rule attribution. It mirrors
// internal/db.GetProjectInventory (SQLite) and the DuckDB version: a
// source archive is only "in scope" for rule attribution when it currently
// contributes at least one visible session.
func (s *Store) GetProjectInventory(ctx context.Context, filter db.ProjectDateFilter) (db.ProjectInventory, error) {
	agg, err := s.projectInventoryAggregate(ctx, filter)
	if err != nil {
		return db.ProjectInventory{}, err
	}

	rawProjects := make([]string, 0, len(agg))
	for project := range agg {
		rawProjects = append(rawProjects, project)
	}
	mappings, eval, err := s.projectInventoryGovernance(ctx)
	if err != nil {
		return db.ProjectInventory{}, err
	}
	projects, err := s.BuildProjectIdentityMap(ctx, rawProjects)
	if err != nil {
		return db.ProjectInventory{}, err
	}
	rows, totalSessions := db.BuildProjectInventoryRows(agg, rawProjects, projects)
	db.AnnotateProjectInventoryRows(rows, mappings, eval, projects)

	return db.ProjectInventory{
		Projects:         rows,
		TotalProjects:    len(rows),
		TotalSessions:    totalSessions,
		GovernedSessions: eval.GovernedSessions,
	}, nil
}

// projectInventoryAggregate runs the one-pass aggregation over visible
// sessions, grouped by raw (unsanitized) project label. Identical in
// shape to the SQLite and DuckDB versions, including cross-platform cwd
// normalization; no source_archive_id filter, since visibility (unlike
// governedness) does not depend on provenance.
func (s *Store) projectInventoryAggregate(
	ctx context.Context,
	filter db.ProjectDateFilter,
) (map[string]db.ProjectInventoryAgg, error) {
	where, args := db.BuildSessionBaseFilterSQL(filter.SessionFilter(), db.ClickHouseQueryDialect())
	rows, err := s.queryContext(ctx, `
		SELECT project,
		       toInt64(COUNT(*)),
		       toInt64(COUNT(DISTINCT machine)),
		       toInt64(COUNT(DISTINCT agent)),
		       toInt64(COUNT(DISTINCT CASE WHEN cwd IS NOT NULL AND cwd != ''
		             THEN replaceAll(cwd, char(92), '/') END)),
		       MIN(started_at),
		       MAX(COALESCE(ended_at, started_at))
		FROM sessions
		WHERE `+where+`
		GROUP BY project
		ORDER BY project`, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregating clickhouse project inventory: %w", err)
	}
	defer rows.Close()

	out := map[string]db.ProjectInventoryAgg{}
	for rows.Next() {
		var project string
		var agg db.ProjectInventoryAgg
		var first, last any
		if err := rows.Scan(
			&project, &agg.Sessions, &agg.Machines, &agg.Agents,
			&agg.DistinctCwds, &first, &last,
		); err != nil {
			return nil, fmt.Errorf("scanning clickhouse project inventory row: %w", err)
		}
		agg.First, err = parseClickInventoryTime(first)
		if err != nil {
			return nil, fmt.Errorf("parsing clickhouse project inventory first activity: %w", err)
		}
		agg.Last, err = parseClickInventoryTime(last)
		if err != nil {
			return nil, fmt.Errorf("parsing clickhouse project inventory last activity: %w", err)
		}
		out[project] = agg
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse project inventory rows: %w", err)
	}
	return out, nil
}

// parseClickInventoryTime converts a scanned MIN/MAX(DateTime64) value into a
// *time.Time, treating NULL as "no bound".
func parseClickInventoryTime(v any) (*time.Time, error) {
	formatted := formatDBTime(v)
	if formatted == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, formatted)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// projectInventoryGovernance loads every worktree mapping rule (enabled and
// disabled) from source archives that currently contribute at least one
// visible session, then loads the candidate session rows and runs the
// shared governed-session evaluator across all in-scope archives at once.
func (s *Store) projectInventoryGovernance(
	ctx context.Context,
) ([]db.WorktreeProjectMapping, db.GovernedEvaluation, error) {
	visibleArchives, err := s.projectInventoryVisibleArchives(ctx)
	if err != nil {
		return nil, db.GovernedEvaluation{}, err
	}
	archiveMappings, rows, err := s.projectInventoryMappings(ctx, nil, visibleArchives)
	if err != nil {
		return nil, db.GovernedEvaluation{}, err
	}
	flatMappings := make([]db.WorktreeProjectMapping, len(rows))
	for i, row := range rows {
		flatMappings[i] = row.mapping
	}
	candidates, err := s.projectInventoryCandidateRows(ctx, nil)
	if err != nil {
		return nil, db.GovernedEvaluation{}, err
	}

	eval := db.EvaluateGovernedSessions(archiveMappings, candidates)
	return flatMappings, eval, nil
}

// projectInventoryVisibleArchives returns the set of source archive IDs
// that currently contribute at least one visible (non-deleted) session with
// known provenance. Only these archives' worktree-mapping rules count
// toward inventory attribution.
func (s *Store) projectInventoryVisibleArchives(
	ctx context.Context,
) (map[string]struct{}, error) {
	rows, err := s.queryContext(ctx, `
		SELECT DISTINCT source_archive_id
		FROM sessions
		WHERE deleted_at IS NULL AND source_archive_id != ''`)
	if err != nil {
		return nil, fmt.Errorf("listing clickhouse visible source archives: %w", err)
	}
	defer rows.Close()

	out := map[string]struct{}{}
	for rows.Next() {
		var archiveID string
		if err := rows.Scan(&archiveID); err != nil {
			return nil, fmt.Errorf("scanning clickhouse visible source archive: %w", err)
		}
		out[archiveID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse visible source archives: %w", err)
	}
	return out, nil
}

// projectMappingRow pairs one worktree mapping rule with the real source
// archive it was published from, since the mirror's rows -- unlike
// SQLite's single-archive worktree_project_mappings table -- span every
// archive pushed into this store.
type projectMappingRow struct {
	archiveID string
	mapping   db.WorktreeProjectMapping
}

// projectInventoryMappings loads every worktree-mapping rule, shared by both
// the project inventory (visibility-gated, every machine) and project rules
// (every archive, one exact machine) reads. machine distinguishes
// "unrestricted" from "restrict to this exact value": nil matches every
// machine (GetProjectInventory's call), a non-nil pointer -- including one
// pointing at "" -- restricts to that literal machine value via
// `WHERE machine = ...` (ListProjectRules's call, so an empty/whitespace
// machine argument correctly matches zero rows instead of silently falling
// back to "every machine," since no mapping row ever has an empty machine
// column). visibleArchives additionally drops any row whose source archive
// isn't in the set when non-nil; a nil map means no visibility restriction
// (every archive's rules are returned, as project rules needs). Rows are
// grouped by source archive for the evaluator and returned flattened, in
// query order, pairing each mapping with its real source archive for
// read-side callers that need per-row attribution.
func (s *Store) projectInventoryMappings(
	ctx context.Context, machine *string, visibleArchives map[string]struct{},
) ([]db.ArchiveMappings, []projectMappingRow, error) {
	var rows *sql.Rows
	var err error
	if machine == nil {
		rows, err = s.queryContext(ctx, `
			SELECT source_archive_id, machine, path_prefix, layout, project,
			       original_project, enabled, updated_at
			FROM source_worktree_project_mappings
			ORDER BY source_archive_id, machine, path_prefix`)
	} else {
		rows, err = s.queryContext(ctx, `
			SELECT source_archive_id, machine, path_prefix, layout, project,
			       original_project, enabled, updated_at
			FROM source_worktree_project_mappings
			WHERE machine = ?
			ORDER BY path_prefix, source_archive_id`, *machine)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("listing clickhouse worktree mappings: %w", err)
	}
	defer rows.Close()

	byArchive := map[string][]db.WorktreeProjectMapping{}
	var archiveOrder []string
	var flat []projectMappingRow
	for rows.Next() {
		var archiveID string
		var m db.WorktreeProjectMapping
		if err := rows.Scan(
			&archiveID, &m.Machine, &m.PathPrefix, &m.Layout, &m.Project,
			&m.OriginalProject, &m.Enabled, &m.UpdatedAt,
		); err != nil {
			return nil, nil, fmt.Errorf("scanning clickhouse worktree mapping: %w", err)
		}
		if visibleArchives != nil {
			if _, ok := visibleArchives[archiveID]; !ok {
				continue
			}
		}
		if _, seen := byArchive[archiveID]; !seen {
			archiveOrder = append(archiveOrder, archiveID)
		}
		byArchive[archiveID] = append(byArchive[archiveID], m)
		flat = append(flat, projectMappingRow{archiveID: archiveID, mapping: m})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterating clickhouse worktree mappings: %w", err)
	}

	archiveMappings := make([]db.ArchiveMappings, len(archiveOrder))
	for i, archiveID := range archiveOrder {
		archiveMappings[i] = db.ArchiveMappings{
			SourceArchiveID: archiveID,
			Mappings:        byArchive[archiveID],
		}
	}
	return archiveMappings, flat, nil
}

// projectInventoryCandidateRows returns the prefiltered session rows the
// evaluator needs, shared by both the project inventory and project rules
// reads: every visible session with known provenance whose (source archive,
// machine) has at least one enabled worktree mapping. machine follows the
// same nil-vs-pointer convention as projectInventoryMappings: nil matches
// every machine, a non-nil pointer -- including one pointing at "" --
// restricts to that literal machine value. Sessions with empty
// source_archive_id (lost or missing provenance) are never governed and are
// excluded here rather than "fixed" upstream.
func (s *Store) projectInventoryCandidateRows(
	ctx context.Context, machine *string,
) ([]db.MappingEvaluationRow, error) {
	var rows *sql.Rows
	var err error
	if machine == nil {
		rows, err = s.queryContext(ctx, `
			SELECT id, machine, project, cwd, COALESCE(file_path, ''),
				project_assigned, source_archive_id
			FROM sessions
			WHERE deleted_at IS NULL
			  AND source_archive_id != ''
			  AND (source_archive_id, machine) IN
			      (SELECT source_archive_id, machine
			       FROM source_worktree_project_mappings WHERE enabled)`)
	} else {
		rows, err = s.queryContext(ctx, `
			SELECT id, machine, project, cwd, COALESCE(file_path, ''),
				project_assigned, source_archive_id
			FROM sessions
			WHERE deleted_at IS NULL
			  AND source_archive_id != ''
			  AND machine = ?
			  AND (source_archive_id, machine) IN
			      (SELECT source_archive_id, machine
			       FROM source_worktree_project_mappings
			       WHERE enabled AND machine = ?)`, *machine, *machine)
	}
	if err != nil {
		return nil, fmt.Errorf(
			"querying clickhouse project inventory candidate sessions: %w", err)
	}
	defer rows.Close()

	var out []db.MappingEvaluationRow
	for rows.Next() {
		var row db.MappingEvaluationRow
		if err := rows.Scan(
			&row.SessionID, &row.Machine, &row.Project, &row.Cwd, &row.FilePath,
			&row.ProjectAssigned, &row.SourceArchiveID,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning clickhouse project inventory candidate session: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating clickhouse project inventory candidate sessions: %w", err)
	}
	return out, nil
}
