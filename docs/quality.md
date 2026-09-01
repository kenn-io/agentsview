---
title: Quality
description: Deterministic quality signals and recommendations computed from session data
---

Quality summarizes observable patterns in your session archive. Unlike
[Generated insights](/docs/recall/#current-surface), every score, count, and
recommendation on this page is computed directly from stored session data; the
page does not call a model.

Open **Quality** from the header or navigate to `/quality`.

![Quality page](/docs/assets/generated/screenshots/quality.png)

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
