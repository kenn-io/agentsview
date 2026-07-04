package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	assert.Equal(t,
		projectIdentitySHA("git_remote\n"+"github.com/Org/Repo"),
		projects["app"].Identity.Key,
	)
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
			Project:    "path-project",
			Machine:    "golden-host",
			RootPath:   "/fixtures/path-project",
			ObservedAt: observedAt,
		},
	))

	got, err := d.BuildProjectIdentityMap(ctx,
		[]string{"remote-project", "path-project", "unknown-project"})
	require.NoError(t, err)
	require.Equal(t, export.ProjectResolutionResolved,
		got["remote-project"].Resolution)
	require.NotNil(t, got["remote-project"].Identity)
	assert.Equal(t, "git_remote", got["remote-project"].Identity.KeySource)
	assert.Equal(t, "github.com/acme/remote-project",
		got["remote-project"].Identity.NormalizedRemote)
	assert.Equal(t,
		projectIdentitySHA("git_remote\n"+"github.com/acme/remote-project"),
		got["remote-project"].Identity.Key)

	require.Equal(t, export.ProjectResolutionResolved,
		got["path-project"].Resolution)
	require.NotNil(t, got["path-project"].Identity)
	assert.Equal(t, "root_path", got["path-project"].Identity.KeySource)
	assert.Equal(t, "/fixtures/path-project",
		got["path-project"].Identity.RootPath)
	assert.Equal(t,
		projectIdentitySHA("root_path\n"+"/fixtures/path-project"),
		got["path-project"].Identity.Key)

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

	projects := export.BuildProjectsMap([]string{"mapped-project"}, got)
	require.Equal(t, export.ProjectResolutionResolved, projects["mapped-project"].Resolution)
	require.NotNil(t, projects["mapped-project"].Identity)
	assert.Equal(t, "root_path", projects["mapped-project"].Identity.KeySource)
	assert.Empty(t, projects["mapped-project"].Identity.NormalizedRemote)
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

func TestProjectIdentityMapPrecedencePersistedLiveWorktreeUnknown(t *testing.T) {
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

	require.Equal(t, export.ProjectResolutionResolved, got["live"].Resolution)
	require.NotNil(t, got["live"].Identity)
	assert.Equal(t, "github.com/acme/live", got["live"].Identity.NormalizedRemote)
	assert.Equal(t, "git_remote", got["live"].Identity.KeySource)

	require.Equal(t, export.ProjectResolutionResolved, got["mapped"].Resolution)
	require.NotNil(t, got["mapped"].Identity)
	assert.Empty(t, got["mapped"].Identity.NormalizedRemote)
	assert.Equal(t, "root_path", got["mapped"].Identity.KeySource)

	require.Equal(t, export.ProjectResolutionResolved, got["file-parent"].Resolution)
	require.NotNil(t, got["file-parent"].Identity)
	assert.Equal(t, "github.com/acme/file-parent", got["file-parent"].Identity.NormalizedRemote)
	assert.Equal(t, "git_remote", got["file-parent"].Identity.KeySource)

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

	require.Equal(t, export.ProjectResolutionResolved, got["windows-backslash"].Resolution)
	require.NotNil(t, got["windows-backslash"].Identity)
	assert.Equal(t, export.ProjectIdentityKeySourceRootPath, got["windows-backslash"].Identity.KeySource)
	assert.Equal(t, "C:/repo", got["windows-backslash"].Identity.RootPath)

	require.Equal(t, export.ProjectResolutionResolved, got["windows-slash"].Resolution)
	require.NotNil(t, got["windows-slash"].Identity)
	assert.Equal(t, export.ProjectIdentityKeySourceRootPath, got["windows-slash"].Identity.KeySource)
	assert.Equal(t, "C:/repo", got["windows-slash"].Identity.RootPath)

	assert.Equal(t, export.ProjectResolutionUnknown, got["remote-prefixed"].Resolution)
	assert.Nil(t, got["remote-prefixed"].Identity)
}

func TestProjectIdentityMapUnknownPersistedObservationUsesLegacyFallback(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()

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
	require.Equal(t, export.ProjectResolutionResolved,
		got["fallback"].Resolution)
	require.NotNil(t, got["fallback"].Identity)
	assert.Equal(t, export.ProjectIdentityKeySourceRootPath,
		got["fallback"].Identity.KeySource)
	assert.Equal(t, projectIdentityTestRoot(t, root),
		got["fallback"].Identity.RootPath)
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
	wantRoot := projectIdentityTestRoot(t, repo)
	require.Equal(t, export.ProjectResolutionResolved,
		got["git-root"].Resolution)
	require.NotNil(t, got["git-root"].Identity)
	assert.Equal(t, export.ProjectIdentityKeySourceRootPath,
		got["git-root"].Identity.KeySource)
	assert.Equal(t, wantRoot, got["git-root"].Identity.RootPath)
}

func projectIdentityTestRoot(t *testing.T, root string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
	}
	normalized, ok, err := export.NormalizeRootPath(root)
	require.NoError(t, err)
	require.True(t, ok)
	return normalized
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
	require.Equal(t, export.ProjectResolutionResolved,
		got["mapped"].Resolution)
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
