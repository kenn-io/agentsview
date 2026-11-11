---
last_edited: 2026-11-09
title: Quality
description: Deterministic quality signals and recommendations computed from session data
---

Quality summarizes observable patterns in your session archive. Unlike
[Generated insights](/docs/recall/#current-surface), every score, count, and
recommendation on this page is computed directly from stored session data; the
page does not call a model.

Open **Quality** from the header or navigate to `/quality`.

![Quality page](/docs/assets/generated/screenshots/quality.png)

For day-by-day corrections, tool errors, workarounds, deferrals, frustration and
interruptions, open the [Friction Log](#friction-log) from the card at the top
of this page.

## Scope

The toolbar controls the complete page scope:

- date range;
- project;
- session agent; and
- human, automated, or combined sessions.

Quality keeps its date state separate from other pages unless date-range yoking
is enabled. With yoking enabled, changing the range can update other analytical
pages that participate in the shared range.

## Recommendations and patterns

The first section translates measured patterns into rule-based next actions. A
recommendation appears only when its corresponding threshold is met.

The quality-pattern section shows aggregate scores, session coverage, severity,
affected-session counts, and calibration context. Expand a pattern to inspect
the source sessions and jump to the relevant transcript evidence. Sessions that
cannot be scored remain visible in the coverage counts instead of being silently
treated as healthy.

Use Generated insights when you want a model-written report over a chosen scope.
Use Quality when you need repeatable metrics whose results do not depend on a
model response.

## Friction Log

The Friction Log shows where sessions went wrong each day, so you can fix what
keeps failing. Each finding links to the exact message. Findings come from
rules, not a model, so check the session before acting on one.

![Friction Log daily digest with alerts, new patterns, and recurring patterns](/docs/assets/generated/screenshots/friction-log.png)

Turn it on in `config.toml`, then restart the daemon:

```toml
[friction]
enabled = true
```

Open **Friction Log** from the header, or go to `/friction`.

AgentsView builds one digest for each completed day. It never builds today's.
Choose **Build now** to build missing days without waiting for the hourly job.

Each digest puts the most urgent items first:

- **Tools failing across sessions** lists tools with repeated failures.
- **New patterns** lists problems first seen that day.
- **Recurring patterns** lists problems seen before, with how often they
  happened and in how many sessions.
- Sections below list every finding by kind, each linked to its message.

From a digest you can:

- Move between dates with the date control.
- Download the digest as Markdown.
- Choose **File to Kata** or **Link issue** on a pattern, and **Unlink** to
  remove a link. These need a
  [Kata connection](/docs/kata/#file-friction-log-patterns).
- Choose **Generate summary** for an optional model-written overview. It runs
  only when you ask and uses your `[insights]` provider.

Agents and scripts can read the same data with
[`agentsview friction`](/docs/commands/#agentsview-friction) and the
`get_friction_digest` and `list_friction_patterns` [MCP tools](/docs/mcp/). See
[Friction Log settings](/docs/configuration/#friction-log) for backfill, time
zones, size limits, and what the summary sends to the model.
