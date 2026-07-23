package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

func TestGetProjectInventoryAggregates(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "alpha-1", "alpha", func(s *Session) {
		s.Machine = "m1"
		s.Agent = "claude"
		s.Cwd = "/w/a"
		s.StartedAt = Ptr("2024-01-01T00:00:00Z")
		s.EndedAt = Ptr("2024-01-01T02:00:00Z")
	})
	insertSession(t, d, "alpha-2", "alpha", func(s *Session) {
		s.Machine = "m2"
		s.Agent = "codex"
		s.Cwd = "/w/a"
		s.StartedAt = Ptr("2024-01-05T00:00:00Z")
	})
	insertSession(t, d, "alpha-3", "alpha", func(s *Session) {
		s.Machine = "m1"
		s.Agent = "claude"
		s.Cwd = ""
		s.StartedAt = Ptr("2023-12-25T00:00:00Z")
		s.EndedAt = Ptr("2024-01-10T00:00:00Z")
	})
	insertSession(t, d, "alpha-trashed", "alpha", func(s *Session) {
		s.Machine = "m9"
		s.Agent = "trashed-agent"
		s.Cwd = "/w/trash"
		s.StartedAt = Ptr("2020-01-01T00:00:00Z")
		s.EndedAt = Ptr("2020-01-02T00:00:00Z")
	})
	require.NoError(t, d.SoftDeleteSession("alpha-trashed"))

	insertSession(t, d, "beta-1", "beta", func(s *Session) {
		s.Machine = "m3"
		s.Agent = "gemini"
		s.Cwd = "/w/b"
		s.StartedAt = Ptr("2024-02-01T00:00:00Z")
	})

	inv, err := d.GetProjectInventory(ctx)
	require.NoError(t, err)

	require.Len(t, inv.Projects, 2)
	assert.Equal(t, 2, inv.TotalProjects)
	assert.Equal(t, 4, inv.TotalSessions)
	assert.Equal(t, 0, inv.GovernedSessions)

	// Row ordering must be deterministic, sorted by label.
	assert.Equal(t, "alpha", inv.Projects[0].Label)
	assert.Equal(t, "beta", inv.Projects[1].Label)

	alpha := inv.Projects[0]
	assert.Equal(t, 3, alpha.Sessions, "trashed session excluded")
	assert.Equal(t, 2, alpha.Machines)
	assert.Equal(t, 2, alpha.Agents)
	assert.Equal(t, 1, alpha.DistinctCwds, "empty cwd not counted, duplicate collapses")
	require.NotNil(t, alpha.FirstActivity)
	require.NotNil(t, alpha.LastActivity)
	assert.Equal(t, "2023-12-25T00:00:00Z", alpha.FirstActivity.UTC().Format(time.RFC3339))
	assert.Equal(t, "2024-01-10T00:00:00Z", alpha.LastActivity.UTC().Format(time.RFC3339))
	assert.Equal(t, 0, alpha.EnabledRulesTargeting)
	assert.False(t, alpha.RecordedAsOriginal)

	beta := inv.Projects[1]
	assert.Equal(t, 1, beta.Sessions)
	assert.Equal(t, 1, beta.Machines)
	assert.Equal(t, 1, beta.Agents)
	assert.Equal(t, 1, beta.DistinctCwds)
	require.NotNil(t, beta.FirstActivity)
	require.NotNil(t, beta.LastActivity)
	assert.Equal(t, *beta.FirstActivity, *beta.LastActivity,
		"LastActivity falls back to started_at when ended_at is unset")

	projects, err := d.BuildProjectIdentityMap(ctx, []string{"alpha", "beta"})
	require.NoError(t, err)
	assert.NotEmpty(t, alpha.ProjectKey)
	assert.Equal(t, export.ProjectKeyForEntry(projects["alpha"]), alpha.ProjectKey)
	assert.NotEmpty(t, beta.ProjectKey)
	assert.Equal(t, export.ProjectKeyForEntry(projects["beta"]), beta.ProjectKey)
}

func TestGetProjectInventoryIgnoresEmptyTimestampStrings(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "legacy-1", "alpha", func(s *Session) {
		s.StartedAt = Ptr("2024-01-05T00:00:00Z")
	})
	insertSession(t, d, "legacy-2", "alpha")
	// Legacy rows can hold '' instead of NULL in the TEXT timestamp
	// columns; an empty string sorts before every real timestamp and would
	// corrupt MIN(started_at) without the NULLIF guard.
	_, err := d.getWriter().Exec(
		`UPDATE sessions SET started_at = '', ended_at = '' WHERE id = 'legacy-2'`)
	require.NoError(t, err)

	inv, err := d.GetProjectInventory(ctx)
	require.NoError(t, err)

	require.Len(t, inv.Projects, 1)
	row := inv.Projects[0]
	require.NotNil(t, row.FirstActivity,
		"'' started_at must not shadow the real first activity")
	assert.Equal(t, "2024-01-05T00:00:00Z",
		row.FirstActivity.UTC().Format(time.RFC3339))
	require.NotNil(t, row.LastActivity)
	assert.Equal(t, "2024-01-05T00:00:00Z",
		row.LastActivity.UTC().Format(time.RFC3339))
}

func TestGetProjectInventoryCwdNormalization(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "win-1", "winproj", func(s *Session) {
		s.Cwd = `C:\w\repo`
		s.StartedAt = Ptr("2024-01-01T00:00:00Z")
	})
	insertSession(t, d, "win-2", "winproj", func(s *Session) {
		s.Cwd = `C:/w/repo`
		s.StartedAt = Ptr("2024-01-02T00:00:00Z")
	})

	inv, err := d.GetProjectInventory(ctx)
	require.NoError(t, err)

	require.Len(t, inv.Projects, 1)
	assert.Equal(t, 2, inv.Projects[0].Sessions)
	assert.Equal(t, 1, inv.Projects[0].DistinctCwds,
		"backslash and forward-slash cwds normalize to the same path")
}

func TestGetProjectInventoryAnnotations(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "alpha-1", "alpha", func(s *Session) {
		s.StartedAt = Ptr("2024-01-01T00:00:00Z")
	})
	insertSession(t, d, "beta-1", "beta", func(s *Session) {
		s.StartedAt = Ptr("2024-01-01T00:00:00Z")
	})
	insertSession(t, d, "gamma-1", "gamma", func(s *Session) {
		s.StartedAt = Ptr("2024-01-01T00:00:00Z")
	})

	// Enabled explicit rule targeting "alpha" whose machine has no
	// sessions at all, so it contributes zero governed sessions but must
	// still count as static attribution.
	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine:    "empty-host",
		PathPrefix: "/unused/prefix",
		Layout:     WorktreeMappingLayoutExplicit,
		Project:    "alpha",
		Enabled:    true,
	})
	require.NoError(t, err)

	// Disabled rule recording original_project "beta": must set
	// RecordedAsOriginal but must NOT contribute to EnabledRulesTargeting.
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine:         "disabled-host",
		PathPrefix:      "/unused/other",
		Layout:          WorktreeMappingLayoutExplicit,
		Project:         "beta",
		OriginalProject: "beta",
		Enabled:         false,
	})
	require.NoError(t, err)

	// Enabled repo_dot_worktrees rule that dynamically resolves one
	// visible session's cwd to project "gamma".
	repoRoot := t.TempDir()
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine:    "dyn-host",
		PathPrefix: repoRoot,
		Layout:     WorktreeMappingLayoutRepoDotWorktrees,
		Enabled:    true,
	})
	require.NoError(t, err)

	insertSession(t, d, "gamma-dynamic", "misc", func(s *Session) {
		s.Machine = "dyn-host"
		s.Cwd = repoRoot + "/gamma.worktrees/branch1"
		s.StartedAt = Ptr("2024-01-02T00:00:00Z")
	})

	inv, err := d.GetProjectInventory(ctx)
	require.NoError(t, err)

	byLabel := map[string]ProjectInventoryRow{}
	for _, row := range inv.Projects {
		byLabel[row.Label] = row
	}

	require.Contains(t, byLabel, "alpha")
	assert.Equal(t, 1, byLabel["alpha"].EnabledRulesTargeting,
		"static attribution counts even with zero governed sessions")
	assert.False(t, byLabel["alpha"].RecordedAsOriginal)

	require.Contains(t, byLabel, "beta")
	assert.True(t, byLabel["beta"].RecordedAsOriginal)
	assert.Equal(t, 0, byLabel["beta"].EnabledRulesTargeting,
		"disabled rule's target must not count toward enabled attribution")

	require.Contains(t, byLabel, "gamma")
	assert.Equal(t, 1, byLabel["gamma"].EnabledRulesTargeting,
		"dynamic attribution from a resolving repo_dot_worktrees rule")

	assert.Equal(t, 1, inv.GovernedSessions)
}

// TestProjectInventorySingleAggregationPass is a cardinality-scaling
// regression on the inventory read path's SQL shape: the aggregation query
// must do a single sessions scan (no correlated subquery re-scanning
// sessions per output row), and the governed-evaluation candidate query
// must use the sessions machine index rather than a full table scan.
func TestGetProjectInventoryManyDistinctProjects(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	// Exceed SQLite's bind-variable budget (32766) with distinct visible
	// project labels: the inventory feeds every raw label to
	// BuildProjectIdentityMap at once, so the identity-observation lookup
	// must chunk its IN list instead of binding one variable per label.
	const projectCount = 33000
	seedDistinctProjectSessions(t, d, projectCount)

	inv, err := d.GetProjectInventory(ctx)
	require.NoError(t, err,
		"inventory must not expand one bind variable per distinct project")
	assert.Equal(t, projectCount, inv.TotalProjects)
	assert.Equal(t, projectCount, inv.TotalSessions)
	assert.Len(t, inv.Projects, projectCount)
}

func TestProjectInventorySingleAggregationPass(t *testing.T) {
	t.Run("aggregation query is a single sessions scan", func(t *testing.T) {
		d := testDB(t)
		rows, err := d.getReader().Query(
			"EXPLAIN QUERY PLAN " + projectInventoryAggregateQuery(),
		)
		require.NoError(t, err)
		defer rows.Close()

		details := explainQueryPlanDetails(t, rows)
		sessionsScans := 0
		for _, detail := range details {
			if strings.Contains(detail, "sessions") {
				sessionsScans++
			}
		}
		assert.Equal(t, 1, sessionsScans,
			"aggregation must scan sessions exactly once, got plan: %s",
			strings.Join(details, "; "))
	})

	t.Run("candidate query uses the sessions machine index", func(t *testing.T) {
		d := testDB(t)
		query, args := projectInventoryCandidateQuery([]string{"ws"})
		rows, err := d.getReader().Query("EXPLAIN QUERY PLAN "+query, args...)
		require.NoError(t, err)
		defer rows.Close()

		details := explainQueryPlanDetails(t, rows)
		assert.Contains(t, strings.Join(details, "\n"), "idx_sessions_machine",
			"candidate fetch must use the sessions machine index, not a full scan")
	})
}

// explainQueryPlanDetails scans the detail column of an EXPLAIN QUERY PLAN
// result set.
func explainQueryPlanDetails(t *testing.T, rows *sql.Rows) []string {
	t.Helper()
	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	return details
}
