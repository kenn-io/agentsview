package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

func TestListArchiveWorktreeCandidatesFallsBackAndBoundsExamples(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const raw = "branch-label"
	for i := range 12 {
		id := "aggregate-" + string(rune('a'+i))
		seedCandidateSession(t, d, id, raw, "host.example",
			"/srv/repository/subdir", "2025-06-02T10:00:00Z")
	}
	seedCandidateSession(t, d, "fallback", raw, "host.example",
		"/opt/exact", "2025-06-02T10:00:00Z")
	deleteCandidateSnapshot(t, d, "fallback")
	seedCandidateSession(t, d, "unavailable", raw, "host.example", "",
		"2025-06-02T10:00:00Z")
	deleteCandidateSnapshot(t, d, "unavailable")
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: raw, Machine: "host.example", RootPath: "/srv/repository",
		}), "seed aggregate evidence")
	projects, err := d.BuildProjectIdentityMap(ctx, []string{raw})
	require.NoError(t, err)

	request := ArchiveWorktreeCandidateRequest{
		ProjectLabel: raw, ProjectKey: projects[raw].ProjectKey,
	}
	candidates, err := d.ListArchiveWorktreeCandidates(ctx, request)
	require.NoError(t, err)
	require.Len(t, candidates, 3)
	assert.Equal(t, "aggregate", candidates[0].EvidenceKind)
	assert.Equal(t, 12, candidates[0].ContributingSessions)
	assert.Len(t, candidates[0].Examples, 10)
	assert.Equal(t, "fallback", candidates[1].EvidenceKind)
	assert.Equal(t, "/opt/exact", candidates[1].SuggestedPrefix)
	assert.False(t, candidates[2].Available)
	assert.Empty(t, candidates[2].SuggestedPrefix)
	assert.Equal(t, "unavailable", candidates[2].EvidenceKind)
	again, err := d.ListArchiveWorktreeCandidates(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, candidates, again, "candidate order and IDs should be deterministic")
}

func TestListArchiveWorktreeCandidatesSelectsByProjectIdentity(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const (
		clickedRaw = "/private/example/repository"
		otherRaw   = "/another/private/repository"
	)

	seedCandidateSession(t, d, "clicked", clickedRaw, "host-a.example",
		"/srv/worktrees/repository/feature/cmd", "2025-06-02T10:00:00Z")
	seedCandidateSession(t, d, "same-display-other-key", otherRaw, "host-a.example",
		"/srv/worktrees/other/main", "2025-06-02T10:00:00Z")
	setCandidateSnapshot(t, d, "clicked", clickedRaw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")
	setCandidateSnapshot(t, d, "same-display-other-key", otherRaw, "host-a.example",
		"/srv/worktrees/other", "/srv/worktrees/other/main")

	projects, err := d.BuildProjectIdentityMap(ctx, []string{clickedRaw, otherRaw})
	require.NoError(t, err)
	require.NotEqual(t, projects[clickedRaw].ProjectKey, projects[otherRaw].ProjectKey)
	require.Equal(t, export.SafeProjectDisplayLabel(clickedRaw),
		export.SafeProjectDisplayLabel(otherRaw),
		"fixture must collide on display label so only the key disambiguates")

	candidates, err := d.ListArchiveWorktreeCandidates(ctx, ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel(clickedRaw),
		ProjectKey:   projects[clickedRaw].ProjectKey,
	})
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	assert.Equal(t, "/srv/worktrees/other/main", candidates[0].SuggestedPrefix)
	assert.Equal(t, "/srv/worktrees/repository/feature/cmd",
		candidates[1].SuggestedPrefix)
	assert.Equal(t, 1, candidates[0].ContributingSessions)
	assert.Equal(t, 1, candidates[1].ContributingSessions)
}

func TestListArchiveWorktreeCandidatesIgnoresDateRange(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const raw = "date-spread-project"

	seedCandidateSession(t, d, "old-session", raw, "host-a.example",
		"/srv/worktrees/repository/feature/cmd", "2020-01-01T10:00:00Z")
	seedCandidateSession(t, d, "new-session", raw, "host-a.example",
		"/srv/worktrees/repository/feature/frontend", "2025-06-02T10:00:00Z")
	setCandidateSnapshot(t, d, "old-session", raw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")
	setCandidateSnapshot(t, d, "new-session", raw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")

	projects, err := d.BuildProjectIdentityMap(ctx, []string{raw})
	require.NoError(t, err)

	candidates, err := d.ListArchiveWorktreeCandidates(ctx, ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel(raw),
		ProjectKey:   projects[raw].ProjectKey,
	})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, "snapshot", candidates[0].EvidenceKind)
	assert.Equal(t, "/srv/worktrees/repository/feature", candidates[0].SuggestedPrefix)
	assert.Equal(t, 2, candidates[0].ContributingSessions,
		"archive-wide selection must cover both the old and new session")
	assert.Equal(t, 2, candidates[0].DistinctCwds)
	assert.True(t, candidates[0].Available)
}

func TestListArchiveWorktreeCandidatesKeyMismatch(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const raw = "mismatch-project"

	seedCandidateSession(t, d, "session-a", raw, "host-a.example",
		"/srv/worktrees/repository/feature", "2025-06-02T10:00:00Z")
	setCandidateSnapshot(t, d, "session-a", raw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")

	candidates, err := d.ListArchiveWorktreeCandidates(ctx, ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel(raw),
		ProjectKey:   "wrong-key",
	})
	require.NoError(t, err)
	assert.Empty(t, candidates, "right label with wrong key must return no candidates, no error")

	_, err = d.ListArchiveWorktreeCandidates(ctx, ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel(raw),
		ProjectKey:   "",
	})
	require.Error(t, err, "empty project key must error")
}

func TestListArchiveWorktreeCandidatesBoundsIdentityLookupToClickedLabel(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const raw = "clicked-project"

	seedCandidateSession(t, d, "clicked-a", raw, "host-a.example",
		"/srv/worktrees/repository/feature/cmd", "2025-06-02T10:00:00Z")
	seedCandidateSession(t, d, "clicked-b", raw, "host-a.example",
		"/srv/worktrees/repository/feature/frontend", "2025-06-02T10:00:00Z")
	setCandidateSnapshot(t, d, "clicked-a", raw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")
	setCandidateSnapshot(t, d, "clicked-b", raw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")
	// Exceed SQLite's bind-variable budget (32766) with distinct visible
	// project labels: passing every label to BuildProjectIdentityMap would
	// expand one placeholder each and fail the whole request, so the
	// selection must prefilter to the clicked label's raw variants.
	seedDistinctProjectSessions(t, d, 33000)

	projects, err := d.BuildProjectIdentityMap(ctx, []string{raw})
	require.NoError(t, err)

	candidates, err := d.ListArchiveWorktreeCandidates(ctx, ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel(raw),
		ProjectKey:   projects[raw].ProjectKey,
	})
	require.NoError(t, err,
		"archive-wide candidates must not expand one bind variable per distinct project")
	require.Len(t, candidates, 1)
	assert.Equal(t, 2, candidates[0].ContributingSessions)
}

func TestListArchiveWorktreeCandidatesIncludesZeroMessageSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const raw = "empty-project"
	seedCandidateSession(t, d, "empty", raw, "host-a.example",
		"/srv/worktrees/empty", "2025-06-02T10:00:00Z")
	_, err := d.getWriter().Exec(
		`UPDATE sessions SET message_count = 0 WHERE id = 'empty'`)
	require.NoError(t, err)
	projects, err := d.BuildProjectIdentityMap(ctx, []string{raw})
	require.NoError(t, err)

	candidates, err := d.ListArchiveWorktreeCandidates(ctx,
		ArchiveWorktreeCandidateRequest{
			ProjectLabel: raw,
			ProjectKey:   projects[raw].ProjectKey,
		})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, 1, candidates[0].ContributingSessions)
}

func TestListArchiveWorktreeCandidatesManyCollidingRawLabels(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const clickedRaw = "/bulk/clicked-project"

	seedCandidateSession(t, d, "clicked", clickedRaw, "host-a.example",
		"/srv/worktrees/repository/feature/cmd", "2025-06-02T10:00:00Z")
	setCandidateSnapshot(t, d, "clicked", clickedRaw, "host-a.example",
		"/srv/worktrees/repository", "/srv/worktrees/repository/feature")
	// Exceed SQLite's bind-variable budget (32766) with distinct raw labels
	// that all sanitize to the SAME display label as clickedRaw (absolute
	// paths display as ""), so the clicked-label prefilter cannot bound the
	// identity-map lookup and the observation queries must chunk instead.
	seedBulkProjectSessions(t, d, 33000, "/bulk/project-%05d")
	require.Empty(t, export.SafeProjectDisplayLabel(clickedRaw),
		"fixture labels must collide on the empty display label")

	projects, err := d.BuildProjectIdentityMap(ctx, []string{clickedRaw})
	require.NoError(t, err)

	candidates, err := d.ListArchiveWorktreeCandidates(ctx, ArchiveWorktreeCandidateRequest{
		ProjectLabel: "", ProjectKey: projects[clickedRaw].ProjectKey,
	})
	require.NoError(t, err,
		"colliding raw labels must not exhaust SQLite bind variables")
	require.Len(t, candidates, 2)
	assert.Equal(t, 1, candidates[0].ContributingSessions)
	assert.Equal(t, "/srv/worktrees/repository/feature/cmd",
		candidates[0].SuggestedPrefix)
	assert.Equal(t, 33000, candidates[1].ContributingSessions)
	assert.Equal(t, "unavailable", candidates[1].EvidenceKind)
}

// seedDistinctProjectSessions batch-inserts n visible sessions, each under
// its own distinct project label, bypassing UpsertSession so seeding tens of
// thousands of rows stays cheap.
func seedDistinctProjectSessions(t *testing.T, d *DB, n int) {
	t.Helper()
	seedBulkProjectSessions(t, d, n, "bulk-project-%05d")
}

// seedBulkProjectSessions batch-inserts n visible sessions whose project
// labels follow projectFormat (one %05d verb), bypassing UpsertSession so
// seeding tens of thousands of rows stays cheap.
func seedBulkProjectSessions(t *testing.T, d *DB, n int, projectFormat string) {
	t.Helper()
	const batch = 400
	for start := 0; start < n; start += batch {
		end := min(start+batch, n)
		var sb strings.Builder
		sb.WriteString("INSERT INTO sessions (id, project, message_count) VALUES ")
		args := make([]any, 0, (end-start)*2)
		for i := start; i < end; i++ {
			if i > start {
				sb.WriteString(",")
			}
			sb.WriteString("(?, ?, 1)")
			args = append(args,
				fmt.Sprintf("bulk-session-%05d", i),
				fmt.Sprintf(projectFormat, i))
		}
		_, err := d.getWriter().Exec(sb.String(), args...)
		require.NoError(t, err, "seed bulk sessions %d-%d", start, end)
	}
}

func seedCandidateSession(
	t *testing.T, d *DB, id, project, machine, cwd, started string,
) {
	t.Helper()
	ended := started
	require.NoError(t, d.UpsertSession(Session{
		ID: id, Project: project, Machine: machine, Agent: "codex", Cwd: cwd,
		StartedAt: &started, EndedAt: &ended, MessageCount: 1,
	}), "seed candidate session %s", id)
}

func deleteCandidateSnapshot(t *testing.T, d *DB, id string) {
	t.Helper()
	_, err := d.getWriter().Exec(
		`DELETE FROM session_project_identity_snapshots WHERE session_id = ?`, id)
	require.NoError(t, err)
}

func setCandidateSnapshot(
	t *testing.T, d *DB, id, project, machine, root, worktreeRoot string,
) {
	t.Helper()
	deleteCandidateSnapshot(t, d, id)
	_, err := d.getWriter().Exec(`
		INSERT INTO session_project_identity_snapshots (
			session_id, project, machine, root_path, worktree_root_path, observed_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		id, project, machine, root, worktreeRoot, "2025-06-02T10:00:00Z")
	require.NoError(t, err)
}
