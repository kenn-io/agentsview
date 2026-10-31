package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func TestCommitObjectsAndTrailerPages(t *testing.T) {
	repo := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ctx := t.Context()
	r := gitcmd.New()
	r.Env = append(os.Environ(), "HOME="+home)
	run := func(args ...string) string {
		out, err := r.Output(ctx, repo, args...)
		require.NoError(t, err)
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	run("config", "user.name", "Fixture")
	run("config", "user.email", "fixture@example.test")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "example.go"), []byte("package example\n"), 0600))
	run("add", "example.go")
	run("commit", "-m", "Initial\n\nAgent-Session: custom:one")
	first := run("rev-parse", "HEAD")
	run("checkout", "-b", "side")
	run("commit", "--allow-empty", "-m", "Side\n\nAgent-Session: custom:two")
	second := run("rev-parse", "HEAD")
	run("checkout", "main")
	object, err := ReadCommitObject(ctx, repo, first[:7])
	require.NoError(t, err)
	assert.Equal(t, first, object.SHA)
	assert.Equal(t, "Initial", object.Subject)
	assert.Equal(t, []string{"example.go"}, object.Paths)
	tips, err := SnapshotCommitTips(ctx, repo)
	require.NoError(t, err)
	assert.Contains(t, tips, first)
	assert.Contains(t, tips, second)
	page, err := ScanSessionTrailersPage(ctx, repo, tips, 0, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	next, err := ScanSessionTrailersPage(ctx, repo, tips, 1, 1)
	require.NoError(t, err)
	require.Len(t, next, 1)
	assert.NotEqual(t, page[0].SHA, next[0].SHA)
	delta, err := ScanSessionTrailersPageExcluding(ctx, repo, []string{second}, []string{first}, 0, 10)
	require.NoError(t, err)
	require.Len(t, delta, 1, "a completed frontier excludes its reachable history")
	assert.Equal(t, second, delta[0].SHA)
	unavailableExclusion, err := ScanSessionTrailersPageExcluding(ctx, repo, []string{second}, []string{strings.Repeat("0", 40)}, 0, 10)
	require.NoError(t, err, "a pruned exclusion is discarded so the current tip can still scan")
	require.Len(t, unavailableExclusion, 2)
	_, err = ReadCommitObject(t.Context(), repo, "--help")
	assert.Error(t, err)
	_, err = ReadCommitObject(ctx, filepath.Join(repo, "missing"), first[:7])
	assert.Error(t, err)
}
