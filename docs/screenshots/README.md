# Refresh release screenshots

Regenerate the full screenshot set for every release. Capture new visible
features, inspect the images, and preview the assembled website before opening
the release documentation pull request. Generated images live on the
`docs-generated-assets` orphan branch; capture scripts and documentation live on
the release branch.

## What do I need?

- Docker and SQLite's `sqlite3` command.
- AgentsView source in the current checkout (override with `AGENTSVIEW_SRC`).
- A sessions database at `~/.agentsview/sessions.db` (override with
  `SOURCE_DB`).

On macOS, if the bundled SQLite reports `unable to open database file` for a
closed WAL-mode archive, use a current SQLite CLI, such as Homebrew's `sqlite3`.
Put its `bin` directory first on `PATH`. Do not open the source archive for
writing to work around a read-only capture error.

The extractor first copies the whole archive. Allow free disk space for that
copy, the filtered database, and the Docker build. For a large archive, prepare
the corpus on a machine with enough space and use that smaller database as
`SOURCE_DB` on capture machines.

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
   Filter and redact that disposable copy before passing it to Docker. Home
   paths are redacted in both normal paths and encoded Claude project folders.
1. Build the current frontend and Go binary, then assemble a runner image with
   Chromium, Playwright, PostgreSQL, and the filtered database.
1. Start isolated SQLite and PostgreSQL servers inside the container. The
   PostgreSQL fixture shows sessions from two example machines.
1. Capture the UI and write PNG files to `docs/assets/generated/screenshots/`.

The capture uses a 1440×900 viewport at scale 1, dark mode, English, and the
`America/Chicago` timezone. One worker runs the captures in order. The browser's
date is fixed to noon UTC on the day after the corpus's newest session, so
relative dates and default report windows stay stable when the corpus is reused.
Transcript timestamps remain unchanged. Its frontend build enables
`VITE_PROJECT_MAPPING_WORKSPACE=true` to show the opt-in project mapping
workspace. Some captures use fixed response fixtures to illustrate states such
as image-cleanup totals, token usage, Recall review, and Git/pull-request
totals. The Outcomes capture intercepts GitHub requests; it does not call GitHub
or require credentials. Captures hide session IDs because imported IDs can
contain original machine names. See `playwright.config.ts` and
`tests/screenshots.spec.ts` for the exact setup.

## How can colleagues repeat a capture?

Use the same release source and a reviewed copy of the same corpus. `SOURCE_DB`
already accepts a curated archive; the extractor reapplies the current filters
to a disposable copy. It does not need access to the original contributor's
archive, home directory, or running daemon.

```bash
SOURCE_DB=/path/to/curated/sessions.db make docs-screenshots
```

Run from the release documentation checkout. If its application source has moved
beyond the release tag, set `AGENTSVIEW_SRC` to a separate source directory at
that tag. The capture scripts come from the documentation checkout; the
application comes from `AGENTSVIEW_SRC`.

Keep the corpus in restricted artifact storage, outside the source and public
asset branches. Filtering makes captures easier to review; it does not certify
every archive table for redistribution. Review the database separately before
sharing it with colleagues, including export records, stored tool inputs, paths,
identities, and any attachments.

For each reviewed corpus, retain:

- A version and SHA-256 checksum of the database.
- The source release and extraction settings, including the history window and
  the versions of the private term lists. Keep those lists private.
- Coverage notes for transcripts with reasoning, tool results, subagents, errors
  and recovery, multiple providers, and populated usage reports.
- A list of capture cases that use response fixtures instead of corpus data.

For each capture run, retain the application commit, capture-script commit,
corpus checksum, container image ID, browser version, capture log, and the final
images. These identify the inputs without publishing transcript identifiers or
private source paths. Locked JavaScript dependencies and fixed rendering
settings reduce drift; Docker base images and operating-system packages still
need pinning before claiming byte-identical rebuilds.

## What remains for release automation?

The next step is a shared capture runner with a versioned corpus of real,
reviewed sessions. Curation should be a separate operation: refresh the corpus
when it lacks examples for a new feature, then reuse it across releases. Keep
the original prompts, tool results, timing, and usage where they can be shared.
Avoid rewriting every conversation into a demonstration script.

Release automation can invoke the existing pipeline with the release source and
corpus version, then run the asset updater and docs checks. A complete job
should produce a screenshot set, a capture receipt with the inputs above, and an
assembled website preview for review. It must fail on missing captures and
retain failure logs. Image inspection and private-data review remain required
before publishing the asset branch or opening the release documentation PR.

The shared runner, corpus distribution, pinned container build, and release
automation integration are follow-up work. The commands here run locally today;
no release event currently schedules this capture workflow.
