package sync

import (
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testutil"
)

func TestReconcileCommitTrailersEditorAndLateRepository(t *testing.T) {
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
	found, err := database.CommitSessionLinks(t.Context(), []string{first[:7]})
	require.NoError(t, err)
	require.Len(t, found.Commits[0].Links, 1)
	l := found.Commits[0].Links[0]
	assert.Equal(t, first, l.SHA)
	assert.Equal(t, "trailer", l.Source)
	runGit(repo, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Later\n\nAgent-Session: claude:editor-session")
	second := runGit(repo, "rev-parse", "HEAD")
	engine.ReconcileCommitTrailers(t.Context())
	found, err = database.CommitSessionLinks(t.Context(), []string{second[:7]})
	require.NoError(t, err)
	require.Len(t, found.Commits[0].Links, 1)
	assert.Equal(t, second, found.Commits[0].Links[0].SHA)
	// Count actual Git invocations on platforms that can execute a shell wrapper.
	if runtime.GOOS != "windows" {
		realGit, err := exec.LookPath("git")
		require.NoError(t, err)
		bin := t.TempDir()
		trace := filepath.Join(bin, "trace")
		require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TRAILER_TEST_TRACE\"\nexec \"$TRAILER_TEST_GIT\" \"$@\"\n"), 0o700))
		t.Setenv("TRAILER_TEST_TRACE", trace)
		t.Setenv("TRAILER_TEST_GIT", realGit)
		oldPath := os.Getenv("PATH")
		t.Setenv("PATH", bin+string(os.PathListSeparator)+oldPath)
		engine.ReconcileCommitTrailers(t.Context())
		t.Setenv("PATH", oldPath)
		traced, err := os.ReadFile(trace)
		require.NoError(t, err)
		assert.Contains(t, string(traced), "show-ref --head")
		assert.NotContains(t, string(traced), "log --all")
	}
	// This checkout's commit predates every scan of the first checkout.
	late, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(late, "init", "-q", "-b", "main")
	runGit(late, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Older\n\nAgent-Session: codex:older")
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "codex:older", Agent: "codex", Project: "project-a", Machine: "local", Cwd: late}))
	engine.ReconcileCommitTrailers(t.Context())
	list, err := database.SessionCommitLinks(t.Context(), "codex:older")
	require.NoError(t, err)
	require.Len(t, list.Commits, 1)
	assert.Equal(t, "trailer", list.Commits[0].Source)
	// A failed history scan leaves the previous checkpoint intact.
	key := "commit_trailers:" + late
	before, err := database.GetSyncState(t.Context(), key)
	require.NoError(t, err)
	broken := runGit(late, "rev-parse", "HEAD")
	runGit(late, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Broken\n\nAgent-Session: codex:older")
	require.NoError(t, os.Remove(filepath.Join(late, ".git", "objects", broken[:2], broken[2:])))
	engine.ReconcileCommitTrailers(t.Context())
	after, err := database.GetSyncState(t.Context(), key)
	require.NoError(t, err)
	assert.Equal(t, before, after)
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
	// Only trailer discovery looks past the removed cwd; the session's
	// recorded project identity does not claim the enclosing checkout.
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
		require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: repo, Agent: "claude", Project: "project-a", Machine: "local", Cwd: repo}))
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
