package sync

import (
	"context"
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
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
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
	require.NotNil(t, l.Turn)
	assert.True(t, l.Turn.Heuristic)
	assert.Equal(t, "Tidy docs", l.Turn.Text)
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
	late := t.TempDir()
	runGit(late, "init", "-q", "-b", "main")
	runGit(late, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Older\n\nAgent-Session: codex:older")
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "codex:older", Agent: "codex", Project: "project-a", Machine: "local", Cwd: late}))
	database.SetArchiveContent(config.ArchiveContentTranscripts)
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
	// Stored evidence survives a resync after its checkout has disappeared.
	require.NoError(t, os.RemoveAll(repo))
	engine.ResyncAll(t.Context(), nil)
	list, err = database.SessionCommitLinks(t.Context(), "editor-session")
	require.NoError(t, err)
	assert.Len(t, list.Commits, 2)
}

func TestReconcileCommitTrailersRemovedCwd(t *testing.T) {
	testutil.IsolateGit(t)
	database := openTestDB(t)
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Editor\n\nAgent-Session: claude:removed-cwd"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	cwd := filepath.Join(repo, "temporary", "nested")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "removed-cwd", Agent: "claude", Project: "project-a", Machine: "local", Cwd: cwd}))
	require.NoError(t, os.RemoveAll(filepath.Join(repo, "temporary")))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	engine.ReconcileCommitTrailers(t.Context())
	list, err := database.SessionCommitLinks(t.Context(), "removed-cwd")
	require.NoError(t, err)
	require.Len(t, list.Commits, 1)
	assert.Equal(t, "trailer", list.Commits[0].Source)
	assert.Equal(t, "Editor", list.Commits[0].Subject)
}

func TestReconcileCommitTrailersRotatesAfterBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a shell Git wrapper")
	}
	testutil.IsolateGit(t)
	database := openTestDB(t)
	root := t.TempDir()
	slow := filepath.Join(root, "a-slow")
	later := filepath.Join(root, "z-later")
	for _, repo := range []string{slow, later} {
		require.NoError(t, os.Mkdir(repo, 0o700))
		for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "Editor\n\nAgent-Session: claude:fair"}} {
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
	// Resolve identities before installing the wrapper so only the scan stalls.
	engine.cachedProjectIdentity("local", slow)
	engine.cachedProjectIdentity("local", later)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	bin := t.TempDir()
	wrapper := "#!/bin/sh\nif [ \"$PWD\" = \"$TRAILER_TEST_SLOW\" ]; then\n  case \" $* \" in\n    *' show-ref '*) exec sleep 30 ;;\n  esac\nfi\nexec \"$TRAILER_TEST_GIT\" \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o700))
	t.Setenv("TRAILER_TEST_GIT", realGit)
	t.Setenv("TRAILER_TEST_SLOW", slow)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Exhaust the shared pass budget in the first repository.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	engine.ReconcileCommitTrailers(ctx)
	cancel()
	list, err := database.SessionCommitLinks(t.Context(), "fair")
	require.NoError(t, err)
	require.Empty(t, list.Commits)
	// The next pass must reach the waiting repository before retrying the slow one.
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	engine.ReconcileCommitTrailers(ctx)
	list, err = database.SessionCommitLinks(t.Context(), "fair")
	require.NoError(t, err)
	require.Len(t, list.Commits, 1)
	assert.Equal(t, later, list.Commits[0].Repository)
}
