---
title: Recent Edits
description: Browse the files your agents edited most recently across every session in AgentsView
---

Use the **Recent Edits** page to browse the files your agents changed most
recently across every session in one place. Open it from the **Recent Edits**
tab in the header, or go directly to `/recent-edits`.

Each row represents one file, grouped by project and path and ordered by its
most recent edit. Expand a row to see the individual edits that touched the
file, newest first, and select any edit to jump to the exact message in the
session that produced it.

![Recent Edits feed](/docs/assets/generated/screenshots/recent-edits.png)

## What it shows

Recent Edits collects every tool call in the Edit or Write category that
recorded a file path, including tools such as `Edit`, `Write`, `NotebookEdit`,
and `apply_patch`, and groups them by `(project, file path)`. Each file row
shows:

- The project and file path, with the full path on hover.
- How many times the file was edited.
- When it was last edited, as a relative time.

Files appear in order of their most recent edit. When two files were last
touched in the same message, the later tool call ranks first, so the feed
reflects the true order of changes.

## Open an edit

Expand a file row to list its recent edits, capped at the most recent entries
for that file. Older edits appear as a "showing latest N of M" summary. Each
edit shows the tool, the session it ran in, and when it happened. Select an edit
to open that session and scroll to the message that made the change.

## Filter and page

The feed honors the header's **project filter**. Pick a project to limit the
feed to files in that project, or clear the filter to see files across all
projects. Results are paged, and you can select **Load more** to fetch the next
set of files.

The feed loads when you open the page, when you change the project filter, and
when you select **Reload list**. It does not reload automatically as new
sessions sync, so the list stays stable while you read it. Select **Reload
list** to pull in the latest edits.

## API

The `GET /api/v1/recent-edits` endpoint backs this page. It accepts `limit`,
`offset`, and `project` query parameters and returns the grouped files with
their inlined edits. The local SQLite store and the PostgreSQL and ClickHouse
read backends serve it identically.
