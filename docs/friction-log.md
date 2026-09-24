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

Digests and pattern reads work without Kata.

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

## Diagnostics from your own tools

Enable the event ledger and opt in to diagnostics to include operational
failures from your own tools in the daily digest:

```toml
[ledger]
enabled = true

[friction.diagnostics]
enabled = true
subsystems = ["*"] # default: all subsystems
# zones = ["default"] # default: all configured ledger zones
```

A diagnostic is a `health` event whose structured payload has `kind` set to
`diagnostic`. It needs a lowercase `diagnostic` name (1–64 characters from
`a-z`, `0-9`, `.`, `_`, `:` and `-`), a stable nonblank `identity`, and a string
`detail`. An optional `seat` names the affected seat. The event becomes a P3
error signal; diagnostics never raise P0 alerts.

For example, a CI job could append a diagnostic after a nightly check fails:

```sh
agentsview ledger append --class health --subsystem ci \
  --summary "nightly lint failed" \
  --payload '{"kind":"diagnostic","diagnostic":"ci_nightly_failed","identity":"'"$RUN_ID"':ci_nightly_failed","detail":"lint step exited 1"}'
```

The review keeps one line per identity, including when the same diagnostic is
sent by more than one source or on another day. A rebuild keeps identities
already recorded for that date. A producer that also needs exact event dedupe
can set the event id with `ledger.DeterministicEventID(source, identity)` when
using the ledger append API. Automatic issue filing for diagnostics arrives
with the filing integration.
## Filing to Kata

When `[kata]` is enabled, the AgentsView hub can file Friction Log patterns to
Kata. The hub is `pg serve`, or a standalone instance that does not push to
PostgreSQL. Pushing laptops never file, even when they share the hub's config.
Filing is manual by default. To file during each digest build, set:

```toml
[friction.kata]
auto_file = true
kinds = ["correction", "error", "workaround", "deferral", "pattern"]
```

The hub looks for an issue with matching `friction.fingerprint` metadata before
creating one. It uses an idempotency key for creates, so retries do not create
duplicates. It does not match issues by title. Titles and bodies pass through
secret redaction; home-directory paths are shortened to `~`. Session and digest
links use `public_url`.

When automatic filing is enabled and Kata is down, the digest is still written.
Pending patterns wait in an outbox and are filed when Kata returns. The digest
is then re-rendered with a Kata issue link. A failed filing is abandoned after
14 days; use `agentsview friction file <fingerprint>` to retry it manually.

Ambiguous matches and other conflicts are marked `needs_human`. Resolve them
with `agentsview friction link <fingerprint> <kata-ref>` or
`agentsview friction file <fingerprint> --force-new`. Use
`agentsview friction unlink <fingerprint>` to remove only the local link. To
preview a create request, use `agentsview friction file <fingerprint> --dry-run`.
`agentsview friction file --date YYYY-MM-DD` files every pattern in a stored
digest. These commands need the running hub daemon or `--server URL`.

### When a pattern comes back

When the hub files a recurring pattern whose Kata issue was closed as `done`,
it reopens the issue, comments
`Recurred on <date> — closure may have been premature.` (with a session link
when `public_url` is set), and adds the `friction:recurred` label. It does this
at most once per issue per digest date.

Issues closed for any other reason (`wontfix`, `duplicate`, `superseded`, or
`audit-no-change`) stay closed; the pattern is only linked. Set
`reopen_on_recurrence = false` under `[friction.kata]` to turn reopening off.

When a digest contains a pattern already linked to an open issue before the
digest was built, its line ends with `(recurred in sessions totaling $X)`, the
summed cost of the sessions where it appeared that day. Deferrals and
interruptions are never annotated.

## Limits

- The review reads only stored session data; it needs no transcript files.
- On `pg serve`, the hub builds its own digests from pushed sessions. Digests
    are not pushed between stores.
- DuckDB and ClickHouse mirrors do not serve the Friction Log.
- Session links in digests need `public_url` to be set.
