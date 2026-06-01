package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skipIfNoGit lets CI environments without git on PATH pass cleanly instead
// of failing the package.
func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available on PATH: %v", err)
	}
}

// gitRun executes a git subcommand inside repo and fails the test on error.
// Env overrides let callers control author identity per commit.
func gitRun(t *testing.T, repo string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
}

// initRepo creates a fresh repo at t.TempDir() with a deterministic default
// identity. Individual commits override the author via GIT_AUTHOR_* envs.
func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, nil, "init", "-q", "-b", "main")
	gitRun(t, repo, nil, "config", "user.email", "test@example.com")
	gitRun(t, repo, nil, "config", "user.name", "Test User")
	// Disable signing so tests don't hang on a GPG/passphrase prompt
	// when the user's global config has signing enabled.
	gitRun(t, repo, nil, "config", "commit.gpgsign", "false")
	return repo
}

// writeFile writes content under repo/relpath, creating parents as needed.
func writeFile(t *testing.T, repo, relpath string, content []byte) {
	t.Helper()
	p := filepath.Join(repo, relpath)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755),
		"mkdir %s", filepath.Dir(p))
	require.NoError(t, os.WriteFile(p, content, 0o644), "write %s", p)
}

// commitAs stages all changes and commits with an explicit author identity.
func commitAs(t *testing.T, repo, email, name, message string) {
	t.Helper()
	env := []string{
		"GIT_AUTHOR_NAME=" + name,
		"GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name,
		"GIT_COMMITTER_EMAIL=" + email,
	}
	gitRun(t, repo, nil, "add", "-A")
	gitRun(t, repo, env, "commit", "-q", "-m", message)
}

func TestAggregateLog_CountsCommitsLOCAndFiles(t *testing.T) {
	skipIfNoGit(t)
	repo := initRepo(t)

	// Commit 1 (test@example.com): add a.txt with 3 lines.
	writeFile(t, repo, "a.txt", []byte("a1\na2\na3\n"))
	commitAs(t, repo, "test@example.com", "Test User", "c1: add a.txt")

	// Commit 2 (test@example.com): modify a.txt (+3 -1) and add b.txt (+5).
	writeFile(t, repo, "a.txt", []byte("a1\na2-changed\na3\na4\na5\n"))
	writeFile(t, repo, "b.txt", []byte("b1\nb2\nb3\nb4\nb5\n"))
	commitAs(t, repo, "test@example.com", "Test User", "c2: edit a.txt, add b.txt")

	// Commit 3 (test@example.com): add a binary file (null byte triggers git's
	// binary detection, so numstat emits "-\t-\t...").
	writeFile(t, repo, "binary.dat", []byte{0x00, 0x01, 0x02, 0x03, 0xff})
	commitAs(t, repo, "test@example.com", "Test User", "c3: add binary")

	// Non-matching commit (other@example.com): should be excluded.
	writeFile(t, repo, "a.txt", []byte("a1\na2-changed\na3\na4\na5\nfrom-other\n"))
	commitAs(t, repo, "other@example.com", "Other User", "c4: other author")

	// Use a wide window — all commits are "now".
	got, err := AggregateLog(
		context.Background(),
		repo, "test@example.com",
		"1970-01-01T00:00:00Z", "2099-01-01T00:00:00Z",
	)
	require.NoError(t, err, "AggregateLog")

	// Expected totals for test@example.com across the three commits. Values
	// reflect git's diff for each commit; verified manually via
	// `git log --numstat --format=%H` against an identical fixture.
	//   c1 a.txt      +3 -0
	//   c2 a.txt      +3 -1  (line 2 replaced + two trailing lines added)
	//   c2 b.txt      +5 -0
	//   c3 binary.dat  0  0  (binary: LOC skipped, file still counted)
	want := LogResult{
		Commits:      3,
		LOCAdded:     11,
		LOCRemoved:   1,
		FilesChanged: 4,
	}
	assert.Equal(t, want, got, "AggregateLog")
}

func TestAggregateLog_EmptyWindowReturnsZero(t *testing.T) {
	skipIfNoGit(t)
	repo := initRepo(t)

	writeFile(t, repo, "a.txt", []byte("hello\n"))
	commitAs(t, repo, "test@example.com", "Test User", "c1")

	// Window in the distant past — no commits fall inside.
	got, err := AggregateLog(
		context.Background(),
		repo, "test@example.com",
		"1970-01-01T00:00:00Z", "1970-01-02T00:00:00Z",
	)
	require.NoError(t, err, "AggregateLog")
	assert.Equal(t, LogResult{}, got, "AggregateLog")
}

func TestAggregateLog_UnknownAuthorReturnsZero(t *testing.T) {
	skipIfNoGit(t)
	repo := initRepo(t)

	writeFile(t, repo, "a.txt", []byte("hello\n"))
	commitAs(t, repo, "test@example.com", "Test User", "c1")

	got, err := AggregateLog(
		context.Background(),
		repo, "nobody@example.invalid",
		"1970-01-01T00:00:00Z", "2099-01-01T00:00:00Z",
	)
	require.NoError(t, err, "AggregateLog")
	assert.Equal(t, LogResult{}, got, "AggregateLog")
}

func TestAggregateLog_BadRepoReturnsError(t *testing.T) {
	skipIfNoGit(t)
	// A temp directory that is NOT a git repo.
	notARepo := t.TempDir()

	_, err := AggregateLog(
		context.Background(),
		notARepo, "test@example.com",
		"1970-01-01T00:00:00Z", "2099-01-01T00:00:00Z",
	)
	require.Error(t, err, "AggregateLog on non-repo")
}

// TestAggregateLog_EmptyRepoReturnsZero covers the "git init but no
// commits yet" case (e.g., a freshly-created worktree). git exits 128
// with "your current branch 'main' does not have any commits yet";
// this is normal state, not an error, so AggregateLog must return a
// zero LogResult and nil error rather than spamming the user log.
func TestAggregateLog_EmptyRepoReturnsZero(t *testing.T) {
	skipIfNoGit(t)
	repo := initRepo(t) // creates the repo but never commits

	got, err := AggregateLog(
		context.Background(),
		repo, "test@example.com",
		"1970-01-01T00:00:00Z", "2099-01-01T00:00:00Z",
	)
	require.NoError(t, err, "AggregateLog on empty repo")
	assert.Equal(t, LogResult{}, got, "AggregateLog on empty repo")
}

func TestAggregateLog_UsesGlobalGitConfig(t *testing.T) {
	skipIfNoGit(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	globalConfig := filepath.Join(home, ".gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

	attrsPath := filepath.Join(home, "attributes")
	require.NoError(t, os.WriteFile(
		attrsPath, []byte("*.txt binary\n"), 0o644,
	), "write global attributes")
	require.NoError(t, os.WriteFile(
		globalConfig,
		[]byte("[core]\n\tattributesfile = "+filepath.ToSlash(attrsPath)+"\n"),
		0o644,
	), "write global git config")

	repo := initRepo(t)
	writeFile(t, repo, "configured-binary.txt", []byte("a\nb\nc\n"))
	commitAs(t, repo, "test@example.com", "Test User", "c1")

	got, err := AggregateLog(
		context.Background(),
		repo, "test@example.com",
		"1970-01-01T00:00:00Z", "2099-01-01T00:00:00Z",
	)
	require.NoError(t, err, "AggregateLog")
	assert.Equal(t, LogResult{
		Commits:      1,
		LOCAdded:     0,
		LOCRemoved:   0,
		FilesChanged: 1,
	}, got, "AggregateLog should respect global git config")
}

func TestAuthorEmail_LocalConfig(t *testing.T) {
	skipIfNoGit(t)
	repo := t.TempDir()
	gitRun(t, repo, nil, "init", "-q", "-b", "main")
	gitRun(t, repo, nil, "config", "user.email", "local@example.com")

	got := AuthorEmail(context.Background(), repo)
	assert.Equal(t, "local@example.com", got, "AuthorEmail")
}

func TestAuthorEmail_FallsBackToGlobal(t *testing.T) {
	skipIfNoGit(t)
	// Isolate HOME + XDG_CONFIG_HOME so the "global" config is a scratch file
	// we control; the local repo intentionally has no user.email set.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	// Some git builds also consult GIT_CONFIG_GLOBAL; point it at a known path.
	globalCfg := filepath.Join(home, ".gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	// Seed the global config with our expected email.
	setGlobal := exec.Command("git", "config", "--global", "user.email", "global@example.com")
	setGlobal.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL="+globalCfg,
	)
	out, err := setGlobal.CombinedOutput()
	require.NoError(t, err, "seed global config: %s", out)

	repo := t.TempDir()
	// Init with no local user.email — `AuthorEmail` must fall through to global.
	initCmd := exec.Command("git", "init", "-q", "-b", "main")
	initCmd.Dir = repo
	initCmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL="+globalCfg,
	)
	out, err = initCmd.CombinedOutput()
	require.NoError(t, err, "git init: %s", out)

	got := AuthorEmail(context.Background(), repo)
	assert.Equal(t, "global@example.com", got, "AuthorEmail (global fallback)")
}

func TestAuthorEmail_UsesIncludeIfGitdir(t *testing.T) {
	skipIfNoGit(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	globalConfig := filepath.Join(home, ".gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

	repo := t.TempDir()
	gitRun(t, repo, nil, "init", "-q", "-b", "main")

	includePath := filepath.Join(home, "repo.gitconfig")
	require.NoError(t, os.WriteFile(
		includePath,
		[]byte("[user]\n\temail = includeif@example.com\n"),
		0o644,
	), "write include config")
	gitdir, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	require.NoError(t, err, "resolve repo gitdir")
	require.NoError(t, os.WriteFile(
		globalConfig,
		[]byte(`[includeIf "gitdir:`+filepath.ToSlash(gitdir)+`"]
	path = `+filepath.ToSlash(includePath)+"\n"),
		0o644,
	), "write global config")

	got := AuthorEmail(context.Background(), repo)
	assert.Equal(t, "includeif@example.com", got, "AuthorEmail (includeIf.gitdir)")
}

func TestParseNumstat_SkipsBinaryLOCButCountsFile(t *testing.T) {
	// Unit test of the pure parser, independent of git exec.
	input := []byte(strings.Join([]string{
		"a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0",
		"",
		"3\t0\ta.txt",
		"-\t-\tbinary.dat",
		"",
		"b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0",
		"",
		"2\t1\ta.txt",
		"5\t0\tb.txt",
		"",
	}, "\n"))

	got := parseNumstat(input)
	want := LogResult{
		Commits:      2,
		LOCAdded:     10,
		LOCRemoved:   1,
		FilesChanged: 4,
	}
	assert.Equal(t, want, got, "parseNumstat")
}
