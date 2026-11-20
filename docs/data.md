---
title: Data
description: Correct project assignments with worktree rules and an opt-in project workspace
---

Use **Data** to group sessions under the right project name. Open the **Data**
tab in the header to manage [project mapping rules](#rules), which match
session folders to projects.

For offline original-file retention and moving an archive to another machine,
see [`agentsview archive`](/docs/commands/#agentsview-archive), available in
0.46.0 and later. Seed imports use a separate, permanently archive-only data
directory. Keep new local sessions in your normal data directory.

## Enable the project workspace

Version 0.44.0 adds a project workspace with folder suggestions, transcript
previews, and bulk corrections. It is **off by default**. Standard builds show
the rules editor without the inventory or workspace pictured below.

To enable it, [build from source](/docs/quickstart/#build-from-source) with:

```bash
VITE_PROJECT_MAPPING_WORKSPACE=true make build
```

This is a build-time choice rather than a `config.toml` or browser setting. Run
the resulting build to use the workspace.

With the workspace enabled, open a project from **Data** or from the
[Activity breakdown](/docs/activity/#breakdowns). Use `/data?project_key=<key>`
to select a project directly, or `/data?view=rules` to open the rules editor.
The screenshots below show an enabled build.

![Project inventory in the Data workspace](/docs/assets/generated/screenshots/data-inventory.png)

## Project inventory

With the workspace enabled, the inventory lists every project with its session,
machine, and working-directory counts, the last activity date, and how many
enabled rules target the project. A summary strip totals the projects,
sessions, and sessions currently governed by classification rules.

- You can sort the table by any column and filter it by project name.
- Projects targeted by enabled rules carry a rule badge, and projects recorded
  as a rule's original label carry an original-label badge.
- Sessions with an empty stored project label appear together under a single
  "unknown" row.
- Activity bounds come from session timestamps only. Rows with no recorded
  timestamps show a no-activity state.

Select a row to open the project workspace. If you follow a deep link with an
unknown `project_key`, AgentsView shows the full inventory with a non-blocking
notice.

The optional date picker limits the sessions used for project counts, folder
suggestions, and session previews. Choose a calendar month such as August, a
custom range, or **All time** to remove the filter. Dates use your browser's
timezone and include sessions whose activity overlaps the range. The governed
session total is archive-wide, and AgentsView hides it while a date filter is
active. Folder rules still apply across all dates. This filter changes what you
browse, not which sessions a rule can correct.

![Observed folders for a selected project](/docs/assets/generated/screenshots/data-workspace.png)

## Create a project mapping

The workspace creates
[worktree project mappings](/docs/configuration/#worktree-project-mappings)
directly:

- **Folder suggestions** lists the session folders associated with the selected
  project. Choose **One folder** to correct a single path, or **All folders**
  to prepare corrections for the whole project.
- Selecting a folder opens one mapping row: **Path prefix → Project**. The
  suggested path prefix covers that group's working directories and stays
  editable, so you can shorten it to cover sibling folders when appropriate.
- The **Project** typeahead suggests known projects and accepts a new name.
  When the server normalizes the name (for example, `sample-service` becomes
  `sample_service`), the editor shows the stored form before you apply it.
- The **Full archive impact** preview is live and authoritative, counting
  matching sessions across all dates for that machine. If a prefix touches more
  than one existing project, AgentsView shows a warning with per-project
  counts, which usually means the prefix is too broad. You cannot apply a
  prefix that matches zero sessions.

When you click **Save correction**, AgentsView saves the rule and rewrites the
matching sessions in one atomic step, then reloads the inventory. If the applied
rule renames the selected project, your selection follows the new name. If
mappings change between preview and apply, AgentsView rejects the apply and asks
for a fresh preview.

Session previews appear below the correction controls. Their header includes
previous and next navigation and a filter button to include automated sessions,
which are hidden by default. Navigation loads another page of session records
only when needed, and loads transcript messages only for the selected session.
Sessions without stored messages remain available for mapping, showing a notice
instead of a transcript. Collapse folder suggestions to give previews more
room.

To correct only the session you are previewing, choose a target under **Session
project** and click **Assign session**. That creates a manual override for that
session without changing folder rules. Choose **Use automatic** to remove the
override and restore automatic assignment.

To correct several projects at once, select multiple projects in the inventory
to open **Bulk project correction**. Review the folder suggestions and target
project for each correction before saving. You can also use **All folders**
within a single project.

![Bulk project correction in the opt-in workspace](/docs/assets/generated/screenshots/project-mapping-bulk.png)

During a bulk correction, the **Save** button displays the count of completed
corrections and a progress bar. Each correction applies separately, and
overlapping previews count each matched or changing session once. If the rules
also match projects outside your selection, **Save** asks you to confirm first.
Folder suggestions stay grouped by project, then machine, with larger session
groups first within each machine.

Bulk saves stop if another rule edit or a change to the affected sessions makes
the reviewed impact stale. The editor refreshes the impact and asks you to
confirm again. Corrections already saved remain applied, and their expected
effects do not interrupt the rest of the batch.

## Rules

The rules editor is the default **Data** view. In builds with the workspace
enabled, choose **Project mapping rules** to open it. You can add, edit, apply,
or delete rules for one machine at a time. See
[Worktree Project Mappings](/docs/configuration/#worktree-project-mappings) for
the full rule semantics. Each rule row shows its **governed sessions** count
(how many sessions the rule currently classifies) and the **original label**
recorded when you created the rule through the mapping editor. Rule targets
link back to the corresponding inventory row.

## Repair worktree project names

In 0.47.0 and later, SQLite imports use the repository URL recorded by the
agent when a checkout cannot be identified on disk. That keeps temporary
worktree folders from becoming separate projects. AgentsView reuses an existing
project with the repository's name or a single established Git checkout name,
and otherwise falls back to the repository name. Conflicting checkout names are
left for an explicit rule. AgentsView saves the chosen name so later imports
keep using it.

Session assignments and enabled folder rules take priority. The original parser
label and repository evidence remain in the archive. AgentsView leaves sessions
unchanged if they have a previously identified Git checkout, no usable recorded
repository URL, or an already corrected project.

The first normal writable startup after upgrading repairs existing sessions
automatically, including sessions whose source files are gone. This runs once
and updates project metadata without replacing transcripts or message IDs. If
the repair is interrupted, it retries at the next startup. Read-only opens do
not repair the archive.

To inspect any remaining corrections without reading source files, run:

```bash
agentsview db repair-projects
agentsview db repair-projects --project sample_feature --format json
```

To run a manual repair:

1. Review the proposed names from `agentsview db repair-projects`.
1. Stop the daemon or desktop app.
1. Run the same command with `--apply`. Pass `--project` to limit the repair to
   that label, even before the automatic upgrade has run. Any pending automatic
   repair waits until the next normal writable startup.
1. Restart AgentsView.

Subsequent imports and full resyncs retain the corrected attribution, and
replica pushes carry the updated session projects to PostgreSQL and ClickHouse.

For sessions without recorded repository evidence, use a [folder rule](#rules).

## Read-only servers

On a read-only server (`pg serve` or `clickhouse serve`), manage rules and
session assignments from the writable SQLite archive. The rules table stays
readable. Enabled workspace builds also show the inventory and folder
suggestions, with editing controls replaced by a notice.

## Storage maintenance

Decide what to keep before reclaiming disk space. Removing content and
shrinking the SQLite file are separate steps.

| What you want                                | What to use                                                                                                                                       |
| -------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| Keep images outside SQLite                   | Set `tool_result_images = "offload"` for future imports. Run `db migrate --images` for stored images. Back up `assets/` with the archive.         |
| Keep image descriptions without image access | Set `tool_result_images = "drop"` for future imports. Run `db strip --images` for stored results. Neither deletes asset files.                    |
| Omit results from selected tool categories   | Set `result_content_blocked_categories`, then run `sync --full`. This needs the source files.                                                     |
| Keep transcripts or reporting metadata only  | Set `archive_content`, restart the daemon, then run `sync --full`. The rebuild also applies the policy to copied sessions whose sources are gone. |
| Reclaim SQLite free space after cleanup      | Run `db compact`. It preserves stored sessions and messages and does not change retention settings.                                               |

Change these settings in `config.toml`, or choose the image policy in
**Settings > Tool-result images**. Restart the daemon to apply a changed policy
to new imports. Existing rows stay as they are until you resync or run the
matching maintenance command. See
[Archive content](/docs/configuration/#archive-content) for what each policy
retains and which source files are needed to recover removed content.

### Remove archived images

Use the image cleanup control in **Settings > Tool-result images** to preview
and remove images from stored tool results. You can filter by project or a
cutoff date. The date uses each session's end time, falling back to its start
time and then its creation time when earlier fields are missing. Selected
sessions can include parents, trashed sessions, and sessions whose sources are
gone. Provider transcripts and standalone image files stay unchanged.

![Stored tool-result image cleanup preview](/docs/assets/generated/screenshots/settings-image-cleanup.png)

The command-line equivalent is
[`agentsview db strip --images`](/docs/commands/#agentsview-db-strip-images).
To run it from the CLI:

1. Preview the changes with `--dry-run`.
1. Stop the daemon.
1. Run the same command without `--dry-run` to apply the cleanup. Keep any
   `--project` or `--before` filters you used in the preview.
1. Start the daemon again.

The **Settings** control runs through the daemon and does not need that
stop-and-start sequence.

Cleanup replaces supported inline images and offloaded image references with
readable text descriptions that keep the media type and decoded byte size. New
placeholders use version `1` and an empty `sha256` unless a hash was already
present. Removing an offloaded reference clears `image_ref` and preserves its
hash, without deleting the asset file.

The daemon exposes preview and apply at `POST /api/v1/data/strip-images/preview`
and `POST /api/v1/data/strip-images`. Both routes require localhost and accept
the same project and date selection. Apply returns HTTP 409 if another archive
maintenance operation holds the write barrier, rather than queuing behind it.

### Ingest-time image offload

Set `tool_result_images = "offload"` to store supported tool-result images in
`{dataDir}/assets` instead of embedding their bytes in SQLite, then restart the
daemon. Supported formats are PNG, JPEG, WebP, and GIF. Unsupported media and
malformed data stay inline, and archives that omit tool content write no image
assets.

To move images already in the archive, use
[`agentsview db migrate --images`](/docs/commands/#agentsview-db-migrate-images):

1. Preview the migration first.
1. Stop the daemon.
1. Apply the migration.
1. Start the daemon again.

Migration preserves image access, whereas `db strip --images` removes it.

AgentsView names each asset `<sha256hex><ext>` from its content hash and
writes the file before saving the reference in SQLite. It reuses an existing
asset only when its byte count and hash match, and it can replace a missing or
corrupt asset while the inline source bytes remain available. Stored
`agentsview_image` blocks use `image_ref` for the `asset://` reference.

If an asset write fails during ingestion or copied-session resync, AgentsView
keeps the original inline content. Restore access to the asset directory, then
retry with `db migrate --images`. A failed database transaction can leave
complete unreferenced assets on disk, and a retry reuses matching files. There
is no automatic cleanup of unreferenced assets. Both CLI image commands report
sessions committed before a later failure.

Back up `assets/` alongside the database. A separate local serving host needs
both. PostgreSQL and CockroachDB retain the references, though they cannot
resolve local image assets. Normalized Markdown session exports carry the
stored content. Artifact exports carry references without asset files, and
artifact imports replace those references with readable descriptions. HTML
export keeps its own [export behavior](/docs/usage/#session-export). The raw
`agentsview session export` command streams provider source bytes, including
original inline payloads.

The daemon may retain recently served canonical image bytes in a bounded
in-process cache for up to seven days from generation. Entries can leave sooner
under the 64-entry or 64 MiB limits, and process exit clears them. This cache
is separate from the durable `{dataDir}/assets` store and from your browser's
cache policy. Evicting a cache entry never changes the durable asset or its
backup.

### Reclaim free space

Image cleanup reports stored-content and decoded-image byte counts. Those are
content measurements, not reclaimed disk space. Run
[`agentsview db compact`](/docs/commands/#agentsview-db-compact) afterward to
reclaim free SQLite pages and truncate the write-ahead log (WAL).

Compaction reports database, WAL, SHM, free-page, staging, and reclaimed sizes
separately. Do not add its reported savings to the content byte counts. If you
request compaction while sync, resync, or another compaction holds the archive
maintenance barrier, it fails with a conflict instead of queuing. See the
command reference for staging requirements and interrupted-compaction recovery.
