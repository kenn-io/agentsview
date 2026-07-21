# Agent Instructions

## Scope

- These rules apply to all agent work in this repository.
- Requests to review, analyze, or explain are read-only unless the user also
  asks for changes.
- Read every focused guide whose route matches the task before editing files.
- More specific instructions override broader ones.
- Keep `CLAUDE.md` as a symlink to this file. Record new durable rules here or
  in the matching focused guide.

## Task Routes

| Task or path                                                                  | Read before editing                  |
| ----------------------------------------------------------------------------- | ------------------------------------ |
| Tests or test helpers                                                         | `docs/agents/testing.md`             |
| SQLite, PostgreSQL, CockroachDB, DuckDB, archive resync, or storage queries   | `docs/agents/storage.md`             |
| Watchers, polling, sync scheduling, background work, or memory investigations | `docs/agents/background-work.md`     |
| Build commands, toolchains, CI build tags, or dependencies                    | `docs/agents/build.md`               |
| Any frontend file                                                             | `frontend/AGENTS.md`                 |
| Frontend controls, styling, or reusable components                            | `frontend/AGENTS.md` and `DESIGN.md` |

The `README.md` and `Makefile` are the sources for project facts, setup, and
commands. Do not copy their catalogues into this file.

## Roborev

- Never run `roborev review` unless the user asks for it.
- Never invoke a roborev skill, including `roborev-fix` or
  `roborev-design-review-branch`, unless the user asks for that skill.
- Other roborev commands may be used when they fit the task.

## Git and Delivery

1. Check the branch state before editing. Do not create or switch branches
   without the user's permission. A Codex-managed detached worktree may
   receive commits; leave branch creation to the user or app.
1. Commit every turn that changes tracked files. Do not make empty commits. If a
   turn is read-only or changes only ignored files, say that no commit was
   made.
1. Keep commits focused and use clear conventional commit messages.
1. Do not amend, squash, rebase, push, or pull unless the user asks.
1. Do not add generated-with lines, attribution blocks, validation footers, or
   command transcripts to commit messages.
1. Deliver changes through pull requests from feature branches. Never merge a
   pull request; merging is the user's decision.

## Safety

- Do not revert user work or unrelated local changes unless the user asks.
- Avoid destructive git commands unless the user asks.
- Never install over a live binary, run migrations against production, or write
  to live data directories without permission. Use isolated scratch data for
  branch builds and profiling.
- For login or OAuth, give the user the exact command instead of driving the
  interactive flow.
- SQLite is the persistent archive. Never delete, drop, truncate, or recreate it
  to handle data-version changes. Read `docs/agents/storage.md` before any
  archive, database, parser-resync, or storage change.

## Content and Publishing

- Keep private project names, hostnames, personal identities, infrastructure
  details, and absolute user paths out of code, tests, fixtures,
  documentation, commit messages, and pull request text. Run the private-data
  scrub before publishing.
- Keep pull request titles and descriptions in sync with the current diff.
- Do not post pull request or issue comments unless the user asks.

## Definition of Done

- Follow every focused guide matched by the task routes above.
- Run relevant checks before committing when practical. If a check cannot run,
  state that in the handoff.
- After changing Go code, run `go fmt ./...` and `go vet ./...` before
  committing.
- Preserve observable behavior and update documentation when behavior changes.

## Provider Format Provenance

When adding a provider, changing its format or usage/cost accounting, or
investigating a provider release, new artifact generation, parser bug, or usage
discrepancy, consult `docs/internal/session-format-sources.md` and reverify or
update its evidence entry in the same change. Grok remains temporarily excluded
only until its separately owned format-alignment work lands.

## Project Map

agentsview syncs local AI agent sessions into SQLite, serves a Svelte 5 web UI,
and can mirror data to PostgreSQL or DuckDB.

- `cmd/agentsview/`: CLI and server entry points
- `internal/db/`: SQLite archive and search
- `internal/postgres/`: PostgreSQL sync and read store
- `internal/duckdb/`: disposable DuckDB mirror and Quack reads
- `internal/parser/`: agent session parsers
- `internal/server/`: HTTP API and SSE
- `internal/sync/`: discovery, file watching, and sync
- `internal/vector/`: semantic and hybrid search
- `frontend/`: Svelte 5 application

## Conventions

- Prefer the standard library over new dependencies.
- Do not use emojis in code or output.
- Format Markdown with `mdformat --wrap 80` when `mdformat` and
  `mdformat-tables` are available.

## Pull Requests

- Write summary-only pull request descriptions. Do not add test plans,
  checklists, command transcripts, or sections named Tests, Testing,
  Verification, or Test plan.
- Explain what the code does now, why it changed, tradeoffs, limits, and where
  reviewers should look.
