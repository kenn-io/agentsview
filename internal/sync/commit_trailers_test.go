package sync

import (
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testutil"
)

func commitTrailerFixture(t *testing.T) (*db.DB, *Engine, string, string, func(string, ...string) string) {
	t.Helper()
	testutil.IsolateGit(t)
	database := openTestDB(t)
	root := t.TempDir()
	repo := t.TempDir()
	runGit := func(dir string, args ...string) string {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com", "GIT_AUTHOR_DATE=2026-10-30T10:10:00Z", "GIT_COMMITTER_DATE=2026-10-30T10:10:00Z")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return strings.TrimSpace(string(out))
	}
	runGit(repo, "init", "-q", "-b", "main")
	runGit(repo, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Editor\n\nAgent-Session: claude:editor-session")
	first := runGit(repo, "rev-parse", "HEAD")
	project := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(project, 0o700))
	record := map[string]any{"type": "user", "sessionId": "editor-session", "cwd": repo, "timestamp": "2026-10-30T10:00:00Z", "message": map[string]any{"role": "user", "content": "Tidy docs"}}
	line, err := json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(project, "editor-session.jsonl"), append(line, '\n'), 0o600))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	return database, engine, repo, first, runGit
}

func TestReconcileCommitTrailersEditor(t *testing.T) {
	database, _, _, first, _ := commitTrailerFixture(t)
	found, err := database.CommitSessionLinks(t.Context(), []string{first[:7]})
	require.NoError(t, err)
	require.Len(t, found.Commits[0].Links, 1)
	l := found.Commits[0].Links[0]
	assert.Equal(t, first, l.SHA)
	assert.Equal(t, "trailer", l.Source)
}

func TestReconcileCommitTrailersProtectedGitConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe classifier walks POSIX paths and symlinks")
	}
	for _, tt := range []struct {
		name          string
		linked, allow bool
		wantLink      bool
	}{
		{name: "protected config"},
		{name: "protected common config in linked worktree", linked: true},
		{name: "protected config with opt-in", allow: true, wantLink: true},
		{name: "protected common config with opt-in", linked: true, allow: true, wantLink: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testutil.IsolateGit(t)
			home := t.TempDir()
			repo := filepath.Join(home, "src", "repo")
			require.NoError(t, os.MkdirAll(repo, 0o700))
			runGit := func(args ...string) string {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), "git", args...)
				cmd.Dir = repo
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", out)
				return strings.TrimSpace(string(out))
			}
			runGit("init", "-q", "-b", "main")
			runGit("config", "trailer.separators", "=")
			runGit("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Editor\n\nAgent-Session=claude:editor")
			sha := runGit("rev-parse", "HEAD")
			cwd := repo
			if tt.linked {
				cwd = filepath.Join(home, "src", "worktree")
				runGit("worktree", "add", "-q", "-b", "linked", cwd)
			}
			config := filepath.Join(repo, ".git", "config")
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
	database, engine, repo, first, runGit := commitTrailerFixture(t)
	runGit(repo, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Later\n\nAgent-Session: claude:editor-session")
	second := runGit(repo, "rev-parse", "HEAD")
	engine.ReconcileCommitTrailers(t.Context())
	found, err := database.CommitSessionLinks(t.Context(), []string{second[:7]})
	require.NoError(t, err)
	require.Len(t, found.Commits[0].Links, 1)
	assert.Equal(t, second, found.Commits[0].Links[0].SHA)
	list, err := database.SessionCommitLinks(t.Context(), "editor-session")
	require.NoError(t, err)
	require.Len(t, list.Commits, 2)
	assert.ElementsMatch(t, []string{first, second}, []string{list.Commits[0].SHA, list.Commits[1].SHA})
}

func TestCommitTrailersSkipPartialSync(t *testing.T) {
	for _, tt := range []struct {
		name   string
		scoped bool
		since  time.Time
	}{
		{name: "recent files", since: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)},
		{name: "selected roots", scoped: true},
		{name: "recent files in selected roots", scoped: true, since: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			database, engine, repo, first, runGit := commitTrailerFixture(t)
			runGit(repo, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Later\n\nAgent-Session: claude:editor-session")
			second := runGit(repo, "rev-parse", "HEAD")
			if tt.scoped {
				engine.SyncRootsSince(t.Context(), engine.ReconciliationRootsForAgent("claude"), tt.since, nil)
			} else {
				engine.SyncAllSince(t.Context(), tt.since, nil)
			}
			list, err := database.SessionCommitLinks(t.Context(), "editor-session")
			require.NoError(t, err)
			require.Len(t, list.Commits, 1)
			assert.Equal(t, first, list.Commits[0].SHA)

			engine.SyncAll(t.Context(), nil)
			list, err = database.SessionCommitLinks(t.Context(), "editor-session")
			require.NoError(t, err)
			require.Len(t, list.Commits, 2)
			assert.ElementsMatch(t, []string{first, second}, []string{list.Commits[0].SHA, list.Commits[1].SHA})
		})
	}
}

func TestReconcileCommitTrailersLateRepository(t *testing.T) {
	database, engine, _, _, runGit := commitTrailerFixture(t)
	// Discover a new session checkout after the initial reconciliation.
	late, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(late, "init", "-q", "-b", "main")
	runGit(late, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Older\n\nAgent-Session: codex:older")
	sha := runGit(late, "rev-parse", "HEAD")
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "codex:older", Agent: "codex", Project: "project-a", Machine: "local", Cwd: late}))
	engine.ReconcileCommitTrailers(t.Context())
	list, err := database.SessionCommitLinks(t.Context(), "codex:older")
	require.NoError(t, err)
	require.Len(t, list.Commits, 1)
	assert.Equal(t, "trailer", list.Commits[0].Source)
	assert.Equal(t, sha, list.Commits[0].SHA)
	assert.Equal(t, late, list.Commits[0].Repository)
}

func TestReconcileCommitTrailersRemovedCwd(t *testing.T) {
	testutil.IsolateGit(t)
	database := openTestDB(t)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Scratch\n\nAgent-Session: codex:removed"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
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
	// A removed cwd retains its recorded root and unknown worktree relationship.
	obs, ok := engine.projectIdentityObservation(session)
	require.True(t, ok)
	assert.Equal(t, filepath.ToSlash(cwd), obs.RootPath)
	assert.Empty(t, obs.RepositoryPath)
	assert.Equal(t, export.WorktreeUnknown, obs.WorktreeRelationship)
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
			cmd := exec.CommandContext(t.Context(), "git", args...)
			cmd.Dir = repo
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
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

func TestReconcileCommitTrailersKeepsFailedOrMissingRepository(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "missing"}[missing], func(t *testing.T) {
			database, engine, repo, first, _ := commitTrailerFixture(t)
			if missing {
				require.NoError(t, os.RemoveAll(repo))
				engine.ReconcileCommitTrailers(t.Context())
				require.Error(t, engine.reconcileRepositoryTrailers(t.Context(), repo, repo))
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("invalid HEAD\n"), 0o600))
				require.Error(t, engine.reconcileRepositoryTrailers(t.Context(), repo, repo))
			}
			found, err := database.CommitSessionLinks(t.Context(), []string{first})
			require.NoError(t, err)
			require.Len(t, found.Commits[0].Links, 1)
		})
	}
}
