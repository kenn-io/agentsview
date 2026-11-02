package git

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommitTrailersHistory(t *testing.T) {
	skipIfNoGit(t)
	repo := initRepo(t)
	fingerprint, tips, err := RefsFingerprint(t.Context(), repo)
	require.NoError(t, err)
	assert.Empty(t, fingerprint)
	assert.Empty(t, tips)
	commits, err := ScanCommitTrailers(t.Context(), repo, nil)
	require.NoError(t, err)
	assert.Empty(t, commits)
	gitRun(t, repo, nil, "commit", "--allow-empty", "-qm", "inline see Agent-Session: claude:inline")
	gitRun(t, repo, nil, "commit", "--allow-empty", "-qm", "Body\n\nAgent-Session: claude:body\nthis is prose, not a trailer")
	gitRun(t, repo, nil, "checkout", "-qb", "side")
	gitRun(t, repo, nil, "commit", "--allow-empty", "-qm", "Editor\n\nAgent-Session: claude:editor\nAgent-Session: codex:thread\nAgent-Session: claude:bad/id")
	before, oldTips, err := RefsFingerprint(t.Context(), repo)
	require.NoError(t, err)
	commits, err = ScanCommitTrailers(t.Context(), repo, nil)
	require.NoError(t, err)
	require.Len(t, commits, 1)
	assert.Equal(t, []string{"claude:editor", "codex:thread"}, commits[0].Sessions)
	assert.Equal(t, "side", commits[0].Branch)
	assert.Len(t, commits[0].SHA, 40)
	same, _, err := RefsFingerprint(t.Context(), repo)
	require.NoError(t, err)
	assert.Equal(t, before, same)
	gitRun(t, repo, nil, "checkout", "-q", "main")
	gitRun(t, repo, nil, "merge", "--no-ff", "side", "-qm", "Merge\n\nAgent-Session: claude:merge")
	after, _, err := RefsFingerprint(t.Context(), repo)
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
	commits, err = ScanCommitTrailers(t.Context(), repo, oldTips)
	require.NoError(t, err)
	require.Len(t, commits, 1)
	assert.Equal(t, []string{"claude:merge"}, commits[0].Sessions)
	assert.Equal(t, "main", commits[0].Branch)
	commits, err = ScanCommitTrailers(t.Context(), repo, []string{strings.Repeat("1", 40)})
	require.NoError(t, err)
	assert.Len(t, commits, 2, "a removed saved tip triggers a full scan")
	_, saved, err := RefsFingerprint(t.Context(), repo)
	require.NoError(t, err)
	gitRun(t, repo, nil, "checkout", "--orphan", "older")
	gitRun(t, repo, []string{"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2020-01-01T00:00:00Z"}, "commit", "--allow-empty", "-qm", "Older fetched commit\n\nAgent-Session: claude:older")
	gitRun(t, repo, nil, "update-ref", "refs/remotes/origin/older", "HEAD")
	gitRun(t, repo, nil, "checkout", "-q", "main")
	gitRun(t, repo, nil, "branch", "-D", "older")
	commits, err = ScanCommitTrailers(t.Context(), repo, saved)
	require.NoError(t, err)
	require.Len(t, commits, 1)
	assert.Equal(t, []string{"claude:older"}, commits[0].Sessions)
	assert.Equal(t, "origin/older", commits[0].Branch)

}

func TestCommitTrailersSHA256(t *testing.T) {
	skipIfNoGit(t)
	repo := t.TempDir()
	gitRun(t, repo, nil, "init", "-q", "--object-format=sha256", "-b", "main")
	configureTestRepoIdentity(t, repo)
	gitRun(t, repo, nil, "commit", "--allow-empty", "-qm", "SHA256\n\nAgent-Session: claude:hash")
	commits, err := ScanCommitTrailers(t.Context(), repo, nil)
	require.NoError(t, err)
	require.Len(t, commits, 1)
	assert.Len(t, commits[0].SHA, 64)
}

func TestCommitTrailersDetachedWorktree(t *testing.T) {
	skipIfNoGit(t)
	repo := initRepo(t)
	gitRun(t, repo, nil, "commit", "--allow-empty", "-qm", "Initial")
	worktree := t.TempDir()
	gitRun(t, repo, nil, "worktree", "add", "--detach", worktree)
	before, previous, err := RefsFingerprint(t.Context(), repo)
	require.NoError(t, err)
	gitRun(t, worktree, nil, "commit", "--allow-empty", "-qm", "Detached editor\n\nAgent-Session: codex:detached")
	after, tips, err := RefsFingerprint(t.Context(), repo)
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
	commits, err := ScanCommitTrailers(t.Context(), repo, previous)
	require.NoError(t, err)
	require.Len(t, commits, 1)
	assert.Equal(t, []string{"codex:detached"}, commits[0].Sessions)
	assert.Contains(t, tips, commits[0].SHA)
	assert.Empty(t, commits[0].Branch)
	commits, err = ScanCommitTrailers(t.Context(), repo, tips)
	require.NoError(t, err)
	assert.Empty(t, commits)
}

func TestCommitTrailersKeyCase(t *testing.T) {
	skipIfNoGit(t)
	repo := initRepo(t)
	for _, key := range []string{"agent-session", "AGENT-SESSION", "aGeNt-SeSsIoN"} {
		gitRun(t, repo, nil, "commit", "--allow-empty", "-qm", "Editor\n\n"+key+": claude:editor")
	}
	commits, err := ScanCommitTrailers(t.Context(), repo, nil)
	require.NoError(t, err)
	require.Len(t, commits, 3)
	for _, commit := range commits {
		assert.Equal(t, []string{"claude:editor"}, commit.Sessions)
	}
}

func TestCommitTrailersUTC(t *testing.T) {
	skipIfNoGit(t)
	repo := initRepo(t)
	gitRun(t, repo, []string{"GIT_AUTHOR_DATE=2026-10-30T12:10:00+02:00", "GIT_COMMITTER_DATE=2026-10-30T12:10:00+02:00"}, "commit", "--allow-empty", "-qm", "Editor\n\nAgent-Session: claude:editor")
	commits, err := ScanCommitTrailers(t.Context(), repo, nil)
	require.NoError(t, err)
	require.Len(t, commits, 1)
	assert.Equal(t, "2026-10-30T10:10:00Z", commits[0].CommittedAt)
}
