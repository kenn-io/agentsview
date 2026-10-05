---
last_edited: 2026-09-24
title: Kata
description: Connect AgentsView to a Kata issue tracker as an optional spoke
---

AgentsView can connect to a [Kata](https://github.com/kenn-io/kata) issue
tracker through a local daemon or an HTTPS hub. The connection is off by
default. The AgentsView hub can file Friction Log patterns manually or during
digest builds. Kata owns issue state; AgentsView keeps the link between a
pattern and its issue.

## Configure

On the AgentsView hub, add these settings to `config.toml`. `public_url` is
the browser address of AgentsView, including any path prefix. `endpoint` is
the address of Kata Hub:

```toml
public_url = "https://archive.example.test"

[kata]
enabled = true
endpoint = "https://kata.example.test"
# Unix socket: unix:///path/to/daemon.sock
# Windows: unix:///C:/path/to/daemon.sock
token_env = "AGENTSVIEW_KATA_TOKEN"    # variable name, never the token
project = "agentsview"
actor = "agentsview"
timeout = "10s"

[friction.kata]
auto_file = true
kinds = ["correction", "error", "workaround", "deferral", "pattern"]
```

Set `AGENTSVIEW_KATA_TOKEN` in the environment of the process running
AgentsView, then restart it. This token authenticates AgentsView to Kata Hub.
It is separate from the credential your browser or CLI uses to access
AgentsView. Omit `auto_file` or set it to `false` for manual filing only.

- Leave `endpoint` empty to use `kata daemon locate --json`, Kata's own
  discovery command. It may start a stopped local daemon.
- HTTPS endpoints need `token_env`. Use a dedicated token for AgentsView so
  its writes are attributed to the integration. Under Kata identity mode, the
  token decides attribution and `actor` is ignored.
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
`GET /api/v1/kata/status`; `/api/v1/version` reports `kata_available`.
The command uses the AgentsView daemon that owns the data directory. It does
not start one. When no daemon owns the directory, it probes from the local
config. If the owning daemon is unreachable, it returns an error.
Use `--format json` or its `--json` alias for machine-readable output.

Use `--server URL` to ask an explicit AgentsView daemon. Supply its credential
with `AGENTSVIEW_SERVER_TOKEN` or `--server-token-file PATH`.

When status is `ready`, preview and file an already stored finding by its
fingerprint, including a finding from today:

```bash
agentsview friction findings --session <session-id> --json
agentsview friction file <fingerprint> --dry-run
agentsview friction file <fingerprint>
```

Manual filing does not wait for a daily digest. Automatic filing runs when a
digest is built for a completed day. See
[filing to Kata](/docs/friction-log/#filing-to-kata) for evidence limits,
retry behavior, and the page controls.

Manual filing requires Kata status `ready`. If the hub's readiness check fails,
the request returns `kata_unavailable` without an outbox entry. Check the status
again and retry the file command when Kata is ready. Automatic filing and create
attempts that fail after the readiness check use the retry outbox.

## Who files issues

Only the AgentsView instance that holds the aggregated archive can file to
Kata: `pg serve` on a PostgreSQL hub, or a standalone instance that does not
push to PostgreSQL. A laptop that pushes to a hub reports `not_hub` and never
contacts Kata, even when it shares the hub's `[kata]` config. See
[Friction Log: filing to Kata](/docs/friction-log/#filing-to-kata).

## Limits

- Kata redirects are refused and responses are capped at 1 MiB.
- When Kata is down, the rest of AgentsView keeps working; only the connection
  status changes.
