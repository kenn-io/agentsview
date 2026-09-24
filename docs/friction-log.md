---
last_edited: 2026-09-24
title: Friction Log
description: Daily heuristic review of corrections, tool errors, workarounds, deferrals, frustration, interruptions and recurring patterns
---

The Friction Log reviews your archived sessions once per local day and writes a
dated digest of the places where work went wrong. It raises a P0 alert when the
same tool fails in three or more sessions, and it tracks how often each pattern
recurs across days.

These findings are heuristics, not ground truth. Every finding comes from
regular expressions and thresholds applied to stored session data; no model
decides a finding. Expect false positives, especially for corrections,
workarounds and frustration, and use the links to read the session before acting
on one.

The review logic is adapted from [jilog](https://github.com/Joi/jilog)'s session
review (MIT).

## The Friction Log page

Open **Friction Log** from the header or navigate to `/friction`. The page opens
the latest digest. Use the arrows or date picker to move between days. The
selected date stays in the URL as `?date=YYYY-MM-DD`, so a link opens the same
digest.

![Friction Log page](/docs/assets/generated/screenshots/friction-log.png)

The page shows P0 alerts and new or recurring patterns first, then sections for
each kind of finding. Session findings link to the session and, when available,
the exact message. Interruptions are grouped by session with a count.
Diagnostics recorded by another tool have no session link. Personas and spend
appear when the digest contains them. **Show Markdown** displays the stored
digest text and offers copy and download actions.

**Build now** builds missing complete days when this server is allowed to build
digests. It never builds today. If digest building is disabled and no digests
exist, the page explains why. Read-only servers can still show stored digests
but do not offer **Build now**. The session signal panel also lists findings
for the open session, including findings from messages added after its daily
digest was built.

## What it detects

| Kind         | What it means                                                                                                                                             |
| ------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Correction   | A short user reply right after an assistant turn, between two assistant turns                                                                             |
| Error        | A tool call that failed or was cancelled, minus known noise such as a bare command timeout                                                                |
| Workaround   | An assistant message that says "for now", "temporary", "hack", "TODO" and similar                                                                         |
| Deferral     | An assistant message that puts work off ("I'll come back to this", "next session")                                                                        |
| Pattern      | Session health: retry loops, runaway tool loops, edit churn, mid-task compaction, context pressure, and long stretches of tool calls with no user message |
| Frustration  | A user message with frustration phrases ("still broken", "same error") or mostly capital letters, using the same rule as the Quality page                 |
| Interruption | A turn the user interrupted. Only Claude Code sessions record interruptions today                                                                         |

## On by default

The Friction Log is on by default. The daemon checks every hour for complete
local days without a digest and builds them. Today is never built, because it is
not finished. Each session is reviewed once, in the digest for the day of its
last activity; messages added after that day's digest was built still show on
the session, but not in a later digest.

To turn it off, or to pick the time zone that dates the digests:

```
[friction]
enabled = false      # default true
timezone = ""        # IANA zone; empty uses the machine's local zone
backfill_days = 7    # how far back the first run looks
```

Issue filing is planned for a later update. Digests and pattern reads work
without it.

## Read digests

- CLI: `agentsview friction digest` prints the latest digest as Markdown.
    `--date YYYY-MM-DD` picks a day, and `--format json` prints the counts-only
    summary object.
- API: `GET /api/v1/friction/digests`, `GET /api/v1/friction/digests/{date}` and
    `GET /api/v1/friction/digests/{date}/md`.
- MCP: `get_friction_digest` and `list_friction_patterns`.

## Build on demand

`agentsview friction run` builds every missing complete day. `--date` builds one
day, `--rebuild` re-renders an existing day while keeping the sessions it
already covered, and `--dry-run` computes without writing. The command prints
one summary line per day, or the summary object with `--json`. It exits non-zero
only when the review itself fails.

## Findings and patterns

`agentsview friction findings` lists stored findings, filtered by `--date`,
`--kind` or `--session`. `agentsview friction patterns` ranks recurring patterns
by how many times they occurred, with the first and last day seen. Each pattern
has a fingerprint (`fl1:` plus a SHA-256 of its title) that stays the same
across days.

## Limits

- The review reads only stored session data; it needs no transcript files.
- On `pg serve`, the hub builds its own digests from pushed sessions. Digests
    are not pushed between stores.
- DuckDB and ClickHouse mirrors do not serve the Friction Log.
- Session links in digests need `public_url` to be set.
