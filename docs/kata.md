---
title: Kata
description: Connect AgentsView to a Kata issue tracker and file Friction Log patterns as issues
---

You can connect AgentsView to a [Kata](https://github.com/kenn-io/kata) issue
tracker, either through a local daemon or an HTTPS hub, and file Friction Log
patterns as Kata issues. The connection is off by default. Kata owns issue
state, and AgentsView keeps only the link between a pattern and its issue.

## Configure

Add a `[kata]` section to `config.toml`:

```toml
[kata]
enabled = true
endpoint = "https://kata.example.test"
# Unix socket: unix:///path/to/daemon.sock
# Windows: unix:///C:/path/to/daemon.sock
token_env = "AGENTSVIEW_KATA_TOKEN"    # variable name, never the token
# token_file = "~/.config/agentsview/kata-token" # or a private file holding it
project = "agentsview"
actor = "agentsview"
timeout = "10s"
```

- Leave `endpoint` empty to use `kata daemon locate --json`, Kata's own
  discovery command. That command may start a stopped local daemon.
- For HTTPS endpoints, set either `token_env` or `token_file`, not both.
  `token_file` points to a private file holding the token; see
  [Read secrets from files](/docs/configuration/#read-secrets-from-files). Use
  a dedicated token for AgentsView so Kata attributes its writes to the
  integration. Under Kata identity mode, the token decides attribution and
  Kata ignores `actor`.
- `actor` names AgentsView on the issues it files.
- Plain `http://` works only for loopback hosts unless you set
  `allow_insecure = true`.
- The project must already exist in Kata. AgentsView never creates it.

## Check the connection

Run `agentsview kata status` to verify the integration:

```bash
agentsview kata status [--format human|json] [--json]
```

The command reports `disabled`, `not_hub`, `unavailable`, `incompatible`,
`unauthenticated`, `wrong_project`, or `ready`. `incompatible` means Kata's API
schema is older than 0.21.0. Because a non-ready state is informational, the
command still exits with success. Pass `--format json` or `--json` for
machine-readable output.

The same status object is available at `GET /api/v1/kata/status`.
`/api/v1/version` also reports `kata_available`, which is true when the most
recent check found Kata ready; it never contacts Kata itself.

By default, `agentsview kata status` talks to the AgentsView daemon that owns
the data directory, including a running `pg serve`, without starting one. When
no daemon owns the directory, it probes from your local config instead. If the
owning daemon is unreachable or needs an upgrade, the command returns an error
rather than falling back to a local probe.

To query a specific AgentsView daemon, pass `--server URL` and supply its
credential with `AGENTSVIEW_SERVER_TOKEN` or `--server-token-file PATH`.

## Who files issues

Only the AgentsView instance that holds the aggregated archive can file to
Kata: either `pg serve` on a PostgreSQL hub, or a standalone instance that does
not push to PostgreSQL. A laptop that pushes to a hub reports `not_hub` and
never contacts Kata, even when it shares the hub's `[kata]` config.

## File Friction Log patterns

You can file patterns by hand, or let the hub file recent patterns every hour.
On the hub's Friction Log page, once Kata is ready, each pattern under **Needs
attention** offers **File to Kata** and **Link issue**. A linked pattern shows
its issue alongside **Unlink**. These actions appear only where the server
accepts writes, the same as **Build now**.

From the command line, find a pattern's fingerprint with
`agentsview friction patterns`, then file or link it from the hub:

```bash
agentsview friction file <fingerprint> [--dry-run] [--format human|json | --json]
agentsview friction link <fingerprint> <issue>
agentsview friction unlink <fingerprint>
```

- `friction file` first asks Kata for an issue already tagged with the
  pattern's fingerprint in the `friction.fingerprint` metadata key. If Kata
  finds one, AgentsView links it and creates a new issue only when none exists.
  Filing a pattern again returns its existing link. If Kata returns more than
  one candidate, filing stops so you can pick one with `friction link`.
- Pass `--dry-run` to print the issue without contacting Kata.
- The issue title and body describe the finding the pattern was last seen as,
  along with how often it recurred. Before anything leaves the machine,
  AgentsView masks secrets and shortens home directories to `~`. When
  `public_url` is set, the body includes links to the session and the digest.
- You can file corrections, errors, workarounds, deferrals, and patterns.
  Frustration quotes the user and interruptions carry no text, so neither is
  filed.
- `friction link` records an existing issue given as a qualified ID such as
  `agentsview#f001`, a short ID, or a UID. `friction unlink` removes only the
  link in AgentsView, leaving the Kata issue as it is.

Pattern reads show the link beside each pattern in the `ISSUE` column of
`friction patterns`, the `issue` field of API and MCP results, and the digest's
`patterns`. AgentsView does not rewrite stored digests, and links survive a
full resync. The same actions are available over HTTP with `POST`, `PUT`, and
`DELETE` on `/api/v1/friction/patterns/{fingerprint}/issue`. Like
`friction run`, those endpoints require authentication or a localhost request.
If a PostgreSQL hub's database role cannot write links, the hub refuses the
request before contacting Kata.

### File patterns automatically

To turn on automatic filing, update the hub's configuration and restart the
daemon:

```toml
[friction.kata]
auto_file = true
kinds = ["error", "workaround"] # Optional; empty means every kind that can be filed.
min_sessions = 2 # Optional; file only patterns seen in at least this many sessions.
```

- Each hour, after building digests, the Friction Log job files unlinked
  patterns of a chosen kind that were last seen within
  `[friction] backfill_days` and appeared in at least `min_sessions` sessions
  (default 2). It uses the same lookup as `friction file`, so it links an issue
  Kata already holds instead of filing a duplicate.
- With automatic filing enabled, the hourly job reopens a linked issue closed
  as `done` if a signal in a saved digest within `backfill_days` occurred after
  the close, and adds a comment with the date it recurred.
- Corrections, `pattern` findings, and deferrals are tracked per session, so
  each appears in only one session. At the default `min_sessions` they are
  never filed automatically. Set `min_sessions = 1` to file all of them.
- Each pass files at most 20 new patterns, most frequent first, so enabling
  automatic filing spreads the backlog over several hours. Patterns waiting to
  retry do not count toward that limit.
- While Kata is not ready, the job files nothing and records no failures.
- After a failed request, the job retries in 1 hour, then doubles the wait up to
  24 hours, even after the pattern is older than `backfill_days`. After 14 days
  of failures, or when Kata rejects the request or holds several candidate
  issues, the pattern waits for you to file or link it manually.
- Unlinking a pattern stops automatic filing for it. Filing or linking it by
  hand starts automatic filing again.
- If you link a pattern by hand while the job is creating an issue for it, your
  link wins, and the issue the job created stays in Kata without a link.

When a pattern has no issue, AgentsView shows why: a note on the Friction Log
page, `(retry)` or `(file or link)` in the `ISSUE` column of
`friction patterns`, and the `filing` field in API and MCP results. An unlinked
pattern appears as paused only on the page of a hub that files automatically.
`--json` output always includes the `filing` state.

## Limits

- AgentsView refuses Kata redirects and caps responses at 1 MiB.
- When Kata is down, the rest of AgentsView keeps working and only the
  connection status changes.
