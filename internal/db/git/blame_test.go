package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlameCommits(t *testing.T) {
	skipIfNoGit(t)
	repo := initRepo(t)
	writeFile(t, repo, "a.txt", []byte("first\nsecond\nthird\n"))
	commitAs(t, repo, "test@example.com", "Test User", "Initial lines")
	writeFile(t, repo, "a.txt", []byte("first\nchanged\nthird\n"))
	commitAs(t, repo, "test@example.com", "Test User", "Change middle")
	got, err := BlameCommits(t.Context(), repo, "a.txt", 0, 0)
	require.NoError(t, err)
	require.Len(t, got.Commits, 2)
	assert.Equal(t, "Initial lines", got.Commits[0].Subject)
	assert.Equal(t, 2, got.Commits[0].Lines)
	assert.Equal(t, "Change middle", got.Commits[1].Subject)
	assert.Equal(t, 1, got.Commits[1].Lines)
	assert.NotEqual(t, got.Commits[0].SHA, got.Commits[1].SHA)
	_, err = BlameCommits(t.Context(), repo, "a.txt", 20, 30)
	require.ErrorContains(t, err, "has only 3 lines")
}

func TestBlameCommitsSHA256(t *testing.T) {
	skipIfNoGit(t)
	repo := t.TempDir()
	gitRun(t, repo, nil, "init", "-q", "-b", "main", "--object-format=sha256")
	configureTestRepoIdentity(t, repo)
	writeFile(t, repo, "a.txt", []byte("committed\noriginal\n"))
	commitAs(t, repo, "test@example.com", "Test User", "Initial lines")
	writeFile(t, repo, "a.txt", []byte("committed\nuncommitted\n"))
	got, err := BlameCommits(t.Context(), repo, "a.txt", 0, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, got.Uncommitted)
	require.Len(t, got.Commits, 1)
	assert.Len(t, got.Commits[0].SHA, 64)
	assert.Equal(t, "Initial lines", got.Commits[0].Subject)
	assert.Equal(t, 1, got.Commits[0].Lines)
}
