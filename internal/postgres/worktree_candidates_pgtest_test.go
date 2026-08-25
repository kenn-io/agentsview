//go:build pgtest

package postgres

import (
	"context"
	"path/filepath"
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

// seedPGCandidateSessionNoSnapshot leaves the sessions-insert trigger's
// uninspected placeholder in place. Publication must omit that row so the
// mirror sees the same non-authoritative evidence as SQLite.
func seedPGCandidateSessionNoSnapshot(
	t *testing.T, _ context.Context, localDB *db.DB,
	id, project, machine, cwd, started string,
) {
	t.Helper()
	seedPGCandidateSession(
		t, localDB, id, project, machine, cwd, started,
	)
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
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID: "zero-message-session", Project: project, Machine: machine,
		Agent: "codex",
	}), "seed zero-message session")

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
	assert.Equal(t, 2, localCandidates[3].ContributingSessions,
		"PG and SQLite include zero-message inventory sessions")
	assert.Equal(t, "host-b.example", localCandidates[4].Machine,
		"the other-machine session forms its own group despite sharing a cwd")
	assert.Equal(t, "fallback", localCandidates[4].EvidenceKind)
}

func TestPGWorktreeCandidatesExcludeDifferentProjectKeys(t *testing.T) {
	const schema = "agentsview_worktree_candidates_alias_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)
	const (
		primary = "current-project-name"
		alias   = "historical-project-name"
		machine = "host-a.example"
		remote  = "https://example.com/team/repository.git"
	)
	for _, fixture := range []struct {
		id, project, cwd string
	}{
		{"primary-session", primary, "/srv/worktrees/repo/feature/cmd"},
		{"alias-session", alias, "/srv/worktrees/repo/feature/frontend"},
	} {
		seedPGCandidateSession(
			t, localDB, fixture.id, fixture.project, machine, fixture.cwd,
			"2025-06-02T10:00:00Z",
		)
		require.NoError(t, localDB.UpsertProjectIdentityObservation(ctx,
			export.ProjectIdentityObservation{
				SessionID: fixture.id, Project: fixture.project, Machine: machine,
				RootPath:         "/srv/worktrees/repo",
				WorktreeRootPath: "/srv/worktrees/repo/feature",
				GitRemote:        remote,
				RemoteResolution: export.ProjectResolutionResolved,
				ObservedAt:       time.Date(2025, 6, 2, 10, 0, 0, 0, time.UTC),
			},
		))
	}
	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err)

	projects, err := localDB.BuildProjectIdentityMap(
		ctx, []string{primary, alias},
	)
	require.NoError(t, err)
	require.NotNil(t, projects[primary].Identity)
	require.NotNil(t, projects[alias].Identity)
	require.Equal(t, projects[primary].Identity.Key, projects[alias].Identity.Key)
	require.NotEqual(t, projects[primary].ProjectKey, projects[alias].ProjectKey)
	request := db.ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel(primary),
		ProjectKey:   projects[primary].ProjectKey,
	}
	localCandidates, err :=
		localDB.ListArchiveWorktreeCandidates(ctx, request)
	require.NoError(t, err)
	pgCandidates, err :=
		(&Store{pg: pg}).ListArchiveWorktreeCandidates(ctx, request)
	require.NoError(t, err)

	assert.Equal(t, localCandidates, pgCandidates)
	require.Len(t, pgCandidates, 1)
	assert.Equal(t, 1, pgCandidates[0].ContributingSessions)
	require.Len(t, pgCandidates[0].Examples, 1)
	assert.Equal(t, "primary-session", pgCandidates[0].Examples[0].SessionID)
}

func TestPGWorktreeCandidatesUseSessionDatabaseGeneration(t *testing.T) {
	const (
		schema        = "agentsview_worktree_candidates_generation_test"
		sessionID     = "generation-session"
		machine       = "host-a.example"
		oldProject    = "old-project"
		newProject    = "new-project"
		oldGeneration = "zzzz-old-generation"
		newGeneration = "aaaa-new-generation"
	)
	_, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)
	newFilteredSync := func(source *db.DB, project string) *Sync {
		scope := pushSyncStateScope("", []string{project}, nil)
		return &Sync{
			pg: pg, local: source, machine: "workstation",
			schema: schema, schemaDone: true,
			syncState:          newScopedSyncStateStore(source, scope, false),
			aliasBackfillState: newScopedSyncStateStore(source, "", false),
			projects:           []string{project},
		}
	}

	require.NoError(t, localDB.SetDatabaseIDForTest(ctx, oldGeneration))
	seedPGCandidateSession(t, localDB, sessionID, oldProject, machine,
		"/srv/old/repo/worktree", "2025-06-02T10:00:00Z")
	setPGCandidateSnapshot(t, ctx, localDB, sessionID, oldProject, machine,
		"/srv/old/repo", "/srv/old/repo/worktree")
	_, err := newFilteredSync(localDB, oldProject).Push(ctx, false, nil)
	require.NoError(t, err, "push old generation")

	archiveID, err := localDB.GetArchiveID(ctx)
	require.NoError(t, err)
	archiveSalt, err := localDB.GetArchiveSalt(ctx)
	require.NoError(t, err)
	newLocalDB, err := db.Open(filepath.Join(t.TempDir(), "new-local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, newLocalDB.Close()) })
	require.NoError(t, newLocalDB.SetArchiveIdentityForTest(
		ctx, archiveID, archiveSalt,
	))
	require.NoError(t, newLocalDB.SetDatabaseIDForTest(ctx, newGeneration))
	markerID, err := localDB.GetSyncState(pushMarkerIDStateKey)
	require.NoError(t, err)
	require.NotEmpty(t, markerID)
	require.NoError(t, newLocalDB.SetSyncState(pushMarkerIDStateKey, markerID))
	seedPGCandidateSession(t, newLocalDB, sessionID, newProject, machine,
		"/srv/new/repo/worktree", "2025-06-02T10:00:00Z")
	setPGCandidateSnapshot(t, ctx, newLocalDB, sessionID, newProject, machine,
		"/srv/new/repo", "/srv/new/repo/worktree")
	_, err = newFilteredSync(newLocalDB, newProject).Push(ctx, false, nil)
	require.NoError(t, err, "push new generation")

	var snapshotCount int
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM source_session_project_identity_snapshots
		WHERE source_session_id = $1`, sessionID,
	).Scan(&snapshotCount))
	require.Equal(t, 2, snapshotCount,
		"independent filtered scopes preserve both database generations")

	store := &Store{pg: pg}
	projects, err := store.BuildProjectIdentityMap(ctx, []string{newProject})
	require.NoError(t, err)
	request := db.ArchiveWorktreeCandidateRequest{
		ProjectLabel: export.SafeProjectDisplayLabel(newProject),
		ProjectKey:   projects[newProject].ProjectKey,
	}
	candidates, err := store.ListArchiveWorktreeCandidates(ctx, request)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, "snapshot", candidates[0].EvidenceKind)
	assert.Equal(t, "/srv/new/repo/worktree", candidates[0].EvidenceRoot)
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
