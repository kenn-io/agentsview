package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlameHonorsUserGlobalConfig(t *testing.T) {
	skipIfNoGit(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	config := []byte("[blame]\n\tignoreRevsFile = missing-revisions\n")
	require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"), config, 0o600))

	repo := initRepo(t)
	writeFile(t, repo, "a.txt", []byte("first\n"))
	commitAs(t, repo, "test@example.com", "Test User", "Initial lines")
	_, err := BlameCommits(t.Context(), repo, "a.txt", 0, 0)
	require.ErrorContains(t, err, "missing-revisions")
}

func TestBlameCommits(t *testing.T) {
	skipIfNoGit(t)
	repo := initRepo(t)
	writeFile(t, repo, "a.txt", []byte("first\nsecond\nthird\n"))
	commitAs(t, repo, "test@example.com", "Test User", "Initial lines")
	gitRun(t, repo, nil, "config", "i18n.logOutputEncoding", "ISO-8859-1")
	writeFile(t, repo, "a.txt", []byte("first\nchanged\nthird\n"))
	commitAs(t, repo, "test@example.com", "Test User", "Fix café")
	got, err := BlameCommits(t.Context(), repo, "a.txt", 0, 0)
	require.NoError(t, err)
	require.Len(t, got.Commits, 2)
	assert.Equal(t, "Initial lines", got.Commits[0].Subject)
	assert.Equal(t, 2, got.Commits[0].Lines)
	assert.Equal(t, "Fix café", got.Commits[1].Subject)
	assert.Equal(t, 1, got.Commits[1].Lines)
	assert.NotEqual(t, got.Commits[0].SHA, got.Commits[1].SHA)

	got, err = BlameCommits(t.Context(), repo, "a.txt", 2, 3)
	require.NoError(t, err)
	require.Len(t, got.Commits, 2)
	assert.Equal(t, "Fix café", got.Commits[0].Subject)
	assert.Equal(t, 1, got.Commits[0].Lines)
	assert.Equal(t, "Initial lines", got.Commits[1].Subject)
	assert.Equal(t, 1, got.Commits[1].Lines)

	_, err = BlameCommits(t.Context(), repo, "a.txt", 20, 30)
	require.ErrorContains(t, err, "has only 3 lines")
	assert.Equal(t, 1, strings.Count(err.Error(), "has only 3 lines"))
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
