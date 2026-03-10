package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
			got := ExtractProjectFromCwd(cwd)
			if got != tt.want {
				t.Fatalf("ExtractProjectFromCwd(%q) = %q, want %q", cwd, got, tt.want)
			}
		})
	}
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractProjectFromCwdWithBranch(tt.cwd, tt.branch)
			if got != tt.want {
				t.Fatalf("ExtractProjectFromCwdWithBranch(%q, %q) = %q, want %q", tt.cwd, tt.branch, got, tt.want)
			}
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
	got := ExtractProjectFromCwdWithBranch(
		`C:\Users\dev\projects\my-app`, "",
	)
	if got != "my_app" {
		t.Errorf(
			"foreign Windows path: got %q, want %q",
			got, "my_app",
		)
	}
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

	got := ExtractProjectFromCwd(subdir)
	if got != "my_repo" {
		t.Errorf(
			"native Windows git path: got %q, want %q",
			got, "my_repo",
		)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
