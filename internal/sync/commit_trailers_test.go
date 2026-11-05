package sync

import (
	"database/sql"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testutil"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com", "GIT_AUTHOR_DATE=2026-10-30T10:10:00Z", "GIT_COMMITTER_DATE=2026-10-30T10:10:00Z")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	return strings.TrimSpace(string(out))
}

func commitTrailerFixture(t *testing.T) (*db.DB, *Engine, string, string, func(string, ...string) string) {
	t.Helper()
	testutil.IsolateGit(t)
	database := openTestDB(t)
	root := t.TempDir()
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Editor\n\nAgent-Session: claude:editor-session")
	first := runGit(t, repo, "rev-parse", "HEAD")
	project := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(project, 0o700))
	record := map[string]any{"type": "user", "sessionId": "editor-session", "cwd": repo, "timestamp": "2026-10-30T10:00:00Z", "message": map[string]any{"role": "user", "content": "Tidy docs"}}
	line, err := json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(project, "editor-session.jsonl"), append(line, '\n'), 0o600))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	return database, engine, repo, first, func(dir string, args ...string) string {
		return runGit(t, dir, args...)
	}
}

func TestResyncAbortsOnCommitTrailerCopyFailure(t *testing.T) {
	database, engine, repo, first, _ := commitTrailerFixture(t)
	require.NoError(t, os.RemoveAll(repo))
	// A newer archive schema cannot be copied into this build's trailer table.
	conn, err := sql.Open("sqlite3", database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.ExecContext(t.Context(), "ALTER TABLE commit_trailers ADD COLUMN extra TEXT")
	require.NoError(t, err)

	_, stats, err := engine.ResyncBuild(t.Context(), nil)
	require.Error(t, err)
	assert.True(t, stats.Aborted)
	assert.NoFileExists(t, engine.ResyncTempPath())
	found, err := database.CommitSessionLinks(t.Context(), []string{first})
	require.NoError(t, err)
	require.Len(t, found.Commits[0].Links, 1)
	assert.Equal(t, first, found.Commits[0].Links[0].SHA)
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced)
}

func TestResyncDiscoversCommitTrailersForOrphanedSession(t *testing.T) {
	database, engine, repo, first, runGit := commitTrailerFixture(t)
	session, err := database.GetSessionFull(t.Context(), "editor-session")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.FilePath)
	require.NoError(t, os.Remove(*session.FilePath))
	// Keep discovery nonempty so the rebuild can install the replacement.
	// This session has no cwd, leaving the checkout known only to the orphan.
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(*session.FilePath), "live-session.jsonl"), []byte(
		`{"type":"user","sessionId":"live-session","timestamp":"2026-10-30T10:00:00Z","message":{"role":"user","content":"Other work"}}`+"\n",
	), 0o600))
	runGit(repo, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Later\n\nAgent-Session: claude:editor-session")
	second := runGit(repo, "rev-parse", "HEAD")

	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "warnings: %v", stats.Warnings)
	require.Equal(t, 1, stats.OrphanedCopied)
	assert.Equal(t, 1, stats.LinksUpdated)
	assert.Equal(t, 1, engine.LastSyncStats().LinksUpdated)

	list, err := database.SessionCommitLinks(t.Context(), "editor-session")
	require.NoError(t, err)
	require.Len(t, list.Commits, 2)
	assert.ElementsMatch(t, []string{first, second}, []string{list.Commits[0].SHA, list.Commits[1].SHA})
	for _, commit := range list.Commits {
		assert.Equal(t, "trailer", commit.Source)
	}
}

func TestReconcileCommitTrailersUsageOnly(t *testing.T) {
	database, engine, _, _, _ := commitTrailerFixture(t)
	database.SetArchiveContent(config.ArchiveContentUsage)
	clear(engine.trailerScanAttempts)

	engine.ReconcileCommitTrailers(t.Context())

	assert.Empty(t, engine.trailerScanAttempts, "usage-only archives must skip repository scans")
}

func TestReconcileCommitTrailersCwdFilter(t *testing.T) {
	for _, tt := range []struct {
		name         string
		includeRepo  bool
		otherCwd     string
		wantSessions []string
	}{
		{name: "excluded repository", wantSessions: []string{}},
		{name: "excluded owner", includeRepo: true, otherCwd: "outside", wantSessions: []string{"allowed"}},
		{name: "owner without cwd", includeRepo: true, wantSessions: []string{"allowed"}},
		{name: "allowed owners", includeRepo: true, otherCwd: "allowed", wantSessions: []string{"allowed", "codex:other"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testutil.IsolateGit(t)
			database := openTestDB(t)
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			repo := filepath.Join(root, "repo")
			allowed := filepath.Join(root, "allowed")
			require.NoError(t, os.Mkdir(repo, 0o700))
			require.NoError(t, os.Mkdir(allowed, 0o700))
			for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Editor\n\nAgent-Session: claude:allowed\nAgent-Session: codex:other"}} {
				runGit(t, repo, args...)
			}
			otherCwd := ""
			if tt.otherCwd != "" {
				otherCwd = filepath.Join(root, tt.otherCwd)
			}
			prefixes := []string{allowed}
			if tt.includeRepo {
				prefixes = append(prefixes, repo)
			}
			engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", IncludeCwdPrefixes: prefixes})
			t.Cleanup(engine.Close)
			if tt.name == "allowed owners" {
				// Scan an existing checkout before discovering another session's repository.
				initial := filepath.Join(allowed, "initial")
				require.NoError(t, os.Mkdir(initial, 0o700))
				runGit(t, initial, "init", "-q", "-b", "main")
				runGit(t, initial, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Initial\n\nAgent-Session: claude:initial")
				require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "initial", Agent: "claude", Machine: "local", Cwd: initial}))
				engine.ReconcileCommitTrailers(t.Context())
				list, err := database.SessionCommitLinks(t.Context(), "initial")
				require.NoError(t, err)
				require.Len(t, list.Commits, 1)
			}
			sha := runGit(t, repo, "rev-parse", "HEAD")
			for _, session := range []db.Session{
				{ID: "discovery", Agent: "claude", Machine: "local", Cwd: repo},
				{ID: "allowed", Agent: "claude", Machine: "local", Cwd: allowed},
				{ID: "codex:other", Agent: "codex", Machine: "local", Cwd: otherCwd},
			} {
				require.NoError(t, database.UpsertSession(t.Context(), session))
			}

			engine.ReconcileCommitTrailers(t.Context())

			for _, id := range []string{"allowed", "codex:other"} {
				list, err := database.SessionCommitLinks(t.Context(), id)
				require.NoError(t, err)
				if slices.Contains(tt.wantSessions, id) {
					require.Len(t, list.Commits, 1)
					assert.Equal(t, "trailer", list.Commits[0].Source)
					assert.Equal(t, sha, list.Commits[0].SHA)
					assert.Equal(t, repo, list.Commits[0].Repository)
				} else {
					assert.Empty(t, list.Commits)
				}
			}
		})
	}
}

func TestReconcileCommitTrailersProtectedGitConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe classifier walks POSIX paths and symlinks")
	}
	for _, tt := range []struct {
		name          string
		linked, allow bool
		commondir     bool
		wantLink      bool
	}{
		{name: "protected config"},
		{name: "protected common config in linked worktree", linked: true},
		{name: "protected commondir in linked worktree", linked: true, commondir: true},
		{name: "protected config with opt-in", allow: true, wantLink: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testutil.IsolateGit(t)
			home := t.TempDir()
			repo := filepath.Join(home, "src", "repo")
			require.NoError(t, os.MkdirAll(repo, 0o700))
			runGit(t, repo, "init", "-q", "-b", "main")
			runGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Editor\n\nAgent-Session=claude:editor")
			sha := runGit(t, repo, "rev-parse", "HEAD")
			cwd := repo
			if tt.linked {
				cwd = filepath.Join(home, "src", "worktree")
				runGit(t, repo, "worktree", "add", "-q", "-b", "linked", cwd)
			}
			config := filepath.Join(repo, ".git", "config")
			if tt.commondir {
				config = filepath.Join(repo, ".git", "worktrees", "worktree", "commondir")
			}
			target := filepath.Join(home, "Documents", "config")
			require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o700))
			require.NoError(t, os.Rename(config, target))
			require.NoError(t, os.Symlink(target, config))

			database := openTestDB(t)
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "editor", Agent: "claude", Project: "project-a", Machine: "local", Cwd: cwd}))
			engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", ScanProtectedPaths: tt.allow})
			t.Cleanup(engine.Close)
			engine.goos = "darwin"
			engine.homeDir = home
			engine.ReconcileCommitTrailers(t.Context())
			found, err := database.CommitSessionLinks(t.Context(), []string{sha})
			require.NoError(t, err)
			require.Len(t, found.Commits, 1)
			if tt.wantLink {
				require.Len(t, found.Commits[0].Links, 1)
				assert.Equal(t, "editor", found.Commits[0].Links[0].SessionID)
			} else {
				assert.Empty(t, found.Commits[0].Links)
			}
		})
	}
}

func TestReconcileCommitTrailersNewCommit(t *testing.T) {
	for _, tt := range []struct {
		name    string
		wrapper bool
		scoped  bool
		since   time.Time
	}{
		{name: "reconcile", wrapper: true},
		{name: "full sync"},
		{name: "recent files", since: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)},
		{name: "selected roots", scoped: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			database, engine, repo, first, runGit := commitTrailerFixture(t)
			emitter := &fakeEmitter{}
			engine.emitter = emitter
			if tt.wrapper {
				engine.ReconcileCommitTrailers(t.Context())
			} else {
				unchanged := engine.SyncAll(t.Context(), nil)
				assert.False(t, unchanged.ChangedArchive())
			}
			assert.Empty(t, emitter.got())

			runGit(repo, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Later\n\nAgent-Session: claude:editor-session")
			second := runGit(repo, "rev-parse", "HEAD")
			if tt.scoped || !tt.since.IsZero() {
				if tt.scoped {
					engine.SyncRootsSince(t.Context(), engine.ReconciliationRootsForAgent("claude"), tt.since, nil)
				} else {
					engine.SyncAllSince(t.Context(), tt.since, nil)
				}
				list, err := database.SessionCommitLinks(t.Context(), "editor-session")
				require.NoError(t, err)
				require.Len(t, list.Commits, 1)
				assert.Equal(t, first, list.Commits[0].SHA)
				assert.Equal(t, "trailer", list.Commits[0].Source)
				assert.Empty(t, emitter.got())
			}

			if tt.wrapper {
				engine.ReconcileCommitTrailers(t.Context())
			} else {
				stats := engine.SyncAll(t.Context(), nil)
				assert.Zero(t, stats.Synced)
				assert.Equal(t, 1, stats.LinksUpdated)
				assert.True(t, stats.ChangedArchive())
				assert.Equal(t, 1, engine.LastSyncStats().LinksUpdated)
			}
			assert.Equal(t, []string{"sessions"}, emitter.got())
			found, err := database.CommitSessionLinks(t.Context(), []string{second[:7]})
			require.NoError(t, err)
			require.Len(t, found.Commits[0].Links, 1)
			assert.Equal(t, second, found.Commits[0].Links[0].SHA)
			list, err := database.SessionCommitLinks(t.Context(), "editor-session")
			require.NoError(t, err)
			require.Len(t, list.Commits, 2)
			assert.ElementsMatch(t, []string{first, second}, []string{list.Commits[0].SHA, list.Commits[1].SHA})

			if tt.wrapper {
				engine.ReconcileCommitTrailers(t.Context())
			} else {
				unchanged := engine.SyncAll(t.Context(), nil)
				assert.False(t, unchanged.ChangedArchive())
			}
			assert.Equal(t, []string{"sessions"}, emitter.got())
		})
	}
}

func TestReconcileCommitTrailersRemovedCwd(t *testing.T) {
	testutil.IsolateGit(t)
	database := openTestDB(t)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Scratch\n\nAgent-Session: codex:removed"}} {
		runGit(t, repo, args...)
	}
	cwd := filepath.Join(repo, "temporary", "nested")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	require.NoError(t, os.RemoveAll(filepath.Join(repo, "temporary")))
	session := db.Session{ID: "codex:removed", Agent: "codex", Project: "project-a", Machine: "local", Cwd: cwd}
	require.NoError(t, database.UpsertSession(t.Context(), session))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	engine.ReconcileCommitTrailers(t.Context())
	list, err := database.SessionCommitLinks(t.Context(), "codex:removed")
	require.NoError(t, err)
	require.Len(t, list.Commits, 1)
	assert.Equal(t, "Scratch", list.Commits[0].Subject)
}

func TestReconcileCommitTrailersRotatesAfterBudget(t *testing.T) {
	testutil.IsolateGit(t)
	database := openTestDB(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	first := filepath.Join(root, "a-first")
	later := filepath.Join(root, "z-later")
	for _, repo := range []string{first, later} {
		require.NoError(t, os.Mkdir(repo, 0o700))
		for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", filepath.Base(repo) + "\n\nAgent-Session: claude:fair"}} {
			runGit(t, repo, args...)
		}
		require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: filepath.Base(repo), Agent: "claude", Project: "project-a", Machine: "local", Cwd: repo}))
	}
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "fair", Agent: "claude", Project: "project-a", Machine: "local"}))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	// A zero budget yields after exactly one attempt, regardless of Git speed.
	engine.reconcileCommitTrailersWithBudget(t.Context(), 0)
	list, err := database.SessionCommitLinks(t.Context(), "fair")
	require.NoError(t, err)
	require.Len(t, list.Commits, 1)
	assert.Equal(t, first, list.Commits[0].Repository)
	// The next pass must scan the waiting repository before revisiting the first.
	engine.reconcileCommitTrailersWithBudget(t.Context(), 0)
	list, err = database.SessionCommitLinks(t.Context(), "fair")
	require.NoError(t, err)
	require.Len(t, list.Commits, 2)
	assert.ElementsMatch(t, []string{first, later}, []string{list.Commits[0].Repository, list.Commits[1].Repository})
}

func TestReconcileCommitTrailersRepositoryErrors(t *testing.T) {
	testutil.IsolateGit(t)
	database := openTestDB(t)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(t, repo, "init", "-q", "-b", "main")
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "editor", Agent: "claude", Project: "project-a", Machine: "local", Cwd: repo}))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	identity := engine.cachedProjectIdentity("local", repo)
	require.NotEmpty(t, identity.gitDir)
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("invalid HEAD\n"), 0o600))
	before := time.Now()
	assert.Zero(t, engine.reconcileCommitTrailersWithBudget(t.Context(), 0))
	attempt, ok := engine.trailerScanAttempts[identity.repositoryPath]
	require.True(t, ok, "failed scans must yield their turn")
	assert.False(t, attempt.Before(before))
	assert.False(t, attempt.After(time.Now()))
}
