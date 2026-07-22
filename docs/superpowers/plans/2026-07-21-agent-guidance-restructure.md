# Agent Guidance Restructure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the large root `AGENTS.md` with short repository-wide rules
and explicit routes to focused guidance.

**Architecture:** The root file will hold rules that apply to most work and a
path-based routing table. Focused files will hold testing, storage, background
work, build, frontend, and localization rules. Existing project documentation
will remain the source for facts and command catalogues.

**Tech Stack:** Markdown, Codex `AGENTS.md` discovery, git hooks

## Global Constraints

- Preserve all current safety and delivery rules.
- Keep `CLAUDE.md` as a symlink to `AGENTS.md`.
- Do not change code or runtime behavior.
- Do not add tests or run product test suites for this prose-only change.
- Use plain English and keep technical terms that carry meaning.
- Commit each task separately without amending earlier commits.

______________________________________________________________________

### Task 1: Route testing and frontend work

**Files:**

- Create: `docs/agents/testing.md`
- Modify: `frontend/AGENTS.md`

**Interfaces:**

- Consumes: current root testing and localization rules

- Produces: focused guides that the root routing table will name

- [ ] **Step 1: Add the testing guide**

    Move the current test requirements into `docs/agents/testing.md`. Keep the
    rules for required tests, table-driven Go tests, testify, `testDB(t)`, temp
    directories, colocated frontend tests, Playwright, and observable shell
    tests. Remove repeated wording.

- [ ] **Step 2: Extend the frontend guide**

    Add the current localization catalogue and validation rules to
    `frontend/AGENTS.md`. Keep its Vite+ and UI guidance intact.

- [ ] **Step 3: Format and inspect the files**

    Run:

    ```bash
    mdformat --wrap 80 docs/agents/testing.md frontend/AGENTS.md
    git diff --check
    ```

    Expected: both commands exit zero and the diff contains only the focused
    guidance.

- [ ] **Step 4: Commit**

    ```bash
    git add docs/agents/testing.md frontend/AGENTS.md
    git commit -m "docs: focus testing and frontend agent rules"
    ```

### Task 2: Extract storage rules

**Files:**

- Create: `docs/agents/storage.md`

**Interfaces:**

- Consumes: current SQLite, backend parity, DuckDB, and PostgreSQL test rules

- Produces: one storage guide named by the root routing table

- [ ] **Step 1: Add the storage guide**

    Group the rules under SQLite archive safety, backend parity, DuckDB mirror,
    and PostgreSQL integration tests. Preserve the rule that SQLite is the
    system of record, DuckDB is disposable, schema changes rebuild DuckDB, and
    PostgreSQL tests must use a dedicated database.

- [ ] **Step 2: Format and inspect the file**

    Run:

    ```bash
    mdformat --wrap 80 docs/agents/storage.md
    git diff --check
    ```

    Expected: both commands exit zero.

- [ ] **Step 3: Commit**

    ```bash
    git add docs/agents/storage.md
    git commit -m "docs: isolate storage agent rules"
    ```

### Task 3: Extract background and build rules

**Files:**

- Create: `docs/agents/background-work.md`
- Create: `docs/agents/build.md`

**Interfaces:**

- Consumes: current memory, profiling, build, and dependency notes

- Produces: focused guides named by the root routing table

- [ ] **Step 1: Add the background-work guide**

    Preserve the bounded-work, capability, cardinality, profiling, isolated-data,
    and retention-observation rules. Use direct sentences and keep the
    diagnostic terms exact.

- [ ] **Step 2: Add the build guide**

    Keep only build constraints that change agent behavior: CGO and FTS5, test
    telemetry behavior, frontend prerequisites, the pinned kit-ui dependency,
    and the lockfile transport note. Point readers to the `Makefile` for
    commands.

- [ ] **Step 3: Format and inspect the files**

    Run:

    ```bash
    mdformat --wrap 80 docs/agents/background-work.md docs/agents/build.md
    git diff --check
    ```

    Expected: both commands exit zero.

- [ ] **Step 4: Commit**

    ```bash
    git add docs/agents/background-work.md docs/agents/build.md
    git commit -m "docs: isolate operational agent rules"
    ```

### Task 4: Slim the root instructions

**Files:**

- Modify: `AGENTS.md`

**Interfaces:**

- Consumes: all focused guides from Tasks 1 through 3

- Produces: repository-wide rules and path-based routes

- [ ] **Step 1: Replace copied detail with routes**

    Keep scope, Roborev restrictions, git and delivery rules, content hygiene,
    core validation, safety, pull request rules, broad conventions, and a short
    project map. Add a table that tells agents when they must read each focused
    guide, `frontend/AGENTS.md`, and `DESIGN.md`.

- [ ] **Step 2: Resolve the branch-state ambiguity**

    State that agents must not create or switch branches without permission. A
    managed detached worktree may receive commits; branch creation remains a
    user or app action.

- [ ] **Step 3: Remove copied reference material**

    Remove the architecture diagram, key-file catalogue, command catalogue,
    duplicate test section, subsystem detail, and dependency troubleshooting.
    Point to `README.md` and the `Makefile` instead.

- [ ] **Step 4: Format and inspect the file**

    Run:

    ```bash
    mdformat --wrap 80 AGENTS.md
    git diff --check
    awk '/^## / { count[$0]++ } END { for (h in count) if (count[h] > 1) exit 1 }' AGENTS.md
    ```

    Expected: all commands exit zero and `AGENTS.md` is below 1,000 words.

- [ ] **Step 5: Commit**

    ```bash
    git add AGENTS.md
    git commit -m "docs: route agent guidance by task"
    ```

### Task 5: Align DuckDB documentation

**Files:**

- Modify: `internal/duckdb/README.md`

**Interfaces:**

- Consumes: the current DuckDB rebuild contract in code and
  `docs/agents/storage.md`

- Produces: package documentation that no longer claims in-place migration

- [ ] **Step 1: Replace the stale schema sentence**

    State that schema version changes rebuild a fresh mirror and atomically swap
    it into place. Do not describe `EnsureSchema` as a production migration
    path.

- [ ] **Step 2: Format and inspect the file**

    Run:

    ```bash
    mdformat --wrap 80 internal/duckdb/README.md
    git diff --check
    ```

    Expected: both commands exit zero and no other DuckDB claims change.

- [ ] **Step 3: Commit**

    ```bash
    git add internal/duckdb/README.md
    git commit -m "docs: correct DuckDB schema lifecycle"
    ```

### Task 6: Verify the instruction set

**Files:**

- Verify: `AGENTS.md`
- Verify: `CLAUDE.md`
- Verify: `docs/agents/*.md`
- Verify: `frontend/AGENTS.md`
- Verify: `internal/duckdb/README.md`

**Interfaces:**

- Consumes: the complete refactor
- Produces: evidence that all routes and rules remain valid

This task uses document checks only. It does not add tests or run product test
suites.

- [ ] **Step 1: Check structure and links**

    Run:

    ```bash
    test -L CLAUDE.md
    test "$(readlink CLAUDE.md)" = "AGENTS.md"
    for file in docs/agents/testing.md docs/agents/storage.md \
      docs/agents/background-work.md docs/agents/build.md \
      frontend/AGENTS.md DESIGN.md README.md Makefile; do test -e "$file"; done
    ```

    Expected: every command exits zero.

- [ ] **Step 2: Check prose and formatting**

    Run:

    ```bash
    mdformat --check AGENTS.md docs/agents/*.md frontend/AGENTS.md \
      internal/duckdb/README.md
    git diff --check
    wc -l -w -c AGENTS.md
    ```

    Expected: formatting and diff checks exit zero; the root file is below 1,000
    words and smaller than its original 17,401 bytes.

- [ ] **Step 3: Review the commit series**

    Run:

    ```bash
    git status --short
    git log --oneline -7
    git diff HEAD~5..HEAD --stat
    ```

    Expected: the worktree is clean and each implementation concern has its own
    commit.
