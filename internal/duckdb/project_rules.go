package duckdb

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// ListProjectRules lists every worktree mapping rule for machine, enabled
// and disabled, across every source archive mirrored into this DuckDB
// store, each annotated with its current governed-session count. It
// mirrors internal/db.ListProjectRules and internal/postgres's PG version
// with the same multi-archive shape: unlike SQLite (one archive, so one
// rule per (machine, path_prefix)), two archives can each have their own
// rule for the same (machine, path_prefix) pair, and both appear here with
// their own SourceArchiveID and their own governed count, keyed by
// db.GovernedRuleKey{SourceArchiveID, Machine, PathPrefix}. Rows are
// ordered by (path_prefix, source_archive_id); machine is constant across
// the result set since it is the filter. The machine list keeps the
// typeahead contract of internal/db.ListProjectRules: every machine with a
// live session or a stored mapping, across every archive, regardless of
// the machine argument.
//
// The mapping load and candidate-row query are shared with
// GetProjectInventory (project_inventory.go's projectInventoryMappings and
// projectInventoryCandidateRows): this read passes a non-nil pointer to its
// (possibly empty) machine filter and no visibility restriction (nil),
// while the inventory read passes nil (unrestricted) and a visibility-scoped
// archive set. Passing a non-nil pointer even when machine is empty matters:
// it makes an empty/whitespace machine argument match zero rows (the same
// as SQLite's contract, which filters on the machine column equaling an
// empty string), rather than silently falling back to "every machine."
func (s *Store) ListProjectRules(ctx context.Context, machine string) (db.ProjectRules, error) {
	machine = strings.TrimSpace(machine)

	machines, err := s.projectRulesMachines(ctx)
	if err != nil {
		return db.ProjectRules{}, fmt.Errorf("listing duckdb project rule machines: %w", err)
	}

	archiveMappings, rows, err := s.projectInventoryMappings(ctx, &machine, nil)
	if err != nil {
		return db.ProjectRules{}, err
	}

	sessionsByRule, err := s.projectRulesGovernedCounts(ctx, machine, archiveMappings)
	if err != nil {
		return db.ProjectRules{}, err
	}

	rules := make([]db.ProjectRule, len(rows))
	for i, row := range rows {
		rules[i] = db.ProjectRule{
			WorktreeProjectMapping: row.mapping,
			SourceArchiveID:        row.archiveID,
			GovernedSessions: sessionsByRule[db.GovernedRuleKey{
				SourceArchiveID: row.archiveID,
				Machine:         row.mapping.Machine,
				PathPrefix:      row.mapping.PathPrefix,
			}],
		}
	}

	return db.ProjectRules{Machine: machine, Machines: machines, Rules: rules}, nil
}

// projectRulesMachines returns every distinct machine with a live session or
// a stored mapping, across every source archive mirrored into this store.
// Unfiltered by design: it feeds the typeahead list, not the rules list.
func (s *Store) projectRulesMachines(ctx context.Context) ([]string, error) {
	rows, err := s.queryContext(ctx, `
		SELECT machine FROM sessions WHERE deleted_at IS NULL AND machine != ''
		UNION
		SELECT machine FROM source_worktree_project_mappings WHERE machine != ''
		ORDER BY machine`)
	if err != nil {
		return nil, fmt.Errorf("listing duckdb project rule machines: %w", err)
	}
	defer rows.Close()

	machines := []string{}
	for rows.Next() {
		var machine string
		if err := rows.Scan(&machine); err != nil {
			return nil, fmt.Errorf("scanning duckdb project rule machine: %w", err)
		}
		machines = append(machines, machine)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb project rule machines: %w", err)
	}
	return machines, nil
}

// projectRulesGovernedCounts evaluates machine's enabled rules, across every
// archive that has one, against their own archive's visible candidate
// sessions on machine, and returns the resulting per-rule governed counts.
func (s *Store) projectRulesGovernedCounts(
	ctx context.Context, machine string, archiveMappings []db.ArchiveMappings,
) (map[db.GovernedRuleKey]int, error) {
	hasEnabled := false
	for _, am := range archiveMappings {
		for _, m := range am.Mappings {
			if m.Enabled {
				hasEnabled = true
				break
			}
		}
	}
	if !hasEnabled {
		return nil, nil
	}

	candidates, err := s.projectInventoryCandidateRows(ctx, &machine)
	if err != nil {
		return nil, err
	}

	eval := db.EvaluateGovernedSessions(archiveMappings, candidates)
	return eval.SessionsByRule, nil
}
