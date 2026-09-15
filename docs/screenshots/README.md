# Refresh release screenshots

Regenerate the full screenshot set for every release. Capture new visible
features, inspect the images, and preview the assembled website before opening
the release documentation pull request. Generated images live on the
`docs-generated-assets` orphan branch; capture scripts and documentation live on
the release branch.

## What do I need?

- Docker and SQLite's `sqlite3` command.
- AgentsView source at `~/code/agentsview` (override with `AGENTSVIEW_SRC`).
- A sessions database at `~/.agentsview/sessions.db` (override with
  `SOURCE_DB`).

The extractor keeps local transcripts from approved public projects in the
newest 60-day session window. Override the window with
`SCREENSHOT_HISTORY_DAYS`. It replaces the current home directory with `~` and
excludes sessions containing private terms or original remote-machine names. The
runner uses example machine names for remote screenshots. It reads
`~/.config/agentsview-docs/screenshot-blocked-terms.txt` and
`~/.config/kenn/private-terms.txt` by default. Override these paths with
`SCREENSHOT_BLOCKED_TERMS_FILE` and `KENN_PRIVATE_TERMS_FILE`, or add terms with
`SCREENSHOT_BLOCKED_TERMS`.

## How do I prepare a release?

Run these commands from the repository root:

1. Add captures for new visible features in `tests/screenshots.spec.ts`. Add
   their filenames to `update-generated-assets-branch.sh` and
   `../assets/hydrate-assets.sh`, then link each image from its owning guide.

1. Regenerate every screenshot and update the local orphan asset branch:

    ```bash
    make docs-generated-assets-branch
    ```

1. Inspect the images under `docs/assets/generated/screenshots/`. Check that
   each image shows populated content, readable controls, and no private data.

1. Build and check the website using the new local assets:

    ```bash
    AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES=1 make docs-check
    make docs-preview
    ```

    Open the URL printed by the preview command. Review the homepage and the
    affected guides before opening the documentation pull request.

The branch update stays local unless you request `--push`. See the
[docs maintainer guide](../README.md#updating-generated-screenshots) for
publishing commands.

## How do I retry a capture?

Pass a test name to run only that capture:

```bash
bash docs/screenshots/run.sh --grep "session filters active"
```

Other arguments pass through to Playwright. A targeted run helps with iteration;
every release still needs a fresh full set. To store a completed, inspected set
without regenerating it again:

```bash
bash docs/screenshots/update-generated-assets-branch.sh --skip-generate
```

## What does the pipeline run?

1. Copy the source into a temporary build directory.
1. Open the source database read-only and take a consistent SQLite snapshot.
   Filter and redact that disposable copy before passing it to Docker.
1. Build the current frontend and Go binary, then assemble a runner image with
   Chromium, Playwright, PostgreSQL, and the filtered database.
1. Start isolated SQLite and PostgreSQL servers inside the container. The
   PostgreSQL fixture shows sessions from two example machines.
1. Capture the UI and write PNG files to `docs/assets/generated/screenshots/`.

The capture uses a 1440×900 viewport, dark mode, and the `America/Chicago`
timezone. Some captures use fixed response fixtures to illustrate states such as
image-cleanup totals and token usage. Captures hide session IDs because imported
IDs can contain original machine names. See `playwright.config.ts` and
`tests/screenshots.spec.ts` for the exact setup.
