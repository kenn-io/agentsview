---
title: Kata
description: Connect AgentsView to a Kata issue tracker as an optional spoke
---

AgentsView can connect to a [Kata](https://github.com/kenn-io/kata) issue
tracker, either through a local daemon or an HTTPS hub, and file Friction Log
patterns as Kata issues. The connection is off by default. Kata owns issue
state; AgentsView keeps only the link between a pattern and its issue.

## Configure

Add a `[kata]` section to `config.toml`:

```toml
[kata]
enabled = true
endpoint = "https://kata.example.test"
# Unix socket: unix:///path/to/daemon.sock
# Windows: unix:///C:/path/to/daemon.sock
token_env = "AGENTSVIEW_KATA_TOKEN"    # variable name, never the token
project = "agentsview"
actor = "agentsview"
timeout = "10s"
```

- Leave `endpoint` empty to use `kata daemon locate --json`, Kata's own
  discovery command. It may start a stopped local daemon.
- HTTPS endpoints need `token_env`. Use a dedicated token for AgentsView so its
  writes are attributed to the integration. Under Kata identity mode, the
  token decides attribution and `actor` is ignored.
- `actor` names AgentsView on the issues it files.
- Plain `http://` works only for loopback hosts unless `allow_insecure = true`.
- The project must already exist in Kata. AgentsView never creates it.

## Check the connection

```bash
agentsview kata status [--format human|json] [--json]
```

The command reports `disabled`, `not_hub`, `unavailable`, `incompatible`,
`unauthenticated`, `wrong_project`, or `ready`. `incompatible` means Kata's API
schema is older than 0.21.0. A non-ready state is information, so the command
still succeeds. The same status object is available at
`GET /api/v1/kata/status`. `/api/v1/version` reports `kata_available`, which is
true when the most recent check found Kata ready; it never contacts Kata itself.
The command uses the AgentsView daemon that owns the data directory, including a
running `pg serve`. It does not start one. When no daemon owns the directory, it
probes from the local config. If the owning daemon is unreachable or needs an
upgrade, the command returns an error instead of probing locally. Use
`--format json` or its `--json` alias for machine-readable output.

Use `--server URL` to ask an explicit AgentsView daemon. Supply its credential
with `AGENTSVIEW_SERVER_TOKEN` or `--server-token-file PATH`.

## Who files issues

Only the AgentsView instance that holds the aggregated archive can file to Kata:
`pg serve` on a PostgreSQL hub, or a standalone instance that does not push to
PostgreSQL. A laptop that pushes to a hub reports `not_hub` and never contacts
Kata, even when it shares the hub's `[kata]` config.

## File Friction Log patterns

Filing is manual. Pick a pattern's fingerprint from
`agentsview friction patterns`, then file it from the hub:

```bash
agentsview friction file <fingerprint> [--dry-run] [--format human|json | --json]
agentsview friction link <fingerprint> <issue>
agentsview friction unlink <fingerprint>
```

- `friction file` first asks Kata for an issue already tagged with the pattern's
  fingerprint, in the `friction.fingerprint` metadata key. It links one it
  finds, and creates an issue only when Kata holds none. Filing a pattern
  again returns its existing link. When Kata holds more than one candidate,
  filing stops so you can choose one with `friction link`.
- `--dry-run` prints the issue without contacting Kata.
- The issue title and body describe the finding the pattern was last seen as,
  with how often it recurred. Secrets are masked and home directories
  shortened to `~` before anything leaves the machine. With `public_url` set,
  the body links to the session and the digest.
- Corrections, errors, workarounds, deferrals, and patterns can be filed.
  Frustration quotes the user and interruptions carry no text, so neither is
  filed.
- `friction link` records an existing issue, given as a qualified ID such as
  `agentsview#f001`, a short ID, or a UID. `friction unlink` removes the
  AgentsView link only; the Kata issue stays as it is.

Pattern reads show the link beside each pattern: the `ISSUE` column of
`friction patterns`, the `issue` field in the API and MCP results, and the
digest's `patterns`. Stored digests are not rewritten. Links survive a full
resync. The same actions are available over HTTP as `POST`, `PUT`, and `DELETE`
on `/api/v1/friction/patterns/{fingerprint}/issue`; they require auth or a
localhost request, like `friction run`.

## Limits

- Kata redirects are refused and responses are capped at 1 MiB.
- When Kata is down, the rest of AgentsView keeps working; only the connection
  status changes.
