package parser

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractProjectFromCwd_Git(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string) string
		want  string
	}{
		{
			name: "GitRepoRoot",
			setup: func(t *testing.T, root string) string {
				repo := filepath.Join(root, "my-app")
				subdir := filepath.Join(repo, "internal", "sync")

				mustMkdirAll(t, filepath.Join(repo, ".git"))
				mustMkdirAll(t, subdir)
				return subdir
			},
			want: "my_app",
		},
		{
			name: "GitWorktree",
			setup: func(t *testing.T, root string) string {
				mainRepo := filepath.Join(root, "agentsview")
				worktree := filepath.Join(root, "agentsview-worktree-tool-calls")
				worktreeGitDir := filepath.Join(mainRepo, ".git", "worktrees", "feature")

				mustMkdirAll(t, filepath.Join(mainRepo, ".git"))
				mustMkdirAll(t, worktreeGitDir)
				mustMkdirAll(t, filepath.Join(worktree, "internal"))

				mustWriteFile(t, filepath.Join(worktree, ".git"),
					"gitdir: "+worktreeGitDir+"\n")
				// Matches git's linked-worktree layout.
				mustWriteFile(t, filepath.Join(worktreeGitDir, "commondir"), "../..\n")

				return filepath.Join(worktree, "internal")
			},
			want: "agentsview",
		},
		{
			name: "GitWorktreeFallbackWithoutCommondir",
			setup: func(t *testing.T, root string) string {
				mainRepo := filepath.Join(root, "my-repo")
				worktree := filepath.Join(root, "my-repo-experiment")
				worktreeGitDir := filepath.Join(mainRepo, ".git", "worktrees", "exp")

				mustMkdirAll(t, filepath.Join(mainRepo, ".git"))
				mustMkdirAll(t, worktreeGitDir)
				mustMkdirAll(t, worktree)

				mustWriteFile(t, filepath.Join(worktree, ".git"),
					"gitdir: "+worktreeGitDir+"\n")

				return worktree
			},
			want: "my_repo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cwd := tt.setup(t, root)
			assert.Equal(t, tt.want, ExtractProjectFromCwd(cwd),
				"ExtractProjectFromCwd(%q)", cwd)
		})
	}
}

func TestExtractProjectFromCwdWithBranchContext_GitWorktreeMainRoot(t *testing.T) {
	skipIfNoGit(t)

	root := t.TempDir()
	mainRepo := filepath.Join(root, "agentsview")
	mustMkdirAll(t, mainRepo)
	gitRun(t, mainRepo, "init", "-q", "-b", "main")
	gitRun(t, mainRepo, "config", "user.email", "test@example.com")
	gitRun(t, mainRepo, "config", "user.name", "Test User")
	gitRun(t, mainRepo, "config", "commit.gpgsign", "false")
	mustWriteFile(t, filepath.Join(mainRepo, "README.md"), "seed\n")
	gitRun(t, mainRepo, "add", "-A")
	gitRun(t, mainRepo, "commit", "-q", "-m", "seed")

	worktree := filepath.Join(root, "agentsview-feature")
	gitRun(t, mainRepo, "worktree", "add", "-q", "-b", "feature", worktree)
	subdir := filepath.Join(worktree, "internal", "parser")
	mustMkdirAll(t, subdir)

	got := ExtractProjectFromCwdWithBranchContext(
		context.Background(), subdir, "feature",
	)
	assert.Equal(t, "agentsview", got,
		"kit-backed worktree resolution should use the main repo name")
}

func TestExtractProjectFromCwd_DeletedNestedWorktree(t *testing.T) {
	// Simulates a nested worktree layout where the session's
	// worktree has been deleted but a sibling worktree still
	// exists and can reveal the true repo root.
	root := t.TempDir()

	mainRepo := filepath.Join(root, "my-project")
	mustMkdirAll(t, filepath.Join(mainRepo, ".git", "worktrees", "other-branch"))

	container := filepath.Join(root, "worktrees", "my-project")
	sibling := filepath.Join(container, "other-branch")
	mustMkdirAll(t, sibling)

	worktreeGitDir := filepath.Join(
		mainRepo, ".git", "worktrees", "other-branch",
	)
	mustWriteFile(t, filepath.Join(sibling, ".git"),
		"gitdir: "+worktreeGitDir+"\n")
	mustWriteFile(t, filepath.Join(worktreeGitDir, "commondir"),
		"../..\n")

	// The deleted worktree path — not created on disk.
	deleted := filepath.Join(container, "tauri-packaging")

	assert.Equal(t, "my_project", ExtractProjectFromCwd(deleted),
		"ExtractProjectFromCwd(%q)", deleted)
}

func TestExtractProjectFromCwd_DeletedNestedWorktreeNoCommondir(
	t *testing.T,
) {
	// When commondir is missing, repoRootFromGitFile falls back
	// to the strings.Cut marker check. The gitdir must be
	// normalized so the separator-based marker matches.
	root := t.TempDir()

	mainRepo := filepath.Join(root, "my-project")
	worktreeGitDir := filepath.Join(
		mainRepo, ".git", "worktrees", "other-branch",
	)
	mustMkdirAll(t, filepath.Join(mainRepo, ".git"))
	mustMkdirAll(t, worktreeGitDir)
	// No commondir file written.

	container := filepath.Join(root, "worktrees", "my-project")
	sibling := filepath.Join(container, "other-branch")
	mustMkdirAll(t, sibling)

	mustWriteFile(t, filepath.Join(sibling, ".git"),
		"gitdir: "+worktreeGitDir+"\n")

	deleted := filepath.Join(container, "tauri-packaging")

	assert.Equal(t, "my_project", ExtractProjectFromCwd(deleted),
		"ExtractProjectFromCwd(%q)", deleted)
}

func TestExtractProjectFromCwd_DeletedNestedWorktreeDeep(
	t *testing.T,
) {
	// When the deleted worktree path includes subdirectories
	// (e.g. .../tauri-packaging/cmd/server), the walk must
	// reach the first existing ancestor before sibling detection.
	root := t.TempDir()

	mainRepo := filepath.Join(root, "my-project")
	mustMkdirAll(t, filepath.Join(
		mainRepo, ".git", "worktrees", "other-branch",
	))

	container := filepath.Join(root, "worktrees", "my-project")
	sibling := filepath.Join(container, "other-branch")
	mustMkdirAll(t, sibling)

	worktreeGitDir := filepath.Join(
		mainRepo, ".git", "worktrees", "other-branch",
	)
	mustWriteFile(t, filepath.Join(sibling, ".git"),
		"gitdir: "+worktreeGitDir+"\n")
	mustWriteFile(t, filepath.Join(worktreeGitDir, "commondir"),
		"../..\n")

	// Nested path inside a deleted worktree — neither
	// tauri-packaging/ nor cmd/server/ exist on disk.
	deep := filepath.Join(
		container, "tauri-packaging", "cmd", "server",
	)

	assert.Equal(t, "my_project", ExtractProjectFromCwd(deep),
		"ExtractProjectFromCwd(%q)", deep)
}

func TestExtractProjectFromCwd_SubmoduleSiblingIgnored(
	t *testing.T,
) {
	// A sibling directory with a submodule .git file (pointing
	// to .git/modules/) must not be mistaken for a linked
	// worktree. The function should return "" rather than the
	// submodule's repo root.
	root := t.TempDir()

	parentRepo := filepath.Join(root, "parent-repo")
	mustMkdirAll(t, filepath.Join(
		parentRepo, ".git", "modules", "sub-lib",
	))

	container := filepath.Join(root, "worktrees", "parent-repo")
	submod := filepath.Join(container, "sub-lib")
	mustMkdirAll(t, submod)

	// Submodule .git file: points to .git/modules/, not
	// .git/worktrees/.
	submodGitDir := filepath.Join(
		parentRepo, ".git", "modules", "sub-lib",
	)
	mustWriteFile(t, filepath.Join(submod, ".git"),
		"gitdir: "+submodGitDir+"\n")

	deleted := filepath.Join(container, "deleted-branch")

	// No worktree sibling found, falls back to basename.
	assert.Equal(t, "deleted_branch", ExtractProjectFromCwd(deleted),
		"ExtractProjectFromCwd(%q)", deleted)
}

func TestExtractProjectFromCwd_UnrelatedSiblingWorktree(
	t *testing.T,
) {
	// When sibling worktrees belong to different repos, sibling
	// detection must bail out to avoid misattributing the path.
	root := t.TempDir()

	repoA := filepath.Join(root, "repo-a")
	mustMkdirAll(t, filepath.Join(
		repoA, ".git", "worktrees", "feature-a",
	))
	repoB := filepath.Join(root, "repo-b")
	mustMkdirAll(t, filepath.Join(
		repoB, ".git", "worktrees", "feature-b",
	))

	container := filepath.Join(root, "mixed")
	sibA := filepath.Join(container, "feature-a")
	sibB := filepath.Join(container, "feature-b")
	mustMkdirAll(t, sibA)
	mustMkdirAll(t, sibB)

	gitDirA := filepath.Join(
		repoA, ".git", "worktrees", "feature-a",
	)
	mustWriteFile(t, filepath.Join(sibA, ".git"),
		"gitdir: "+gitDirA+"\n")
	mustWriteFile(t, filepath.Join(gitDirA, "commondir"),
		"../..\n")

	gitDirB := filepath.Join(
		repoB, ".git", "worktrees", "feature-b",
	)
	mustWriteFile(t, filepath.Join(sibB, ".git"),
		"gitdir: "+gitDirB+"\n")
	mustWriteFile(t, filepath.Join(gitDirB, "commondir"),
		"../..\n")

	deleted := filepath.Join(container, "deleted-thing")

	// Siblings disagree, falls back to basename.
	assert.Equal(t, "deleted_thing", ExtractProjectFromCwd(deleted),
		"ExtractProjectFromCwd(%q)", deleted)
}

func TestExtractProjectFromCwd_AncestorHasGitDir(
	t *testing.T,
) {
	// When the first existing ancestor has its own .git,
	// sibling detection must be skipped so the normal upward
	// walk finds it instead.
	root := t.TempDir()

	repo := filepath.Join(root, "my-repo")
	mustMkdirAll(t, filepath.Join(repo, ".git"))

	// A deleted path inside the repo.
	deleted := filepath.Join(repo, "deleted-subdir", "file")

	assert.Equal(t, "my_repo", ExtractProjectFromCwd(deleted),
		"ExtractProjectFromCwd(%q)", deleted)
}

func TestExtractProjectFromCwd_RepoSiblingWithWorktree(
	t *testing.T,
) {
	// A container with a normal repo (.git dir) alongside a
	// linked worktree from a different project is not a
	// dedicated worktree container. Sibling detection must
	// bail out to avoid misattributing the deleted path.
	root := t.TempDir()

	normalRepo := filepath.Join(root, "container", "repo-a")
	mustMkdirAll(t, filepath.Join(normalRepo, ".git"))

	otherRepo := filepath.Join(root, "repo-b")
	mustMkdirAll(t, filepath.Join(
		otherRepo, ".git", "worktrees", "feature-b",
	))

	worktree := filepath.Join(root, "container", "feature-b")
	mustMkdirAll(t, worktree)
	gitDirB := filepath.Join(
		otherRepo, ".git", "worktrees", "feature-b",
	)
	mustWriteFile(t, filepath.Join(worktree, ".git"),
		"gitdir: "+gitDirB+"\n")
	mustWriteFile(t, filepath.Join(gitDirB, "commondir"),
		"../..\n")

	deleted := filepath.Join(root, "container", "deleted-dir")

	// Container has a normal repo child, not a worktree-only
	// container. Falls back to basename.
	assert.Equal(t, "deleted_dir", ExtractProjectFromCwd(deleted),
		"ExtractProjectFromCwd(%q)", deleted)
}

func TestExtractProjectFromCwd_MainRepoWithOwnWorktrees(
	t *testing.T,
) {
	// A container with a main checkout (.git dir) alongside
	// linked worktrees of the SAME repo should still resolve
	// to the repo root, not fall back to basename.
	root := t.TempDir()

	mainRepo := filepath.Join(root, "container", "my-project")
	mustMkdirAll(t, filepath.Join(
		mainRepo, ".git", "worktrees", "feature",
	))

	worktree := filepath.Join(root, "container", "my-project-feature")
	mustMkdirAll(t, worktree)
	gitDir := filepath.Join(
		mainRepo, ".git", "worktrees", "feature",
	)
	mustWriteFile(t, filepath.Join(worktree, ".git"),
		"gitdir: "+gitDir+"\n")
	mustWriteFile(t, filepath.Join(gitDir, "commondir"),
		"../..\n")

	deleted := filepath.Join(
		root, "container", "my-project-hotfix",
	)

	// Main repo and worktree agree on same root.
	assert.Equal(t, "my_project", ExtractProjectFromCwd(deleted),
		"ExtractProjectFromCwd(%q)", deleted)
}

func TestExtractProjectFromCwd_DeletedSiblingOfNormalRepo(
	t *testing.T,
) {
	// A deleted path next to a single normal repo (no linked
	// worktrees) must NOT be claimed by that repo. Without
	// worktree evidence, sibling detection should not fire.
	root := t.TempDir()

	container := filepath.Join(root, "container")
	normalRepo := filepath.Join(container, "my-project")
	mustMkdirAll(t, filepath.Join(normalRepo, ".git"))

	// Deleted path — just a former directory, not a worktree.
	deleted := filepath.Join(container, "scratch-old")

	assert.Equal(t, "scratch_old", ExtractProjectFromCwd(deleted),
		"ExtractProjectFromCwd(%q)", deleted)
}

func TestExtractProjectFromCwd_UnrelatedDeletedNextToWorktreeRepo(
	t *testing.T,
) {
	// A repo that uses worktrees should NOT claim an unrelated
	// deleted sibling that doesn't match any worktree entry.
	root := t.TempDir()

	container := filepath.Join(root, "container")
	mainRepo := filepath.Join(container, "my-project")
	mustMkdirAll(t, filepath.Join(
		mainRepo, ".git", "worktrees", "feature-a",
	))

	// Deleted path that is NOT a worktree of this repo.
	deleted := filepath.Join(container, "scratch-old")

	assert.Equal(t, "scratch_old", ExtractProjectFromCwd(deleted),
		"ExtractProjectFromCwd(%q)", deleted)
}

func TestExtractProjectFromCwd_DeletedOnlyWorktreeNextToMain(
	t *testing.T,
) {
	// When the only worktree is deleted but the main checkout
	// still exists with a .git/worktrees/ directory, sibling
	// detection should still resolve to the repo root.
	root := t.TempDir()

	container := filepath.Join(root, "container")
	mainRepo := filepath.Join(container, "my-project")
	mustMkdirAll(t, filepath.Join(
		mainRepo, ".git", "worktrees", "feature",
	))

	// Deleted worktree — not created on disk.
	deleted := filepath.Join(container, "feature")

	assert.Equal(t, "my_project", ExtractProjectFromCwd(deleted),
		"ExtractProjectFromCwd(%q)", deleted)
}

func TestExtractProjectFromCwdWithBranch_NestedWorktree(
	t *testing.T,
) {
	// When a nested worktree is deleted and the branch name
	// matches the directory name, the sibling-based git root
	// detection should resolve the correct project name.
	root := t.TempDir()

	mainRepo := filepath.Join(root, "agentsview")
	mustMkdirAll(t, filepath.Join(
		mainRepo, ".git", "worktrees", "fix-worktrees",
	))

	container := filepath.Join(root, "worktrees", "agentsview")
	sibling := filepath.Join(container, "fix-worktrees")
	mustMkdirAll(t, sibling)

	worktreeGitDir := filepath.Join(
		mainRepo, ".git", "worktrees", "fix-worktrees",
	)
	mustWriteFile(t, filepath.Join(sibling, ".git"),
		"gitdir: "+worktreeGitDir+"\n")
	mustWriteFile(t, filepath.Join(worktreeGitDir, "commondir"),
		"../..\n")

	// Deleted worktree where branch name = directory name.
	deleted := filepath.Join(container, "tauri-packaging")

	assert.Equal(t, "agentsview",
		ExtractProjectFromCwdWithBranch(deleted, "tauri-packaging"),
		"ExtractProjectFromCwdWithBranch(%q, %q)", deleted, "tauri-packaging")
}

func TestExtractProjectFromCwdWithBranch(t *testing.T) {
	tests := []struct {
		name   string
		cwd    string
		branch string
		want   string
	}{
		{
			name:   "OfflineWorktreePath",
			cwd:    filepath.FromSlash("/Users/wesm/code/agentsview-worktree-tool-call-arguments"),
			branch: "worktree-tool-call-arguments",
			want:   "agentsview",
		},
		{
			name:   "BranchWithSlash",
			cwd:    filepath.FromSlash("/Users/wesm/code/agentsview-feature-worktree-support"),
			branch: "feature/worktree-support",
			want:   "agentsview",
		},
		{
			name:   "MismatchNoTrim",
			cwd:    filepath.FromSlash("/Users/wesm/code/agentsview-hotfix"),
			branch: "feature/other",
			want:   "agentsview_hotfix",
		},
		{
			name:   "DefaultBranchNoTrim",
			cwd:    filepath.FromSlash("/Users/wesm/code/project-main"),
			branch: "main",
			want:   "project_main",
		},
		{
			name:   "SupersetWorktreeFlat",
			cwd:    filepath.FromSlash("/Users/wesm/.superset/worktrees/agentsview/tauri-packaging"),
			branch: "tauri-packaging",
			want:   "agentsview",
		},
		{
			name:   "SupersetWorktreeNested",
			cwd:    filepath.FromSlash("/Users/wesm/.superset/worktrees/agentsview/fix/worktrees"),
			branch: "fix/worktrees",
			want:   "agentsview",
		},
		{
			name:   "SupersetWorktreeContainerOnly",
			cwd:    filepath.FromSlash("/Users/wesm/.superset/worktrees/agentsview"),
			branch: "",
			want:   "agentsview",
		},
		{
			name:   "ConductorWorktreeFlat",
			cwd:    filepath.FromSlash("/Users/wesm/conductor/workspaces/my-app/feature-branch"),
			branch: "feature-branch",
			want:   "my_app",
		},
		{
			name:   "ConductorWorktreeNested",
			cwd:    filepath.FromSlash("/Users/wesm/conductor/workspaces/my-app/fix/auth-bug"),
			branch: "fix/auth-bug",
			want:   "my_app",
		},
		{
			name: "MiddlemanGitHubWorktree",
			cwd: filepath.FromSlash(
				"/Users/wesm/.config/middleman/worktrees/github.com/wesm/middleman/pr-205",
			),
			branch: "fix-exited-agent-session-cleanup",
			want:   "middleman",
		},
		{
			name: "MiddlemanGitHubWorktreeSubdir",
			cwd: filepath.FromSlash(
				"/Users/wesm/.config/middleman/worktrees/github.com/wesm/middleman/pr-205/internal/parser",
			),
			branch: "fix-exited-agent-session-cleanup",
			want:   "middleman",
		},
		{
			name: "CodexAppWorktree",
			cwd: filepath.FromSlash(
				"/Users/wesm/.codex/worktrees/44be/middleman/internal/parser",
			),
			branch: "fix-exited-agent-session-cleanup",
			want:   "middleman",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				ExtractProjectFromCwdWithBranch(tt.cwd, tt.branch),
				"ExtractProjectFromCwdWithBranch(%q, %q)", tt.cwd, tt.branch)
		})
	}
}

func TestForeignWindowsPathSkipsGitRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test is for non-Windows hosts only")
	}

	// On non-Windows, a Windows-style path like C:\repo\subdir
	// should NOT trigger findGitRepoRoot (which would walk the
	// process CWD). It should fall back to the basename.
	assert.Equal(t, "my_app",
		ExtractProjectFromCwdWithBranch(`C:\Users\dev\projects\my-app`, ""),
		"foreign Windows path")
}

func TestNativeWindowsPathUsesGitRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("test is for Windows hosts only")
	}

	// On Windows, a drive-letter path inside a git repo should
	// still resolve to the repo root name, not the leaf dir.
	root := t.TempDir()
	repo := filepath.Join(root, "my-repo")
	subdir := filepath.Join(repo, "cmd", "server")
	mustMkdirAll(t, filepath.Join(repo, ".git"))
	mustMkdirAll(t, subdir)

	assert.Equal(t, "my_repo", ExtractProjectFromCwd(subdir),
		"native Windows git path")
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, 0o755), "MkdirAll(%q)", path)
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644),
		"WriteFile(%q)", path)
}

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available on PATH: %v", err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
}
