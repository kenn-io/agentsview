//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// seedInventorySession inserts a session with the fields the project
// inventory aggregation cares about (project, machine, agent, cwd, activity
// bounds, optional file path), plus one message so the row looks like a
// normal pushed session.
func seedInventorySession(
	t *testing.T, localDB *db.DB, id, project string, configure func(*db.Session),
) {
	t.Helper()
	sess := db.Session{
		ID:           id,
		Project:      project,
		Machine:      "workstation",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}
	if configure != nil {
		configure(&sess)
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     id,
		Ordinal:       0,
		Role:          "assistant",
		Content:       "hello",
		ContentLength: 5,
	}}), "InsertMessages")
}

// buildInventoryFixture seeds the shared alpha/beta/gamma/misc aggregate
// fixture used by both PG inventory tests: duplicate and empty cwds, a
// trashed session excluded from every count, and three mapping rules that
// together exercise every branch of annotateProjectInventoryRows:
//
//   - an enabled explicit rule whose Project ("alpha") matches an existing
//     session label, governing alpha-1 (cwd matches the rule's path prefix;
//     alpha-2 is on a different machine and alpha-3 has no cwd) and setting
//     EnabledRulesTargeting on the "alpha" row;
//   - a disabled rule recording OriginalProject "beta", setting
//     RecordedAsOriginal on the "beta" row without contributing to
//     EnabledRulesTargeting (it's disabled);
//   - an enabled repo_dot_worktrees rule that dynamically resolves
//     gamma-dynamic's cwd (raw project "misc") to project "gamma", which
//     matches the real "gamma-1" session's label and sets
//     EnabledRulesTargeting on the "gamma" row via DynamicLabelRules.
func buildInventoryFixture(t *testing.T, localDB *db.DB, ctx context.Context) {
	t.Helper()
	seedInventorySession(t, localDB, "alpha-1", "alpha", func(s *db.Session) {
		s.Machine = "m1"
		s.Agent = "claude"
		s.Cwd = "/w/a"
		s.StartedAt = strPtr("2024-01-01T00:00:00Z")
		s.EndedAt = strPtr("2024-01-01T02:00:00Z")
	})
	seedInventorySession(t, localDB, "alpha-2", "alpha", func(s *db.Session) {
		s.Machine = "m2"
		s.Agent = "codex"
		s.Cwd = "/w/a"
		s.StartedAt = strPtr("2024-01-05T00:00:00Z")
	})
	seedInventorySession(t, localDB, "alpha-3", "alpha", func(s *db.Session) {
		s.Machine = "m1"
		s.Agent = "claude"
		s.Cwd = ""
		s.StartedAt = strPtr("2023-12-25T00:00:00Z")
		s.EndedAt = strPtr("2024-01-10T00:00:00Z")
	})
	seedInventorySession(t, localDB, "alpha-trashed", "alpha", func(s *db.Session) {
		s.Machine = "m9"
		s.Agent = "trashed-agent"
		s.Cwd = "/w/trash"
		s.StartedAt = strPtr("2020-01-01T00:00:00Z")
		s.EndedAt = strPtr("2020-01-02T00:00:00Z")
	})
	require.NoError(t, localDB.SoftDeleteSession("alpha-trashed"))

	seedInventorySession(t, localDB, "beta-1", "beta", func(s *db.Session) {
		s.Machine = "m3"
		s.Agent = "gemini"
		s.Cwd = "/w/b"
		s.StartedAt = strPtr("2024-02-01T00:00:00Z")
	})

	seedInventorySession(t, localDB, "gamma-1", "gamma", func(s *db.Session) {
		s.Machine = "m4"
		s.Agent = "claude"
		s.Cwd = "/w/g"
		s.StartedAt = strPtr("2024-01-03T00:00:00Z")
	})
	repoRoot := t.TempDir()
	seedInventorySession(t, localDB, "gamma-dynamic", "misc", func(s *db.Session) {
		s.Machine = "dyn-host"
		s.Agent = "claude"
		s.Cwd = repoRoot + "/gamma.worktrees/branch1"
		s.StartedAt = strPtr("2024-01-04T00:00:00Z")
	})

	_, err := localDB.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine:    "m1",
		PathPrefix: "/w/a",
		Layout:     db.WorktreeMappingLayoutExplicit,
		Project:    "alpha",
		Enabled:    true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping alpha")

	_, err = localDB.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine:         "disabled-host",
		PathPrefix:      "/unused/other",
		Layout:          db.WorktreeMappingLayoutExplicit,
		Project:         "beta",
		OriginalProject: "beta",
		Enabled:         false,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping beta original")

	_, err = localDB.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine:    "dyn-host",
		PathPrefix: repoRoot,
		Layout:     db.WorktreeMappingLayoutRepoDotWorktrees,
		Enabled:    true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping gamma dynamic")
}

// truncateInventoryRows truncates every activity timestamp in an inventory
// to second precision so PG (microsecond) and SQLite (millisecond, then
// text-round-tripped) timestamps compare equal.
func truncateInventoryRows(rows []db.ProjectInventoryRow) []db.ProjectInventoryRow {
	out := make([]db.ProjectInventoryRow, len(rows))
	for i, row := range rows {
		out[i] = row
		if row.FirstActivity != nil {
			truncated := row.FirstActivity.UTC().Truncate(time.Second)
			out[i].FirstActivity = &truncated
		}
		if row.LastActivity != nil {
			truncated := row.LastActivity.UTC().Truncate(time.Second)
			out[i].LastActivity = &truncated
		}
	}
	return out
}

// TestPGProjectInventoryMatchesSQLite verifies that pushing a local SQLite
// archive's sessions and worktree mappings into PG, then reading the
// inventory back from both sides, produces the same aggregate/annotation
// result. It exercises the real replicated shape (push, not hand-inserted
// mirror rows) so provenance columns and mapping mirroring are covered too.
func TestPGProjectInventoryMatchesSQLite(t *testing.T) {
	const schema = "agentsview_project_inventory_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)

	buildInventoryFixture(t, localDB, ctx)

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	localInv, err := localDB.GetProjectInventory(ctx)
	require.NoError(t, err, "local GetProjectInventory")

	pgStore := &Store{pg: pg}
	pgInv, err := pgStore.GetProjectInventory(ctx)
	require.NoError(t, err, "pg GetProjectInventory")

	assert.Equal(t, localInv.TotalProjects, pgInv.TotalProjects)
	assert.Equal(t, localInv.TotalSessions, pgInv.TotalSessions)
	assert.Equal(t, localInv.GovernedSessions, pgInv.GovernedSessions)
	require.Equal(t, len(localInv.Projects), len(pgInv.Projects))
	assert.Equal(t,
		truncateInventoryRows(localInv.Projects),
		truncateInventoryRows(pgInv.Projects),
	)

	require.Len(t, pgInv.Projects, 4)
	assert.Equal(t, "alpha", pgInv.Projects[0].Label)
	assert.Equal(t, "beta", pgInv.Projects[1].Label)
	assert.Equal(t, "gamma", pgInv.Projects[2].Label)
	assert.Equal(t, "misc", pgInv.Projects[3].Label)
	assert.Equal(t, 3, pgInv.Projects[0].Sessions, "trashed session excluded")
	assert.Equal(t, 2, pgInv.GovernedSessions,
		"alpha-1 via the explicit rule, gamma-dynamic via the dynamic rule")

	assert.Equal(t, 1, pgInv.Projects[0].EnabledRulesTargeting,
		"explicit rule statically targets the alpha row by its own Project field")
	assert.False(t, pgInv.Projects[0].RecordedAsOriginal)

	assert.True(t, pgInv.Projects[1].RecordedAsOriginal,
		"disabled rule's original_project recorded even though it's disabled")
	assert.Equal(t, 0, pgInv.Projects[1].EnabledRulesTargeting,
		"disabled rule must not contribute enabled attribution")

	assert.Equal(t, 1, pgInv.Projects[2].EnabledRulesTargeting,
		"dynamic repo_dot_worktrees rule resolves gamma-dynamic's cwd to gamma")
	assert.False(t, pgInv.Projects[2].RecordedAsOriginal)

	assert.Equal(t, 0, pgInv.Projects[3].EnabledRulesTargeting,
		"misc has no rule targeting it by raw label, only gamma is resolved to")
}

// TestPGProjectInventoryIgnoresUnattributedSessions verifies that a session
// with blanked source_archive_id (lost provenance) is dropped from the
// governed count but stays visible in every aggregate count: provenance
// only gates governedness, not visibility.
func TestPGProjectInventoryIgnoresUnattributedSessions(t *testing.T) {
	const schema = "agentsview_project_inventory_provenance_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)

	buildInventoryFixture(t, localDB, ctx)

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	pgStore := &Store{pg: pg}
	before, err := pgStore.GetProjectInventory(ctx)
	require.NoError(t, err, "GetProjectInventory before")
	require.Equal(t, 2, before.GovernedSessions,
		"alpha-1 and gamma-dynamic governed before provenance is cleared")

	_, err = pg.ExecContext(ctx,
		`UPDATE sessions SET source_archive_id = '' WHERE id = 'alpha-1'`)
	require.NoError(t, err, "clear provenance")

	after, err := pgStore.GetProjectInventory(ctx)
	require.NoError(t, err, "GetProjectInventory after")

	assert.Equal(t, before.GovernedSessions-1, after.GovernedSessions,
		"unattributed session drops out of the governed count")
	assert.Equal(t, before.TotalSessions, after.TotalSessions,
		"aggregate visibility is unaffected by provenance")
	assert.Equal(t, before.TotalProjects, after.TotalProjects)

	var beforeAlpha, afterAlpha db.ProjectInventoryRow
	for _, row := range before.Projects {
		if row.Label == "alpha" {
			beforeAlpha = row
		}
	}
	for _, row := range after.Projects {
		if row.Label == "alpha" {
			afterAlpha = row
		}
	}
	assert.Equal(t, beforeAlpha.Sessions, afterAlpha.Sessions,
		"alpha's session count is unchanged")
}

// TestPGProjectInventoryCrossArchiveIsolation verifies that inventory
// governance is scoped per source archive when a PG mirror serves more than
// one archive. It pushes one archive through the real sync path (archive A)
// and hand-inserts a second archive's mapping and session rows directly
// into PG (archive B), following the pattern in
// TestPGProjectIdentityAggregatesSourceArchives. Archive B's rule shares
// both machine name and path prefix with a session that belongs to archive
// A, so a broken (source_archive_id, machine) scope -- either in the
// candidate-row SQL or in how projectInventoryMappings groups rules by
// archive -- would incorrectly let archive B's rule govern archive A's
// session. Archive A carries its own enabled mapping on the shared machine
// whose path prefix matches nothing: it admits archive A's session into
// the candidate rows, so per-archive rule evaluation -- not the candidate
// prefilter -- is what must keep archive B's identical-path rule from
// governing it. A third archive (C) contributes a session on the same
// shared machine but has NO mapping of its own: it must be absent from the
// candidate rows, proving the prefilter matches on the full
// (source_archive_id, machine) tuple rather than machine alone.
func TestPGProjectInventoryCrossArchiveIsolation(t *testing.T) {
	const schema = "agentsview_project_inventory_multi_archive_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)

	// Archive A (the local push archive) has an enabled mapping on
	// "shared-machine" whose path prefix matches none of its sessions.
	seedInventorySession(t, localDB, "a-session", "proj-a", func(s *db.Session) {
		s.Machine = "shared-machine"
		s.Cwd = "/repos/shared"
		s.StartedAt = strPtr("2024-01-01T00:00:00Z")
	})
	_, err := localDB.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine:    "shared-machine",
		PathPrefix: "/repos/matches-nothing",
		Layout:     db.WorktreeMappingLayoutExplicit,
		Project:    "proj-a-unused",
		Enabled:    true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping archive A decoy")
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	// Archive B: hand-inserted mirror rows for a second source archive,
	// reusing the same machine name and path prefix as archive A's
	// session, plus its own governed session.
	const archiveB = "archive-b"
	_, err = pg.ExecContext(ctx, `
		INSERT INTO source_worktree_project_mappings
		(source_archive_id, machine, path_prefix, layout, project,
		 original_project, enabled, updated_at)
		VALUES ($1, 'shared-machine', '/repos/shared', 'explicit', 'proj-b',
		 '', TRUE, '')`, archiveB)
	require.NoError(t, err, "seed archive B mapping")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions
		(id, machine, project, agent, cwd, started_at, source_archive_id)
		VALUES ('b-session', 'shared-machine', 'proj-b', 'claude',
		 '/repos/shared', '2024-03-01T00:00:00Z', $1)`, archiveB)
	require.NoError(t, err, "seed archive B session")

	// Archive C: a session on the same shared machine with NO mapping of
	// its own. Archives A and B both have enabled mappings on
	// "shared-machine", so a candidate prefilter that matched on machine
	// alone -- ignoring source_archive_id -- would admit this session.
	const archiveC = "archive-c"
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions
		(id, machine, project, agent, cwd, started_at, source_archive_id)
		VALUES ('c-session', 'shared-machine', 'proj-c', 'claude',
		 '/repos/shared', '2024-03-02T00:00:00Z', $1)`, archiveC)
	require.NoError(t, err, "seed archive C session")

	pgStore := &Store{pg: pg}
	inv, err := pgStore.GetProjectInventory(ctx)
	require.NoError(t, err, "GetProjectInventory")

	byLabel := map[string]db.ProjectInventoryRow{}
	for _, row := range inv.Projects {
		byLabel[row.Label] = row
	}
	require.Contains(t, byLabel, "proj-a")
	require.Contains(t, byLabel, "proj-b")

	assert.Equal(t, 1, inv.GovernedSessions,
		"only archive B's own session is governed by archive B's rule; "+
			"archive A's own rule matches nothing, so its session must not "+
			"be governed by archive B's identical-path rule")
	assert.Equal(t, 0, byLabel["proj-a"].EnabledRulesTargeting,
		"archive B's rule must not statically attribute to archive A's project")
	assert.Equal(t, 1, byLabel["proj-b"].EnabledRulesTargeting,
		"archive B's own rule attributes correctly to its own project")

	// Archive A's own enabled mapping admits its session into the candidate
	// rows, so the governed count above proves per-archive rule evaluation
	// excluded it -- not the candidate prefilter.
	candidates, err := pgStore.projectInventoryCandidateRows(ctx, nil)
	require.NoError(t, err, "projectInventoryCandidateRows")
	var candidateIDs []string
	for _, c := range candidates {
		candidateIDs = append(candidateIDs, c.SessionID)
	}
	assert.Contains(t, candidateIDs, "a-session",
		"archive A's own enabled mapping must admit its session as a "+
			"candidate; only rule evaluation may reject it")
	assert.Contains(t, candidateIDs, "b-session",
		"archive B's own session is a legitimate candidate under its own "+
			"enabled mapping")
	assert.NotContains(t, candidateIDs, "c-session",
		"archive C has no enabled mapping, so its session must not be a "+
			"candidate; a machine-only prefilter that ignores "+
			"source_archive_id would admit it via archive A's or B's "+
			"shared-machine mappings")
}
