---
title: Recent Edits
description: Browse the files your agents edited most recently across every session in AgentsView
---

The **Recent Edits** page is a top-level feed of the files your agents changed
most recently, gathered from every session in one place. Open it from the
**More** menu in the header, or go directly to `/recent-edits`.

Each row is one file, grouped by project and path and ordered by its most recent
edit. Expand a row to see the individual edits that touched the file, newest
first, and click any edit to jump to the exact message in the session that
produced it.

![Recent Edits feed](/assets/generated/screenshots/recent-edits.png)

## What It Shows

Recent Edits collects every `Edit` and `Write` tool call that recorded a file
path and groups them by `(project, file path)`. For each file it shows:

- The project and the file path, with the full path on hover.
- How many times the file was edited.
- When it was last edited, as a relative time.

Files are ordered by their most recent edit. When two files were last touched in
the same message, the later tool call ranks first, so the feed reflects the true
order of changes.

## Opening An Edit

Expand a file row to list its recent edits, capped at the most recent for that
file — older edits are summarized as "showing latest N of M". Each edit shows
the tool, the session it ran in, and when it happened. Selecting an edit
navigates to that session and scrolls to the message that made the change.

## Filtering And Paging

The feed honors the header's **project filter**: pick a project to limit the
feed to files in that project, or clear it to see everything across projects.
Results are paged — use **Load more** to fetch the next set of files.

The feed loads when you open the page, when you change the project filter, and
when you press **Refresh**. It does not reload automatically as new sessions
sync, so the list stays stable while you read it; press **Refresh** to pull in
the latest edits.

## API

The same data backs the `GET /api/v1/recent-edits` endpoint, which accepts
`limit`, `offset`, and `project` query parameters and returns the grouped files
with their inlined edits. It is served identically by the local SQLite store and
by the PostgreSQL and DuckDB read backends.
