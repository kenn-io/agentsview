# Docs Repository Integration Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the AgentsView docs site into this repository with `docs/` as the
Vercel root and with screenshots/icons/diagrams stored on orphan asset branches,
not on `main`.

**Architecture:** Import the Zensical docs source into a self-contained `docs/`
project, mirroring `roborev`'s Vercel wrapper and root Makefile targets.
Hydrated assets live only in ignored `docs/assets/static/` and
`docs/assets/generated/`; local branch update scripts publish curated and
generated media to `docs-assets` and `docs-generated-assets` without switching
the current branch.

**Tech Stack:** Go tests with `testify`, Bash docs tooling, Git orphan refs,
Zensical/uv, Vercel CLI, existing Playwright screenshot tooling.

______________________________________________________________________

### File Structure

- Modify: `.gitignore` Ignore docs build output, hydrated asset directories,
  docs-local Vercel files, and docs-local env files.
- Modify: `Makefile` Add `docs-*` targets matching `roborev`.
- Modify: `README.md` Update docs image URLs that move under
  `/assets/generated/`.
- Create: `scripts/check-docs.sh` Root docs validation entrypoint.
- Create: `scripts/update-docs.sh` Manual docs deployment helper.
- Create: `scripts/docs_assets_test.go` Unit tests for asset hydration and
  publisher safety.
- Replace/create: `docs/` Public Zensical source, docs-local toolchain,
  maintainer guide, wrappers, screenshot tooling, and preserved internal
  reference pages.
- Create local refs: `docs-assets`, `docs-generated-assets` Single-commit asset
  branches seeded from `~/code/agentsview-docs` media files.

### Task 1: Add Docs Guardrail Tests

**Files:**

- Create: `scripts/docs_assets_test.go`

- Later implementation targets:

  - `docs/assets/hydrate-assets.sh`
  - `docs/assets/update-static-assets-branch.sh`
  - `docs/screenshots/update-generated-assets-branch.sh`

- [ ] **Step 1: Write failing asset script tests**

  Add Go tests using `github.com/stretchr/testify`. Cover:

  - `hydrate-assets.sh` refreshes stale local hydrated assets from a
    force-updated remote `docs-assets` ref and hydrates `docs-generated-assets`.
  - Static and generated asset branch publishers reject unexpected files such as
    `.env.local`.

  Test helpers should create temp git repos with `t.TempDir()`, configure local
  git identity, and write a minimal expected AgentsView asset set:

  ```go
  static := []string{
      "agents/claude-code.svg",
      "agents/codex.svg",
      "agents/gemini.svg",
      "agents/forge.png",
      "architecture.svg",
      "og-image.png",
  }
  generated := []string{
      "screenshots/dashboard.png",
      "screenshots/session-list.png",
      "screenshots/usage-page.png",
  }
  ```

- [ ] **Step 2: Run tests to verify RED**

  Run:

  ```bash
  go test -tags "fts5" ./scripts -run 'TestHydrateAssets|TestAssetPublishers' -count=1
  ```

  Expected: FAIL because the docs asset scripts do not exist yet.

### Task 2: Import Public Docs Source Without Media

**Files:**

- Delete or replace existing tracked files under `docs/`.

- Create from `~/code/agentsview-docs`: Markdown docs, `overrides/`,
  `stylesheets/extra.css`, `javascripts/lightbox.js`, `pyproject.toml`,
  `uv.lock`, `zensical.toml`, `vercel.json`, screenshot tooling, and check
  scripts.

- Preserve:

  - `docs/internal/desktop-release-setup.md`
  - `docs/internal/huma-api-routes.md`
  - `docs/internal/visual-studio-copilot-traces.md`

- Drop:

  - `docs/screenshots/phase-6-*.png`
  - `docs/duckdb-backend-plan.md`
  - `docs/session-quality-heuristic-map.md`
  - `docs/session-quality-validation-rollout.md`

- [ ] **Step 1: Remove old docs tree contents**

  Remove stale in-repo docs files that would conflict with the imported site.
  Keep the committed `docs/superpowers/` spec and plan files out of published
  docs by excluding them in `zensical-docs.sh`.

- [ ] **Step 2: Copy docs source files**

  Copy public docs files from `~/code/agentsview-docs`, excluding media files:

  - Exclude `docs/screenshots/`
  - Exclude `docs/agents/`
  - Exclude `docs/architecture.svg`
  - Exclude `docs/og-image.png`
  - Exclude local-only outputs such as `.venv`, `.cache`, `site`, `dist`,
    `.vercel`, and `node_modules`

- [ ] **Step 3: Move JavaScript asset**

  Move `docs/lightbox.js` to `docs/javascripts/lightbox.js` and update
  `extra_javascript` in `docs/zensical.toml` to:

  ```toml
  extra_javascript = ["javascripts/lightbox.js"]
  ```

- [ ] **Step 4: Add internal reference pages**

  Move durable current repo technical notes into `docs/internal/`, which is
  excluded from `zensical-docs.sh`:

  - `Desktop Release Setup`
  - `Huma API Routes`
  - `Visual Studio Copilot Trace Format`

  Do not keep stale planning documents whose product behavior is already covered
  by the current public docs.

- [ ] **Step 5: Rewrite media references**

  Rewrite all docs source references:

  - `/screenshots/<name>.png` -> `/assets/generated/screenshots/<name>.png`
  - `/agents/<name>` -> `/assets/static/agents/<name>`
  - `/architecture.svg` -> `/assets/static/architecture.svg`
  - `/og-image.png` and `https://agentsview.io/og-image.png` ->
    `/assets/static/og-image.png` or
    `https://agentsview.io/assets/static/og-image.png` where an absolute URL is
    required

  Apply this in Markdown, CSS, JS, HTML overrides, check scripts, and root
  `README.md`.

### Task 3: Add Docs Build And Asset Tooling

**Files:**

- Create: `docs/README.md`

- Create: `docs/vercel-build.sh`

- Create: `docs/zensical-docs.sh`

- Create: `docs/assets/hydrate-assets.sh`

- Create: `docs/assets/update-static-assets-branch.sh`

- Create: `docs/screenshots/update-generated-assets-branch.sh`

- Create: `scripts/check-docs.sh`

- Create: `scripts/update-docs.sh`

- Modify: `.gitignore`

- Modify: `Makefile`

- [ ] **Step 1: Add `zensical-docs.sh`**

  Adapt the `roborev` wrapper so it builds a temporary public-docs directory and
  excludes maintainer-only files:

  - `README.md`
  - `assets/*.sh`
  - `screenshots/`
  - `scripts/`
  - `superpowers/`
  - `pyproject.toml`, `uv.lock`, `vercel.json`, `vercel-build.sh`,
    `zensical-docs.sh`, `zensical.toml`

- [ ] **Step 2: Add Vercel build entrypoint**

  Update `docs/vercel.json` so Vercel uses the hydration-aware build command:

  ```json
  {
    "$schema": "https://openapi.vercel.sh/vercel.json",
    "framework": null,
    "installCommand": "uv sync --frozen --no-dev",
    "buildCommand": "uv run --frozen bash ./vercel-build.sh",
    "outputDirectory": "site",
    "trailingSlash": true
  }
  ```

  Add `docs/vercel-build.sh`:

  ```bash
  #!/usr/bin/env bash
  set -euo pipefail

  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  "$script_dir/assets/hydrate-assets.sh"
  "$script_dir/zensical-docs.sh" build
  ```

- [ ] **Step 3: Add asset hydration and publisher scripts**

  Adapt the `roborev` scripts with `AGENTSVIEW_` environment variable names:

  - `AGENTSVIEW_DOCS_ASSETS_BRANCH`
  - `AGENTSVIEW_DOCS_GENERATED_ASSETS_BRANCH`
  - `AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES`
  - `AGENTSVIEW_DOCS_STATIC_ASSETS_DIR`

  Publishers must:

  - reject symlinks
  - reject dotfiles and unexpected files
  - copy only expected assets from the full current `agentsview-docs` media set,
    not only the reduced test fixture list
  - update local branch refs from a temporary git repository without checking
    out or switching branches

- [ ] **Step 4: Retarget screenshot tooling**

  Update imported screenshot tooling so generated screenshots are written to
  `docs/assets/generated/screenshots/`, not to tracked source paths:

  - `docs/screenshots/run.sh` should set
    `OUTPUT_DIR="$ROOT/assets/generated/screenshots"`.
  - Playwright `SCREENSHOT_DIR` defaults should point at
    `assets/generated/screenshots` when run through the docs wrapper.
  - `docs/screenshots/update-generated-assets-branch.sh` should publish from
    `docs/assets/generated` by default and expect paths like
    `screenshots/dashboard.png`.

- [ ] **Step 5: Add root targets**

  Add the following `Makefile` phony targets:

  ```make
  docs-install:
  	cd docs && uv sync --frozen --no-dev

  docs-build:
  	cd docs && uv run --frozen bash ./vercel-build.sh

  docs-serve:
  	cd docs && bash assets/hydrate-assets.sh && uv run bash ./zensical-docs.sh serve

  docs-check:
  	bash scripts/check-docs.sh
  ```

  Also add `docs-screenshots`, `docs-assets-branch`,
  `docs-generated-assets-branch`, `docs-deploy-staging`, and `docs-deploy`
  matching `roborev`.

- [ ] **Step 6: Add docs checker**

  `scripts/check-docs.sh` should fail if:

  - repository root has `zensical.toml` or `vercel.json`
  - any tracked docs image media exists
  - hydrated asset directories contain tracked files
  - docs media references bypass `/assets/static` or `/assets/generated` in any
    Markdown, HTML override, CSS, JavaScript, script, root README, or metadata
    validation file

  Then hydrate assets and run:

  ```bash
  cd docs
  uv run --frozen bash ./zensical-docs.sh build
  uv run --frozen python scripts/check_built_site.py
  uv run --frozen python scripts/check_vercel_redirects.py
  ```

- [ ] **Step 7: Add docs maintainer guide**

  `docs/README.md` should document:

  - layout
  - `docs-assets` and `docs-generated-assets`
  - local build/check commands
  - static asset and screenshot branch update commands
  - Vercel root directory settings
  - no tracked media on `main`

### Task 4: Seed Local Orphan Asset Branches

**Files/refs:**

- Local branch ref: `docs-assets`

- Local branch ref: `docs-generated-assets`

- Ignored local directories:

  - `docs/assets/static/`
  - `docs/assets/generated/`

- [ ] **Step 1: Stage static media into ignored assets**

  Copy from `~/code/agentsview-docs/docs/` into `docs/assets/static/`:

  - `agents/*`
  - `architecture.svg`
  - `og-image.png`

- [ ] **Step 2: Publish local static asset branch**

  Run:

  ```bash
  bash docs/assets/update-static-assets-branch.sh
  ```

  Expected: updates local `docs-assets` branch ref without changing the current
  branch.

- [ ] **Step 3: Stage generated screenshots into ignored assets**

  Copy from `~/code/agentsview-docs/docs/screenshots/` into
  `docs/assets/generated/screenshots/`.

- [ ] **Step 4: Publish local generated asset branch**

  Run:

  ```bash
  bash docs/screenshots/update-generated-assets-branch.sh --source docs/assets/generated
  ```

  Expected: updates local `docs-generated-assets` branch ref without changing
  the current branch.

- [ ] **Step 5: Hydrate from local branches for validation**

  Run:

  ```bash
  AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 bash docs/assets/hydrate-assets.sh
  ```

  Expected: hydrated asset files appear under ignored asset directories.

### Task 5: Verify And Commit Migration

**Files:**

- All migration files from Tasks 1-4.

- [ ] **Step 1: Run focused tests**

  Run:

  ```bash
  go test -tags "fts5" ./scripts -run 'TestHydrateAssets|TestAssetPublishers' -count=1
  ```

  Expected: PASS.

- [ ] **Step 2: Run docs check**

  Run:

  ```bash
  AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 make docs-check
  ```

  Expected: PASS.

- [ ] **Step 3: Check no media is tracked**

  Run:

  ```bash
  git ls-files docs | rg '\\.(png|svg|jpg|jpeg|webp|gif)$'
  ```

  Expected: no output.

- [ ] **Step 4: Inspect working tree**

  Run:

  ```bash
  git status --short
  git diff --stat
  git diff --check
  ```

  Expected: only intended docs/tooling changes, and no whitespace errors.

- [ ] **Step 5: Commit**

  Commit the migration with a focused conventional message. Do not include
  generated-with lines, attribution blocks, validation footers, or command
  transcripts in the commit message.

### Task 6: Final Review

**Files:**

- Entire branch diff since `origin/main`.

- [ ] **Step 1: Request implementation review**

  Ask reviewer subagents to check:

  - spec compliance
  - asset publication safety
  - Vercel/root-directory parity with `roborev`
  - stale existing-docs handling

- [ ] **Step 2: Fix review findings**

  Address blocking findings and rerun the relevant verification command for each
  fix.

- [ ] **Step 3: Final status**

  Report:

  - commits created
  - validation commands and outcomes
  - local asset branch refs created
  - reminder that remote asset refs must be pushed before Vercel can build from
    a fresh checkout
