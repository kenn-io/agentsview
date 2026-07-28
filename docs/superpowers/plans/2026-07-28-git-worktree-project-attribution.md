# Git Worktree Project Attribution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Attribute sessions from live and removed standard Git worktrees to the
owning repository instead of a generated checkout or branch name.

**Architecture:** Extend the existing parser fast path in
`internal/parser/project.go`. Hosting-oriented paths recover the repository
component when a worktree has disappeared, while live worktrees backed by a bare
common Git directory resolve through filesystem metadata without spawning Git.
Bump the parser data version so source-backed historical sessions are rebuilt
through the corrected resolver.

**Tech Stack:** Go standard library, Git CLI in integration-style parser test
setup, `github.com/stretchr/testify`.

## Global Constraints

- Follow
  `docs/superpowers/specs/2026-07-28-git-worktree-project-attribution-design.md`.
- Preserve the existing resolution order: managed layouts before the Git-root
  walk.
- Keep routine project extraction filesystem-only; do not add a Git subprocess
  to the bare-repository fast path.
- Do not extend deleted-worktree sibling recovery to bare common repositories
  outside the recognized hosting layouts in this change.
- Do not add database schema, activity query, PostgreSQL query, or DuckDB query
  changes.
- Preserve non-destructive full-resync and orphan-copy behavior.
- Use `require.X` for setup failures and `assert.X` for independent behavior
  checks.
- Keep private paths, repository names, hostnames, and identities out of code,
  tests, documentation, commits, and pull request text.
- Run `go fmt ./...` and `go vet ./...` after Go changes.

______________________________________________________________________

### Task 1: Resolve Hosting Layouts and Bare-Backed Live Worktrees

**Files:**

- Modify: `internal/parser/project_git_test.go`
- Modify: `internal/parser/project.go`

**Interfaces:**

- Consumes: `ExtractProjectFromCwd(cwd string) string`,
  `projectFromWorktreeLayout(path string) string`,
  `repoRootFromGitFile(repoDir, gitFilePath string) string`.

- Produces: `gitConfigCoreBare(gitDir string) bool`, a filesystem-only helper
  used by `repoRootFromGitFile`.

- Preserves: `findGitRepoRoot` continues returning a path whose basename is the
  normalized project-name input. For a bare common directory, that path is a
  synthetic sibling with the conventional `.git` suffix removed; callers do
  not require the returned path to exist.

- [ ] **Step 1: Add failing hosting-layout behavior tests**

Add these tests to `internal/parser/project_git_test.go` near the existing
managed-worktree cases:

```go
func TestExtractProjectFromCwd_HostingWorktreeLayouts(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name  string
		parts []string
		want  string
	}{
		{
			name: "HostingWorktree",
			parts: []string{
				"worktrees", "github.com", "example-org",
				"sample-repo", "feature-branch",
			},
			want: "sample_repo",
		},
		{
			name: "HostingWorktreeSubdirectory",
			parts: []string{
				"worktrees", "github.com", "example-org",
				"sample-repo", "feature-branch", "internal", "parser",
			},
			want: "sample_repo",
		},
		{
			name: "NamespacedHostingWorktree",
			parts: []string{
				"worktrees", "github", "github.com", "example-org",
				"data-pipeline", "pr-17",
			},
			want: "data_pipeline",
		},
		{
			name: "NamespacedHostingWorktreeSubdirectory",
			parts: []string{
				"worktrees", "github", "github.com", "example-org",
				"data-pipeline", "pr-17", "cmd", "worker",
			},
			want: "data_pipeline",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := filepath.Join(append([]string{root}, tt.parts...)...)
			assert.Equal(t, tt.want, ExtractProjectFromCwd(cwd))
		})
	}
}

func TestProjectFromWorktreeLayoutRequiresWorktreeLeaf(t *testing.T) {
	root := t.TempDir()
	tests := []string{
		filepath.Join(
			root, "worktrees", "github.com", "example-org", "sample-repo",
		),
		filepath.Join(
			root, "worktrees", "github", "github.com",
			"example-org", "sample-repo",
		),
	}

	for _, path := range tests {
		assert.Empty(t, projectFromWorktreeLayout(path), path)
	}
}
```

These tests protect the consumer-visible project result for removed worktrees
and the layout boundary that requires a distinct worktree component. Replacing
the repository index with the worktree index or lowering `minParts` must make at
least one test fail.

- [ ] **Step 2: Add a failing real-Git test for a bare-backed live worktree**

Add this test beside
`TestExtractProjectFromCwdWithBranchContext_GitWorktreeMainRoot`:

```go
func TestExtractProjectFromCwd_BareBackedGitWorktree(t *testing.T) {
	skipIfNoGit(t)

	root := t.TempDir()
	source := filepath.Join(root, "source")
	bareRepo := filepath.Join(root, "shared", "sample-repo.git")
	worktree := filepath.Join(root, "checkouts", "generated-leaf")
	subdir := filepath.Join(worktree, "internal", "parser")

	mustMkdirAll(t, source)
	mustMkdirAll(t, filepath.Dir(bareRepo))
	mustMkdirAll(t, filepath.Dir(worktree))
	gitRun(t, source, "init", "-q", "-b", "main")
	gitRun(t, source,
		"-c", "user.email=test@example.com",
		"-c", "user.name=Test User",
		"-c", "commit.gpgsign=false",
		"commit", "--allow-empty", "-q", "-m", "seed",
	)
	gitRun(t, root, "clone", "--bare", "-q", source, bareRepo)
	gitRun(t, root,
		"--git-dir", bareRepo,
		"worktree", "add", "-q", "-b", "feature", worktree, "main",
	)
	mustMkdirAll(t, subdir)

	assert.Equal(t, "sample_repo", ExtractProjectFromCwd(subdir))
}

func TestRepoRootFromGitFileDoesNotTreatNonBareCommonDirAsBare(
	t *testing.T,
) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkouts", "generated-leaf")
	commonDir := filepath.Join(root, "shared", "sample-repo.git")
	gitDir := filepath.Join(commonDir, "worktrees", "generated-leaf")
	gitFile := filepath.Join(checkout, ".git")

	mustMkdirAll(t, checkout)
	mustMkdirAll(t, gitDir)
	mustWriteFile(t, gitFile, "gitdir: "+gitDir+"\n")
	mustWriteFile(t, filepath.Join(gitDir, "commondir"), "../..\n")
	mustWriteFile(t, filepath.Join(commonDir, "config"),
		"[core]\n\tbare = false\n")

	assert.Equal(t, checkout, repoRootFromGitFile(checkout, gitFile))
}
```

This test owns agentsview's integration contract: a standard `.git` file and
bare common directory must produce the repository name. It does not assert Git's
internal output or mock the resolver. The companion non-bare fixture protects
the fail-closed check: an implementation that strips every `.git` suffix without
reading `core.bare` will fail.

- [ ] **Step 3: Run the focused tests and verify the expected failures**

Run:

```bash
go test ./internal/parser \
  -run 'TestExtractProjectFromCwd_(HostingWorktreeLayouts|BareBackedGitWorktree)|TestProjectFromWorktreeLayoutRequiresWorktreeLeaf|TestRepoRootFromGitFileDoesNotTreatNonBareCommonDirAsBare' \
  -count=1
```

Expected: FAIL. Hosting cases return normalized worktree or subdirectory names,
and the bare-backed case returns `generated_leaf`. The incomplete-layout helper
test already passes and guards the boundary for the implementation step.

- [ ] **Step 4: Add the hosting-oriented layout definitions**

In `init()` in `internal/parser/project.go`, replace the existing tool-specific
GitHub worktree entry with the two structural layouts below. Put the namespaced
form first for readability; the markers do not overlap.

```go
		// .../worktrees/github/github.com/$OWNER/$REPO/$WORKTREE[/...]
		{
			marker: sep + "worktrees" + sep + "github" + sep +
				"github.com" + sep,
			projectPart: 1,
			minParts:    3,
		},
		// .../worktrees/github.com/$OWNER/$REPO/$WORKTREE[/...]
		{
			marker: sep + "worktrees" + sep + "github.com" + sep,
			projectPart: 1,
			minParts:    3,
		},
```

Keep the Superset, Conductor, Codex, and roborev definitions unchanged. The
generic `worktrees/github.com` entry preserves the existing managed-worktree
behavior while covering any root prefix.

- [ ] **Step 5: Add filesystem-only bare Git config detection**

Add this helper below `readCommonDir` in `internal/parser/project.go`:

```go
func gitConfigCoreBare(gitDir string) bool {
	b, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		return false
	}

	inCore := false
	for raw := range strings.SplitSeq(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" ||
			strings.HasPrefix(line, "#") ||
			strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				inCore = false
				continue
			}
			section := strings.TrimSpace(line[1:end])
			section, _, _ = strings.Cut(section, " ")
			inCore = strings.EqualFold(section, "core")
			continue
		}
		if !inCore {
			continue
		}

		key, value, hasValue := strings.Cut(line, "=")
		if !strings.EqualFold(strings.TrimSpace(key), "bare") {
			continue
		}
		if !hasValue {
			return true
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "yes", "on", "1":
			return true
		default:
			return false
		}
	}
	return false
}
```

This accepts Git's standard boolean spellings and implicit true form while
failing closed on missing, malformed, or non-boolean values.

- [ ] **Step 6: Return a confident synthetic repository path for bare common
  directories**

Extend the `commonDir != ""` block in `repoRootFromGitFile`:

```go
	commonDir := readCommonDir(gitDir)
	if commonDir != "" {
		if filepath.Base(commonDir) == ".git" {
			return filepath.Dir(commonDir)
		}
		if gitConfigCoreBare(commonDir) {
			name := strings.TrimSuffix(filepath.Base(commonDir), ".git")
			if !isInvalidPathBase(name) {
				return filepath.Join(filepath.Dir(commonDir), name)
			}
		}
	}
```

The returned path is used only for basename extraction and sibling agreement.
Returning a path different from `repoDir` also marks the local result
non-conservative, so `findGitRepoRoot` skips the existing `gitMainRoot`
subprocess that would otherwise return the worktree checkout root.

- [ ] **Step 7: Format and verify the parser behavior**

Run:

```bash
go fmt ./...
go test ./internal/parser \
  -run 'TestExtractProjectFromCwd_(HostingWorktreeLayouts|BareBackedGitWorktree)|TestProjectFromWorktreeLayoutRequiresWorktreeLeaf|TestRepoRootFromGitFileDoesNotTreatNonBareCommonDirAsBare' \
  -count=1
go test ./internal/parser -count=1
```

Expected: all commands PASS. The full parser package protects normal checkouts,
non-bare linked worktrees, submodules, managed layouts, and deleted-sibling
behavior from regression.

- [ ] **Step 8: Commit the parser fix**

Invoke the mandatory `kenn:commit` skill, then run:

```bash
git add internal/parser/project.go internal/parser/project_git_test.go
git commit -m "fix(parser): attribute standard git worktrees to repositories"
```

The commit body should explain why bare common directories and removed
hosting-layout worktrees previously fragmented project activity. Do not include
attribution blocks or routine test transcripts.

______________________________________________________________________

### Task 2: Reparse Existing Session Project Metadata

**Files:**

- Modify: `internal/db/db_test.go`
- Modify: `internal/db/db.go`

**Interfaces:**

- Consumes: `CurrentDataVersion() int` and the existing `NeedsResync`
  non-destructive rebuild flow.

- Produces: parser data version `75`.

- [ ] **Step 1: Update the exact-version regression test first**

Replace `TestCurrentDataVersionClaudeIDEContext` in `internal/db/db_test.go`
with:

```go
func TestCurrentDataVersionGitWorktreeProjectAttribution(t *testing.T) {
	assert.Equal(t, 75, CurrentDataVersion(),
		"git worktree project attribution requires a data version bump")
}
```

- [ ] **Step 2: Run the version test and verify it fails for the intended
  reason**

Run:

```bash
go test ./internal/db \
  -run TestCurrentDataVersionGitWorktreeProjectAttribution \
  -count=1
```

Expected: FAIL with expected `75` and actual `74`.

- [ ] **Step 3: Bump the parser data version**

Append this rationale to the version history in `internal/db/db.go` and change
the constant:

```go
// (75: Git worktree project attribution reparse. Hosting-oriented worktree
// paths retain the owning repository after checkout removal, and live linked
// worktrees backed by bare common repositories resolve to the repository
// instead of the generated checkout leaf. Existing rows need re-parsing so
// project activity is no longer fragmented by worktree names.)
const dataVersion = 75
```

- [ ] **Step 4: Run focused and package verification**

Run:

```bash
go fmt ./...
go test ./internal/db \
  -run TestCurrentDataVersionGitWorktreeProjectAttribution \
  -count=1
go test ./internal/parser ./internal/db -count=1
go vet ./...
git diff --check
```

Expected: all commands PASS. `go vet ./...` must report no diagnostics, and
`git diff --check` must report no whitespace errors.

- [ ] **Step 5: Commit the data-version bump**

Invoke the mandatory `kenn:commit` skill, then run:

```bash
git add internal/db/db.go internal/db/db_test.go
git commit -m "fix(db): reparse git worktree project attribution"
```

The commit body should explain that existing source-backed sessions require the
non-destructive full-resync to receive corrected project names. Do not include
attribution blocks or routine test transcripts.

______________________________________________________________________

### Task 3: Verify the Complete Change

**Files:**

- Verify only; no file changes expected.

**Interfaces:**

- Consumes the parser behavior and data version produced by Tasks 1 and 2.

- Produces verification evidence for handoff and pull request preparation.

- [ ] **Step 1: Run the repository's fast test suite**

Run:

```bash
make test-short
```

Expected: PASS.

- [ ] **Step 2: Confirm the branch contains only the planned commits**

Run:

```bash
git status --short --branch
git log --oneline origin/main..HEAD
```

Expected: the worktree is clean. The branch contains the design and plan commits
plus the focused parser and data-version commits, with no unrelated files.

- [ ] **Step 3: Prepare the implementation handoff**

Report:

- Live bare-backed worktrees now resolve to the owning repository.
- Removed worktrees in both hosting layouts resolve from their stable repository
  path component.
- Parser data version `75` triggers the established non-destructive full resync.
- The bare-backed sibling-recovery edge outside recognized layouts remains
  intentionally out of scope.
- Exact commands run and their outcomes.

Do not push, open a pull request, merge, or poll CI unless the user explicitly
requests those actions.
