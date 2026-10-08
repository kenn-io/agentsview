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
	require.NoError(t, localDB.UpsertSession(t.Context(), sess), "UpsertSession")
	require.NoError(t, localDB.InsertMessages(t.Context(), []db.Message{{
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
	require.NoError(t, localDB.SoftDeleteSession(t.Context(), "alpha-trashed"))

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

	localInv, err := localDB.GetProjectInventory(ctx, db.ProjectDateFilter{})
	require.NoError(t, err, "local GetProjectInventory")

	pgStore := &Store{pg: pg}
	pgInv, err := pgStore.GetProjectInventory(ctx, db.ProjectDateFilter{})
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
