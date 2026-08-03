//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// projectRulesByPrefix indexes rules by path prefix for pairwise comparison
// between the local (SQLite) and mirror (DuckDB) sides of a differential
// test; both fixtures below use a distinct path prefix per rule, so this is
// unambiguous.
func projectRulesByPrefix(rules []db.ProjectRule) map[string]db.ProjectRule {
	out := map[string]db.ProjectRule{}
	for _, r := range rules {
		out[r.PathPrefix] = r
	}
	return out
}

// TestDuckProjectRulesMatchesSQLite verifies that pushing a local SQLite
// archive's sessions and worktree mappings into DuckDB, then reading the
// rules list back from both sides for the same machine, produces the same
// rule set and governed counts, with the mirror's SourceArchiveID equal to
// the pushing archive's id. It also exercises the disabled-rule-included
// and machine-filter contracts: a disabled rule appears with a zero
// governed count, and a rule for a different machine is excluded from the
// rules list but its machine still appears in the typeahead list.
func TestDuckProjectRulesMatchesSQLite(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)

	seedInventorySession(t, local, "alpha-1", "alpha", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.Cwd = "/w/a"
	})
	seedInventorySession(t, local, "alpha-2", "alpha", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.Cwd = "/w/other"
	})
	seedInventorySession(t, local, "beta-1", "beta", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.Cwd = "/w/b"
	})

	_, err := local.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: duckPushMachine, PathPrefix: "/w/a",
		Layout: db.WorktreeMappingLayoutExplicit, Project: "alpha", Enabled: true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping alpha")
	_, err = local.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: duckPushMachine, PathPrefix: "/w/b",
		Layout: db.WorktreeMappingLayoutExplicit, Project: "beta",
		OriginalProject: "beta", Enabled: false,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping beta")
	_, err = local.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: "m-other", PathPrefix: "/w/c",
		Layout: db.WorktreeMappingLayoutExplicit, Project: "gamma", Enabled: true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping gamma")

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	pushDataReadMirror(t, ctx, syncer)

	localRules, err := local.ListProjectRules(ctx, duckPushMachine)
	require.NoError(t, err, "local ListProjectRules")

	duckStore := NewStoreFromDB(syncer.DB())
	duckRules, err := duckStore.ListProjectRules(ctx, duckPushMachine)
	require.NoError(t, err, "duckdb ListProjectRules")

	assert.ElementsMatch(t, localRules.Machines, duckRules.Machines,
		"machine typeahead list is the same set regardless of the machine filter")
	assert.Contains(t, duckRules.Machines, "m-other",
		"machines list includes a machine that only has a mapping, no live session")

	require.Len(t, duckRules.Rules, 2, "only duckPushMachine's rules are returned")
	require.Len(t, localRules.Rules, 2)

	localByPrefix := projectRulesByPrefix(localRules.Rules)
	duckByPrefix := projectRulesByPrefix(duckRules.Rules)
	for prefix, localRule := range localByPrefix {
		duckRule, ok := duckByPrefix[prefix]
		require.True(t, ok, "prefix %s present on both sides", prefix)
		assert.Equal(t, localRule.Machine, duckRule.Machine)
		assert.Equal(t, localRule.Layout, duckRule.Layout)
		assert.Equal(t, localRule.Project, duckRule.Project)
		assert.Equal(t, localRule.OriginalProject, duckRule.OriginalProject)
		assert.Equal(t, localRule.Enabled, duckRule.Enabled)
		assert.Equal(t, localRule.GovernedSessions, duckRule.GovernedSessions)
		assert.Equal(t, localRule.SourceArchiveID, duckRule.SourceArchiveID,
			"duckdb rule's source_archive_id must equal the pushing archive's id")
	}

	assert.Equal(t, 1, duckByPrefix["/w/a"].GovernedSessions,
		"alpha-1 governed via the enabled rule; alpha-2's cwd doesn't match")
	assert.Equal(t, 0, duckByPrefix["/w/b"].GovernedSessions,
		"disabled rule never governs sessions, even though its cwd matches")
}

// TestDuckProjectRulesEmptyMachineMatchesSQLite verifies that
// ListProjectRules with an empty (or whitespace-only) machine argument
// returns zero rules on both SQLite and DuckDB, not "every machine's
// rules." SQLite's ListWorktreeProjectMappings filters with a literal
// `WHERE machine = ?` bound to "" (confirmed directly: no
// normalizeWorktreeMapping-created mapping row ever has an empty machine
// column, so that query always returns nothing); the DuckDB mirror's shared
// projectInventoryMappings/projectInventoryCandidateRows helpers must
// reject the temptation to treat machine == "" as a magic "unrestricted"
// sentinel, since that value is a completely ordinary (if never matched)
// machine value here, and doing so previously leaked every archive's rules
// for every machine into an empty-machine request. The machine typeahead
// list is unaffected either way, per ListProjectRules's contract.
func TestDuckProjectRulesEmptyMachineMatchesSQLite(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)

	seedInventorySession(t, local, "alpha-1", "alpha", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.Cwd = "/w/a"
	})
	_, err := local.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: duckPushMachine, PathPrefix: "/w/a",
		Layout: db.WorktreeMappingLayoutExplicit, Project: "alpha", Enabled: true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping alpha")

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	pushDataReadMirror(t, ctx, syncer)

	localRules, err := local.ListProjectRules(ctx, "")
	require.NoError(t, err, "local ListProjectRules")
	require.Empty(t, localRules.Rules,
		"sanity: SQLite's own empty-machine contract returns zero rules")
	require.NotEmpty(t, localRules.Machines,
		"sanity: SQLite's machine list stays populated regardless of the filter")

	duckStore := NewStoreFromDB(syncer.DB())
	duckRules, err := duckStore.ListProjectRules(ctx, "")
	require.NoError(t, err, "duckdb ListProjectRules")

	assert.Empty(t, duckRules.Rules,
		"an empty machine argument must match zero rules, not every archive's "+
			"rules for every machine")
	assert.ElementsMatch(t, localRules.Machines, duckRules.Machines,
		"machine typeahead list is unaffected by the empty machine filter")

	whitespaceRules, err := duckStore.ListProjectRules(ctx, "   ")
	require.NoError(t, err, "duckdb ListProjectRules whitespace")
	assert.Empty(t, whitespaceRules.Rules,
		"a whitespace-only machine argument trims to empty and must also "+
			"match zero rules")
}

// TestDuckProjectRulesCrossArchiveIsolation verifies that rule governance is
// scoped per source archive when a DuckDB mirror serves more than one
// archive, and that rules from every archive sharing (machine, path_prefix)
// are all returned. It pushes one archive through the real sync path
// (archive A) and hand-inserts a second archive's mapping and session rows
// directly into the DuckDB mirror (archive B), following the pattern in
// TestDuckProjectInventoryCrossArchiveIsolation. Archive A's own session
// deliberately does not match archive A's own rule's path prefix (governed
// count 0), while archive B's session does match archive B's rule (governed
// count 1): a broken SourceArchiveID key in the per-rule count map -- for
// example, keying only on (machine, path_prefix) -- would let archive B's
// governed session leak into archive A's count too, producing 1/1 instead
// of the expected 0/1.
func TestDuckProjectRulesCrossArchiveIsolation(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)

	seedInventorySession(t, local, "a-session", "proja", func(s *db.Session) {
		s.Cwd = "/repos/other"
	})
	_, err := local.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: duckPushMachine, PathPrefix: "/repos/shared",
		Layout: db.WorktreeMappingLayoutExplicit, Project: "proja", Enabled: true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping proja")

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	pushDataReadMirror(t, ctx, syncer)

	const archiveB = "archive-b"
	_, err = syncer.DB().ExecContext(ctx, `
		INSERT INTO source_worktree_project_mappings
		(source_archive_id, machine, path_prefix, layout, project,
		 original_project, enabled, updated_at)
		VALUES (?, ?, '/repos/shared', 'explicit', 'projb',
		 '', TRUE, '')`, archiveB, duckPushMachine)
	require.NoError(t, err, "seed archive B mapping")
	_, err = syncer.DB().ExecContext(ctx, `
		INSERT INTO sessions
		(id, machine, project, agent, message_count, user_message_count,
		 relationship_type, cwd, started_at, created_at, source_archive_id)
		VALUES ('b-session', ?, 'projb', 'claude', 1, 1,
		 'root', '/repos/shared', CAST(? AS TIMESTAMP), CAST(? AS TIMESTAMP), ?)`,
		duckPushMachine, "2024-03-01T00:00:00Z", "2024-03-01T00:00:00Z", archiveB)
	require.NoError(t, err, "seed archive B session")

	localArchiveID, err := local.GetArchiveID(ctx)
	require.NoError(t, err, "GetArchiveID")

	duckStore := NewStoreFromDB(syncer.DB())
	rules, err := duckStore.ListProjectRules(ctx, duckPushMachine)
	require.NoError(t, err, "ListProjectRules")

	require.Len(t, rules.Rules, 2,
		"both archives' rules for duckPushMachine/repos/shared are returned")

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
