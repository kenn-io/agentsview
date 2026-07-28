# Git Worktree Project Attribution Design

## Goal

Attribute sessions created inside standard Git worktrees to the owning
repository instead of the worktree directory or branch name. The result must
remain correct after a short-lived worktree has been removed.

## Problem

Project extraction currently handles linked worktrees whose common Git directory
belongs to a normal checkout. Two standard Git configurations still fragment
activity reports:

1. A linked worktree backed by a bare common repository resolves to the worktree
   checkout root. Its leaf directory therefore becomes the project.
1. Once a worktree has been removed, its `.git` file is unavailable. If no
   surviving sibling provides unambiguous Git metadata, extraction falls back
   to the removed worktree's leaf directory.

The affected worktree roots use hosting-oriented directory structures that
retain the repository component:

```text
.../worktrees/github.com/$OWNER/$REPOSITORY/$WORKTREE[/...]
.../worktrees/github/github.com/$OWNER/$REPOSITORY/$WORKTREE[/...]
```

The second form includes an additional provider namespace used by a
bare-clone-backed worktree manager.

## Considered Approaches

### Path layouts only

Recognize the observed hosting-oriented directory structures and extract the
repository component. This is small and repairs removed worktrees, but an
arbitrary live worktree backed by a bare repository would remain incorrectly
attributed.

### Git metadata only

Teach the Git-file resolver to derive the repository name from a bare common Git
directory. This handles live worktrees without depending on a manager layout,
but cannot help after the worktree and its `.git` file are gone.

### Combined Git and path resolution

Fix bare-repository resolution for live standard worktrees and add conservative
hosting-layout fallbacks for removed worktrees. This addresses both failure
modes while preserving the existing preference for direct filesystem evidence.

This is the selected approach.

## Design

### Live worktrees

`internal/parser/project.go` will continue walking upward to a `.git` file and
reading its `gitdir` and `commondir` values.

When the common directory is a normal checkout's `.git` directory, behavior will
remain unchanged. When its Git config identifies it as a bare repository, the
resolver will use the common directory's basename as the repository identity and
remove a conventional `.git` suffix. This remains a filesystem-only fast path
and avoids adding a Git subprocess to routine sync.

The bare check prevents a non-bare separate Git directory or submodule from
being mistaken for the owning repository.

### Removed worktrees

Managed-layout resolution runs before the Git-root walk because it is the only
available evidence after removal. Two hosting-oriented layouts will be
recognized:

```text
.../worktrees/github.com/$OWNER/$REPOSITORY/$WORKTREE[/...]
.../worktrees/github/github.com/$OWNER/$REPOSITORY/$WORKTREE[/...]
```

Both require owner, repository, and worktree components. The extracted
repository name will pass through the existing project-name normalization. Paths
without all required components will not match and will retain existing fallback
behavior.

### Existing archive data

The parser data version will increase by one. On upgrade, agentsview's existing
non-destructive full-resync flow will rebuild session metadata from transcript
sources and preserve orphaned sessions according to the established archive
rules.

No database schema, activity query, PostgreSQL query, or DuckDB query changes
are required. Corrected project names enter all storage backends through the
existing normalized session data.

Sessions whose source transcript and worktree evidence are both permanently
unavailable cannot be reconstructed without guessing and will remain unchanged.

## Testing

Parser regression coverage will include:

- A real temporary bare repository with a standard linked worktree in an
  arbitrary directory, proving live attribution comes from Git metadata rather
  than a recognized path layout.
- Removed hosting-layout worktree paths for both supported structures.
- Nested working directories beneath those worktrees.
- Incomplete hosting-layout paths that must not match.
- Existing normal checkout, linked-worktree, submodule, and deleted-sibling
  cases through the current parser test suite.

After implementation, run the focused parser tests, the complete parser package,
`go fmt ./...`, and `go vet ./...` before committing.

## Scope

This change affects project-name extraction and the parser data version only. It
does not add worktree-manager metadata, modify user-configured worktree
mappings, change activity aggregation, or infer repository ownership from a
branch name.
