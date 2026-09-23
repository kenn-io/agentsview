---
title: Kata
description: Connect AgentsView to a Kata issue tracker as an optional spoke
---

AgentsView can check a connection to a [Kata](https://github.com/kenn-io/kata)
issue tracker, either through a local daemon or an HTTPS hub. The connection is
off by default. AgentsView does not file issues yet. Kata owns issue state;
AgentsView will keep only the link between a Friction Log pattern and its Kata
issue.

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

## Who files issues

Only the AgentsView instance that holds the aggregated archive can file to
Kata: `pg serve` on a PostgreSQL hub, or a standalone instance that does not
push to PostgreSQL. A laptop that pushes to a hub reports `not_hub` and never
contacts Kata, even when it shares the hub's `[kata]` config. Issue filing will
arrive with the Friction Log Kata integration.

## Limits

- Kata redirects are refused and responses are capped at 1 MiB.
- When Kata is down, the rest of AgentsView keeps working; only the connection
  status changes.
