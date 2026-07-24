package db

import "strings"

// MappingEvaluationRow is one prefiltered session row for governed-count
// evaluation. SourceArchiveID must be the row's real, non-empty source
// archive ID for mirror callers (PostgreSQL/DuckDB) — an ArchiveMappings
// entry with an empty SourceArchiveID is reserved for the SQLite-local
// caller, whose own rows legitimately carry empty provenance. A mirror row
// with empty SourceArchiveID (lost or missing provenance) will never match
// any archive's rule set and is never governed.
type MappingEvaluationRow struct {
	SessionID       string
	Machine         string
	Project         string
	Cwd             string
	FilePath        string
	SourceArchiveID string
}

// GovernedRuleKey identifies a rule across archives.
type GovernedRuleKey struct {
	SourceArchiveID string
	Machine         string
	PathPrefix      string
}

// ArchiveMappings is one source archive's rule set.
type ArchiveMappings struct {
	SourceArchiveID string
	Mappings        []WorktreeProjectMapping
}

// GovernedEvaluation is the result of evaluating a set of prefiltered
// session rows against their source archives' rule sets.
type GovernedEvaluation struct {
	// GovernedSessions is the count of distinct rows governed by a winning
	// enabled rule from the same source archive.
	GovernedSessions int
	// SessionsByRule counts, per winning rule, the rows it governs.
	SessionsByRule map[GovernedRuleKey]int
	// DynamicLabelRules maps a resolved project label to the set of
	// repo_dot_worktrees rules currently resolving >=1 row to it.
	DynamicLabelRules map[string]map[GovernedRuleKey]struct{}
}

// EvaluateGovernedSessions runs the production mapping evaluator over
// prefiltered rows. Disabled rules may be passed; they are filtered out
// internally by enabledMappingsByArchiveAndMachine before matching, so only
// enabled rules ever govern a row.
//
// It groups rows by source archive, then by machine, and runs the same
// pipeline the apply path uses: sibling match-cwd backfill keyed by file
// path, then longest-prefix resolution with layout validation. A row is
// governed when resolution succeeds, regardless of whether the resolved
// project differs from the stored one.
func EvaluateGovernedSessions(
	archives []ArchiveMappings, rows []MappingEvaluationRow,
) GovernedEvaluation {
	result := GovernedEvaluation{
		SessionsByRule:    map[GovernedRuleKey]int{},
		DynamicLabelRules: map[string]map[GovernedRuleKey]struct{}{},
	}
	mappingsByArchive := enabledMappingsByArchiveAndMachine(archives)

	type groupKey struct {
		archiveID string
		machine   string
	}
	groups := map[groupKey][]MappingEvaluationRow{}
	var order []groupKey
	for _, row := range rows {
		key := groupKey{archiveID: row.SourceArchiveID, machine: row.Machine}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], row)
	}

	for _, key := range order {
		mappings := mappingsByArchive[key.archiveID][key.machine]
		if len(mappings) == 0 {
			continue
		}
		evaluateGovernedGroup(key.archiveID, mappings, groups[key], &result)
	}
	return result
}

// enabledMappingsByArchiveAndMachine drops disabled rules and sorts each
// machine's remaining rules by descending path-prefix length, matching the
// production longest-prefix resolution order.
func enabledMappingsByArchiveAndMachine(
	archives []ArchiveMappings,
) map[string]map[string][]WorktreeProjectMapping {
	mappingsByArchive := make(map[string]map[string][]WorktreeProjectMapping, len(archives))
	for _, archive := range archives {
		byMachine := map[string][]WorktreeProjectMapping{}
		for _, m := range archive.Mappings {
			if !m.Enabled {
				continue
			}
			byMachine[m.Machine] = append(byMachine[m.Machine], m)
		}
		for machine := range byMachine {
			sortWorktreeProjectMappings(byMachine[machine])
		}
		mappingsByArchive[archive.SourceArchiveID] = byMachine
	}
	return mappingsByArchive
}

// evaluateGovernedGroup evaluates one (source archive, machine) group of
// rows against its sorted enabled rule set, recording governed counts and
// rule attribution into result.
func evaluateGovernedGroup(
	archiveID string,
	mappings []WorktreeProjectMapping,
	rows []MappingEvaluationRow,
	result *GovernedEvaluation,
) {
	sessionRows := make([]worktreeMappingSessionRow, len(rows))
	for i, row := range rows {
		sessionRows[i] = worktreeMappingSessionRow{
			id:       row.SessionID,
			machine:  row.Machine,
			project:  row.Project,
			cwd:      row.Cwd,
			filePath: row.FilePath,
		}
	}
	applyWorktreeMappingMatchCwdFromSiblings(sessionRows,
		func(row worktreeMappingSessionRow) string {
			return strings.TrimSpace(row.filePath)
		},
		func(row worktreeMappingSessionRow, cwd string) (string, bool) {
			return ResolveWorktreeProjectFromSortedMappings(mappings, cwd, row.project)
		},
	)

	for _, row := range sessionRows {
		recordGovernedRow(archiveID, mappings, row, result)
	}
}

// recordGovernedRow resolves one session row's winning rule (if any) and
// records its counts and label attribution into result.
func recordGovernedRow(
	archiveID string,
	mappings []WorktreeProjectMapping,
	row worktreeMappingSessionRow,
	result *GovernedEvaluation,
) {
	matchCwd := row.matchCwd
	if matchCwd == "" {
		matchCwd = row.cwd
	}
	winner, project, ok := winningWorktreeMapping(mappings, matchCwd, row.project)
	if !ok {
		return
	}
	result.GovernedSessions++
	ruleKey := GovernedRuleKey{
		SourceArchiveID: archiveID,
		Machine:         winner.Machine,
		PathPrefix:      winner.PathPrefix,
	}
	result.SessionsByRule[ruleKey]++
	if winner.Layout == WorktreeMappingLayoutRepoDotWorktrees {
		labels := result.DynamicLabelRules[project]
		if labels == nil {
			labels = map[GovernedRuleKey]struct{}{}
			result.DynamicLabelRules[project] = labels
		}
		labels[ruleKey] = struct{}{}
	}
}

// winningWorktreeMapping duplicates the resolution loop in
// ResolveWorktreeProjectFromSortedMappings because that function does not
// return which mapping won. The scan lives here rather than changing that
// function's signature, since its callers are load-bearing sync paths.
func winningWorktreeMapping(
	mappings []WorktreeProjectMapping, cwd string, currentProject string,
) (WorktreeProjectMapping, string, bool) {
	for _, mapping := range mappings {
		if !worktreePathMatches(mapping.PathPrefix, cwd) {
			continue
		}
		if project, ok := resolveWorktreeProjectFromMapping(mapping, cwd, currentProject); ok {
			return mapping, project, true
		}
	}
	return WorktreeProjectMapping{}, "", false
}
