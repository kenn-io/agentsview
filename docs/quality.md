---
last_edited: 2026-11-09
title: Quality
description: Deterministic quality signals and recommendations computed from session data
---

Use **Quality** to review observable patterns in your session archive. Unlike
[Generated insights](/docs/recall/#current-surface), every score, count, and
recommendation on this page comes directly from stored session data without
calling a model.

Open **Quality** from the header or go to `/quality`.

![Quality page](/docs/assets/generated/screenshots/quality.png)

For day-by-day corrections, tool errors, workarounds, deferrals, frustration,
and interruptions, open the [Friction Log](#friction-log) from the card after
the recommendations in the first section of this page.

## Scope

The toolbar sets the scope for the entire page:

- Date range
- Project
- Session agent
- Human, automated, or combined sessions

Quality keeps its date range separate from other pages unless you enable
date-range yoking. When yoking is on, changing the range can update other
analytical pages that participate in the shared range.

## Recommendations and patterns

The first section turns measured patterns into rule-based next actions. A
recommendation appears only when your sessions meet its threshold.

The quality-pattern section shows aggregate scores, session coverage, severity,
affected-session counts, and calibration context. Expand a pattern to inspect
its source sessions and jump to the relevant transcript evidence. Sessions that
cannot be scored remain visible in the coverage counts instead of being silently
treated as healthy.

Use Generated insights when you want a model-written report over a chosen scope,
and use Quality when you need repeatable metrics whose results do not depend on
a model response.

## Friction Log

The Friction Log shows where sessions went wrong each day, so you can fix what
keeps failing. Each finding links to the exact message. Because findings come
from rules rather than a model, check the session before acting on one.

![Friction Log daily digest with alerts, new patterns, and recurring patterns](/docs/assets/generated/screenshots/friction-log.png)

To turn on the Friction Log:

1. Add the following to `config.toml`:

   ```toml
   [friction]
   enabled = true
   ```

1. Restart the daemon.
1. Open **Friction Log** from the header, or go to `/friction`.

AgentsView builds one digest for each completed day and never builds today's. To
build missing days without waiting for the hourly job, choose **Build now**.

Each digest puts the most urgent items first:

- **Tools failing across sessions.** Tools with repeated failures.
- **New patterns.** Problems first seen that day.
- **Recurring patterns.** Problems seen before, along with how often they
  happened and in how many sessions.

The sections below list every finding by kind, each linked to its message.

From a digest, you can:

- Move between dates with the date control.
- Download the digest as Markdown.
- Choose **File to Kata** or **Link issue** on a pattern, and **Unlink** to
  remove a link. These actions require a
  [Kata connection](/docs/kata/#file-friction-log-patterns).
- Choose **Generate summary** for an optional model-written overview. It runs
  only when you ask and uses your `[insights]` provider.

Agents and scripts can read the same data with
[`agentsview friction`](/docs/commands/#agentsview-friction) and the
`get_friction_digest` and `list_friction_patterns` [MCP tools](/docs/mcp/). For
backfill, time zones, size limits, and details on what the summary sends to the
model, see [Friction Log settings](/docs/configuration/#friction-log).
