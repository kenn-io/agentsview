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

CLI artifact sync needs exclusive write access to the local archive. If a local
`agentsview` daemon is running, stop it first with `agentsview serve stop` and
retry. A running server still participates in a fleet as an
[HTTP peer](#http-peer) target for other machines.

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
It does not reuse the local server's `auth_token` for explicit peer URLs. HTTP
peer sync rejects redirects, so configure the final artifact API URL directly.
Credentials and artifact bodies are not forwarded to a redirect destination.

Non-loopback peers require HTTPS by default. Plain `http://` remains available
for `localhost`, `127.0.0.0/8`, and `::1`. To connect to a remote plaintext peer
on a trusted test network, LAN, or VPN, opt in explicitly:

```bash
agentsview sync http://desktop:8080 --token <peer-token> --allow-insecure
```

The override works for one-shot and `--watch` sync and logs a warning. It sends
the bearer token and full archive content without transport encryption, so use
HTTPS through a reverse proxy or VPN termination whenever possible. If you
expose a server beyond loopback, enable authentication and protect the token
like write access to the full archive.

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

Custom endpoints default to HTTPS when no scheme is given. Plain `http://` is
accepted only for loopback hosts or when
`AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT=true` is explicitly set. Use that
override only on a trusted test or private network because requests and artifact
content travel without TLS. Artifact sync rejects redirects. If a corrupt remote
object must be deleted so a later upload can heal the bucket, that deletion is
HTTPS-only, even when the insecure override permits other requests over HTTP.

## How It Works

Each install has a stable artifact origin ID. Locally owned sessions keep their
ordinary SQLite IDs and `machine='local'`. Foreign sessions are imported as
`<origin>~<native-session-id>` with `machine=<origin>`. Source, parent, and
subagent relationship IDs are rewritten the same way before import, so SQLite,
PostgreSQL, and DuckDB see the same ordinary session graph after projection.

The local artifact repository lives at `$AGENTSVIEW_DATA_DIR/artifacts`. It is a
private Docbank vault whose catalog, blobs, and packs are owned directly by the
running AgentsView process. Do not inspect, edit, copy, or synchronize files
inside this directory, and do not use it as a folder-sync target. A single
writable AgentsView daemon owns the vault; maintenance commands use that daemon
when it is available or acquire exclusive local ownership before opening the
vault directly.

Folder targets keep the external artifact protocol layout:

```text
<dedicated-target>/<origin>/
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

The canonical identity is always the SHA-256 and size of the uncompressed
logical bytes. Manifests and segments use zstd on the external wire, without
changing that identity. The local vault may also zstd-compress eligible loose
objects and later pack them; those physical changes are private storage details,
and every read still verifies and returns the exact canonical bytes.

Logical artifacts are immutable. Folder writes use no-replace semantics, and S3
writes use conditional create, so repeated syncs are idempotent set-union
operations. External names and extensions are independent of the Docbank layout
and remain stable when local objects move between loose and packed storage.

Normal sync publishes only sessions in SQLite's durable changed-session queue.
Transports report the exact artifacts they create or repair, and import works
from those references instead of rescanning checkpoint or metadata history.
Incomplete checkpoints and metadata whose session has not landed remain in a
bounded durable retry queue across restarts. Peer status includes that pending
work; a later dependency transfer or startup retry resumes it.

## Metadata And Deletes

A machine records metadata events only after it has an artifact origin, which it
gets by running `agentsview sync --init`, running any artifact sync, or
receiving a peer exchange. Until then curation stays local and publishes no
artifact metadata; the `--init` baseline snapshot publishes the accumulated
curation state when the machine later joins a fleet.

User curation converges through metadata events. Rename, trash/restore, star,
and pin changes are replayed deterministically with hybrid logical clocks. If
two peers edit the same metadata field close together, AgentsView records the
losing value in the local conflict log while still deriving one deterministic
current value. Conflicts and per-origin sync status are visible on the Peers
page in the UI, and a conflicted session shows a fork badge in its header.

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

A corrupt artifact — a hash-mismatched segment or manifest, an undecodable
compressed object, or an unparseable checkpoint or metadata event — is skipped
so one damaged object never aborts a sync or spreads to other machines. Local
corruption is quarantined inside the private Docbank vault; it is not a loose
file beside the live artifact. A folder target may rename its own damaged wire
file with a `.corrupt` suffix so a valid holder can replace it. Invalid changed
artifacts do not advance landed provenance; a later valid checkpoint or repaired
dependency retries the durable import. Metadata events whose timestamps do not
parse are rejected at write time and quarantined during import.

An object corrupt in place inside an S3 bucket is eligible for remote deletion
when a peer fetches it because it is missing locally and validation fails.
AgentsView attempts that deletion only over HTTPS; if it succeeds, a later push
from a valid holder can re-upload the object. A corrupt remote checkpoint found
while comparing a checkpoint already present locally is retained for its owner
to repair. Over permitted HTTP, corrupt remote objects are retained and skipped;
automatic deletion does not occur.

## Garbage Collection

`agentsview sync gc` performs conservative logical retention and one bounded
physical-maintenance pass in the local Docbank vault. It does not accept or
modify a folder, HTTP, or S3 target. Use `--dry-run` to preview logical
retention without trashing artifacts or running physical reclamation:

```bash
agentsview sync gc --dry-run
agentsview sync gc --grace 168h --quarantine-grace 168h
```

GC keeps the latest checkpoint for each origin and every manifest, segment, and
raw artifact reachable from it. Origins without checkpoints are skipped rather
than interpreted as deleted. Unreachable logical artifacts first enter Docbank
trash. Physical maintenance then empties eligible trash, removes unreferenced
blobs, and repacks live content as separate bounded stages. By default, each
stage processes at most 1,024 objects. Blob garbage collection and repacking
also use a 256 MiB soft byte budget; trash emptying is bounded by object count.
When work remains, the command prints an exact resume command with opaque stage
cursors; do not edit the cursors.

The writable daemon owns maintenance when it is running. If no daemon owns the
archive, the command opens the vault only after acquiring direct ownership. It
fails closed when a known owner is unreachable instead of risking concurrent
catalog or pack writes.

## Resetting A Failed Local Vault

`agentsview sync artifact-reset` is a fail-closed recovery operation for a local
artifact vault that Docbank cannot open or repair normally. It accepts no target
and never deletes or recreates the SQLite archive. The command verifies the
AgentsView ownership marker, moves the entire vault to a timestamped
`artifacts.reset-*` diagnostic path, creates a fresh Docbank vault, and
republishes this machine's artifact origin from authoritative SQLite rows.

The moved-aside vault is never deleted automatically. Preserve it for diagnosis
and remove it manually only when it is no longer needed; repeated resets can
consume substantial disk. Foreign relay artifacts cannot be reconstructed from
local SQLite publication state, so they remain unavailable in the fresh vault
until a trusted peer or target sends them again. Sessions already imported into
SQLite are unchanged. If a reset is interrupted after the move, AgentsView
retains durable republish state and retries local publication when the fresh
repository is recovered.

## Availability

Two intermittent machines only sync directly while both are online and can reach
the same target. A NAS folder, always-on home server, S3-compatible bucket,
cloud-synced folder, or always-running AgentsView peer can act as a rendezvous.

That rendezvous is a deployment convention, not a privileged architecture:
AgentsView still treats every participant as a peer and keeps the complete local
archive on each machine.
