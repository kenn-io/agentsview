package db

import (
	"context"
	"fmt"
	"strings"
)

// ProjectRule is one worktree mapping rule plus its provenance and current
// governed-session count. Rule identity across archives is
// (SourceArchiveID, Machine, PathPrefix), not the embedded
// WorktreeProjectMapping's ID/CreatedAt: those fields are local-archive-only
// conveniences populated from the SQLite rules table. When this type is
// served from a PostgreSQL or DuckDB mirror, the mirror tables carry neither
// column, so ID is zero and CreatedAt is empty — mirrors are read-only, so
// id-based mutations (enable/disable/delete) never apply there anyway.
type ProjectRule struct {
	WorktreeProjectMapping
	SourceArchiveID  string `json:"source_archive_id"`
	GovernedSessions int    `json:"governed_sessions"`
}

// ProjectRules is the full rules read for one machine: every rule for that
// machine (enabled and disabled) plus the machine typeahead list.
type ProjectRules struct {
	Machine  string        `json:"machine"`
	Machines []string      `json:"machines"`
	Rules    []ProjectRule `json:"rules"`
}

// ListProjectRules lists every worktree mapping rule for machine, enabled
// and disabled, each annotated with its current governed-session count.
// Disabled rules always report zero governed sessions since only enabled
// rules enter the evaluator. The machine list retains the typeahead
// contract of worktreeMappingsResponse.Machines: every machine with a live
// session, unioned with every machine that has a stored mapping, regardless
// of the machine argument.
func (db *DB) ListProjectRules(ctx context.Context, machine string) (ProjectRules, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	machine = strings.TrimSpace(machine)

	machines, err := db.ListWorktreeProjectMappingMachines(ctx)
	if err != nil {
		return ProjectRules{}, fmt.Errorf("listing project rule machines: %w", err)
	}

	mappings, err := db.ListWorktreeProjectMappings(ctx, machine)
	if err != nil {
		return ProjectRules{}, fmt.Errorf("listing project rules: %w", err)
	}

	archiveID, err := db.GetArchiveID(ctx)
	if err != nil {
		return ProjectRules{}, fmt.Errorf("resolving archive id for project rules: %w", err)
	}

	sessionsByRule, err := db.projectRulesGovernedCounts(ctx, archiveID, machine, mappings)
	if err != nil {
		return ProjectRules{}, err
	}

	rules := make([]ProjectRule, len(mappings))
	for i, m := range mappings {
		rules[i] = ProjectRule{
			WorktreeProjectMapping: m,
			SourceArchiveID:        archiveID,
			GovernedSessions: sessionsByRule[GovernedRuleKey{
				SourceArchiveID: archiveID,
				Machine:         m.Machine,
				PathPrefix:      m.PathPrefix,
			}],
		}
	}

	return ProjectRules{
		Machine:  machine,
		Machines: machines,
		Rules:    rules,
	}, nil
}

// projectRulesGovernedCounts evaluates the machine's enabled rules against
// its visible sessions and returns the resulting per-rule governed counts.
// It reuses projectInventoryCandidateRows (project_inventory.go) for the
// candidate-row assembly rather than duplicating that query.
func (db *DB) projectRulesGovernedCounts(
	ctx context.Context, archiveID, machine string, mappings []WorktreeProjectMapping,
) (map[GovernedRuleKey]int, error) {
	hasEnabled := false
	for _, m := range mappings {
		if m.Enabled {
			hasEnabled = true
			break
		}
	}
	if !hasEnabled {
		return nil, nil
	}

	candidates, err := db.projectInventoryCandidateRows(
		ctx, archiveID, map[string]struct{}{machine: {}},
	)
	if err != nil {
		return nil, err
	}

	eval := EvaluateGovernedSessions(
		[]ArchiveMappings{{SourceArchiveID: archiveID, Mappings: mappings}},
		candidates,
	)
	return eval.SessionsByRule, nil
}
