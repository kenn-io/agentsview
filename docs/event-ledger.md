---
title: Event Ledger
description: An optional append-only, checksummed event log compatible with jilog segment files
---

The event ledger is an optional, append-only log of structured events. Each
write is a sealed segment: a batch of events plus a CRC-32 checksum over their
exact serialized bytes. Segments are stored in the archive database and never
change after they are written. The format is byte-compatible with
[jilog](https://github.com/Joi/jilog) ledger segments, so existing jilog
segment directories can be imported and exported.

The ledger is off by default and records nothing on its own until something
appends to it.

## Enabling the ledger

```toml
[ledger]
enabled = true
# source = ""          # "" = "av-<installation id>"
# default_zone = "default"

[[ledger.zones]]
id = "default"

[[ledger.zones]]
id = "ops"
import_path = "/srv/ledger/ops"   # follow <import_path>/segments
```

A zone partitions segments and status. The default zone always exists. Zone ids and
the source name must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` because they
become file names.

This host writes as one source. The default source is `av-` plus the data
directory's installation id, so it never collides with a jilog source named
after the host.

## Events

Every event has an id (UUIDv7), a zone, a source and sequence number, a
timestamp, optional correlation and causation ids, optional `actor_ref` and
`object_ref` strings (by convention `kind:value`), a class, a payload tier and
an optional JSON payload. The classes are `ingest`, `route`, `decision`,
`state_change`, `claim`, `delivery`, `projection`, `health`, `approval` and
`note_meta`. The tiers are `metadata_only`, `structured` and `confidential`.

By convention `payload.subsystem` names the component an event is about and
`payload.summary` is a one-line description.

## Commands

```bash
agentsview ledger append --class state-change --subsystem deploy --summary "rolled out v2"
agentsview ledger query --since 24h --subsystem 'hook-*'
agentsview ledger status [--zone Z] [--json]
agentsview ledger verify [--zone Z] [--full]
agentsview ledger import --zone Z --segments DIR
agentsview ledger export --zone Z --dir DIR [--source S]
agentsview ledger rebuild-index --zone Z
```

See the [CLI Reference](/docs/commands/#agentsview-ledger) for every flag.

## Querying

`ledger query` filters events by time, subsystem, class and zone. It prints
newest first per zone. Use `--format json` for the jilog-compatible event
array.

The same query is available as `GET /api/v1/ledger/events` (`since`, repeated
`subsystem`, `class`, `zone`, `limit`) and as the MCP tool `query_ledger`.
`GET /api/v1/ledger/status`, `POST /api/v1/ledger/events` (append as this
machine's source) and `POST /api/v1/ledger/verify` complete the API. Writes
need the auth token, or a localhost request when auth is off. Each event in an
API response carries `serde`, its exact serialized form.

## Guarantees

- **Append-only.** Database triggers reject updates and deletes of stored
  segments and events on SQLite and PostgreSQL.
- **No overwrites.** A segment is identified by zone, source and sequence
  number. A direct write of an identical copy is a no-op. A direct write with
  different content at that identity is an integrity error, and the stored
  segment is kept.
- **Verified imports.** Imported files are checked against their file name and
  their checksum before they are stored. Bad files are reported and retried on
  the next import.
- **Single writer per source.** Only this host's own source is written
  locally; other sources arrive only by import.

`ledger verify` re-checks checksums incrementally and remembers failures and
gaps. `ledger verify --full` re-reads every segment, which also catches damage
to segments that verified earlier.

## Replication to PostgreSQL

`agentsview pg push` copies ledger segments to the PostgreSQL hub with the
archive (see [PostgreSQL Sync](/docs/pg-sync/#event-ledger-push)). The hub keeps
one copy of each segment identity. If it already holds that source and sequence
number with different content, the push refuses the segment, keeps going, and
`ledger status` lists the refusal until it is resolved.

- `replicate = false` on a zone keeps that zone on the machine.
- Segments containing a `confidential` event stay on the machine unless
  `[ledger] replicate_confidential = true`. The whole segment is held back, so
  append confidential events in separate batches.

## Working with jilog ledgers

`ledger import` reads a jilog segments directory (`<ledger_path>/segments`). A
zone with `import_path` is followed automatically every five minutes while the
daemon runs. `ledger export` writes a zone back out as jilog files and never
replaces an existing file.

Limits:

- A segment whose event sequence number is above 2^63-1 cannot be stored and
  is reported.
- Event ids are unique across the archive. If the same event id appears in
  another zone, that zone keeps its segment but does not project the event.
- Import skips a segment identity already in the archive without rereading
  its file. A changed copy of an imported file is not checked again.
- jilog's own `verify` cannot always re-check segments that contain some
  floating-point payload values, because jilog parses floats without full
  round-trip precision. AgentsView verifies them correctly. Prefer integers
  and strings in payloads that jilog must also verify.
