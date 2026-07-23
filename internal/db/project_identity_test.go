package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

func TestProjectObservationDatabaseIDIsCreatedAndStable(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	first, err := d.GetDatabaseID(ctx)
	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
	), first)

	second, err := d.GetOrCreateDatabaseID(ctx)
	require.NoError(t, err)
	assert.Equal(t, first, second)

	require.NoError(t, d.SetDatabaseIDForTest(ctx, "test-database-id"))
	overridden, err := d.GetOrCreateDatabaseID(ctx)
	require.NoError(t, err)
	assert.Equal(t, "test-database-id", overridden)
}

func TestProjectObservationArchiveIDIsCreatedAndStable(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	first, err := d.GetArchiveID(ctx)
	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
	), first)

	second, err := d.GetOrCreateArchiveID(ctx)
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestProjectIdentityPublicationRevisionTracksSnapshotChanges(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	before, err := d.ProjectIdentityPublicationRevision(ctx)
	require.NoError(t, err)

	require.NoError(t, d.UpsertSession(Session{
		ID: "revision-session", Project: "app", Machine: "local", Agent: "codex",
	}))
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "revision-session", Project: "app", Machine: "local",
			RootPath: "/tmp/app", ObservedAt: time.Now().UTC(),
		}))
	afterInsert, err := d.ProjectIdentityPublicationRevision(ctx)
	require.NoError(t, err)
	assert.Greater(t, afterInsert, before)

	require.NoError(t, d.DeleteSession("revision-session"))
	afterDelete, err := d.ProjectIdentityPublicationRevision(ctx)
	require.NoError(t, err)
	assert.Greater(t, afterDelete, afterInsert)
}

func TestSessionProjectIdentitySnapshotPreservesFirstRootKeyUntilRemoteEvidence(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertSession(Session{
		ID: "root-snapshot", Project: "app", Machine: "local", Agent: "codex",
	}))
	firstObservedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "root-snapshot", Project: "app", Machine: "local",
			RootPath: "/first/app", ObservedAt: firstObservedAt,
		}))
	first, err := d.listSessionProjectIdentitySnapshots(
		ctx, []string{"root-snapshot"},
	)
	require.NoError(t, err)
	require.Contains(t, first, "root-snapshot")
	require.NotEmpty(t, first["root-snapshot"].Key)

	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "root-snapshot", Project: "app", Machine: "local",
			RootPath: "/later/app", ObservedAt: firstObservedAt.Add(time.Hour),
		}))
	afterRoot, err := d.listSessionProjectIdentitySnapshots(
		ctx, []string{"root-snapshot"},
	)
	require.NoError(t, err)
	assert.Equal(t, "/first/app", afterRoot["root-snapshot"].RootPath)
	assert.Equal(t, first["root-snapshot"].Key,
		afterRoot["root-snapshot"].Key)

	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "root-snapshot", Project: "app", Machine: "local",
			RootPath: "/later/app", GitRemote: "https://example.com/example/app.git",
			ObservedAt: firstObservedAt.Add(2 * time.Hour),
		}))
	afterRemote, err := d.listSessionProjectIdentitySnapshots(
		ctx, []string{"root-snapshot"},
	)
	require.NoError(t, err)
	assert.Equal(t, export.ProjectResolutionResolved,
		afterRemote["root-snapshot"].RemoteResolution)
	assert.Equal(t, "https://example.com/example/app.git",
		afterRemote["root-snapshot"].GitRemote)
	assert.NotEqual(t, first["root-snapshot"].Key,
		afterRemote["root-snapshot"].Key)

	require.NoError(t, d.UpsertSession(Session{
		ID: "ambiguous-snapshot", Project: "app", Machine: "local", Agent: "codex",
	}))
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "ambiguous-snapshot", Project: "app", Machine: "local",
			RootPath: "/first/ambiguous", ObservedAt: firstObservedAt,
		}))
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "ambiguous-snapshot", Project: "app", Machine: "local",
			RootPath: "/later/ambiguous", RemoteCandidateCount: 2,
			RemoteResolution: export.ProjectResolutionAmbiguous,
			ObservedAt:       firstObservedAt.Add(time.Hour),
		}))
	afterAmbiguous, err := d.listSessionProjectIdentitySnapshots(
		ctx, []string{"ambiguous-snapshot"},
	)
	require.NoError(t, err)
	assert.Equal(t, export.ProjectResolutionAmbiguous,
		afterAmbiguous["ambiguous-snapshot"].RemoteResolution)
	assert.Equal(t, 2,
		afterAmbiguous["ambiguous-snapshot"].RemoteCandidateCount)
}

func TestLoadProjectIdentityPublicationDeltaReturnsRowsAndTombstones(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	observedAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	for _, project := range []string{"alpha", "beta"} {
		sessionID := "identity-" + project
		require.NoError(t, d.UpsertSession(Session{
			ID: sessionID, Project: project, Machine: "local", Agent: "codex",
		}))
		require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
			export.ProjectIdentityObservation{
				SessionID: sessionID, Project: project, Machine: "local",
				RootPath:         "/workspace/" + project,
				GitRemote:        "https://example.com/" + project + ".git",
				GitRemoteName:    "origin",
				RemoteResolution: export.ProjectResolutionResolved,
				ObservedAt:       observedAt,
			},
		))
	}
	baseline, err := d.ProjectIdentityPublicationRevision(ctx)
	require.NoError(t, err)

	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "identity-alpha", Project: "alpha", Machine: "local",
			RootPath:         "/workspace/alpha",
			GitRemote:        "https://example.com/alpha.git",
			GitRemoteName:    "upstream",
			RemoteResolution: export.ProjectResolutionResolved,
			ObservedAt:       observedAt.Add(time.Hour),
		},
	))
	_, err = d.rawWriter().ExecContext(ctx, `
		DELETE FROM project_identity_observations
		WHERE project = ? AND machine = ? AND root_path = ? AND git_remote = ?`,
		"beta", "local", "/workspace/beta", "https://example.com/beta.git",
	)
	require.NoError(t, err)
	require.NoError(t, d.DeleteSession("identity-alpha"))
	through, err := d.ProjectIdentityPublicationRevision(ctx)
	require.NoError(t, err)

	delta, err := d.LoadProjectIdentityPublicationDelta(
		ctx, baseline, through, nil, nil,
	)
	require.NoError(t, err)
	require.Len(t, delta.Observations, 1)
	assert.Equal(t, "alpha", delta.Observations[0].Project)
	assert.Equal(t, "upstream", delta.Observations[0].GitRemoteName)
	assert.Equal(t, []ProjectIdentityObservationKey{{
		Project: "beta", Machine: "local", RootPath: "/workspace/beta",
		GitRemote: "https://example.com/beta.git",
	}}, delta.ObservationDeletes)
	assert.Empty(t, delta.Snapshots)
	assert.Equal(t, []SessionProjectIdentitySnapshotKey{{
		SessionID: "identity-alpha", Project: "alpha",
	}}, delta.SnapshotDeletes)

	betaDelta, err := d.LoadProjectIdentityPublicationDelta(
		ctx, baseline, through, []string{"beta"}, nil,
	)
	require.NoError(t, err)
	assert.Empty(t, betaDelta.Observations)
	assert.Equal(t, delta.ObservationDeletes, betaDelta.ObservationDeletes)
	assert.Empty(t, betaDelta.Snapshots)
	assert.Empty(t, betaDelta.SnapshotDeletes)
}

func TestCopyArchiveIdentityFromPreservesLogicalArchiveAndNewGeneration(
	t *testing.T,
) {
	ctx := context.Background()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(sourcePath)
	require.NoError(t, err)
	require.NoError(t, source.SetDatabaseIDForTest(ctx, "source-generation"))
	require.NoError(t, source.SetArchiveIdentityForTest(
		ctx, "stable-archive", strings.Repeat("a", 64),
	))
	require.NoError(t, source.Close())

	target := testDB(t)
	require.NoError(t, target.SetDatabaseIDForTest(ctx, "new-generation"))
	require.NoError(t, target.SetArchiveIdentityForTest(
		ctx, "temporary-archive", strings.Repeat("b", 64),
	))
	require.NoError(t, target.CopyArchiveIdentityFrom(sourcePath))

	archiveID, err := target.GetArchiveID(ctx)
	require.NoError(t, err)
	assert.Equal(t, "stable-archive", archiveID)
	archiveSalt, err := target.GetArchiveSalt(ctx)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("a", 64), archiveSalt)
	databaseID, err := target.GetDatabaseID(ctx)
	require.NoError(t, err)
	assert.Equal(t, "new-generation", databaseID)
}

func TestProjectObservationArchiveSaltIsCreatedAndStable(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	first, err := d.GetArchiveSalt(ctx)
	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), first)

	second, err := d.GetOrCreateArchiveSalt(ctx)
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestProjectObservationArchiveSaltRejectsCorruptPersistedValue(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.SetArchiveIdentityForTest(ctx, "archive", "not-a-salt"))

	_, err := d.GetArchiveSalt(ctx)
	require.ErrorIs(t, err, ErrArchiveSaltInvalid)
	_, err = d.GetOrCreateArchiveSalt(ctx)
	require.ErrorIs(t, err, ErrArchiveSaltInvalid,
		"a corrupt established salt must not be silently replaced")
}

func TestCopyArchiveIdentityFromRejectsCorruptSalt(t *testing.T) {
	ctx := context.Background()
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	source, err := Open(sourcePath)
	require.NoError(t, err)
	require.NoError(t, source.SetArchiveIdentityForTest(
		ctx, "archive", "truncated",
	))
	require.NoError(t, source.Close())

	target := testDB(t)
	err = target.CopyArchiveIdentityFrom(sourcePath)
	require.ErrorIs(t, err, ErrArchiveSaltInvalid)
}

func TestProjectIdentityObservationRoundTripsRepositoryContext(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	want := export.ProjectIdentityObservation{
		SourceArchiveID:      "archive-source",
		SourceArchiveSalt:    "archive-salt",
		Project:              "app",
		Machine:              "host",
		RootPath:             "/private/app/worktree",
		RepositoryPath:       "/private/app/.git",
		WorktreeRootPath:     "/private/app/worktree",
		WorktreeRelationship: export.WorktreeLinked,
		CheckoutState:        export.CheckoutDetached,
		GitBranch:            "",
		RemoteResolution:     export.ProjectResolutionAmbiguous,
		RemoteCandidateCount: 2,
		ObservedAt:           time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
	}

	require.NoError(t, d.UpsertProjectIdentityObservation(ctx, want))
	got, err := d.ListProjectIdentityObservations(ctx, []string{"app"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, want.RepositoryPath, got[0].RepositoryPath)
	assert.Equal(t, want.SourceArchiveID, got[0].SourceArchiveID)
	assert.Equal(t, want.SourceArchiveSalt, got[0].SourceArchiveSalt)
	assert.Equal(t, want.WorktreeRelationship, got[0].WorktreeRelationship)
	assert.Equal(t, want.CheckoutState, got[0].CheckoutState)
	assert.Equal(t, want.RemoteResolution, got[0].RemoteResolution)
	assert.Equal(t, want.RemoteCandidateCount, got[0].RemoteCandidateCount)
}

func TestProjectObservationDatabaseIDInitializedForReadOnlyOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	writable := testDBAtPath(t, path, "read-only database id seed")
	seeded, err := writable.GetDatabaseID(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, seeded)
	require.NoError(t, writable.Close())

	readonly, err := OpenReadOnly(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readonly.Close()) })

	got, err := readonly.GetDatabaseID(context.Background())
	require.NoError(t, err)
	assert.Equal(t, seeded, got)
}

func TestProjectObservationDatabaseIDReadOnlyExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	writable := testDBAtPath(t, path, "read-only database id seed")
	require.NoError(t, writable.SetDatabaseIDForTest(
		context.Background(), "read-only-db-id"))
	require.NoError(t, writable.Close())

	readonly, err := OpenReadOnly(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readonly.Close()) })

	got, err := readonly.GetDatabaseID(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "read-only-db-id", got)
}

func TestProjectObservationDatabaseIDMissingDoesNotCreate(t *testing.T) {
	d := testDB(t)
	_, err := d.rawWriter().Exec(`
		DELETE FROM archive_metadata WHERE key = ?`,
		archiveMetadataDatabaseIDKey,
	)
	require.NoError(t, err)

	_, err = d.GetDatabaseID(context.Background())
	require.ErrorIs(t, err, ErrDatabaseIDMissing)

	var count int
	require.NoError(t, d.rawReader().QueryRow(`
		SELECT COUNT(*) FROM archive_metadata
		WHERE key = ?`, archiveMetadataDatabaseIDKey).Scan(&count))
	assert.Zero(t, count)
}

func TestProjectObservationRawValuesAreAuthoritativeForExportKeys(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	obs := export.ProjectIdentityObservation{
		Project:          "app",
		Machine:          "laptop",
		RootPath:         "/tmp/app",
		GitRemote:        "git@github.com:Org/Repo.git",
		GitRemoteName:    "origin",
		ObservedAt:       time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC),
		NormalizedRemote: "stale/normalized",
		KeySource:        "root_path",
		Key:              "stale-key",
	}
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx, obs))

	got, err := d.ListProjectIdentityObservations(ctx, []string{"app"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "github.com:Org/Repo.git", got[0].GitRemote)
	assert.Equal(t, "/tmp/app", got[0].RootPath)

	projects := export.BuildProjectsMap([]string{"app"}, got)
	require.Equal(t, export.ProjectResolutionResolved, projects["app"].Resolution)
	require.NotNil(t, projects["app"].Identity)
	assert.Equal(t, "github.com/Org/Repo", projects["app"].Identity.NormalizedRemote)
	assert.Equal(t, export.ProjectKindGitRemote, projects["app"].Identity.Kind)
	assert.NotEmpty(t, projects["app"].Identity.Key)
}

func TestProjectObservationStripsGitRemoteCredentialsBeforeStorage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:       "app",
			Machine:       "laptop",
			RootPath:      "/tmp/app",
			GitRemote:     "https://" + "user:token@" + "example.com/acme/app.git",
			GitRemoteName: "origin",
		},
	))

	got, err := d.ListProjectIdentityObservations(ctx, []string{"app"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "https://example.com/acme/app.git", got[0].GitRemote)
	assert.Equal(t, "example.com/acme/app", got[0].NormalizedRemote)
	assert.Equal(t, export.ProjectIdentityKeySourceGitRemote, got[0].KeySource)
}

func TestProjectObservationMigrationStripsStoredGitRemoteCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d := testDBAtPath(t, path, "credential migration seed")
	_, err := d.rawWriter().Exec(`
		INSERT INTO project_identity_observations (
			project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"app", "laptop", "/tmp/app",
		"https://"+"user:token@"+"example.com/acme/app.git", "origin",
		"", "", "2026-07-03T12:00:00Z",
		"example.com/acme/app", export.ProjectIdentityKeySourceGitRemote,
		projectIdentitySHA("git_remote\n"+"example.com/acme/app"),
	)
	require.NoError(t, err)
	_, err = d.rawWriter().Exec(`DELETE FROM stats WHERE key = ?`,
		projectIdentityRemoteScrubCompletedKey)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })

	got, err := reopened.ListProjectIdentityObservations(
		context.Background(), []string{"app"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "https://example.com/acme/app.git", got[0].GitRemote)
	assert.Equal(t, "example.com/acme/app", got[0].NormalizedRemote)
	var scrubbed string
	require.NoError(t, reopened.getReader().QueryRowContext(
		context.Background(), `SELECT value FROM stats WHERE key = ?`,
		projectIdentityRemoteScrubCompletedKey,
	).Scan(&scrubbed))
	assert.Equal(t, "1", scrubbed)
}

func TestProjectObservationListFiltersLabelsAndKeepsPersistedRemoteMachineRows(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:   "old-project",
			Machine:   "remote-host",
			RootPath:  "remote-host:/srv/app",
			GitRemote: "https://github.com/acme/old-project.git",
		},
	))
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:   "other",
			Machine:   "remote-host",
			RootPath:  "s3://bucket/archive",
			GitRemote: "https://github.com/acme/other.git",
		},
	))

	got, err := d.ListProjectIdentityObservations(ctx, []string{"old-project"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "remote-host", got[0].Machine)
	assert.Equal(t, "remote-host:/srv/app", got[0].RootPath)
	assert.Equal(t, "https://github.com/acme/old-project.git", got[0].GitRemote)
}

func TestProjectIdentityNilLabelsListAllButEmptyLabelsMapEmpty(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedProjectIdentityObservation(t, d, "observed-project")

	all, err := d.ListProjectIdentityObservations(ctx, nil)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "observed-project", all[0].Project)

	empty, err := d.BuildProjectIdentityMap(ctx, []string{})
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestListProjectIdentityObservationsChunksLargeLabelLists(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Cross the maxSQLVars chunk boundary and include duplicate labels that
	// straddle it, proving chunked queries return each row exactly once and
	// preserve the single-query (project, machine, ...) ordering.
	const labelCount = maxSQLVars + 50
	labels := make([]string, 0, 2*labelCount)
	var sb strings.Builder
	sb.WriteString(`INSERT INTO project_identity_observations
		(project, machine, root_path, observed_at) VALUES `)
	args := make([]any, 0, labelCount)
	for i := range labelCount {
		label := fmt.Sprintf("chunked-project-%04d", i)
		labels = append(labels, label, label)
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?, 'host.example', '/srv/app', '2025-06-02T10:00:00Z')")
		args = append(args, label)
	}
	_, err := d.getWriter().Exec(sb.String(), args...)
	require.NoError(t, err, "seed chunked observations")

	// Reverse the (duplicated) label list to prove the lookup sorts it
	// before partitioning into chunks.
	slices.Reverse(labels)
	got, err := d.ListProjectIdentityObservations(ctx, labels)
	require.NoError(t, err)
	all, err := d.ListProjectIdentityObservations(ctx, nil)
	require.NoError(t, err)
	require.Len(t, all, labelCount)
	assert.Equal(t, all, got,
		"chunked label lookup must match the unfiltered scan, order included")
}

func TestProjectIdentityGoldenFixtureObservationsAreDeterministic(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	observedAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:          "remote-project",
			Machine:          "golden-host",
			RootPath:         "/fixtures/remote-project/worktrees/feature",
			GitRemote:        "https://github.com/acme/remote-project.git",
			GitRemoteName:    "origin",
			WorktreeName:     "feature",
			WorktreeRootPath: "/fixtures/remote-project",
			ObservedAt:       observedAt,
		},
	))
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:              "path-project",
			Machine:              "golden-host",
			RootPath:             "/fixtures/path-project",
			RepositoryPath:       "/fixtures/path-project/.git",
			WorktreeRelationship: export.WorktreeMain,
			ObservedAt:           observedAt,
		},
	))

	got, err := d.BuildProjectIdentityMap(ctx,
		[]string{"remote-project", "path-project", "unknown-project"})
	require.NoError(t, err)
	require.Equal(t, export.ProjectResolutionResolved,
		got["remote-project"].Resolution)
	require.NotNil(t, got["remote-project"].Identity)
	assert.Equal(t, export.ProjectKindGitRemote, got["remote-project"].Identity.Kind)
	assert.Equal(t, "github.com/acme/remote-project",
		got["remote-project"].Identity.NormalizedRemote)
	assert.NotEmpty(t, got["remote-project"].Identity.Key)

	require.Equal(t, export.ProjectResolutionResolved,
		got["path-project"].Resolution)
	require.NotNil(t, got["path-project"].Identity)
	assert.Equal(t, export.ProjectKindMachineRoot, got["path-project"].Identity.Kind)
	assert.Empty(t, got["path-project"].Identity.NormalizedRemote)
	encoded, err := json.Marshal(got["path-project"])
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "/fixtures/path-project")

	assert.Equal(t, export.ProjectResolutionUnknown,
		got["unknown-project"].Resolution)
	assert.Nil(t, got["unknown-project"].Identity)
}

func TestProjectObservationSessionBatchWritePersistsObservation(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	result, err := d.WriteSessionBatch([]SessionBatchWrite{{
		Session: Session{
			ID:      "batch-observation",
			Project: "mapped-project",
			Machine: "laptop",
			Agent:   "codex",
		},
		IdentityObservation: export.ProjectIdentityObservation{
			Project:          "mapped-project",
			Machine:          "laptop",
			RootPath:         "/tmp/worktree",
			WorktreeName:     "feature",
			WorktreeRootPath: "/tmp/worktree",
		},
		DataVersion:     CurrentDataVersion(),
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, result.WrittenSessions)

	got, err := d.ListProjectIdentityObservations(ctx, []string{"mapped-project"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "/tmp/worktree", got[0].RootPath)
	assert.Empty(t, got[0].GitRemote)
	snapshots, err := d.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "mapped-project", snapshots[0].Project,
		"an omitted snapshot project keeps the legacy same-label default")

	projects := export.BuildProjectsMap([]string{"mapped-project"}, got)
	require.Equal(t, export.ProjectResolutionUnknown, projects["mapped-project"].Resolution)
	assert.Nil(t, projects["mapped-project"].Identity)
}

func TestProjectObservationSessionBatchExplicitEmptyProjectOmitsSnapshot(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	emptySourceProject := ""
	result, err := d.WriteSessionBatch([]SessionBatchWrite{{
		Session: Session{
			ID:      "batch-empty-source",
			Project: "mapped-project",
			Machine: "laptop",
			Agent:   "codex",
		},
		IdentityObservation: export.ProjectIdentityObservation{
			SessionID: "batch-empty-source",
			Project:   "mapped-project",
			Machine:   "laptop",
			RootPath:  "/tmp/worktree",
		},
		IdentitySnapshotProject: &emptySourceProject,
		DataVersion:             CurrentDataVersion(),
		ReplaceMessages:         true,
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, result.WrittenSessions)

	observations, err := d.ListProjectIdentityObservations(
		ctx, []string{"mapped-project"},
	)
	require.NoError(t, err)
	require.Len(t, observations, 1)
	assert.Equal(t, "mapped-project", observations[0].Project)

	snapshots, err := d.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(t, err)
	assert.Empty(t, snapshots,
		"an explicit empty source must not become mapped snapshot evidence")
}

func TestSessionBatchWritesRejectMismatchedIdentityOwnership(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		writeBatch func(*DB, []SessionBatchWrite) (SessionBatchResult, error)
		wantErr    bool
	}{
		{
			name: "savepoint batch",
			writeBatch: func(
				d *DB, writes []SessionBatchWrite,
			) (SessionBatchResult, error) {
				return d.WriteSessionBatch(writes)
			},
		},
		{
			name: "atomic batch",
			writeBatch: func(
				d *DB, writes []SessionBatchWrite,
			) (SessionBatchResult, error) {
				return d.WriteSessionBatchAtomic(writes)
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			recordedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
			require.NoError(t, d.UpsertSession(Session{
				ID: "unrelated", Project: "unrelated-target",
				Machine: "laptop", Agent: "codex", Cwd: "/tmp/unrelated",
			}))
			require.NoError(t, d.UpsertProjectIdentityObservationWithSnapshotProject(
				ctx,
				export.ProjectIdentityObservation{
					SessionID: "unrelated", Project: "unrelated-target",
					Machine: "laptop", RootPath: "/tmp/unrelated",
					ObservedAt: recordedAt,
				},
				"unrelated-source",
			))
			before, err := d.ListSessionProjectIdentitySnapshots(ctx)
			require.NoError(t, err)
			require.Len(t, before, 1)

			candidateSource := "candidate-source"
			result, err := tc.writeBatch(d, []SessionBatchWrite{{
				Session: Session{
					ID: "candidate", Project: "candidate-target",
					Machine: "laptop", Agent: "codex", Cwd: "/tmp/candidate",
				},
				IdentityObservation: export.ProjectIdentityObservation{
					SessionID: "unrelated", Project: "candidate-target",
					Machine: "laptop", RootPath: "/tmp/candidate",
					ObservedAt: recordedAt.Add(time.Minute),
				},
				IdentitySnapshotProject: &candidateSource,
				DataVersion:             CurrentDataVersion(),
				ReplaceMessages:         true,
			}})
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, 1, result.FailedSessions)
			assert.Equal(t, 0, result.WrittenSessions)
			require.Len(t, result.Errors, 1)
			assert.ErrorContains(t, result.Errors[0],
				"does not match session id")

			candidate, getErr := d.GetSession(ctx, "candidate")
			require.NoError(t, getErr)
			assert.Nil(t, candidate,
				"invalid identity ownership must reject the session write")
			after, listErr := d.ListSessionProjectIdentitySnapshots(ctx)
			require.NoError(t, listErr)
			assert.Equal(t, before, after,
				"invalid identity ownership must not relabel another session")
		})
	}
}

func TestUpsertSessionWithProjectIdentityUsesCurrentTransactionInsertionState(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	session := Session{
		ID:      "atomic-identity",
		Project: "mapped-project",
		Machine: "laptop",
		Agent:   "codex",
		Cwd:     "/tmp/worktree",
	}
	observation := export.ProjectIdentityObservation{
		SessionID: "atomic-identity",
		Project:   "mapped-project",
		Machine:   "laptop",
		RootPath:  "/tmp/worktree",
	}

	require.NoError(t, d.UpsertSessionWithProjectIdentity(
		session, observation, "",
	))
	snapshots, err := d.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(t, err)
	assert.Empty(t, snapshots,
		"fresh empty source must remove its own trigger fallback")

	require.NoError(t, d.UpsertSessionWithProjectIdentity(
		session, observation, "source-project",
	))
	require.NoError(t, d.UpsertSessionWithProjectIdentity(
		session, observation, "",
	))

	stored, err := d.GetSession(ctx, "atomic-identity")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "mapped-project", stored.Project)
	observations, err := d.ListProjectIdentityObservations(
		ctx, []string{"mapped-project"},
	)
	require.NoError(t, err)
	require.Len(t, observations, 1)
	assert.Equal(t, "mapped-project", observations[0].Project)
	snapshots, err = d.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "source-project", snapshots[0].Project,
		"existing source evidence must survive an empty reparse")
}

func TestUpsertSessionWithProjectIdentityRejectsInvalidIdentityBeforeWrite(
	t *testing.T,
) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		sessionID   string
		observation export.ProjectIdentityObservation
	}{
		{
			name:      "empty session id",
			sessionID: "",
			observation: export.ProjectIdentityObservation{
				SessionID: "candidate", Project: "candidate-project",
				Machine: "laptop", RootPath: "/tmp/candidate",
			},
		},
		{
			name:      "empty observation session id",
			sessionID: "candidate",
			observation: export.ProjectIdentityObservation{
				Project: "candidate-project", Machine: "laptop",
				RootPath: "/tmp/candidate",
			},
		},
		{
			name:      "mismatched observation session id",
			sessionID: "candidate",
			observation: export.ProjectIdentityObservation{
				SessionID: "unrelated", Project: "candidate-project",
				Machine: "laptop", RootPath: "/tmp/candidate",
			},
		},
		{
			name:      "empty observation project",
			sessionID: "candidate",
			observation: export.ProjectIdentityObservation{
				SessionID: "candidate", Machine: "laptop",
				RootPath: "/tmp/candidate",
			},
		},
		{
			name:      "empty observation machine",
			sessionID: "candidate",
			observation: export.ProjectIdentityObservation{
				SessionID: "candidate", Project: "candidate-project",
				RootPath: "/tmp/candidate",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			recordedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
			require.NoError(t, d.UpsertSession(Session{
				ID: "unrelated", Project: "unrelated-target",
				Machine: "laptop", Agent: "codex", Cwd: "/tmp/unrelated",
			}))
			require.NoError(t, d.UpsertProjectIdentityObservationWithSnapshotProject(
				ctx,
				export.ProjectIdentityObservation{
					SessionID: "unrelated", Project: "unrelated-target",
					Machine: "laptop", RootPath: "/tmp/unrelated",
					GitRemote:  "https://example.com/acme/unrelated.git",
					ObservedAt: recordedAt,
				},
				"unrelated-source",
			))
			before, err := d.ListSessionProjectIdentitySnapshots(ctx)
			require.NoError(t, err)
			require.Len(t, before, 1)

			err = d.UpsertSessionWithProjectIdentity(
				Session{
					ID: tc.sessionID, Project: "candidate-project",
					Machine: "laptop", Agent: "codex", Cwd: "/tmp/candidate",
				},
				tc.observation,
				"",
			)
			assert.Error(t, err)

			candidate, getErr := d.GetSession(ctx, tc.sessionID)
			require.NoError(t, getErr)
			assert.Nil(t, candidate, "invalid input must not persist a session")
			after, listErr := d.ListSessionProjectIdentitySnapshots(ctx)
			require.NoError(t, listErr)
			assert.Equal(t, before, after,
				"invalid input must not alter unrelated immutable evidence")
		})
	}
}

func TestExplicitSnapshotProjectCorrectsOnlyLegacyProjectLabel(t *testing.T) {
	ctx := context.Background()
	recordedAt := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		existing export.ProjectIdentityObservation
		incoming export.ProjectIdentityObservation
	}{
		{
			name: "resolved",
			existing: export.ProjectIdentityObservation{
				RootPath:         "/legacy/resolved",
				GitRemote:        "https://example.com/acme/legacy.git",
				RemoteResolution: export.ProjectResolutionResolved,
			},
			incoming: export.ProjectIdentityObservation{
				RootPath:         "/incoming/resolved",
				GitRemote:        "https://example.com/acme/incoming.git",
				RemoteResolution: export.ProjectResolutionResolved,
			},
		},
		{
			name: "ambiguous",
			existing: export.ProjectIdentityObservation{
				RootPath: "/legacy/ambiguous", RemoteCandidateCount: 2,
				RemoteResolution: export.ProjectResolutionAmbiguous,
			},
			incoming: export.ProjectIdentityObservation{
				RootPath: "/incoming/ambiguous", RemoteCandidateCount: 3,
				RemoteResolution: export.ProjectResolutionAmbiguous,
			},
		},
		{
			name: "established unknown",
			existing: export.ProjectIdentityObservation{
				RootPath:         "/legacy/unknown",
				RemoteResolution: export.ProjectResolutionUnknown,
			},
			incoming: export.ProjectIdentityObservation{
				RootPath:         "/incoming/unknown",
				RemoteResolution: export.ProjectResolutionUnknown,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			sessionID := "legacy-" + strings.ReplaceAll(tc.name, " ", "-")
			require.NoError(t, d.UpsertSession(Session{
				ID: sessionID, Project: "mapped-target", Machine: "laptop",
				Agent: "codex", Cwd: "/legacy/worktree",
			}))
			existing := tc.existing
			existing.SessionID = sessionID
			existing.Project = "mapped-target"
			existing.Machine = "laptop"
			existing.ObservedAt = recordedAt
			require.NoError(t, d.UpsertProjectIdentityObservation(ctx, existing))

			beforeByID, err := d.listSessionProjectIdentitySnapshots(
				ctx, []string{sessionID},
			)
			require.NoError(t, err)
			before, ok := beforeByID[sessionID]
			require.True(t, ok)

			incoming := tc.incoming
			incoming.SessionID = sessionID
			incoming.Project = "mapped-target"
			incoming.Machine = "laptop"
			incoming.ObservedAt = recordedAt.Add(time.Hour)
			require.NoError(t, d.UpsertProjectIdentityObservationWithSnapshotProject(
				ctx, incoming, "parser-source",
			))

			afterByID, err := d.listSessionProjectIdentitySnapshots(
				ctx, []string{sessionID},
			)
			require.NoError(t, err)
			after, ok := afterByID[sessionID]
			require.True(t, ok)
			want := before
			want.Project = "parser-source"
			assert.Equal(t, want, after,
				"explicit correction must preserve all immutable evidence fields")

			require.NoError(t, d.UpsertProjectIdentityObservation(ctx, incoming))
			legacyByID, err := d.listSessionProjectIdentitySnapshots(
				ctx, []string{sessionID},
			)
			require.NoError(t, err)
			assert.Equal(t, want, legacyByID[sessionID],
				"legacy same-label publication must not relabel the snapshot")
		})
	}
}

func seedProjectIdentityObservation(t *testing.T, d *DB, project string) {
	t.Helper()
	require.NoError(t, d.UpsertProjectIdentityObservation(context.Background(),
		export.ProjectIdentityObservation{
			Project:   project,
			Machine:   "test-machine",
			RootPath:  "/tmp/" + project,
			GitRemote: "https://github.com/acme/" + project + ".git",
		},
	))
}

func TestProjectObservationRemoteReplacesSameRootFallback(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:  "app",
			Machine:  "laptop",
			RootPath: root,
		},
	))
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:       "app",
			Machine:       "laptop",
			RootPath:      root,
			GitRemote:     "https://github.com/acme/app.git",
			GitRemoteName: "origin",
		},
	))

	got, err := d.ListProjectIdentityObservations(ctx, []string{"app"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "https://github.com/acme/app.git", got[0].GitRemote)

	projects := export.BuildProjectsMap([]string{"app"}, got)
	require.Equal(t, export.ProjectResolutionResolved, projects["app"].Resolution)
	require.NotNil(t, projects["app"].Identity)
	assert.Equal(t, "github.com/acme/app", projects["app"].Identity.NormalizedRemote)
}

func TestProjectObservationFallbackDoesNotRecreateSameRootWhenRemoteExists(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:       "app",
			Machine:       "laptop",
			RootPath:      root,
			GitRemote:     "https://github.com/acme/app.git",
			GitRemoteName: "origin",
		},
	))
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:  "app",
			Machine:  "laptop",
			RootPath: root,
		},
	))

	got, err := d.ListProjectIdentityObservations(ctx, []string{"app"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "https://github.com/acme/app.git", got[0].GitRemote)
}

func TestProjectObservationAmbiguousSupersedesResolvedAggregate(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: "app", Machine: "laptop", RootPath: root,
			GitRemote: "https://github.com/acme/app.git",
		},
	))
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: "app", Machine: "laptop", RootPath: root,
			RemoteResolution:     export.ProjectResolutionAmbiguous,
			RemoteCandidateCount: 2,
		},
	))

	got, err := d.BuildProjectIdentityMap(ctx, []string{"app"})
	require.NoError(t, err)
	assert.Equal(t, export.ProjectResolutionAmbiguous, got["app"].Resolution)
	assert.Nil(t, got["app"].Identity)
}

func TestProjectObservationResolvedDoesNotDiscardAmbiguousEvidence(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: "app", Machine: "laptop", RootPath: root,
			RemoteResolution:     export.ProjectResolutionAmbiguous,
			RemoteCandidateCount: 2,
		},
	))
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: "app", Machine: "laptop", RootPath: root,
			GitRemote: "https://github.com/acme/app.git",
		},
	))

	got, err := d.BuildProjectIdentityMap(ctx, []string{"app"})
	require.NoError(t, err)
	assert.Equal(t, export.ProjectResolutionAmbiguous, got["app"].Resolution)
	assert.Nil(t, got["app"].Identity)
}

func TestProjectObservationUnknownDoesNotReplaceAmbiguousEvidence(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: "app", Machine: "laptop", RootPath: root,
			RemoteResolution:     export.ProjectResolutionAmbiguous,
			RemoteCandidateCount: 2,
		},
	))
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project: "app", Machine: "laptop", RootPath: root,
			RemoteResolution: export.ProjectResolutionUnknown,
		},
	))

	got, err := d.BuildProjectIdentityMap(ctx, []string{"app"})
	require.NoError(t, err)
	assert.Equal(t, export.ProjectResolutionAmbiguous, got["app"].Resolution)
	assert.Nil(t, got["app"].Identity)
}

func TestProjectObservationScrubDowngradesUnusableRemoteToFallback(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	_, err := d.getWriter().ExecContext(ctx, `
		INSERT INTO project_identity_observations (
			project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"app", "laptop", root, "file:///tmp/app.git", "origin",
		"", "", "2026-07-03T12:00:00Z", "", "", "",
	)
	require.NoError(t, err)

	tx, err := d.getWriter().BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, scrubProjectIdentityGitRemoteCredentialsTx(ctx, tx))
	require.NoError(t, tx.Commit())

	got, err := d.ListProjectIdentityObservations(ctx, []string{"app"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].GitRemote)
	assert.Equal(t, export.ProjectIdentityKeySourceRootPath, got[0].KeySource)
	assert.NotEmpty(t, got[0].Key)
}

func TestProjectObservationExportReadsDoNotMutateArchive(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:   "app",
			Machine:   "laptop",
			RootPath:  "/tmp/app",
			GitRemote: "https://github.com/acme/app.git",
		},
	))

	before := projectObservationRowCount(t, d)
	_, err := d.ListProjectIdentityObservations(ctx, []string{"app"})
	require.NoError(t, err)
	_, err = d.ListProjectIdentityObservations(ctx, []string{"missing"})
	require.NoError(t, err)
	assert.Equal(t, before, projectObservationRowCount(t, d))
}

func TestProjectObservationDatabaseIDConcurrentCreateReturnsSingleID(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	const workers = 64
	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make(chan string, workers)
	errs := make(chan error, workers)

	for range workers {
		wg.Go(func() {
			<-start
			id, err := d.GetOrCreateDatabaseID(ctx)
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		})
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errs)
	require.Empty(t, errs)

	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	require.Len(t, seen, 1)
}

func TestProjectIdentityMapUsesPersistedFactsWithoutFilesystemInference(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX-style temp git paths")
	}
	d := testDB(t)
	ctx := context.Background()

	persistedRoot := filepath.Join(t.TempDir(), "persisted")
	liveRoot := filepath.Join(t.TempDir(), "live")
	mappedRoot := filepath.Join(t.TempDir(), "mapped")
	fileParentRoot := filepath.Join(t.TempDir(), "file-parent")
	require.NoError(t, os.MkdirAll(filepath.Join(persistedRoot, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(liveRoot, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(mappedRoot, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(fileParentRoot, ".git"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(persistedRoot, ".git", "config"),
		[]byte("[remote \"origin\"]\n\turl = https://github.com/live/wrong.git\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(liveRoot, ".git", "config"),
		[]byte("[remote \"origin\"]\n\turl = https://github.com/acme/live.git\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(fileParentRoot, ".git", "config"),
		[]byte("[remote \"origin\"]\n\turl = https://github.com/acme/file-parent.git\n"),
		0o644,
	))

	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:   "persisted",
			Machine:   "laptop",
			RootPath:  persistedRoot,
			GitRemote: "git@github.com:acme/persisted.git",
		},
	))
	require.NoError(t, d.UpsertSession(Session{
		ID:      "persisted-session",
		Project: "persisted",
		Machine: "laptop",
		Agent:   "codex",
		Cwd:     persistedRoot,
	}))
	require.NoError(t, d.UpsertSession(Session{
		ID:      "live-session",
		Project: "live",
		Machine: "laptop",
		Agent:   "codex",
		Cwd:     liveRoot,
	}))
	require.NoError(t, d.UpsertSession(Session{
		ID:      "mapped-session",
		Project: "mapped",
		Machine: "laptop",
		Agent:   "codex",
		Cwd:     filepath.Join(mappedRoot, "feature"),
	}))
	fileParentPath := filepath.Join(fileParentRoot, "session.jsonl")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "file-parent-session",
		Project:  "file-parent",
		Machine:  "laptop",
		Agent:    "codex",
		FilePath: &fileParentPath,
	}))
	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine:    "laptop",
		PathPrefix: mappedRoot,
		Project:    "mapped",
		Enabled:    true,
	})
	require.NoError(t, err)
	require.NoError(t, d.UpsertSession(Session{
		ID:      "remote-session",
		Project: "remote",
		Machine: "remote-host",
		Agent:   "codex",
		Cwd:     "remote-host:/srv/app",
	}))

	before := projectObservationRowCount(t, d)
	got, err := d.BuildProjectIdentityMap(ctx,
		[]string{"persisted", "live", "mapped", "file-parent", "remote", "unknown"},
	)
	require.NoError(t, err)
	assert.Equal(t, before, projectObservationRowCount(t, d))

	require.Equal(t, export.ProjectResolutionResolved, got["persisted"].Resolution)
	require.NotNil(t, got["persisted"].Identity)
	assert.Equal(t, "github.com/acme/persisted", got["persisted"].Identity.NormalizedRemote)

	require.Equal(t, export.ProjectResolutionUnknown, got["live"].Resolution)
	assert.Nil(t, got["live"].Identity)

	require.Equal(t, export.ProjectResolutionUnknown, got["mapped"].Resolution)
	assert.Nil(t, got["mapped"].Identity)

	require.Equal(t, export.ProjectResolutionUnknown, got["file-parent"].Resolution)
	assert.Nil(t, got["file-parent"].Identity)

	assert.Equal(t, export.ProjectResolutionUnknown, got["remote"].Resolution)
	assert.Nil(t, got["remote"].Identity)
	assert.Equal(t, export.ProjectResolutionUnknown, got["unknown"].Resolution)
	assert.Nil(t, got["unknown"].Identity)
}

func TestProjectIdentityMapLegacyFallbackAcceptsWindowsDriveRoots(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertSession(Session{
		ID:      "windows-backslash-session",
		Project: "windows-backslash",
		Machine: "windows-host",
		Agent:   "codex",
		Cwd:     `C:\repo\`,
	}))
	require.NoError(t, d.UpsertSession(Session{
		ID:      "windows-slash-session",
		Project: "windows-slash",
		Machine: "windows-host",
		Agent:   "codex",
		Cwd:     "C:/repo/",
	}))
	require.NoError(t, d.UpsertSession(Session{
		ID:      "remote-prefixed-session",
		Project: "remote-prefixed",
		Machine: "remote-host",
		Agent:   "codex",
		Cwd:     "host:/srv/repo",
	}))

	got, err := d.BuildProjectIdentityMap(ctx,
		[]string{"windows-backslash", "windows-slash", "remote-prefixed"},
	)
	require.NoError(t, err)

	require.Equal(t, export.ProjectResolutionUnknown, got["windows-backslash"].Resolution)
	assert.Nil(t, got["windows-backslash"].Identity)

	require.Equal(t, export.ProjectResolutionUnknown, got["windows-slash"].Resolution)
	assert.Nil(t, got["windows-slash"].Identity)

	assert.Equal(t, export.ProjectResolutionUnknown, got["remote-prefixed"].Resolution)
	assert.Nil(t, got["remote-prefixed"].Identity)
}

func TestProjectIdentityMapUnknownPersistedObservationUsesLegacyFallback(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "fallback")
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))

	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			Project:    "fallback",
			Machine:    "laptop",
			RootPath:   "remote-host:/srv/app",
			ObservedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		}), "upsert unresolved persisted observation")
	require.NoError(t, d.UpsertSession(Session{
		ID:      "fallback-session",
		Project: "fallback",
		Machine: "laptop",
		Agent:   "codex",
		Cwd:     root,
	}))

	got, err := d.BuildProjectIdentityMap(ctx, []string{"fallback"})
	require.NoError(t, err)
	require.Equal(t, export.ProjectResolutionUnknown,
		got["fallback"].Resolution)
	assert.Nil(t, got["fallback"].Identity)
}

func TestProjectIdentityMapLegacyFallbackUsesNoRemoteGitRoot(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	repo := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repo, ".git"), 0o755))
	subdir := filepath.Join(repo, "pkg", "feature")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	require.NoError(t, d.UpsertSession(Session{
		ID:      "git-root-session",
		Project: "git-root",
		Machine: "laptop",
		Agent:   "codex",
		Cwd:     subdir,
	}))

	got, err := d.BuildProjectIdentityMap(ctx, []string{"git-root"})
	require.NoError(t, err)
	require.Equal(t, export.ProjectResolutionUnknown,
		got["git-root"].Resolution)
	assert.Nil(t, got["git-root"].Identity)
}

func TestProjectIdentityMapLegacyFallbackUsesRepoDotWorktreesMapping(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	parent := t.TempDir()
	worktreesDir := filepath.Join(parent, "acme-app.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine:    "laptop",
		PathPrefix: parent,
		Layout:     WorktreeMappingLayoutRepoDotWorktrees,
		Enabled:    true,
	})
	require.NoError(t, err)
	require.NoError(t, d.UpsertSession(Session{
		ID:      "branch-a-session",
		Project: "acme_app",
		Machine: "laptop",
		Agent:   "codex",
		Cwd:     filepath.Join(worktreesDir, "branch-a"),
	}))
	require.NoError(t, d.UpsertSession(Session{
		ID:      "branch-b-session",
		Project: "acme_app",
		Machine: "laptop",
		Agent:   "codex",
		Cwd:     filepath.Join(worktreesDir, "branch-b"),
	}))

	got, err := d.BuildProjectIdentityMap(ctx, []string{"acme_app"})
	require.NoError(t, err)
	require.Equal(t, export.ProjectResolutionUnknown, got["acme_app"].Resolution)
	assert.Nil(t, got["acme_app"].Identity)
}

func TestProjectIdentityMapLegacyFallbackClosesRowsBeforeMappingLookup(t *testing.T) {
	d := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reader := d.rawReader()
	oldMaxOpen := reader.Stats().MaxOpenConnections
	reader.SetMaxOpenConns(1)
	t.Cleanup(func() { reader.SetMaxOpenConns(oldMaxOpen) })

	root := t.TempDir()
	_, err := d.CreateWorktreeProjectMapping(ctx,
		WorktreeProjectMapping{
			Machine:    "laptop",
			PathPrefix: root,
			Project:    "mapped",
			Enabled:    true,
		})
	require.NoError(t, err, "CreateWorktreeProjectMapping")
	require.NoError(t, d.UpsertSession(Session{
		ID:      "mapped-session",
		Project: "mapped",
		Machine: "laptop",
		Agent:   "codex",
		Cwd:     filepath.Join(root, "pkg"),
	}))

	got, err := d.BuildProjectIdentityMap(ctx, []string{"mapped"})
	require.NoError(t, err)
	require.Equal(t, export.ProjectResolutionUnknown,
		got["mapped"].Resolution)
	assert.Nil(t, got["mapped"].Identity)
}

func projectObservationRowCount(t *testing.T, d *DB) int {
	t.Helper()
	var n int
	require.NoError(t, d.getReader().QueryRow(
		`SELECT COUNT(*) FROM project_identity_observations`,
	).Scan(&n))
	return n
}

func projectIdentitySHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}
