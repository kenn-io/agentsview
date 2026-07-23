//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

// seedPGCandidateSession inserts a minimal session with one message. Unlike
// DuckDB, PostgreSQL's push preserves each session's own Machine field, so
// worktree candidate fixtures can exercise real cross-machine grouping.
func seedPGCandidateSession(
	t *testing.T, localDB *db.DB, id, project, machine, cwd, started string,
) {
	t.Helper()
	ended := started
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID: id, Project: project, Machine: machine, Agent: "codex", Cwd: cwd,
		StartedAt: &started, EndedAt: &ended, MessageCount: 1,
	}), "UpsertSession %s", id)
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID: id, Ordinal: 0, Role: "assistant", Content: "hi", ContentLength: 2,
	}}), "InsertMessages %s", id)
}

// setPGCandidateSnapshot publishes resolved snapshot evidence for a session
// via the public UpsertProjectIdentityObservation API (SessionID set),
// overriding the placeholder row the sessions-insert trigger created.
func setPGCandidateSnapshot(
	t *testing.T, ctx context.Context, localDB *db.DB,
	id, project, machine, root, worktreeRoot string,
) {
	t.Helper()
	require.NoError(t, localDB.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: id, Project: project, Machine: machine,
			RootPath: root, WorktreeRootPath: worktreeRoot,
			RemoteResolution: export.ProjectResolutionResolved,
			ObservedAt:       time.Date(2025, 6, 2, 10, 0, 0, 0, time.UTC),
		}), "set candidate snapshot for %s", id)
}

// seedPGCandidateSessionNoSnapshot inserts a session with no identity
// snapshot at all, deleting the sessions-insert trigger's placeholder row
// via the public UpsertSessionWithProjectIdentity API's empty-snapshotProject
// deletion path. A bare placeholder must not survive to the pushed mirror:
// push sanitization (export.SanitizeStoredProjectIdentityObservation)
// derives a non-empty KeySource from any non-empty RootPath, including a
// placeholder's raw cwd, which would make an uninspected session look like
// snapshot evidence once mirrored. internal/db's own
// worktree_candidates_test.go sidesteps this the same way, deleting the
// placeholder for its aggregate/fallback/unavailable fixture rows rather
// than relying on it staying inert.
func seedPGCandidateSessionNoSnapshot(
	t *testing.T, ctx context.Context, localDB *db.DB,
	id, project, machine, cwd, started string,
) {
	t.Helper()
	ended := started
	require.NoError(t, localDB.UpsertSessionWithProjectIdentity(
		db.Session{
			ID: id, Project: project, Machine: machine, Agent: "codex", Cwd: cwd,
			StartedAt: &started, EndedAt: &ended, MessageCount: 1,
		},
		export.ProjectIdentityObservation{
			SessionID: id, Project: project, Machine: machine,
		},
		"",
	), "UpsertSessionWithProjectIdentity %s", id)
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID: id, Ordinal: 0, Role: "assistant", Content: "hi", ContentLength: 2,
	}}), "InsertMessages %s", id)
}

// TestPGWorktreeCandidatesArchiveWideMatchesSQLite seeds one session of each
// evidence kind (snapshot, aggregate, exact-cwd fallback, unavailable) --
// reusing Task 7's fixture shapes, plus a distinct machine to prove grouping
// stays machine-scoped -- pushes them through the real PG sync path, and
// confirms the mirror's ListArchiveWorktreeCandidates output is identical to
// SQLite's for the same archive. It also proves archive-wideness: the
// snapshot group spans an old (2020) and a new (2025) session, both of which
// must appear in the combined group.
func TestPGWorktreeCandidatesArchiveWideMatchesSQLite(t *testing.T) {
	const schema = "agentsview_worktree_candidates_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)
	const project = "candidate-project"
	const machine = "host-a.example"

	seedPGCandidateSession(t, localDB, "old-session", project, machine,
		"/srv/worktrees/repo/feature/cmd", "2020-01-01T10:00:00Z")
	seedPGCandidateSession(t, localDB, "new-session", project, machine,
		"/srv/worktrees/repo/feature/frontend", "2025-06-02T10:00:00Z")
	setPGCandidateSnapshot(t, ctx, localDB, "old-session", project, machine,
		"/srv/worktrees/repo", "/srv/worktrees/repo/feature")
	setPGCandidateSnapshot(t, ctx, localDB, "new-session", project, machine,
		"/srv/worktrees/repo", "/srv/worktrees/repo/feature")

	seedPGCandidateSessionNoSnapshot(t, ctx, localDB, "aggregate-session", project, machine,
		"/srv/checkouts/repo/docs", "2025-06-02T10:00:00Z")
	require.NoError(t, localDB.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: project, Machine: machine, RootPath: "/srv/checkouts/repo",
		}), "seed aggregate evidence")

	seedPGCandidateSessionNoSnapshot(t, ctx, localDB, "fallback-session", project, machine,
		"/opt/unknown/repo", "2025-06-02T10:00:00Z")

	seedPGCandidateSession(t, localDB, "unavailable-session", project, machine,
		"", "2025-06-02T10:00:00Z")

	// A session on a different machine with the same cwd as the fallback
	// session must not merge into its group: machine is part of the grouping
	// key.
	seedPGCandidateSessionNoSnapshot(t, ctx, localDB, "other-machine-session",
		project, "host-b.example", "/opt/unknown/repo", "2025-06-02T10:00:00Z")

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	projects, err := localDB.BuildProjectIdentityMap(ctx, []string{project})
	require.NoError(t, err, "local BuildProjectIdentityMap")
	req := db.ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel(project),
		ProjectKey:   projects[project].ProjectKey,
	}

	localCandidates, err := localDB.ListArchiveWorktreeCandidates(ctx, req)
	require.NoError(t, err, "local ListArchiveWorktreeCandidates")

	pgStore := &Store{pg: pg}
	pgCandidates, err := pgStore.ListArchiveWorktreeCandidates(ctx, req)
	require.NoError(t, err, "pg ListArchiveWorktreeCandidates")

	assert.Equal(t, localCandidates, pgCandidates,
		"pg archive-wide candidates must match SQLite exactly")

	require.Len(t, localCandidates, 5,
		"snapshot, aggregate, fallback, unavailable, and other-machine fallback groups")
	assert.Equal(t, "host-a.example", localCandidates[0].Machine)
	assert.Equal(t, "snapshot", localCandidates[0].EvidenceKind)
	assert.Equal(t, 2, localCandidates[0].ContributingSessions,
		"archive-wide selection covers both the 2020 and 2025 sessions")
	assert.Equal(t, "aggregate", localCandidates[1].EvidenceKind)
	assert.Equal(t, "fallback", localCandidates[2].EvidenceKind)
	assert.Equal(t, "unavailable", localCandidates[3].EvidenceKind)
	assert.False(t, localCandidates[3].Available)
	assert.Equal(t, "host-b.example", localCandidates[4].Machine,
		"the other-machine session forms its own group despite sharing a cwd")
	assert.Equal(t, "fallback", localCandidates[4].EvidenceKind)
}

// TestPGListArchiveWorktreeCandidatesKeyMismatch verifies the PG mirror
// matches SQLite's key-mismatch semantics exactly: a right label with a
// wrong project key returns an empty candidate list with no error, and an
// empty project key is rejected outright.
func TestPGListArchiveWorktreeCandidatesKeyMismatch(t *testing.T) {
	const schema = "agentsview_worktree_candidates_key_mismatch_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)
	const project = "mismatch-project"

	seedPGCandidateSession(t, localDB, "session-a", project, "host-a.example",
		"/srv/worktrees/repo/feature", "2025-06-02T10:00:00Z")
	setPGCandidateSnapshot(t, ctx, localDB, "session-a", project, "host-a.example",
		"/srv/worktrees/repo", "/srv/worktrees/repo/feature")

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	pgStore := &Store{pg: pg}

	candidates, err := pgStore.ListArchiveWorktreeCandidates(ctx,
		db.ArchiveWorktreeCandidateRequest{
			ProjectLabel: export.SafeProjectDisplayLabel(project),
			ProjectKey:   "wrong-key",
		})
	require.NoError(t, err)
	assert.Empty(t, candidates,
		"right label with wrong key must return no candidates, no error")

	_, err = pgStore.ListArchiveWorktreeCandidates(ctx,
		db.ArchiveWorktreeCandidateRequest{
			ProjectLabel: export.SafeProjectDisplayLabel(project),
			ProjectKey:   "",
		})
	require.Error(t, err, "empty project key must error")
}
