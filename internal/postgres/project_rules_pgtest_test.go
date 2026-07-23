//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// projectRulesByPrefix indexes rules by path prefix for pairwise comparison
// between the local (SQLite) and mirror (PG) sides of a differential test;
// both fixtures below use a distinct path prefix per rule, so this is
// unambiguous.
func projectRulesByPrefix(rules []db.ProjectRule) map[string]db.ProjectRule {
	out := map[string]db.ProjectRule{}
	for _, r := range rules {
		out[r.PathPrefix] = r
	}
	return out
}

// TestPGProjectRulesMatchesSQLite verifies that pushing a local SQLite
// archive's sessions and worktree mappings into PG, then reading the rules
// list back from both sides for the same machine, produces the same rule
// set and governed counts, with the mirror's SourceArchiveID equal to the
// pushing archive's id. It also exercises the disabled-rule-included and
// machine-filter contracts: a disabled rule appears with a zero governed
// count, and a rule for a different machine is excluded from the rules
// list but its machine still appears in the typeahead list.
func TestPGProjectRulesMatchesSQLite(t *testing.T) {
	const schema = "agentsview_project_rules_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)

	seedInventorySession(t, localDB, "alpha-1", "alpha", func(s *db.Session) {
		s.Machine = "m1"
		s.Cwd = "/w/a"
	})
	seedInventorySession(t, localDB, "alpha-2", "alpha", func(s *db.Session) {
		s.Machine = "m1"
		s.Cwd = "/w/other"
	})
	seedInventorySession(t, localDB, "beta-1", "beta", func(s *db.Session) {
		s.Machine = "m1"
		s.Cwd = "/w/b"
	})

	_, err := localDB.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: "m1", PathPrefix: "/w/a",
		Layout: db.WorktreeMappingLayoutExplicit, Project: "alpha", Enabled: true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping alpha")
	_, err = localDB.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: "m1", PathPrefix: "/w/b",
		Layout: db.WorktreeMappingLayoutExplicit, Project: "beta",
		OriginalProject: "beta", Enabled: false,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping beta")
	_, err = localDB.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: "m2", PathPrefix: "/w/c",
		Layout: db.WorktreeMappingLayoutExplicit, Project: "gamma", Enabled: true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping gamma")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	localRules, err := localDB.ListProjectRules(ctx, "m1")
	require.NoError(t, err, "local ListProjectRules")

	pgStore := &Store{pg: pg}
	pgRules, err := pgStore.ListProjectRules(ctx, "m1")
	require.NoError(t, err, "pg ListProjectRules")

	assert.ElementsMatch(t, localRules.Machines, pgRules.Machines,
		"machine typeahead list is the same set regardless of the machine filter")
	assert.Contains(t, pgRules.Machines, "m2",
		"machines list includes a machine that only has a mapping, no live session")

	require.Len(t, pgRules.Rules, 2, "only m1's rules are returned")
	require.Len(t, localRules.Rules, 2)

	localByPrefix := projectRulesByPrefix(localRules.Rules)
	pgByPrefix := projectRulesByPrefix(pgRules.Rules)
	for prefix, localRule := range localByPrefix {
		pgRule, ok := pgByPrefix[prefix]
		require.True(t, ok, "prefix %s present on both sides", prefix)
		assert.Equal(t, localRule.Machine, pgRule.Machine)
		assert.Equal(t, localRule.Layout, pgRule.Layout)
		assert.Equal(t, localRule.Project, pgRule.Project)
		assert.Equal(t, localRule.OriginalProject, pgRule.OriginalProject)
		assert.Equal(t, localRule.Enabled, pgRule.Enabled)
		assert.Equal(t, localRule.GovernedSessions, pgRule.GovernedSessions)
		assert.Equal(t, localRule.SourceArchiveID, pgRule.SourceArchiveID,
			"pg rule's source_archive_id must equal the pushing archive's id")
	}

	assert.Equal(t, 1, pgByPrefix["/w/a"].GovernedSessions,
		"alpha-1 governed via the enabled rule; alpha-2's cwd doesn't match")
	assert.Equal(t, 0, pgByPrefix["/w/b"].GovernedSessions,
		"disabled rule never governs sessions, even though its cwd matches")
}

// TestPGProjectRulesEmptyMachineMatchesSQLite verifies that ListProjectRules
// with an empty (or whitespace-only) machine argument returns zero rules on
// both SQLite and PG, not "every machine's rules." SQLite's
// ListWorktreeProjectMappings filters with a literal `WHERE machine = ?`
// bound to "" (confirmed directly: no normalizeWorktreeMapping-created
// mapping row ever has an empty machine column, so that query always
// returns nothing); the PG mirror's shared projectInventoryMappings/
// projectInventoryCandidateRows helpers must reject the temptation to treat
// machine == "" as a magic "unrestricted" sentinel, since that value is a
// completely ordinary (if never matched) machine value here, and doing so
// previously leaked every archive's rules for every machine into an
// empty-machine request. The machine typeahead list is unaffected either
// way, per ListProjectRules's contract.
func TestPGProjectRulesEmptyMachineMatchesSQLite(t *testing.T) {
	const schema = "agentsview_project_rules_empty_machine_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)

	seedInventorySession(t, localDB, "alpha-1", "alpha", func(s *db.Session) {
		s.Machine = "m1"
		s.Cwd = "/w/a"
	})
	_, err := localDB.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: "m1", PathPrefix: "/w/a",
		Layout: db.WorktreeMappingLayoutExplicit, Project: "alpha", Enabled: true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping alpha")
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	localRules, err := localDB.ListProjectRules(ctx, "")
	require.NoError(t, err, "local ListProjectRules")
	require.Empty(t, localRules.Rules,
		"sanity: SQLite's own empty-machine contract returns zero rules")
	require.NotEmpty(t, localRules.Machines,
		"sanity: SQLite's machine list stays populated regardless of the filter")

	pgStore := &Store{pg: pg}
	pgRules, err := pgStore.ListProjectRules(ctx, "")
	require.NoError(t, err, "pg ListProjectRules")

	assert.Empty(t, pgRules.Rules,
		"an empty machine argument must match zero rules, not every archive's "+
			"rules for every machine")
	assert.ElementsMatch(t, localRules.Machines, pgRules.Machines,
		"machine typeahead list is unaffected by the empty machine filter")

	whitespaceRules, err := pgStore.ListProjectRules(ctx, "   ")
	require.NoError(t, err, "pg ListProjectRules whitespace")
	assert.Empty(t, whitespaceRules.Rules,
		"a whitespace-only machine argument trims to empty and must also "+
			"match zero rules")
}

// TestPGProjectRulesCrossArchiveIsolation verifies that rule governance is
// scoped per source archive when a PG mirror serves more than one archive,
// and that rules from every archive sharing (machine, path_prefix) are all
// returned. It pushes one archive through the real sync path (archive A)
// and hand-inserts a second archive's mapping and session rows directly
// into PG (archive B), following the pattern in
// TestPGProjectInventoryCrossArchiveIsolation. Archive A's own session
// deliberately does not match archive A's own rule's path prefix (governed
// count 0), while archive B's session does match archive B's rule (governed
// count 1): a broken SourceArchiveID key in the per-rule count map -- for
// example, keying only on (machine, path_prefix) -- would let archive B's
// governed session leak into archive A's count too, producing 1/1 instead
// of the expected 0/1.
func TestPGProjectRulesCrossArchiveIsolation(t *testing.T) {
	const schema = "agentsview_project_rules_multi_archive_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)

	seedInventorySession(t, localDB, "a-session", "proja", func(s *db.Session) {
		s.Machine = "shared-machine"
		s.Cwd = "/repos/other"
	})
	_, err := localDB.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: "shared-machine", PathPrefix: "/repos/shared",
		Layout: db.WorktreeMappingLayoutExplicit, Project: "proja", Enabled: true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping proja")
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	const archiveB = "archive-b"
	_, err = pg.ExecContext(ctx, `
		INSERT INTO source_worktree_project_mappings
		(source_archive_id, machine, path_prefix, layout, project,
		 original_project, enabled, updated_at)
		VALUES ($1, 'shared-machine', '/repos/shared', 'explicit', 'projb',
		 '', TRUE, '')`, archiveB)
	require.NoError(t, err, "seed archive B mapping")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions
		(id, machine, project, agent, cwd, started_at, source_archive_id)
		VALUES ('b-session', 'shared-machine', 'projb', 'claude',
		 '/repos/shared', '2024-03-01T00:00:00Z', $1)`, archiveB)
	require.NoError(t, err, "seed archive B session")

	localArchiveID, err := localDB.GetArchiveID(ctx)
	require.NoError(t, err, "GetArchiveID")

	pgStore := &Store{pg: pg}
	rules, err := pgStore.ListProjectRules(ctx, "shared-machine")
	require.NoError(t, err, "ListProjectRules")

	require.Len(t, rules.Rules, 2,
		"both archives' rules for shared-machine/repos/shared are returned")

	byArchive := map[string]db.ProjectRule{}
	for _, r := range rules.Rules {
		byArchive[r.SourceArchiveID] = r
	}
	require.Contains(t, byArchive, localArchiveID)
	require.Contains(t, byArchive, archiveB)

	assert.Equal(t, "proja", byArchive[localArchiveID].Project)
	assert.Equal(t, "projb", byArchive[archiveB].Project)

	assert.Equal(t, 0, byArchive[localArchiveID].GovernedSessions,
		"archive A's own session doesn't match its own rule's cwd prefix")
	assert.Equal(t, 1, byArchive[archiveB].GovernedSessions,
		"archive B's own session is governed by archive B's own rule")
}
