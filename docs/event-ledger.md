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
agentsview ledger status [--zone Z] [--json]
agentsview ledger verify [--zone Z] [--full]
agentsview ledger import --zone Z --segments DIR
agentsview ledger export --zone Z --dir DIR [--source S]
agentsview ledger rebuild-index --zone Z
agentsview ledger spool emit [--zone Z] [--source S] [--cursor-dir DIR]
agentsview ledger spool ingest [--zone Z]
agentsview ledger spool status [--zone Z] [--source S] [--cursor-dir DIR]
```

See the [CLI Reference](/docs/commands/#agentsview-ledger) for every flag.

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

## Working with jilog ledgers

`ledger import` reads a jilog segments directory (`<ledger_path>/segments`). A
zone with `import_path` is followed automatically every five minutes while the
daemon runs. `ledger export` writes a zone back out as jilog files and never
replaces an existing file.

## Spool interop with jilog and opsctl

Set `spool_path` on each zone that exchanges segments. Its `incoming/` and
`processed/` directories use jilog's spool layout. A file-sync tool can
replicate the spool directory between hosts; AgentsView does not sync it.
Set `spool_authority = true` only on the host that ingests that zone.

```toml
[[ledger.zones]]
id = "default"
spool_path = "/path/to/spool"
spool_authority = true
```

`ledger spool emit` copies sealed segments from this host's source into
`incoming/` using no-clobber hard links. It checks every own segment on each
run. The cursor at `<data_dir>/ledger/spool-cursors/<zone>/<source>.json`
reports progress; it does not decide which segments to check. The source is
`[ledger] source`, or `av-<installation_id>` by default.

`ledger spool ingest` runs only on a spool authority. It checks each file's
source name, filename identity, and CRC, then stores it in the archive with
origin `spool` and moves it to `processed/`. Failed files stay in `incoming/`
and make the command exit nonzero. When a writable daemon owns the archive,
the command asks its localhost-only ingest endpoint to do the work.

`ledger spool status` prints incoming and processed counts and the source's
cursor for each zone. It exits nonzero for unreadable directories or a corrupt
cursor.

The spool directory must support hard links, and the AgentsView data directory
must not be inside it. File-sync conflict copies (`*.sync-conflict-*`) stay in
the spool for an operator to inspect. A segment row that cannot be decoded
stops emit's listing for that run.

AgentsView uses its archive as the query index. Its spool messages differ from
jilog where the underlying configuration differs:

| jilog message refers to | AgentsView message refers to |
| --- | --- |
| Mirror zone (`spool = false`) | Spool disabled (`spool_path` unset) |
| `fleet_store_path` and its setup hint | `spool_authority` and its zone setting |
| Separate SQLite index refresh | Archive commit; no index refresh line |
| Fleet store path | This archive, or a producer host |
| Hostname source | Configured ledger source or installation ID |

## Import and export limits

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
