//go:build !(windows && arm64)

package duckdb

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
	require.NoError(t, local.UpsertSession(sess), "UpsertSession")
	require.NoError(t, local.InsertMessages([]db.Message{{
		SessionID:     id,
		Ordinal:       0,
		Role:          "assistant",
		Content:       "hello",
		ContentLength: 5,
	}}), "InsertMessages")
}

// duckPushMachine is the fixed machine name the DuckDB sync harness stamps
// onto every pushed session (internal/duckdb/sync.go's newTestSync passes
// this literal to New). Unlike PostgreSQL's push (which preserves each
// session's own Machine field via pushedSessionMachine, falling back only
// for "local"/empty sentinels), DuckDB's push.go unconditionally writes
// s.machine onto every mirrored session row -- worktree mapping rows are
// not affected, since mapping publication mirrors the Machine field
// verbatim. Fixtures pushed through the sync harness must therefore set
// every session's Machine (and every governing mapping's Machine) to this
// value, or governance evaluation will silently never match after push.
const duckPushMachine = "test-machine"

// buildInventoryFixture seeds the shared alpha/beta/gamma/misc aggregate
// fixture used by the DuckDB inventory tests: distinct and empty cwds, a
// trashed session excluded from every count, and three mapping rules that
// together exercise every branch of annotateProjectInventoryRows. It
// mirrors internal/postgres's buildInventoryFixture in spirit, but every
// session and every governing mapping shares duckPushMachine (see above);
// alpha-2 is excluded from governance by a non-matching cwd prefix instead
// of a different machine, since DuckDB's push collapses per-session
// machine identity and so cannot express a same-push cross-machine
// exclusion the way PostgreSQL's fixture does (that scoping is instead
// covered by TestDuckProjectInventoryCrossArchiveIsolation's hand-inserted
// rows, which bypass the push collapse).
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
	require.NoError(t, local.SoftDeleteSession("alpha-trashed"))

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
	ctx := context.Background()
	local := newLocalDB(t)
	buildInventoryFixture(t, local, ctx)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	pushDataReadMirror(t, ctx, syncer)

	localInv, err := local.GetProjectInventory(ctx)
	require.NoError(t, err, "local GetProjectInventory")

	duckStore := NewStoreFromDB(syncer.DB())
	duckInv, err := duckStore.GetProjectInventory(ctx)
	require.NoError(t, err, "duckdb GetProjectInventory")

	assert.Equal(t, localInv.TotalProjects, duckInv.TotalProjects)
	assert.Equal(t, localInv.TotalSessions, duckInv.TotalSessions)
	assert.Equal(t, localInv.GovernedSessions, duckInv.GovernedSessions)
	require.Equal(t, len(localInv.Projects), len(duckInv.Projects))
	assert.Equal(t,
		truncateInventoryRows(localInv.Projects),
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
}

// TestDuckProjectInventoryIgnoresUnattributedSessions verifies that a
// session with blanked source_archive_id (lost provenance) is dropped from
// the governed count but stays visible in every aggregate count:
// provenance only gates governedness, not visibility.
func TestDuckProjectInventoryIgnoresUnattributedSessions(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	buildInventoryFixture(t, local, ctx)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	pushDataReadMirror(t, ctx, syncer)

	duckStore := NewStoreFromDB(syncer.DB())
	before, err := duckStore.GetProjectInventory(ctx)
	require.NoError(t, err, "GetProjectInventory before")
	require.Equal(t, 2, before.GovernedSessions,
		"alpha-1 and gamma-dynamic governed before provenance is cleared")

	_, err = syncer.DB().ExecContext(ctx,
		`UPDATE sessions SET source_archive_id = '' WHERE id = 'alpha-1'`)
	require.NoError(t, err, "clear provenance")

	after, err := duckStore.GetProjectInventory(ctx)
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

// TestDuckProjectInventoryCrossArchiveIsolation verifies that inventory
// governance is scoped per source archive AND per machine when a DuckDB
// mirror serves more than one archive. It pushes one archive through the
// real sync path (archive A) and hand-inserts a second archive's mapping
// and session rows directly into the DuckDB mirror (archive B), following
// the pattern in TestDuckPushReplicatesWorktreeMappings. Archive A's
// session ends up with machine=duckPushMachine after push (DuckDB's push
// collapses every session's machine field to the syncing machine; see
// duckPushMachine's doc comment), so archive B's hand-inserted rows
// deliberately reuse that same machine name and path prefix, isolating
// source_archive_id as the only variable between archive A and B: a broken
// (source_archive_id, machine) scope -- either in the candidate-row SQL or
// in how projectInventoryMappings groups rules by archive -- would
// incorrectly let archive B's rule govern archive A's session.
//
// A third session, b-session-other-machine, isolates the other half of the
// same tuple filter: it shares archive B's source_archive_id and cwd prefix
// but sits on a machine ("m-other") archive B's mapping does not cover, so
// a filter scoped by source_archive_id alone (dropping the machine
// comparison) would wrongly admit and govern it.
func TestDuckProjectInventoryCrossArchiveIsolation(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)

	// Archive A (the local push archive) has no worktree mapping of its
	// own on duckPushMachine.
	seedInventorySession(t, local, "a-session", "proj-a", func(s *db.Session) {
		s.Cwd = "/repos/shared"
		s.StartedAt = new("2024-01-01T00:00:00Z")
	})

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	pushDataReadMirror(t, ctx, syncer)

	var aSessionMachine string
	require.NoError(t, syncer.DB().QueryRowContext(ctx,
		`SELECT machine FROM sessions WHERE id = 'a-session'`,
	).Scan(&aSessionMachine), "read back a-session machine")
	require.Equal(t, duckPushMachine, aSessionMachine,
		"push must stamp every session with the syncing machine name")

	// Archive B: hand-inserted mirror rows for a second source archive,
	// reusing the same (collapsed) machine name and path prefix as archive
	// A's session, plus its own governed session.
	const archiveB = "archive-b"
	_, err := syncer.DB().ExecContext(ctx, `
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

	duckStore := NewStoreFromDB(syncer.DB())
	inv, err := duckStore.GetProjectInventory(ctx)
	require.NoError(t, err, "GetProjectInventory")

	byLabel := map[string]db.ProjectInventoryRow{}
	for _, row := range inv.Projects {
		byLabel[row.Label] = row
	}
	require.Contains(t, byLabel, "proj-a")
	require.Contains(t, byLabel, "proj-b")

	assert.Equal(t, 1, inv.GovernedSessions,
		"only archive B's own session on duckPushMachine is governed by "+
			"archive B's rule; archive A has no rule of its own on that "+
			"machine, and b-session-other-machine sits on a machine archive "+
			"B's rule does not cover, so neither is governed")
	assert.Equal(t, 2, byLabel["proj-b"].Sessions,
		"b-session and b-session-other-machine are both visible even though "+
			"only one is governed; visibility does not depend on governance")
	assert.Equal(t, 0, byLabel["proj-a"].EnabledRulesTargeting,
		"archive B's rule must not statically attribute to archive A's project")
	assert.Equal(t, 1, byLabel["proj-b"].EnabledRulesTargeting,
		"archive B's own rule attributes correctly to its own project")

	// Directly exercise the (source_archive_id, machine) scope in
	// projectInventoryCandidateRows: archive A's session must not become a
	// governance candidate on the strength of archive B's enabled mapping
	// on the same machine name.
	candidates, err := duckStore.projectInventoryCandidateRows(ctx, nil)
	require.NoError(t, err, "projectInventoryCandidateRows")
	var candidateIDs []string
	for _, c := range candidates {
		candidateIDs = append(candidateIDs, c.SessionID)
	}
	assert.NotContains(t, candidateIDs, "a-session",
		"archive A has no enabled mapping of its own; the (source_archive_id, "+
			"machine) scope must not admit its session just because archive B "+
			"has an enabled mapping on the same machine name")
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
	_, err = syncer.syncProjectIdentityObservations(ctx, 0, true)
	require.NoError(t, err, "syncProjectIdentityObservations")
	_, err = syncer.syncWorktreeMappings(ctx, 0, true)
	require.NoError(t, err, "syncWorktreeMappings")
}
