---
title: Trusted-Fleet Artifact Sync
description: Sync AgentsView archives between trusted personal machines without copying the live SQLite database
---

# Trusted-Fleet Artifact Sync

Artifact sync exchanges immutable AgentsView artifacts between machines and
imports them into each machine's local SQLite archive. It is local-first: every
machine keeps its own complete database, and transports only move
content-addressed session artifacts plus metadata events.

Use it for a fully trusted personal fleet: your laptop, desktop, home server,
NAS, or object-store bucket. Do not treat artifact sync as a team-sharing
security boundary. A peer that can write to the shared artifact target can
publish sessions and metadata for the fleet.

## When To Use It

Artifact sync makes sense when you want multiple machines to converge on the
same session archive without running PostgreSQL as the coordination point.

It is a good fit for:

- laptop plus desktop archives
- NAS, Syncthing, Dropbox, or rclone-backed rendezvous folders
- S3-compatible buckets such as MinIO, Backblaze B2, or AWS S3
- trusted always-on AgentsView peers over HTTP

Use [PostgreSQL Sync](/pg-sync/) or [DuckDB Mirror](/duckdb/) when you want a
read-only aggregation or analytics mirror. Those backends are still mirrors:
SQLite remains the local write/archive database, and artifact sync projects
foreign artifacts into ordinary SQLite rows before they can be pushed onward.

## Quick Start

Use a dedicated artifact share folder:

```bash
agentsview sync --init /path/to/agentsview-artifacts
agentsview sync /path/to/agentsview-artifacts
```

Run `--init` once on each machine. It creates or adopts that machine's artifact
origin, backfills local sessions into the local artifact store, exchanges
artifacts with the target, and imports peer artifacts already present there.

To keep a machine exchanging artifacts while it is online, run watch mode:

```bash
agentsview sync --watch /path/to/agentsview-artifacts
```

Watch mode runs an initial local sync and artifact exchange, debounces local
session-file changes, retries failed exchanges on later changes or interval
ticks, and performs a final best-effort exchange on shutdown.

## Targets

### Folder

```bash
agentsview sync /path/to/agentsview-artifacts
```

The folder may live on a local disk, NAS mount, Syncthing folder, Dropbox
folder, NFS share, or rclone-mounted bucket. The folder must be dedicated to
artifact sync. Do not point artifact sync at:

- `AGENTSVIEW_DATA_DIR`
- the live SQLite database file or its WAL/SHM files
- a whole AgentsView data directory
- raw agent directories that contain live database files

Copying the live SQLite database or the whole data directory with a general
file-sync tool is unsafe. Artifact sync exists specifically to avoid that.

### HTTP Peer

An AgentsView server can expose artifact exchange routes behind the existing
Bearer-token auth middleware:

```bash
agentsview sync https://desktop:8080 --token <peer-token>
```

HTTP peer sync only sends an `Authorization` header when `--token` is provided.
It does not reuse the local server's `auth_token` for explicit peer URLs.

Keep HTTP peers on loopback, a trusted LAN, a VPN, or a reverse proxy you
operate. If you expose a server beyond loopback, enable authentication and
protect the token like write access to the full archive.

The HTTP client pulls every missing artifact from the peer and posts every local
artifact the peer is missing. Garbage collection of superseded artifacts on the
remote peer is the peer's own responsibility.

### S3-Compatible Object Storage

```bash
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
agentsview sync s3://my-bucket/agentsview
```

Credentials come from `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and optional
`AWS_SESSION_TOKEN`. Region resolves from `AGENTSVIEW_S3_REGION`, then
`AWS_REGION`, defaulting to `us-east-1`.

For MinIO, Backblaze B2, or another S3-compatible service, set
`AGENTSVIEW_S3_ENDPOINT`. A custom endpoint automatically uses path-style
addressing; `AGENTSVIEW_S3_PATH_STYLE=true` forces it otherwise.

## How It Works

Each install has a stable artifact origin ID. Locally owned sessions keep their
ordinary SQLite IDs and `machine='local'`. Foreign sessions are imported as
`<origin>~<native-session-id>` with `machine=<origin>`. Source, parent, and
subagent relationship IDs are rewritten the same way before import, so SQLite,
PostgreSQL, and DuckDB see the same ordinary session graph after projection.

The artifact store lives beside, not inside, the database:

```text
$AGENTSVIEW_DATA_DIR/artifacts/<origin>/
  checkpoints/cp-<seq>.json
  manifests/<hash>.json.zst
  segments/<hash>.ndjson.zst
  meta/<hlc>-<hash>.json
  raw/<hash>
```

Artifact kinds:

- **checkpoints** list the current manifest hash for each session published by
  an origin
- **manifests** hold the canonical session header, usage events, and segment
  references
- **segments** hold canonical message NDJSON
- **metadata events** record user edits such as rename, trash/restore, star,
  pin/unpin, and delete-everywhere
- **raw artifacts** are optional source snapshots when a parser can provide a
  safe regular-file snapshot

All artifact files are immutable. Folder writes use no-replace semantics, and S3
writes use conditional create, so repeated syncs are idempotent set-union
operations.

## Metadata And Deletes

User curation converges through metadata events. Rename, trash/restore, star,
and pin changes are replayed deterministically with hybrid logical clocks. If
two peers edit the same metadata field close together, AgentsView records the
losing value in the local conflict log while still deriving one deterministic
current value.

Emptying local trash is local-only. Fleet-wide permanent delete is explicit:
delete-everywhere writes a purge event and an exclusion tombstone so peers do
not resurrect the session from older artifacts.

Checkpoint absence is never a deletion signal. A missing artifact, truncated
checkpoint, offline peer, or old target cannot remove local data.

## Version And Failure Handling

Artifact readers ignore unknown JSON fields. Unknown future metadata operations
are marked applied and skipped. Artifacts with a future format version are
deferred, not treated as successful imports, so older AgentsView versions keep
syncing the artifact kinds and versions they understand.

Manifests that reference missing segments are also deferred. Import watermarks
advance only after all referenced content is hash-verified and applied.

## Garbage Collection

Superseded immutable artifacts can accumulate. Run conservative garbage
collection against a folder target after peers have had time to catch up:

```bash
agentsview sync gc --dry-run /path/to/agentsview-artifacts
agentsview sync gc --grace 168h /path/to/agentsview-artifacts
```

GC keeps the latest checkpoint for each origin and every manifest, segment, and
raw artifact reachable from it. Origins without checkpoints are skipped rather
than interpreted as deleted.

## Availability

Two intermittent machines only sync directly while both are online and can reach
the same target. A NAS folder, always-on home server, S3-compatible bucket,
cloud-synced folder, or always-running AgentsView peer can act as a rendezvous.

That rendezvous is a deployment convention, not a privileged architecture:
AgentsView still treats every participant as a peer and keeps the complete local
archive on each machine.
