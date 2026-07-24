---
title: Data
description: Project inventory, reclassification, and worktree mapping rules
---

The **Data** page is where you inspect and clean project classification across
the whole archive. It is the home of the worktree mapping rules that previously
lived in the Settings **Worktree mappings** section.

Open it from the **Data** tab in the header, or follow a project link from the
[Activity breakdown](/activity/#breakdowns). Deep links are stable:
`/data?project_key=<key>` selects a project, and `/data?view=rules` opens the
[Rules view](#rules).

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

## Reclassify A Project

The workspace embeds the reclassification editor, which works through the
[worktree project mapping](/configuration/#worktree-project-mappings) system:

- It lists the worktrees that produced the project's sessions across the entire
  archive, grouped by machine and worktree evidence. A single group is
  preselected; several groups require an explicit choice, because different
  worktrees usually need different target projects.
- The suggested **path prefix** covers the selected group's working directories.
  You can edit it — for example, shorten it to cover sibling worktrees of the
  same repository.
- The **target project** typeahead suggests known projects and accepts a new
  name. When the server normalizes the name (for example `sample-service`
  becomes `sample_service`), the editor shows the stored form before you
  apply.
- The **full archive impact** preview is live and authoritative: it counts
  matching sessions across all dates for that machine. A prefix that touches
  more than one existing project shows a warning with per-project counts —
  usually a sign the prefix is too broad. A prefix matching zero sessions
  cannot be applied.

**Apply** saves the rule and rewrites the matching sessions in one atomic step,
then reloads the inventory. If the applied rule renamed the selected project,
the selection follows the new name. If mappings changed between preview and
apply, the apply is rejected and a fresh preview is required.

## Rules

The **Rules** toggle shows the worktree mapping rules for one machine at a time,
with the same add, edit, apply, and delete controls that Settings previously
offered — see
[Worktree Project Mappings](/configuration/#worktree-project-mappings) for the
full rule semantics. Each rule row also shows its **governed sessions** count
(how many sessions the rule currently classifies) and the **original label**
recorded when the rule was created through the reclassification editor. Rule
targets link back to the corresponding inventory row.

## Read-Only Servers

On a read-only server (`pg serve` or `duckdb serve`) the inventory, the
candidate evidence, and the Rules table remain fully readable, but the editor
and rule mutations are replaced by a notice: classification rules are managed
from the writable archive that ingests the machine's sessions.
