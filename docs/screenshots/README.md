# Screenshot Pipeline

Generates screenshots for the docs site using Playwright inside Docker.

## Prerequisites

- Docker
- agentsview source at `~/code/agentsview` (override with `AGENTSVIEW_SRC`)
- A sessions database at `~/.agentsview/sessions.db` (override with `SOURCE_DB`)
- The screenshot extractor keeps approved public projects from the newest 60-day
  session window (override with `SCREENSHOT_HISTORY_DAYS`)
- Optional local-only screenshot scrub terms via a line-separated
  `~/.config/agentsview-docs/screenshot-blocked-terms.txt`, override the path
  with `SCREENSHOT_BLOCKED_TERMS_FILE`, or append terms with
  `SCREENSHOT_BLOCKED_TERMS`

## Run all screenshots

```bash
./screenshots/run.sh
```

Output goes to `docs/assets/generated/screenshots/`.

## Run a single screenshot

Pass `--grep` with the test name:

```bash
./screenshots/run.sh --grep "session filters active"
```

Any extra arguments are forwarded to `npx playwright test` inside the container.

## How it works

1. Assembles a Docker build context with the agentsview source, sessions
   database, and test files
1. Builds a multi-stage Docker image:
   - **Stage 1 (builder):** Compiles the agentsview Go binary with frontend
     assets
   - **Stage 2 (db):** Extracts a reduced database with screenshot-safe projects
   - **Stage 3 (playwright):** Installs Playwright + Chromium + PostgreSQL,
     copies binary and DB
1. Runs the container, which:
   - Starts PostgreSQL and pushes test data from two simulated machines
   - Starts agentsview on port 8090 (SQLite mode, for most screenshots)
   - Starts agentsview pg serve on port 8091 (PG mode, for machine label
     screenshots)
   - Executes the Playwright tests against both servers
1. Screenshots are written to the mounted `/output` volume
   (`docs/assets/generated/screenshots/`)

Stages 1 and 2 are cached by Docker, so re-runs after test file changes are
fast.

## Configuration

Viewport: 1440x900, dark mode, `America/Chicago` timezone. See
`playwright.config.ts`.

## Test file

All tests live in `tests/screenshots.spec.ts`. Each test navigates to a UI state
and calls `snap(page, 'name')` or `snapEl(locator, 'name')` to capture a PNG.
