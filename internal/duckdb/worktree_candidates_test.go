//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

// seedDuckCandidateSession inserts a minimal session with one message, using
// duckPushMachine (see project_inventory_test.go) since DuckDB's push
// collapses every session's machine onto the sync harness's fixed value.
func seedDuckCandidateSession(
	t *testing.T, local *db.DB, id, project, cwd, started string,
) {
	t.Helper()
	ended := started
	require.NoError(t, local.UpsertSession(db.Session{
		ID: id, Project: project, Machine: duckPushMachine, Agent: "codex", Cwd: cwd,
		StartedAt: &started, EndedAt: &ended, MessageCount: 1,
	}), "UpsertSession %s", id)
	require.NoError(t, local.InsertMessages([]db.Message{{
		SessionID: id, Ordinal: 0, Role: "assistant", Content: "hi", ContentLength: 2,
	}}), "InsertMessages %s", id)
}

// setDuckCandidateSnapshot publishes resolved snapshot evidence for a
// session via the public UpsertProjectIdentityObservation API (SessionID
// set), overriding the placeholder row the sessions-insert trigger created.
func setDuckCandidateSnapshot(
	t *testing.T, ctx context.Context, local *db.DB, id, project, root, worktreeRoot string,
) {
	t.Helper()
	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: id, Project: project, Machine: duckPushMachine,
			RootPath: root, WorktreeRootPath: worktreeRoot,
			RemoteResolution: export.ProjectResolutionResolved,
			ObservedAt:       time.Date(2025, 6, 2, 10, 0, 0, 0, time.UTC),
		}), "set candidate snapshot for %s", id)
}

// seedDuckCandidateSessionNoSnapshot inserts a session with no identity
// snapshot at all, deleting the sessions-insert trigger's placeholder row
// (root_path = cwd, no worktree root, no key source) via the public
// UpsertSessionWithProjectIdentity API's empty-snapshotProject deletion
// path. A bare placeholder must not survive to the pushed mirror: push
// sanitization (export.SanitizeStoredProjectIdentityObservation) derives a
// non-empty KeySource from any non-empty RootPath, including a placeholder's
// raw cwd, which would make an uninspected session look like snapshot
// evidence once mirrored. internal/db's own worktree_candidates_test.go
// sidesteps this the same way, deleting the placeholder for its
// aggregate/fallback/unavailable fixture rows rather than relying on it
// staying inert.
func seedDuckCandidateSessionNoSnapshot(
	t *testing.T, ctx context.Context, local *db.DB, id, project, cwd, started string,
) {
	t.Helper()
	ended := started
	require.NoError(t, local.UpsertSessionWithProjectIdentity(
		db.Session{
			ID: id, Project: project, Machine: duckPushMachine, Agent: "codex", Cwd: cwd,
			StartedAt: &started, EndedAt: &ended, MessageCount: 1,
		},
		export.ProjectIdentityObservation{
			SessionID: id, Project: project, Machine: duckPushMachine,
		},
		"",
	), "UpsertSessionWithProjectIdentity %s", id)
	require.NoError(t, local.InsertMessages([]db.Message{{
		SessionID: id, Ordinal: 0, Role: "assistant", Content: "hi", ContentLength: 2,
	}}), "InsertMessages %s", id)
}

// TestDuckWorktreeCandidatesArchiveWideMatchesSQLite seeds one session of
// each evidence kind (snapshot, aggregate, exact-cwd fallback, unavailable)
// -- reusing Task 7's fixture shapes -- pushes them through the real DuckDB
// sync path, and confirms the mirror's ListArchiveWorktreeCandidates output
// is identical to SQLite's for the same archive. It also proves
// archive-wideness: the snapshot group spans an old (2020) and a new (2025)
// session, both of which must appear in the combined group.
func TestDuckWorktreeCandidatesArchiveWideMatchesSQLite(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	const project = "candidate-project"

	seedDuckCandidateSession(t, local, "old-session", project,
		"/srv/worktrees/repo/feature/cmd", "2020-01-01T10:00:00Z")
	seedDuckCandidateSession(t, local, "new-session", project,
		"/srv/worktrees/repo/feature/frontend", "2025-06-02T10:00:00Z")
	setDuckCandidateSnapshot(t, ctx, local, "old-session", project,
		"/srv/worktrees/repo", "/srv/worktrees/repo/feature")
	setDuckCandidateSnapshot(t, ctx, local, "new-session", project,
		"/srv/worktrees/repo", "/srv/worktrees/repo/feature")

	seedDuckCandidateSessionNoSnapshot(t, ctx, local, "aggregate-session", project,
		"/srv/checkouts/repo/docs", "2025-06-02T10:00:00Z")
	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: project, Machine: duckPushMachine,
			RootPath: "/srv/checkouts/repo",
		}), "seed aggregate evidence")

	seedDuckCandidateSessionNoSnapshot(t, ctx, local, "fallback-session", project,
		"/opt/unknown/repo", "2025-06-02T10:00:00Z")

	seedDuckCandidateSession(t, local, "unavailable-session", project,
		"", "2025-06-02T10:00:00Z")

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	pushDataReadMirror(t, ctx, syncer)

	projects, err := local.BuildProjectIdentityMap(ctx, []string{project})
	require.NoError(t, err, "local BuildProjectIdentityMap")
	req := db.ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel(project),
		ProjectKey:   projects[project].ProjectKey,
	}

	localCandidates, err := local.ListArchiveWorktreeCandidates(ctx, req)
	require.NoError(t, err, "local ListArchiveWorktreeCandidates")

	duckStore := NewStoreFromDB(syncer.DB())
	duckCandidates, err := duckStore.ListArchiveWorktreeCandidates(ctx, req)
	require.NoError(t, err, "duckdb ListArchiveWorktreeCandidates")

	assert.Equal(t, localCandidates, duckCandidates,
		"duckdb archive-wide candidates must match SQLite exactly")

	require.Len(t, localCandidates, 4,
		"snapshot, aggregate, fallback, and unavailable groups")
	assert.Equal(t, "snapshot", localCandidates[0].EvidenceKind)
	assert.Equal(t, 2, localCandidates[0].ContributingSessions,
		"archive-wide selection covers both the 2020 and 2025 sessions")
	assert.Equal(t, "aggregate", localCandidates[1].EvidenceKind)
	assert.Equal(t, "fallback", localCandidates[2].EvidenceKind)
	assert.Equal(t, "unavailable", localCandidates[3].EvidenceKind)
	assert.False(t, localCandidates[3].Available)
}

// TestDuckListArchiveWorktreeCandidatesKeyMismatch verifies the DuckDB
// mirror matches SQLite's key-mismatch semantics exactly: a right label with
// a wrong project key returns an empty candidate list with no error, and an
// empty project key is rejected outright.
func TestDuckListArchiveWorktreeCandidatesKeyMismatch(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	const project = "mismatch-project"

	seedDuckCandidateSession(t, local, "session-a", project,
		"/srv/worktrees/repo/feature", "2025-06-02T10:00:00Z")
	setDuckCandidateSnapshot(t, ctx, local, "session-a", project,
		"/srv/worktrees/repo", "/srv/worktrees/repo/feature")

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	pushDataReadMirror(t, ctx, syncer)

	duckStore := NewStoreFromDB(syncer.DB())

	candidates, err := duckStore.ListArchiveWorktreeCandidates(ctx,
		db.ArchiveWorktreeCandidateRequest{
			ProjectLabel: export.SafeProjectDisplayLabel(project),
			ProjectKey:   "wrong-key",
		})
	require.NoError(t, err)
	assert.Empty(t, candidates,
		"right label with wrong key must return no candidates, no error")

	_, err = duckStore.ListArchiveWorktreeCandidates(ctx,
		db.ArchiveWorktreeCandidateRequest{
			ProjectLabel: export.SafeProjectDisplayLabel(project),
			ProjectKey:   "",
		})
	require.Error(t, err, "empty project key must error")
}
