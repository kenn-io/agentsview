//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// seedInventorySession inserts a session with the fields the project
// inventory aggregation cares about (project, machine, agent, cwd, activity
// bounds, optional file path), plus one message so the row looks like a
// normal synced session. Mirrors internal/postgres's helper of the same
// name.
func seedInventorySession(
	t *testing.T, local *db.DB, id, project string, configure func(*db.Session),
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
	require.NoError(t, local.UpsertSession(t.Context(), sess), "UpsertSession")
	require.NoError(t, local.InsertMessages(t.Context(), []db.Message{{
		SessionID:     id,
		Ordinal:       0,
		Role:          "assistant",
		Content:       "hello",
		ContentLength: 5,
	}}), "InsertMessages")
}

// duckPushMachine is the fallback machine name used by the DuckDB sync
// harness for sessions whose source machine is empty or the "local" sentinel.
const duckPushMachine = "test-machine"

// buildInventoryFixture seeds the shared alpha/beta/gamma/misc aggregate
// fixture used by the DuckDB inventory tests: distinct and empty cwds, a
// trashed session excluded from every count, and three mapping rules that
// together exercise every branch of annotateProjectInventoryRows. It
// mirrors internal/postgres's buildInventoryFixture in spirit. Every session
// and governing mapping happens to share duckPushMachine; multi-machine push
// behavior is covered separately below.
func buildInventoryFixture(t *testing.T, local *db.DB, ctx context.Context) {
	t.Helper()

	seedInventorySession(t, local, "alpha-1", "alpha", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.Agent = "claude"
		s.Cwd = "/w/a"
		s.StartedAt = new("2024-01-01T00:00:00Z")
		s.EndedAt = new("2024-01-01T02:00:00Z")
	})
	seedInventorySession(t, local, "alpha-2", "alpha", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.Agent = "codex"
		s.Cwd = "/w/other"
		s.StartedAt = new("2024-01-05T00:00:00Z")
	})
	seedInventorySession(t, local, "alpha-3", "alpha", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.Agent = "claude"
		s.Cwd = ""
		s.StartedAt = new("2023-12-25T00:00:00Z")
		s.EndedAt = new("2024-01-10T00:00:00Z")
	})
	seedInventorySession(t, local, "alpha-trashed", "alpha", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.Agent = "trashed-agent"
		s.Cwd = "/w/trash"
		s.StartedAt = new("2020-01-01T00:00:00Z")
		s.EndedAt = new("2020-01-02T00:00:00Z")
	})
	require.NoError(t, local.SoftDeleteSession(ctx, "alpha-trashed"))

	seedInventorySession(t, local, "beta-1", "beta", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.Agent = "gemini"
		s.Cwd = "/w/b"
		s.StartedAt = new("2024-02-01T00:00:00Z")
	})

	seedInventorySession(t, local, "gamma-1", "gamma", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.Agent = "claude"
		s.Cwd = "/w/g"
		s.StartedAt = new("2024-01-03T00:00:00Z")
	})
	repoRoot := t.TempDir()
	seedInventorySession(t, local, "gamma-dynamic", "misc", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.Agent = "claude"
		s.Cwd = repoRoot + "/gamma.worktrees/branch1"
		s.StartedAt = new("2024-01-04T00:00:00Z")
	})

	_, err := local.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine:    duckPushMachine,
		PathPrefix: "/w/a",
		Layout:     db.WorktreeMappingLayoutExplicit,
		Project:    "alpha",
		Enabled:    true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping alpha")

	_, err = local.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine:         "disabled-host",
		PathPrefix:      "/unused/other",
		Layout:          db.WorktreeMappingLayoutExplicit,
		Project:         "beta",
		OriginalProject: "beta",
		Enabled:         false,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping beta original")

	_, err = local.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine:    duckPushMachine,
		PathPrefix: repoRoot,
		Layout:     db.WorktreeMappingLayoutRepoDotWorktrees,
		Enabled:    true,
	})
	require.NoError(t, err, "CreateWorktreeProjectMapping gamma dynamic")
}

// truncateInventoryRows truncates every activity timestamp in an inventory
// to second precision so DuckDB (nanosecond-capable TIMESTAMP) and SQLite
// (millisecond, text-round-tripped) timestamps compare equal.
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

// TestDuckProjectInventoryMatchesSQLite verifies that pushing a local
// SQLite archive's sessions and worktree mappings into DuckDB, then reading
// the inventory back from both sides, produces the same aggregate/
// annotation result. It exercises the real replicated shape (push, not
// hand-inserted mirror rows) so provenance columns and mapping mirroring
// are covered too.
func TestDuckProjectInventoryMatchesSQLite(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	buildInventoryFixture(t, local, ctx)

	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	pushDataReadMirror(t, ctx, syncer)

	localInv, err := local.GetProjectInventory(ctx, db.ProjectDateFilter{})
	require.NoError(t, err, "local GetProjectInventory")

	duckStore := NewStoreFromDB(syncer.DB())
	duckInv, err := duckStore.GetProjectInventory(ctx, db.ProjectDateFilter{})
	require.NoError(t, err, "duckdb GetProjectInventory")

	assert.Equal(t, localInv.TotalProjects, duckInv.TotalProjects)
	assert.Equal(t, localInv.TotalSessions, duckInv.TotalSessions)
	assert.Equal(t, localInv.GovernedSessions, duckInv.GovernedSessions)
	require.Len(t, duckInv.Projects, len(localInv.Projects))
	assert.Equal(t, truncateInventoryRows(localInv.Projects),
		truncateInventoryRows(duckInv.Projects),
	)

	require.Len(t, duckInv.Projects, 4)
	assert.Equal(t, "alpha", duckInv.Projects[0].Label)
	assert.Equal(t, "beta", duckInv.Projects[1].Label)
	assert.Equal(t, "gamma", duckInv.Projects[2].Label)
	assert.Equal(t, "misc", duckInv.Projects[3].Label)
	assert.Equal(t, 3, duckInv.Projects[0].Sessions, "trashed session excluded")
	assert.Equal(t, 2, duckInv.GovernedSessions,
		"alpha-1 via the explicit rule, gamma-dynamic via the dynamic rule")

	assert.Equal(t, 1, duckInv.Projects[0].EnabledRulesTargeting,
		"explicit rule statically targets the alpha row by its own Project field")
	assert.False(t, duckInv.Projects[0].RecordedAsOriginal)

	assert.True(t, duckInv.Projects[1].RecordedAsOriginal,
		"disabled rule's original_project recorded even though it's disabled")
	assert.Equal(t, 0, duckInv.Projects[1].EnabledRulesTargeting,
		"disabled rule must not contribute enabled attribution")

	assert.Equal(t, 1, duckInv.Projects[2].EnabledRulesTargeting,
		"dynamic repo_dot_worktrees rule resolves gamma-dynamic's cwd to gamma")
	assert.False(t, duckInv.Projects[2].RecordedAsOriginal)

	assert.Equal(t, 0, duckInv.Projects[3].EnabledRulesTargeting,
		"misc has no rule targeting it by raw label, only gamma is resolved to")

	_, err = syncer.DB().ExecContext(ctx,
		`UPDATE sessions SET source_archive_id = '' WHERE id = 'alpha-1'`)
	require.NoError(t, err, "clear provenance")

	after, err := duckStore.GetProjectInventory(ctx, db.ProjectDateFilter{})
	require.NoError(t, err, "GetProjectInventory after")

	assert.Equal(t, duckInv.GovernedSessions-1, after.GovernedSessions,
		"unattributed session drops out of the governed count")
	assert.Equal(t, duckInv.TotalSessions, after.TotalSessions,
		"aggregate visibility is unaffected by provenance")
	assert.Equal(t, duckInv.TotalProjects, after.TotalProjects)

	var beforeAlpha, afterAlpha db.ProjectInventoryRow
	for _, row := range duckInv.Projects {
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

func TestDuckGovernedCountExcludesAssignedSiblingEvidence(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	sharedPath := t.TempDir() + "/sessions.jsonl"
	seedInventorySession(t, local, "assigned-reference", "alpha", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.Cwd = "/w/a/run"
		s.FilePath = &sharedPath
	})
	seedInventorySession(t, local, "empty-cwd", "misc", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.FilePath = &sharedPath
	})
	_, err := local.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: duckPushMachine, PathPrefix: "/w/a",
		Project: "alpha", Enabled: true,
	})
	require.NoError(t, err)
	_, err = local.AssignSessionProject(ctx, "assigned-reference", "alpha")
	require.NoError(t, err)

	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	pushDataReadMirror(t, ctx, syncer)
	localInv, err := local.GetProjectInventory(ctx, db.ProjectDateFilter{})
	require.NoError(t, err)
	duckInv, err := NewStoreFromDB(syncer.DB()).GetProjectInventory(ctx, db.ProjectDateFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, localInv.GovernedSessions)
	assert.Equal(t, localInv.GovernedSessions, duckInv.GovernedSessions)
}

// Governance checks archive and machine eligibility before evaluating each archive's own rules.
func TestDuckProjectInventoryCrossArchiveIsolation(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)

	// Archive A has an enabled mapping whose prefix matches no session.
	seedInventorySession(t, local, "a-session", "proj-a", func(s *db.Session) {
		s.Machine = duckPushMachine
		s.Cwd = "/repos/shared"
		s.StartedAt = new("2024-01-01T00:00:00Z")
	})

	_, err := local.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: duckPushMachine, PathPrefix: "/repos/matches-nothing",
		Layout: db.WorktreeMappingLayoutExplicit, Project: "proj-a-unused", Enabled: true,
	})
	require.NoError(t, err, "seed archive A decoy mapping")

	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	pushDataReadMirror(t, ctx, syncer)

	// Archive B: hand-inserted mirror rows for a second source archive,
	// reusing the same machine name and path prefix as archive
	// A's session, plus its own governed session.
	const archiveB = "archive-b"
	_, err = syncer.DB().ExecContext(ctx, `
		INSERT INTO source_worktree_project_mappings
		(source_archive_id, machine, path_prefix, layout, project,
		 original_project, enabled, updated_at)
		VALUES (?, ?, '/repos/shared', 'explicit', 'proj-b',
		 '', TRUE, '')`, archiveB, duckPushMachine)
	require.NoError(t, err, "seed archive B mapping")
	_, err = syncer.DB().ExecContext(ctx, `
		INSERT INTO sessions
		(id, machine, project, agent, message_count, user_message_count,
		 relationship_type, cwd, started_at, created_at, source_archive_id)
		VALUES ('b-session', ?, 'proj-b', 'claude', 1, 1,
		 'root', '/repos/shared', CAST(? AS TIMESTAMP), CAST(? AS TIMESTAMP), ?)`,
		duckPushMachine, "2024-03-01T00:00:00Z", "2024-03-01T00:00:00Z", archiveB)
	require.NoError(t, err, "seed archive B session")

	// b-session-other-machine: same archive B, same path prefix as archive
	// B's own enabled mapping, but a different machine ("m-other") that
	// mapping does not cover. This isolates the *machine* half of the
	// (source_archive_id, machine) tuple filter: a filter that scoped by
	// source_archive_id alone (dropping the machine comparison) would wrongly
	// admit this row as a candidate and govern it, since it shares
	// source_archive_id and cwd prefix with archive B's real mapping.
	_, err = syncer.DB().ExecContext(ctx, `
		INSERT INTO sessions
		(id, machine, project, agent, message_count, user_message_count,
		 relationship_type, cwd, started_at, created_at, source_archive_id)
		VALUES ('b-session-other-machine', 'm-other', 'proj-b', 'claude', 1, 1,
		 'root', '/repos/shared', CAST(? AS TIMESTAMP), CAST(? AS TIMESTAMP), ?)`,
		"2024-03-02T00:00:00Z", "2024-03-02T00:00:00Z", archiveB)
	require.NoError(t, err, "seed archive B session on a different machine")

	// Archive C has no mapping, even though its session shares B's machine and cwd.
	_, err = syncer.DB().ExecContext(ctx, `
        INSERT INTO sessions
        (id, machine, project, agent, message_count, user_message_count,
         relationship_type, cwd, started_at, created_at, source_archive_id)
        VALUES ('c-session', ?, 'proj-c', 'claude', 1, 1,
         'root', '/repos/shared', CAST(? AS TIMESTAMP), CAST(? AS TIMESTAMP), 'archive-c')`,
		duckPushMachine, "2024-03-02T00:00:00Z", "2024-03-02T00:00:00Z")
	require.NoError(t, err, "seed unmapped archive C session")

	duckStore := NewStoreFromDB(syncer.DB())
	inv, err := duckStore.GetProjectInventory(ctx, db.ProjectDateFilter{})
	require.NoError(t, err, "GetProjectInventory")

	byLabel := map[string]db.ProjectInventoryRow{}
	for _, row := range inv.Projects {
		byLabel[row.Label] = row
	}
	require.Contains(t, byLabel, "proj-a")
	require.Contains(t, byLabel, "proj-b")

	assert.Equal(t, 1, inv.GovernedSessions,
		"A passes the prefilter but its decoy rule matches nothing; only B is governed")
	assert.Equal(t, 2, byLabel["proj-b"].Sessions,
		"b-session and b-session-other-machine are both visible even though "+
			"only one is governed; visibility does not depend on governance")
	assert.Equal(t, 0, byLabel["proj-a"].EnabledRulesTargeting,
		"archive B's rule must not statically attribute to archive A's project")
	assert.Equal(t, 1, byLabel["proj-b"].EnabledRulesTargeting,
		"archive B's own rule attributes correctly to its own project")

	// Candidate eligibility and per-archive rule evaluation must hold independently.
	candidates, err := duckStore.catalog().ProjectInventoryCandidateRows(ctx, nil)
	require.NoError(t, err, "projectInventoryCandidateRows")
	var candidateIDs []string
	for _, c := range candidates {
		candidateIDs = append(candidateIDs, c.SessionID)
	}
	assert.Contains(t, candidateIDs, "a-session",
		"A's enabled decoy admits the session; only rule evaluation rejects it")
	assert.NotContains(t, candidateIDs, "c-session",
		"C has no own mapping and must not inherit A's or B's eligibility")
	assert.Contains(t, candidateIDs, "b-session",
		"archive B's own session is a legitimate candidate under its own "+
			"enabled mapping")
	assert.NotContains(t, candidateIDs, "b-session-other-machine",
		"archive B's mapping only covers duckPushMachine; the machine half of "+
			"the (source_archive_id, machine) scope must not admit a same-archive "+
			"session on a different machine just because it shares the archive "+
			"id and cwd prefix")
}

// pushDataReadMirror populates an in-memory mirror the way a full rebuild
// does — sessions (with provenance), identity publication, and worktree
// mapping publication — so Data-read tests observe the same mirror state a
// real push produces.
func pushDataReadMirror(t *testing.T, ctx context.Context, syncer *Sync) {
	t.Helper()

	require.NoError(t, createSchema(ctx, syncer.DB()), "createSchema")
	_, err := syncer.pushEverything(ctx, nil)
	require.NoError(t, err, "pushEverything")
	_, err = syncer.syncProjectIdentityObservations(ctx, 0, true, nil)
	require.NoError(t, err, "syncProjectIdentityObservations")
	_, err = syncer.syncWorktreeMappings(ctx, 0, true)
	require.NoError(t, err, "syncWorktreeMappings")
}
