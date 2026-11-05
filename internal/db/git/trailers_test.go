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
	commits, err := ScanCommitTrailers(t.Context(), repo)
	require.NoError(t, err)
	assert.Empty(t, commits)
	gitRun(t, repo, nil, "commit", "--allow-empty", "-qm", "inline see Agent-Session: claude:inline")
	gitRun(t, repo, nil, "commit", "--allow-empty", "-qm", "Body\n\nAgent-Session: claude:body\nthis is prose, not a trailer")
	gitRun(t, repo, nil, "checkout", "-qb", "side")
	gitRun(t, repo, nil, "commit", "--allow-empty", "-qm", "Editor\n\nAgent-Session: claude:editor\nAgent-Session: codex:thread\nAgent-Session: claude:bad/id")
	commits, err = ScanCommitTrailers(t.Context(), repo)
	require.NoError(t, err)
	require.Len(t, commits, 1)
	assert.Equal(t, []string{"claude:editor", "codex:thread"}, commits[0].Sessions)
	assert.Equal(t, "side", commits[0].Branch)
	assert.Len(t, commits[0].SHA, 40)
	gitRun(t, repo, nil, "checkout", "-q", "main")
	gitRun(t, repo, nil, "merge", "--no-ff", "side", "-qm", "Merge\n\nAgent-Session: claude:merge")
	commits, err = ScanCommitTrailers(t.Context(), repo)
	require.NoError(t, err)
	require.Len(t, commits, 2)
	assert.Equal(t, []string{"claude:merge"}, commits[0].Sessions)
	assert.Equal(t, "main", commits[0].Branch)
	assert.Equal(t, []string{"claude:editor", "codex:thread"}, commits[1].Sessions)
	gitRun(t, repo, nil, "checkout", "--orphan", "older")
	gitRun(t, repo, []string{"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2020-01-01T00:00:00Z"}, "commit", "--allow-empty", "-qm", "Older fetched commit\n\nAgent-Session: claude:older")
	gitRun(t, repo, nil, "update-ref", "refs/remotes/origin/older", "HEAD")
	gitRun(t, repo, nil, "checkout", "-q", "main")
	gitRun(t, repo, nil, "branch", "-D", "older")
	commits, err = ScanCommitTrailers(t.Context(), repo)
	require.NoError(t, err)
	require.Len(t, commits, 3)
	assert.Equal(t, []string{"claude:older"}, commits[2].Sessions)
	assert.Equal(t, "origin/older", commits[2].Branch)
}

func TestCommitTrailersSHA256(t *testing.T) {
	skipIfNoGit(t)
	repo := t.TempDir()
	gitRun(t, repo, nil, "init", "-q", "--object-format=sha256", "-b", "main")
	configureTestRepoIdentity(t, repo)
	gitRun(t, repo, nil, "commit", "--allow-empty", "-qm", "SHA256\n\nAgent-Session: claude:hash")
	commits, err := ScanCommitTrailers(t.Context(), repo)
	require.NoError(t, err)
	require.Len(t, commits, 1)
	assert.Len(t, commits[0].SHA, 64)
}

func TestCommitTrailersControlCharacterSubject(t *testing.T) {
	skipIfNoGit(t)
	for _, tt := range []struct {
		name, subject, trailers string
		sessions                []string
		later                   bool
	}{
		{name: "subject", subject: "Editor\x1f field\x1d record\x1e trailer", trailers: "Agent-Session: claude:editor", sessions: []string{"claude:editor"}, later: true},
		{name: "trailer value", subject: "Editor", trailers: "Agent-Session: claude:first\x1ecodex:second\nAgent-Session: claude:valid", sessions: []string{"claude:valid"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := initRepo(t)
			gitRun(t, repo, nil, "commit", "--allow-empty", "-qm", tt.subject+"\n\n"+tt.trailers)
			if tt.later {
				gitRun(t, repo, nil, "commit", "--allow-empty", "-qm", "Later\n\nAgent-Session: codex:thread")
			}
			commits, err := ScanCommitTrailers(t.Context(), repo)
			require.NoError(t, err)
			if tt.later {
				require.Len(t, commits, 2)
				assert.Equal(t, "Later", commits[0].Subject)
				assert.Equal(t, []string{"codex:thread"}, commits[0].Sessions)
				commits = commits[1:]
			}
			require.Len(t, commits, 1)
			assert.Equal(t, tt.subject, commits[0].Subject)
			assert.Equal(t, tt.sessions, commits[0].Sessions)
		})
	}
}

func TestCommitTrailersParsing(t *testing.T) {
	skipIfNoGit(t)
	repo := initRepo(t)
	gitRun(t, repo, nil, "config", "trailer.separators", "=:")
	sessions := []string{"kimi:wd_abc:agent-a:session_uuid-1", "codebuff:project-a:session-1", "devin:build:session-1"}
	var want [][]string
	for _, tt := range []struct {
		key, separator string
		sessions       []string
	}{
		{"agent-session", ": ", sessions},
		{"AGENT-SESSION", ": ", sessions},
		{"aGeNt-SeSsIoN", ": ", sessions},
		{"Agent-Session", " \t: ", []string{"claude:editor"}},
		{"Agent-Session", "= ", []string{"claude:editor"}},
	} {
		gitRun(t, repo, []string{"GIT_AUTHOR_DATE=2026-10-30T12:10:00+02:00", "GIT_COMMITTER_DATE=2026-11-01T10:00:00Z"}, "commit", "--allow-empty", "-qm", "Editor\n\n"+tt.key+tt.separator+strings.Join(tt.sessions, "\n"+tt.key+tt.separator))
		want = append([][]string{tt.sessions}, want...)
		commits, err := ScanCommitTrailers(t.Context(), repo)
		require.NoError(t, err)
		require.Len(t, commits, len(want))
		for i, commit := range commits {
			assert.Equal(t, want[i], commit.Sessions)
			assert.Equal(t, "2026-10-30T10:10:00Z", commit.CommittedAt)
		}
	}
}

func TestCommitTrailersDetachedWorktree(t *testing.T) {
	skipIfNoGit(t)
	repo := initRepo(t)
	gitRun(t, repo, nil, "commit", "--allow-empty", "-qm", "Initial")
	worktree := t.TempDir()
	gitRun(t, repo, nil, "worktree", "add", "--detach", worktree)
	gitRun(t, worktree, nil, "commit", "--allow-empty", "-qm", "Detached editor\n\nAgent-Session: codex:detached")
	commits, err := ScanCommitTrailers(t.Context(), repo)
	require.NoError(t, err)
	require.Len(t, commits, 1)
	assert.Equal(t, []string{"codex:detached"}, commits[0].Sessions)
	assert.Empty(t, commits[0].Branch)
}
