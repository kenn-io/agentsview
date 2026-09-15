---
title: Data
description: Project inventory and worktree mapping rules
---

The **Data** page is where you inspect and clean project classification across
the whole archive. It is the home of the worktree mapping rules that previously
lived in the Settings **Worktree mappings** section.

Open it from the **Data** tab in the header, or follow a project link from the
[Activity breakdown](/docs/activity/#breakdowns). Deep links are stable:
`/data?project_key=<key>` selects a project, and `/data?view=rules` opens the
[Rules view](#rules).

![Project inventory in the Data workspace](/docs/assets/generated/screenshots/data-inventory.png)

## Project Inventory

The default view lists every project in the archive with its session, machine,
agent, and working-directory counts plus first and last activity timestamps. A
summary strip totals the projects, sessions, and the sessions currently governed
by classification rules.

- The table is sortable by any column and filterable by project name.
- Projects targeted by enabled rules carry a rule badge; projects recorded as a
  rule's original label carry an original-label badge.
- Sessions whose stored project label is empty are grouped under a single
  "unknown" row.
- Activity bounds come from session timestamps only; rows without any recorded
  timestamps show a no-activity state.

Selecting a row opens the project workspace. Unknown `project_key` deep links
show the full inventory with a non-blocking notice.

The optional date picker limits the sessions used for project counts, folder
suggestions, and session previews. Choose a calendar month such as August, a
custom range, or **All time** to remove the filter. Dates use your browser's
timezone and include sessions whose activity overlaps the range. The governed
session total is archive-wide and is hidden while a date filter is active.
Folder rules still apply across all dates; this filter changes what you browse,
not which sessions a rule can correct.

![Observed folders for a selected project](/docs/assets/generated/screenshots/data-workspace.png)

## Create A Project Mapping

The workspace creates
[worktree project mappings](/docs/configuration/#worktree-project-mappings)
directly:

- **Observed folders** lists every session folder associated with the selected
  project. Each folder stays visible instead of being hidden behind a worktree
  selector.
- Selecting a folder opens one mapping row: **Folder path → Project**. The
  suggested folder path covers that group's working directories and remains
  editable, so it can be shortened to cover sibling folders when appropriate.
- The **Project** typeahead suggests known projects and accepts a new name. When
  the server normalizes the name (for example `sample-service` becomes
  `sample_service`), the editor shows the stored form before you apply.
- The **full archive impact** preview is live and authoritative: it counts
  matching sessions across all dates for that machine. A prefix that touches
  more than one existing project shows a warning with per-project counts —
  usually a sign the prefix is too broad. A prefix matching zero sessions
  cannot be applied.

**Save and apply mapping** saves the rule and rewrites the matching sessions in
one atomic step, then reloads the inventory. If the applied rule renamed the
selected project, the selection follows the new name. If mappings changed
between preview and apply, the apply is rejected and a fresh preview is
required.

Session previews appear below the correction controls. Their header contains
previous/next navigation and a filter button to include automated sessions,
which are hidden by default. Navigation loads another page of session records
only when needed, and loads transcript messages only for the selected session.
Sessions without stored messages remain available for mapping, with a notice
instead of a transcript. Collapse folder suggestions to give previews more room.

During a bulk correction, the Save button shows the number of completed
corrections and a progress bar. Each correction applies separately. Overlapping
previews count each matched or changing session once. If the rules also match
projects outside your selection, Save first asks you to confirm. Folder
suggestions stay grouped by project, then machine, with larger session groups
first within each machine.

Bulk saves stop if another rule edit or a change to the affected sessions makes
the reviewed impact stale. The editor refreshes the impact and asks you to
confirm again. Corrections already saved remain applied; their expected effects
do not interrupt the rest of the batch.

## Rules

The **Rules** toggle shows the worktree mapping rules for one machine at a time,
with the same add, edit, apply, and delete controls that Settings previously
offered — see
[Worktree Project Mappings](/docs/configuration/#worktree-project-mappings) for
the full rule semantics. Each rule row also shows its **governed sessions**
count (how many sessions the rule currently classifies) and the **original
label** recorded when the rule was created through the mapping editor. Rule
targets link back to the corresponding inventory row.

## Read-Only Servers

On a read-only server (`pg serve` or `duckdb serve`) the inventory, the
candidate evidence, and the Rules table remain fully readable, but the editor
and rule mutations are replaced by a notice: classification rules are managed
from the writable archive that ingests the machine's sessions.

## Storage maintenance

Choose what to retain before reclaiming disk space. Removing content and
shrinking the SQLite file are separate operations.

| What you want                                | What to use                                                                                                                                       |
| -------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| Keep images outside SQLite                   | Set `tool_result_images = "offload"` for future imports. Run `db migrate --images` for stored images. Back up `assets/` with the archive.         |
| Keep image descriptions without image access | Set `tool_result_images = "drop"` for future imports. Run `db strip --images` for stored results. Neither deletes asset files.                    |
| Omit results from selected tool categories   | Set `result_content_blocked_categories`, then run `sync --full`. This needs the source files.                                                     |
| Keep transcripts or reporting metadata only  | Set `archive_content`, restart the daemon, then run `sync --full`. The rebuild also applies the policy to copied sessions whose sources are gone. |
| Reclaim SQLite free space after cleanup      | Run `db compact`. It preserves stored rows and does not change retention settings.                                                                |

Change these settings in `config.toml`. **Settings > Archive content** also lets
you choose the image policy. Restart the daemon to apply a changed policy to new
imports. Existing rows stay as they are until you resync or run the matching
maintenance command. See [Archive content](/docs/configuration/#archive-content)
for what each policy retains and which source files are needed to recover
removed content.

### Remove archived images

Use the image cleanup control in **Settings > Tool-result images** to preview
and remove images from stored tool results. Filter by project or a cutoff date.
The date uses each session's end time, then its start time, then its creation
time when earlier fields are missing. Selected sessions can include parents,
trashed sessions, and sessions whose sources are gone. Provider transcripts and
standalone image files stay unchanged.

![Stored tool-result image cleanup preview](/docs/assets/generated/screenshots/settings-image-cleanup.png)

The command-line equivalent is
[`agentsview db strip --images`](/docs/commands/#agentsview-db-strip-images).
Preview with `--dry-run`. Stop the daemon before applying changes through the
CLI, then start it again afterward. The Settings control runs through the daemon
and does not need this stop/start sequence.

Cleanup replaces supported inline images and offloaded image references with
readable text descriptions. These keep the media type and decoded byte size. New
placeholders use version `1` and an empty `sha256` unless a hash was already
present. Removing an offloaded reference clears `image_ref` and preserves its
hash, but does not delete the asset file.

The daemon exposes preview and apply as `POST /api/v1/data/strip-images/preview`
and `POST /api/v1/data/strip-images`. Both routes require localhost and accept
the same project and date selection. Apply returns HTTP 409 if another archive
maintenance operation holds the write barrier. It does not queue behind that
operation.

### Ingest-time image offload

Set `tool_result_images = "offload"` to keep supported tool-result images in
`{dataDir}/assets` instead of embedding their bytes in SQLite. Restart the
daemon after changing the setting. Supported formats are PNG, JPEG, WebP, and
GIF. Unsupported media and malformed data stay inline. Archives that omit tool
content write no image assets.

To move images already in the archive, use
[`agentsview db migrate --images`](/docs/commands/#agentsview-db-migrate-images).
Preview first, stop the daemon, apply the migration, and start the daemon again.
This preserves image access; `db strip --images` removes that access.

AgentsView names each asset `<sha256hex><ext>` from its content hash and writes
it before saving the reference in SQLite. An existing asset is reused only if
its byte count and hash match. A missing or corrupt asset can be replaced while
the inline source bytes remain available. Stored `agentsview_image` blocks use
`image_ref` for the `asset://` reference.

If an asset write fails during ingestion or copied-session resync, AgentsView
keeps the original inline content. Restore access to the asset directory, then
retry with `db migrate --images`. A failed database transaction can leave
complete unreferenced assets on disk; a retry reuses matching files. There is no
automatic cleanup of unreferenced assets. Both CLI image commands report
sessions committed before a later failure.

Back up `assets/` with the database. A separate local serving host needs both.
PostgreSQL and CockroachDB retain the references but cannot resolve local image
assets. DuckDB and normalized Markdown session exports carry the stored content.
Artifact exports carry references without asset files; artifact imports replace
those references with readable descriptions. HTML export keeps its own
[export behavior](/docs/usage/#session-export). The raw
`agentsview session export` command streams provider source bytes, including
original inline payloads.

### Reclaim free space

Image cleanup reports stored-content and decoded-image byte counts. These are
content measurements, not reclaimed disk space. Run
[`agentsview db compact`](/docs/commands/#agentsview-db-compact) afterward to
reclaim free SQLite pages and truncate the write-ahead log (WAL).

Compaction reports database, WAL, SHM, free-page, staging, and reclaimed sizes
separately. Do not add its reported savings to the content byte counts. A
compaction requested while sync, resync, or another compaction holds the archive
maintenance barrier fails with a conflict instead of queuing. See the command
reference for staging requirements and interrupted-compaction recovery.
