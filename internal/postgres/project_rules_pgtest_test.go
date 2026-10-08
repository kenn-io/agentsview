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
