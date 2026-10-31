package sync

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	dbgit "go.kenn.io/agentsview/internal/db/git"
	gitcmd "go.kenn.io/kit/git/cmd"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestCommitProvenanceBackfillUsesStoredTranscript(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "fixture", Project: "demo", Agent: "claude"}))
	require.NoError(t, d.ReplaceSessionMessages(ctx, "fixture", []db.Message{{SessionID: "fixture", Ordinal: 0, Role: "user", Content: "Fix it"}, {SessionID: "fixture", Ordinal: 1, Role: "assistant", Content: "done", ToolCalls: []db.ToolCall{{Category: "Bash", InputJSON: `{"command":"git commit -m fix"}`, ResultContent: "[main abcdef1] Fix it"}}}}))
	e := &Engine{db: d, ephemeral: true}
	n, err := e.BackfillCommitProvenance(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	rows, err := d.CommitLinks(ctx, []string{"abcdef1"}, "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Turn)
	assert.Equal(t, "Fix it", rows[0].Turn.Prompt)
	n, err = e.BackfillCommitProvenance(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestCommitTrailerScanCheckpointsBeforeLaterPublicationFailure(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := t.Context()
	repo := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	runner := gitcmd.New()
	_, err := runner.Output(ctx, repo, "init", "-b", "main")
	require.NoError(t, err)
	var input strings.Builder
	for i := 0; i < 120; i++ {
		body := "Fixture commit\n\nAgent-Session: custom:fixture\n"
		fmt.Fprintf(&input, "commit refs/heads/main\ncommitter Fixture <fixture@example.test> %d +0000\ndata %d\n%s\n", 1700000000+i, len(body), body)
	}
	_, _, err = runner.Run(ctx, repo, strings.NewReader(input.String()), "fast-import", "--quiet")
	require.NoError(t, err)
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TRIGGER fail_second_commit BEFORE INSERT ON commit_session_links WHEN (SELECT COUNT(*) FROM commit_session_links)>=1 BEGIN SELECT RAISE(ABORT,'fixture failure'); END`)
		return err
	}))
	e := &Engine{db: d}
	require.Error(t, e.scanCommitRepository(ctx, repo))
	key := "commit_links_scan:" + dbgit.CacheKey("scan-state-v1", repo, "", "", "")
	raw, err := d.GetSyncState(ctx, key)
	require.NoError(t, err)
	var state commitScanState
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	assert.Equal(t, 1, state.Offset)
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, "DROP TRIGGER fail_second_commit"); return err }))
	for range 3 {
		require.NoError(t, e.scanCommitRepository(ctx, repo))
	}
	raw, err = d.GetSyncState(ctx, key)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	assert.Equal(t, 120, state.Offset)
	assert.True(t, state.Complete)
}

func TestCommitRepositoryEligibilityUsesExistingLocalCheckout(t *testing.T) {
	repo := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	_, err := gitcmd.New().Output(t.Context(), repo, "init", "-b", "main")
	require.NoError(t, err)
	e := &Engine{machine: "local"}
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	assert.Equal(t, resolvedRepo, e.localCommitRepository("local", repo))
	assert.Empty(t, e.localCommitRepository("foreign", repo))
	assert.Empty(t, e.localCommitRepository("local", repo+"/missing"))
	assert.Empty(t, e.localCommitRepository("local", "archive://fixture/repo"))
	e.idPrefix = "attached~"
	assert.Empty(t, e.localCommitRepository("local", repo))
}

func TestCommitRepositoryRefusesUnsafeCommondirPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture requires POSIX symlinks")
	}
	for _, test := range []struct {
		name          string
		commondirFile func(t *testing.T, home, gitDir string)
	}{
		{
			name: "commondir symlink into protected folder",
			commondirFile: func(t *testing.T, home, gitDir string) {
				t.Helper()
				target := filepath.Join(home, "Documents", "commondir")
				require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
				require.NoError(t, os.WriteFile(target, []byte("../..\n"), 0o644))
				require.NoError(t, os.Symlink(target, filepath.Join(gitDir, "commondir")))
			},
		},
		{
			name: "common directory in protected folder",
			commondirFile: func(t *testing.T, home, gitDir string) {
				t.Helper()
				commonDir := filepath.Join(home, "Documents", "main", ".git")
				require.NoError(t, os.MkdirAll(commonDir, 0o755))
				require.NoError(t, os.WriteFile(
					filepath.Join(gitDir, "commondir"), []byte(commonDir+"\n"), 0o644,
				))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			worktree := filepath.Join(home, "src", "worktree")
			gitDir := filepath.Join(home, "gitstore", "worktree")
			require.NoError(t, os.MkdirAll(worktree, 0o755))
			require.NoError(t, os.MkdirAll(gitDir, 0o755))
			require.NoError(t, os.WriteFile(
				filepath.Join(worktree, ".git"),
				[]byte("gitdir: "+gitDir+"\n"), 0o644,
			))
			test.commondirFile(t, home, gitDir)
			engine := &Engine{machine: "current-machine", goos: "darwin", homeDir: home}

			assert.Empty(t, engine.localCommitRepository("current-machine", worktree),
				"commit indexing must not invoke Git when common metadata is protected")
		})
	}
}

func TestCommitTrailerScanEmptyThenFirstCommit(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := t.Context()
	repo := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	runner := gitcmd.New()
	_, err := runner.Output(ctx, repo, "init", "-b", "main")
	require.NoError(t, err)
	e := &Engine{db: d}
	_, err = d.BackfillCommitLinks(ctx, 100)
	require.NoError(t, err)
	require.NoError(t, e.scanCommitRepository(ctx, repo))
	body := "First commit\n\nAgent-Session: custom:fixture\n"
	input := fmt.Sprintf("commit refs/heads/main\ncommitter Fixture <fixture@example.test> 1700000000 +0000\ndata %d\n%s\n", len(body), body)
	_, _, err = runner.Run(ctx, repo, strings.NewReader(input), "fast-import", "--quiet")
	require.NoError(t, err)
	require.NoError(t, e.scanCommitRepository(ctx, repo))
	rows, err := d.CommitLinks(ctx, nil, "custom:fixture")
	require.NoError(t, err)
	require.Len(t, rows, 1, "first commit is scanned on the first nonempty pass")
}

func TestCommitTrailerScanRecoversPrunedHistoricalTip(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := t.Context()
	repo := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	runner := gitcmd.New()
	_, err := runner.Output(ctx, repo, "init", "-b", "main")
	require.NoError(t, err)
	for _, setting := range [][2]string{{"user.name", "Fixture"}, {"user.email", "fixture@example.test"}, {"commit.gpgsign", "false"}} {
		_, err := runner.Output(ctx, repo, "config", setting[0], setting[1])
		require.NoError(t, err)
	}
	add := func(at int) string {
		t.Helper()
		body := "Fixture commit\n\nAgent-Session: custom:fixture\n"
		input := fmt.Sprintf("commit refs/heads/main\ncommitter Fixture <fixture@example.test> %d +0000\ndata %d\n%s\n", at, len(body), body)
		_, _, err := runner.Run(ctx, repo, strings.NewReader(input), "fast-import", "--quiet")
		require.NoError(t, err)
		sha, err := runner.Output(ctx, repo, "rev-parse", "HEAD")
		require.NoError(t, err)
		return strings.TrimSpace(string(sha))
	}
	addChild := func(parent string) string {
		t.Helper()
		body := "Fixture commit\n\nAgent-Session: custom:fixture\n"
		tree, err := runner.Output(ctx, repo, "mktree")
		require.NoError(t, err)
		commit, _, err := runner.Run(ctx, repo, strings.NewReader(body), "commit-tree", strings.TrimSpace(string(tree)), "-p", parent, "-F", "-")
		require.NoError(t, err)
		sha := strings.TrimSpace(string(commit))
		_, err = runner.Output(ctx, repo, "update-ref", "refs/heads/main", sha, parent)
		require.NoError(t, err)
		return sha
	}
	old := add(1700000000)
	e := &Engine{db: d}
	_, err = d.BackfillCommitLinks(ctx, 100)
	require.NoError(t, err)
	require.NoError(t, e.scanCommitRepository(ctx, repo))
	_, err = runner.Output(ctx, repo, "update-ref", "-d", "refs/heads/main")
	require.NoError(t, err)
	current := add(1700000001)
	_, err = runner.Output(ctx, repo, "reflog", "expire", "--expire=now", "--all")
	require.NoError(t, err)
	_, err = runner.Output(ctx, repo, "gc", "--prune=now")
	require.NoError(t, err)
	_, err = dbgit.ReadCommitObject(ctx, repo, old)
	require.Error(t, err, "fixture removes the old immutable tip")
	require.NoError(t, e.scanCommitRepository(ctx, repo))
	rows, err := d.CommitLinks(ctx, []string{old, current}, "")
	require.NoError(t, err)
	assert.Len(t, rows, 2, "retains stored facts and scans current history")

	t.Run("unavailable queued frontier yields to current tips", func(t *testing.T) {
		// A later tip can arrive after the queued immutable tip is pruned.
		// The unavailable work cannot block discovery of the current tip.
		next := addChild(current)
		tips, err := dbgit.SnapshotCommitTips(ctx, repo)
		require.NoError(t, err)
		require.Equal(t, []string{next}, tips)
		objects, err := dbgit.ScanSessionTrailersPage(ctx, repo, tips, 0, 100)
		require.NoError(t, err)
		var nextCommit *dbgit.CommitObject
		for i := range objects {
			if objects[i].SHA == next {
				nextCommit = &objects[i]
				break
			}
		}
		require.NotNil(t, nextCommit)
		assert.Equal(t, []string{"custom:fixture"}, nextCommit.Sessions)
		state := commitScanState{Tips: []string{current}, Seen: []string{current}, Complete: true, QueuedTips: []string{old}}
		encoded, err := json.Marshal(state)
		require.NoError(t, err)
		key := "commit_links_scan:" + dbgit.CacheKey("scan-state-v1", repo, "", "", "")
		require.NoError(t, d.SetSyncState(ctx, key, string(encoded)))
		var rows []db.CommitLink
		for range 3 {
			require.NoError(t, e.scanCommitRepository(ctx, repo))
			rows, err = d.CommitLinks(ctx, []string{next}, "")
			require.NoError(t, err)
		}
		require.Len(t, rows, 1, "the reachable current tip is scanned after the stale queue is skipped")
		assert.Equal(t, next, rows[0].CommitSHA)
		raw, err := d.GetSyncState(ctx, key)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal([]byte(raw), &state))
		assert.Empty(t, state.QueuedTips)
	})
}

func TestCommitTrailerScanDoesNotSkipIncompleteHistoryAfterPrunedTip(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := t.Context()
	repo := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	runner := gitcmd.New()
	_, err := runner.Output(ctx, repo, "init", "-b", "main")
	require.NoError(t, err)
	for _, setting := range [][2]string{{"user.name", "Fixture"}, {"user.email", "fixture@example.test"}, {"commit.gpgsign", "false"}} {
		_, err := runner.Output(ctx, repo, "config", setting[0], setting[1])
		require.NoError(t, err)
	}
	var input strings.Builder
	for i := 0; i < 120; i++ {
		body := fmt.Sprintf("Fixture commit %d\n", i)
		if i == 0 {
			body += "\nAgent-Session: custom:oldest\n"
		}
		fmt.Fprintf(&input, "commit refs/heads/main\ncommitter Fixture <fixture@example.test> %d +0000\ndata %d\n%s\n", 1700000000+i, len(body), body)
	}
	_, _, err = runner.Run(ctx, repo, strings.NewReader(input.String()), "fast-import", "--quiet")
	require.NoError(t, err)
	oldest, err := runner.Output(ctx, repo, "rev-list", "--reverse", "refs/heads/main")
	require.NoError(t, err)
	oldestIDs := strings.Fields(string(oldest))
	require.NotEmpty(t, oldestIDs)
	oldestSHA := oldestIDs[0]
	tree, err := runner.Output(ctx, repo, "mktree")
	require.NoError(t, err)
	pruned, _, err := runner.Run(ctx, repo, strings.NewReader("Pruned branch fixture\n"), "commit-tree", strings.TrimSpace(string(tree)), "-F", "-")
	require.NoError(t, err)
	prunedSHA := strings.TrimSpace(string(pruned))
	_, err = runner.Output(ctx, repo, "update-ref", "refs/heads/stale", prunedSHA)
	require.NoError(t, err)

	e := &Engine{db: d}
	require.NoError(t, e.scanCommitRepository(ctx, repo))
	key := "commit_links_scan:" + dbgit.CacheKey("scan-state-v1", repo, "", "", "")
	raw, err := d.GetSyncState(ctx, key)
	require.NoError(t, err)
	var state commitScanState
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	assert.Equal(t, 100, state.Offset)
	assert.False(t, state.Complete, "the captured history is still being paged")

	_, err = runner.Output(ctx, repo, "update-ref", "-d", "refs/heads/stale")
	require.NoError(t, err)
	_, err = runner.Output(ctx, repo, "reflog", "expire", "--expire=now", "--all")
	require.NoError(t, err)
	_, err = runner.Output(ctx, repo, "gc", "--prune=now")
	require.NoError(t, err)
	_, err = dbgit.ReadCommitObject(ctx, repo, prunedSHA)
	require.Error(t, err, "the fixture prunes one of the captured tips")

	for range 4 {
		require.NoError(t, e.scanCommitRepository(ctx, repo))
	}
	_, err = d.BackfillCommitLinks(ctx, 100)
	require.NoError(t, err)
	rows, err := d.CommitLinks(ctx, []string{oldestSHA}, "")
	require.NoError(t, err)
	require.Len(t, rows, 1, "recovery rescans reachable history omitted by the abandoned frontier")
	assert.Equal(t, "custom:oldest", rows[0].SessionID)
}

func TestCommitTrailerScanQueuesOnlyCommitsBeyondCompletedFrontier(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx := t.Context()
	repo := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	runner := gitcmd.New()
	_, err := runner.Output(ctx, repo, "init", "-b", "main")
	require.NoError(t, err)
	var input strings.Builder
	for i := 0; i < 120; i++ {
		body := fmt.Sprintf("Indexed history %d\n", i)
		fmt.Fprintf(&input, "commit refs/heads/main\ncommitter Fixture <fixture@example.test> %d +0000\ndata %d\n%s\n", 1700000000+i, len(body), body)
	}
	_, _, err = runner.Run(ctx, repo, strings.NewReader(input.String()), "fast-import", "--quiet")
	require.NoError(t, err)
	old, err := runner.Output(ctx, repo, "rev-parse", "HEAD")
	require.NoError(t, err)
	oldTip := strings.TrimSpace(string(old))
	for _, setting := range [][2]string{{"user.name", "Fixture"}, {"user.email", "fixture@example.test"}, {"commit.gpgsign", "false"}} {
		_, err = runner.Output(ctx, repo, "config", setting[0], setting[1])
		require.NoError(t, err)
	}
	state := commitScanState{Tips: []string{oldTip}, Seen: []string{oldTip}, Complete: true}
	encoded, err := json.Marshal(state)
	require.NoError(t, err)
	key := "commit_links_scan:" + dbgit.CacheKey("scan-state-v1", repo, "", "", "")
	require.NoError(t, d.SetSyncState(ctx, key, string(encoded)))

	body := "New fixture commit\n\nAgent-Session: custom:fixture\n"
	tree, err := runner.Output(ctx, repo, "mktree")
	require.NoError(t, err)
	commit, _, err := runner.Run(ctx, repo, strings.NewReader(body), "commit-tree", strings.TrimSpace(string(tree)), "-p", oldTip, "-F", "-")
	require.NoError(t, err)
	currentTip := strings.TrimSpace(string(commit))
	_, err = runner.Output(ctx, repo, "update-ref", "refs/heads/main", currentTip, oldTip)
	require.NoError(t, err)
	require.NotEqual(t, oldTip, currentTip)

	e := &Engine{db: d}
	require.NoError(t, e.scanCommitRepository(ctx, repo))
	rows, err := d.CommitLinks(ctx, []string{currentTip}, "")
	require.NoError(t, err)
	require.Len(t, rows, 1, "the changed tip is scanned without re-reading its completed ancestry")
	assert.Equal(t, currentTip, rows[0].CommitSHA)
	raw, err := d.GetSyncState(ctx, key)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	assert.Equal(t, 1, state.QueuedOffset, "only the one commit beyond the completed frontier advances")
	assert.Equal(t, []string{oldTip}, state.Seen, "the frontier advances only after the delta page completes")

	require.NoError(t, e.scanCommitRepository(ctx, repo))
	raw, err = d.GetSyncState(ctx, key)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(raw), &state))
	assert.Empty(t, state.QueuedTips)
	assert.Equal(t, []string{currentTip}, state.Seen)
	assert.True(t, state.Complete, "a completed frontier does not restart the full history pass")
}
