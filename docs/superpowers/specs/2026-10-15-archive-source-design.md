# Use a compressed archive as a session source

Status: implemented in the working branch, with a repository-only rehearsal on
real data. The [implementation record](../plans/2026-10-16-archive-source.md)
lists completed work and remaining release checks. A tagged Docbank release
containing its snapshot reader is still required before merging. This design was
revised after reviews of `3acf86f0`, `d1c0fd79`, and `c8c7e53a`.

## What users get

Use a Docbank-backed session archive as a source in an ordinary AgentsView
installation. Sync reads its archived Claude and Codex sessions alongside local
sessions. Both appear in the same application, attributed to their original
machines. This lets users retire a machine and keep using its history.

The backup reader uses existing AgentsView snapshots without restoring a vault.
The directory reader below handles archives that already have a Docbank vault.
The original files stay compressed. Users do not need to restore a vault, keep
an extracted transcript tree, or run a separate archive server. Parsing uses
temporary files for one source at a time.

The delivery also accepts an existing archive directory through `directory`.
This includes an original AgentsView archive, an AgentsView restore, or a
Docbank restore containing `application/sessions.db`. It uses the same accepted
source records, machine identities, parser and receiving database. Directory
reading holds the source application writer lock and embedded Docbank owner for
one pass; the source must be stopped and writable. The application database is
opened read-only, without migrations. Docbank uses its normal catalog open and
maintenance path. A compact acceptance catalog is rebuilt without copying
transcripts; its digest identifies the selected generation. Path changes do not
change ownership. A changed source manifest still conflicts with an existing
binding. Rebuild preparation pins the selected catalog digest and refuses a
directory changed before the rebuild starts.

Optional `machine_name` overrides the display name of a single-machine archive.
Labels change independently of identity and completion, without a reparse.
Multi-machine archives keep their captured names and reject this override. Users
do not configure installation IDs.

Configuration, followed by the existing `agentsview sync` command:

```toml
[[archive_sources]]
name = "retired-desktop"
repository = "/mnt/archives/desktop/backup-repo"
snapshot = "<exact snapshot ID from the backup report>"
```

The name is a configuration label, not a machine identity. Repository paths may
point to a local disk or a mounted network share. Restart a running daemon after
editing this configuration. The first sync reports supported sources, suppressed
sessions, unsupported originals, conflicts, and failures. Later unchanged syncs
skip the archive's transcripts.

Browsing uses the receiving installation's normal SQLite database. That parsed
data takes local space even though the originals remain compressed.
Disconnecting the drive leaves indexed history browsable; reparsing needs the
archive again.

Two limits affect what appears. History already received through artifact or
HTTP/SSH sync can appear again under the archive's machine identity; this first
version does not deduplicate across those routes. Sessions deleted in the saved
archive after capture can also reappear: only the deletion policy frozen at
capture and deletions in the receiving installation are honored.

A full parser rebuild rereads available archive sources. The old history stays
browsable, but writes receive a retryable HTTP 503 until the rebuild finishes;
large attached archives extend that window. If a backup starts partway through,
the rebuild waits and resumes without losing completed work. That wait also
extends the write pause and can be cancelled. Ordinary sync gives local work
priority between archived sources, so it does not wait for the whole backlog.

## First delivery and its limits

- Read existing AgentsView backup snapshots, including chunked content and
  chunked application extras. Do not require recapture or rewrite a backup.
- Parse accepted Claude and Codex source manifests. Other provider files kept as
  supplemental originals remain recoverable through existing archive tools;
  attaching them does not yet make them browsable.
- Pin an exact snapshot. Do not follow the newest snapshot, advance conflicting
  captures, merge publisher identities, or add continuous capture.
- Use the receiver's content and image policy. Preserve its curation and
  deletions. Honor deletions recorded for the source installation. Do not
  merge the source database's stars, pins, renames, or other curation in this
  slice.
- Read backup repositories and stopped archive directories. Concurrent access to
  a running vault, direct object-storage access, and a source-management UI
  are outside this delivery.

The reviewed design permits one bootstrap extraction: build the source index
from the snapshot's application database.

## Bootstrap without restoring a vault

Today's backups put the accepted-source index inside `application/sessions.db`.
Docbank also retains manifests that were *not* accepted, so listing every stored
manifest cannot reconstruct that index.

First implementation:

1. Open the selected snapshot and verify its identity and reader requirement.
1. Copy only `application/sessions.db` to private temporary storage, verifying
   its full hash and length before SQLite opens it. Inspect it read-only, with
   no application migrations or sidecar writes. Check the source schema as
   defined below; reject an unsupported input without changing either
   database.
1. Build a compact index of the archive ID, original installations and roots,
   accepted manifests, suppression rules, and identity bindings. Include
   coverage counts without copying messages or the full file inventory. The
   suppression policy is the one frozen with each imported device's capture;
   later curation in the saved archive is not merged by this delivery.
1. Validate canonical manifests against the original archive and device IDs.
   Validate every referenced logical object's membership and size against the
   selected snapshot. Publish the index only after all checks succeed.
1. Remove the temporary database. Reuse the index on subsequent syncs, keyed by
   repository UUID, snapshot ID, and index format version. Rebuild it if
   absent or incompatible; never replace the receiving database with the saved
   one.

This requires temporary space for the saved database once per index build. It
does not restore the Docbank catalog or the original session files. The
bootstrap must report its byte requirement before copying, stream with bounded
memory, report progress, and clean up on failure or cancellation. A process
crash may leave temporary files; the next run removes its abandoned work before
starting another copy.

Supported input means recovery inventory version 1 and an archive-only database
with the recorded archive ID and all six `raw_archive_*` tables in
[schema.sql](../../../internal/db/schema.sql). Require the named columns and
types in this baseline for roots, files, sources, devices, root aliases, and
session bindings; select columns explicitly and allow additional columns.
Validate record identities, canonical manifests, suppression records, and
references before publishing the index. Missing tables or required columns,
incompatible types, or inconsistent records are errors, not empty history.

Use the `mode=ro&immutable=1` opening pattern in
[capture preflight](../../../internal/rawarchive/capture_preflight.go). Decode
`PRAGMA user_version` with `db.DecodeUserVersion`: the archive-only marker is
`1 << 20`, added to the parser version. Require the marker and the
`archive_only` record in `archive_metadata` to agree. The decoded parser version
is not a schema revision or a reason to migrate the saved database. These
capability checks define the supported schema; a newer recovery inventory
requires an explicit reader update.

Alternatives considered:

| Approach                                            | Consequence                                                                                                                                                |
| --------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Temporary application database, then compact index  | Selected approach. Reads existing backups with the smallest change; needs temporary database-sized disk space.                                             |
| SQLite reads directly from compressed backup chunks | Avoids the full temporary database, but needs a random-access reader and SQLite filesystem integration. Defer unless zero database extraction is required. |
| Restore a Docbank vault first                       | Supported when the user already has a restored archive; not required for reading backups.                                                                  |

A separate index in future backups could reduce bootstrap cost. It is not a
prerequisite and is not another format or fallback in this delivery.

## Read individual objects through Docbank

Docbank needs a public snapshot reader. AgentsView must not open Kit pack files
or depend on their extensions and directory layout. The proposed reader:

- Opens one exact snapshot and exposes repository ID, snapshot ID, and minimum
  reader version.
- Provides metadata and verified copies of a named extra or a content object
  identified by its logical hash. Membership comes from that snapshot's extras
  tree and attachment lists, not merely from a repository-wide blob lookup.
- Reuses Kit's plain-object and chunk-recipe readers. Checks chunk integrity,
  whole-object hash, and length. A copy succeeds only after verification
  reaches EOF; cancellation, a short read, or a writer error fails the copy.
- Acquires Kit's shared repository lease before reading manifests and indexes,
  holds it for one source's materialization, and releases it before parsing.
  Bootstrap uses a separate lease to copy the application database and
  snapshot metadata, released before SQLite index construction. Never hold a
  lease for an entire sync pass or rebuild, and never force a lock.

Open and close a reader within each lease window. Load pack, recipe, and
membership indexes once per reader, not once per object. Revalidate the pinned
repository/snapshot and reload pack placement on each new window: prune may have
moved objects between windows. Keep no pack descriptors across the gap. Keep a
cache of open pack readers and verified footer entries for that reader's
lifetime, following Kit verification's existing pattern and limit of 64 open
packs. Reuse them for neighboring objects and chunks. Close readers on eviction
and close; do not evict one while its object stream is active. Keep the current
index-versus-footer and per-object integrity checks. `Repo.OpenBlob` currently
reopens the pack and parses its footer for every object, so this reuse needs
work in Kit. Cache metadata and descriptors, not decoded payloads.

Kit already exports `Open`, `LoadManifest`, `LoadBlobIndex`, and `OpenBlob` for
plain objects. Chunked logical reads are implemented privately by `openObject`,
`loadObjectRecipes`, and `recipeStream` in `backup/object_recipe.go`. Expose
that existing chunk-aware path through the snapshot reader, with pack reader
reuse and snapshot membership checks; do not invent another chunk format. For
backup sources, Docbank wraps the reader. Calling `Restore` or opening a second
live vault does not implement this contract. Index construction must not decode
unrequested content or application extras.

The repository's archive payload is unchanged. Existing leases write heartbeat
files, so the first version requires permission to write those files, including
on a network share. A physically read-only mount is not supported by this
design. A configured source is also not a retention pin: operators must retain
the selected snapshot.

A concurrent backup holds an exclusive lease.
`docbank.ErrBackupRepositoryLocked` means temporarily unavailable. Ordinary sync
retains history and pending work, lets local sync continue, and tries on the
next normal sync. An active full rebuild instead waits as described below. Do
not call contention corruption or force access. There is no tight retry loop or
new background polling service.

Conversely, a backup waits up to 60 seconds for active readers. Releasing each
source's lease lets a waiting backup enter while that source is parsed; a new
source reader must yield to its exclusive lease. One large source or the
bootstrap copy can still exceed that wait. The backup then returns the same
retryable locked error; retry it after the current materialization finishes.

Leases protect reads from ordinary maintenance. An operator's forced prune can
remove a live reader's lease without notifying it. A resulting missing pack,
short read, or integrity failure aborts the source before publication and leaves
it pending for a fresh-reader retry. Report the actual error; it does not prove
that forced prune caused it. A full rebuild discards its replacement database on
such a mid-read failure. If the snapshot was pruned, retry continues to report
it missing and never switches snapshots.

## Parse with bounded temporary space

Adapt the snapshot reader to the existing `rawderive.ObjectStore.CopyObject`
contract. Use `Materializer` to verify and reconstruct one accepted source and
its companion files, then `PrepareLocalSource` and the normal sync engine to
parse it. Close the snapshot reader and release its lease after all files
verify, before parsing. Clean that source's files before starting the next
source.

This is temporary per-source extraction, not zero decompression. It preserves
the existing file-based provider parsers, fork resolution, large Codex staging,
secret scanning, content policy, asset handling, and export-index writes.
Rewriting every parser to take streams is unnecessary for the first delivery.

Use the existing archive reparse default of 16 GiB per materialized source,
counting companions and fork dependencies as well as the primary transcript.
Report a source that exceeds it; never silently truncate or fall back to
whole-archive extraction. The disk estimate must also include parser staging,
newly indexed SQLite data, and any full-rebuild database copy. A materialization
limit alone is not a bound on total disk use. Copying backup objects must stream
rather than buffer whole payloads in memory.

The first sync must read all selected supported sources. Shared companions may
be read again for different sources; do not claim each blob is decoded only
once. A cache of decoded transcripts or companions is not part of this slice.

## Identity, writes, and failures

Keep original installation IDs as machine keys. Readable machine labels come
from the captured roots. All attached sessions use the existing foreign-ID rule,
`installation~parser-id`, including fork parents. Repository paths,
configuration names, and the receiver's identity never become source identity.
The receiver keeps its own telemetry ID, credentials, settings, and live roots.
Store stable `archive://` source paths derived from original root and source
identity, using the existing path rewriter; never persist temporary paths.

Keep attachment registrations and session provenance distinct from the
`raw_archive_*` custody tables. An attachment references objects elsewhere; it
does not mean the receiving installation owns a local raw vault. Durable state
must record the pinned repository/snapshot, original source key and manifest,
resulting session IDs, and the parser/policy version last completed. The compact
index is replaceable; session ownership and completion records are not.

Repeated attachment of the same snapshot, including a copied repository at a
different path, must reuse existing identities. Refuse a source whose device ID
is the receiver's installation ID, is in `ArtifactLocalMachines`, or resolves
through the receiver's `machine_alias:` records to either. This includes
`artifact_local_installation_id` and retained local publication authority. Check
without adopting aliases or changing ownership, at attachment and before session
publication; do not overlay live files. Refuse a different accepted manifest for
an already bound source; keep existing rows and report the conflict. Switching
snapshots does not imply permission to replace history.

Check the exact resulting session ID and its recorded owner before writes.
Refuse a collision with another source, including an artifact or remote import.
Archive identity is the original installation ID; artifact identity is a
publisher origin; remote identity is a configured host name. This delivery
defines no equivalence between those namespaces. Matching native parser IDs or
display names is not evidence of common ownership.

Consequently, copies received under different namespaces can coexist. State this
limitation in setup guidance and the first attachment report; do not claim to
detect an overlap or report a duplicate count. Do not extract or infer
cross-route aliases from display labels. Artifact-origin evidence cannot be a
general solution for these backups: current capture preflight refuses it.
Cross-route reconciliation needs a separate explicit identity-binding design.

Use the ordinary sync writer and its staged large-session path. Add provenance
to the same session write transaction, so interruption cannot leave an imported
session looking locally owned. This needs a new transaction-level write hook for
both ordinary batches and staged Codex writes. `ArchiveSessionPolicy` runs
before publication and is not that hook. Mark a source complete only after all
its sessions are written. Normal incremental sync can retain completed sessions
if a later session fails; retry must be idempotent. Do not claim batch-wide
atomicity or copy the whole receiving database for every source.

All source bytes must verify before parsing starts. A missing or corrupt object
leaves that source's previous content unchanged and records a retryable failure.
Receiver deletions must not reappear on retry or reparse. Source suppression is
applied to native parser IDs before namespacing; receiver suppression is applied
to resulting IDs. Preserve existing message IDs and curation through the normal
reparse rules. Report cleanup errors rather than silently accumulating scratch.

## Participate in ordinary sync and rebuilds

Wire attachments through daemon sync, direct CLI sync, startup sync, and the
separate full-resync worker. `sync --host` remains scoped to the named remote
host. A local file watcher event must not enumerate every archived source.

An ordinary sync pass finishes local sync first, then drains pending archive
work in batches of one source. One drainer owns archive work for the receiving
database; concurrent periodic and requested syncs join that drain rather than
selecting the same pending sources. Its ownership is separate from
`Engine.syncMu`, which must not be held across the archive pass.

Before each archive source, perform an explicit handoff to the watcher's
dispatch loop. With `syncMu` released, wait for any dispatched callback and
process a snapshot of fresh pending local work through the existing callback
before acknowledging the handoff. Preserve retry backoff and error handling;
events arriving after that snapshot wait for the next boundary. This new handoff
must coordinate with normal watcher dispatch so each batch has one owner. Merely
unlocking and relocking `syncMu` does not establish priority. At startup, finish
local reconciliation and open watcher dispatch before starting the archive
drain. Without a watcher, there is no watcher handoff.

These rules apply to initial, periodic, scoped-upgrade, and requested
incremental sync. Local ingestion may wait for one source's entire copy and
parse. The 16 GiB limit bounds temporary space, not elapsed time; processing
remains cancellable. Reuse the current sync scheduling; do not add a separate
archive polling service.

For an already indexed snapshot, freshness depends on the pinned snapshot and
the relevant parser/policy version. An unchanged sync checks registration state
and availability once per attachment, without scanning manifests or hashing
transcripts. Startup does not reparse a current attachment just because it is
configured. Parser upgrades follow these explicit rules:

- A version bump requiring full rebuild includes every available attachment in
  the startup rebuild worker. This rereads all its accepted sources and
  reports progress. Explicit full resync has the same behavior. Attachments
  unavailable or busy before their contributor starts are carried forward
  unchanged.
- A scoped upgrade uses `parser.ProviderStoredAgents` and `db.AgentDataVersion`
  to find the versions relevant to each source provider. Reparse only sources
  whose required version changed. Policy changes likewise invalidate the
  affected source completion records.
- Keep a durable completion fingerprint per source: snapshot/manifest, relevant
  agent versions, and receiver policy. Advance it only after successful source
  processing, using the versions and policy captured for that attempt. Keep a
  per-attachment pending summary with its target versions and policy so an
  unchanged sync need not scan all sources. A changed target invalidates a
  previously empty pending summary.
- Copy those records and the pending summary through every rebuild. Carrying
  orphaned rows forward does not advance their completion fingerprints.
  Missing, failed, cancelled, or deferred sources remain pending. A completed
  source in a published incremental pass need not repeat work on retry. During
  a full rebuild, write completion into the replacement database only; a
  failed swap publishes neither content nor completion, so retry may repeat
  that work.
- `MarkDataCurrent` and clearing `scoped_upgrade_pending` keep their existing
  database-wide meaning. Neither clears attachment pending state. Every
  startup reconciliation and normal sync checks that separate state even if
  `NeedsResync` is false and `scoped_upgrade_pending` is absent. Do not force
  a database-wide rebuild merely because an attachment is pending. A later
  normal sync retries it when available; watcher events do not trigger that
  sweep.

This per-source state and its scheduler integration are new work. An archive
remains visibly pending until every selected source is current or intentionally
suppressed, even when the database's global upgrade markers have cleared.

Use the unified rebuild coordinator. Its current `RebuildContributor` expects
filesystem engine configurations; add the narrow execution hook needed to run
bounded archive batches against that same replacement database. Do not invoke
`rawarchive.Reparse`, which owns a separate whole-database copy and swap.

If the repository is unavailable, ordinary local sync continues and reports the
archive unavailable. Full resync carries forward its existing sessions,
provenance, and curation and reports deferred reparsing. A failed available
contributor must not publish a partial full rebuild. Lease contention before a
contributor starts permits carry-forward. Once it starts, contention between
sources pauses the same replacement database instead of discarding completed
work. Retry only `docbank.ErrBackupRepositoryLocked`, with capped backoff bound
to the rebuild context and no elapsed-time cutoff. Report the waiting
attachment, completed-source count, and elapsed wait. Hold no repository lease,
open pack reader, or SQLite transaction while waiting. Reacquisition revalidates
the pinned snapshot and reloads pack placement as for every source.

Cancellation ends the wait and discards the unpublished replacement; other
errors, including a missing snapshot or failed verification, still abort the
rebuild. The old database remains readable, and writes stay blocked for the
whole rebuild, including waits. Removing the configuration also leaves indexed
history in place; it does not authorize deletion. Preserve detached history and
provenance on subsequent rebuilds.

Mount-path changes are allowed only after confirming the same repository UUID
and snapshot. A replaced repository, missing snapshot, newer reader requirement,
or failed verification is an explicit error. Never select another snapshot or an
ambient provider directory as a substitute.

## How this relates to other sync features

| Feature                              | Purpose                                                                        | Relationship to an attached archive                                                                                                      |
| ------------------------------------ | ------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------- |
| Local sync                           | Follow this installation's agent files.                                        | Runs alongside archive sources; keeps its own identity.                                                                                  |
| HTTP/SSH remote sync                 | Fetch files from an active remote machine into a local mirror.                 | Shares sync scheduling and rebuilds. Its tar-and-mirror transport is not used to read a compressed backup.                               |
| Artifact folder sync                 | Exchange parsed sessions and curation between publishers.                      | Existing artifact ownership stays separate. Attaching originals does not adopt the old publisher or grant republishing authority.        |
| Hosted raw sync                      | Upload originals and authenticated source generations for server-side parsing. | Reuses manifest validation and materialization. An offline snapshot is not a hosted receipt, tenant, device credential, or upload queue. |
| Archive capture, backup, and restore | Preserve originals and recover a complete archive-only installation.           | Remains the recovery workflow. Attaching is an additional way to read the backup from a normal installation.                             |
| Attached archive source              | Index a fixed saved snapshot alongside live sessions.                          | Reads originals as needed and stores parsed results locally.                                                                             |

Do not add attached roots to raw-sync watch/backfill or artifact export.
Existing guards against exporting foreign sessions must still hold. Database
mirrors are a different operation: this feature does not disable a user's
configured mirror of parsed data. Exercise those paths to ensure original
machine attribution and receiving content policy survive. Archive-only mode
remains enforced for the existing custody commands; do not relax it to implement
this feature.

## Evidence and implementation boundaries

Design review baseline: AgentsView `ba819a4e`, Docbank v0.15.2, Kit v0.34.1.
Docbank main `c71cab86` and Kit main `4609e13f` were also inspected for the
reader boundary.

| Existing code                                                                                                             | What it supplies; what remains new                                                                                                                       |
| ------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [Archive backup](../../../internal/rawarchive/recovery.go)                                                                | Saves application SQLite as an extra. Compact-index bootstrap is new.                                                                                    |
| [Archive source records](../../../internal/db/raw_archive.go)                                                             | Original roots and accepted manifests. Ordinary sync attachment provenance is new.                                                                       |
| [Materializer](../../../internal/rawderive/materializer.go) and [local preparation](../../../internal/rawderive/local.go) | Verified per-source files and normal parser configuration. A backup reader adapter is new.                                                               |
| [Archive reparse](../../../internal/rawarchive/reparse.go)                                                                | Reference for identity, suppression, and policy. Its independent database swap is not reused.                                                            |
| [Sync preparation and writes](../../../internal/sync/engine.go)                                                           | `ArchiveSessionPolicy` runs before writes. A provenance hook inside ordinary and staged write transactions is new.                                       |
| [Parser version scopes](../../../internal/db/data_version_scope.go)                                                       | Provider-specific version requirements exist. Durable attachment completion, pending summaries, and retry scheduling are new.                            |
| [Watcher dispatch](../../../internal/sync/watcher.go)                                                                     | Bounded pending batches, callbacks, and retry handling exist. An acknowledged handoff and single archive drainer are new.                                |
| [Rebuild contributors](../../../internal/sync/rebuild_contributor.go)                                                     | Shared replacement database and unavailable-source handling. Archive execution and durable state carry-forward are new.                                  |
| [Remote transport](../../../internal/remotesync/source.go)                                                                | Fetches tar streams. It is not an object-reader API.                                                                                                     |
| Docbank `backup.go`; Kit `backup/object_recipe.go`, `stream.go`, `lock.go`, `verify.go`                                   | Plain `BlobStream` and private verified chunk streams exist. Snapshot-scoped reads, chunk-aware API exposure, and pack-reader cache integration are new. |

Kit's reader is released in v0.37.0. Docbank's wrapper is merged in #988; its
release is pending. AgentsView consumes tagged releases. Its changes reach
config validation, snapshot indexing, session write/provenance storage, all sync
entry points, rebuild carry-forward, source status reporting, and the user
guides. Do not land a dependency on an untagged commit.

## Acceptance before calling this usable

Use production backup creation and readers, not mocks of the new API:

1. Create a backup with small and chunked originals and a chunked application
   database. Make the original capture and restored vault inaccessible, then
   attach the backup. No restore call, restored vault, or persistent
   transcript tree may be needed. Accept the archive-only version marker and
   required schema; reject an ordinary database, missing columns, or a newer
   recovery inventory without creating sidecars or running migrations.
1. Sync into an isolated ordinary installation containing local sessions. Verify
   transcripts, original machines, forks across roots, same parser IDs on
   different installations, suppressions, content policy, assets, and
   ownership.
1. Repeat sync, restart, move a repository copy, and force a full rebuild. Check
   stable session/message IDs and curation. With increasing source counts,
   unchanged sync and local watcher work must perform no transcript reads.
   Queue a local change between archive batches during a scoped upgrade and
   prove it is ingested before the next archive source begins. Use observable
   ordering, not a throughput deadline; include a backlog larger than one
   batch. Overlap periodic and requested drains and verify each pending source
   is processed once. Exercise handoffs with an in-flight watcher callback,
   new events, and a retained retry without duplicating or losing batches.
1. Exercise a full parser-version bump with an available archive, then with the
   archive offline. Also exercise a provider-scoped bump and a failed source.
   Offline sessions must remain browsable with their existing curation. After
   the global markers clear, restart, reconnect, and run ordinary sync.
   Pending sources must reparse and clear their own markers only on success;
   unaffected sources and successful incremental completions must not be read
   again. Abort a full rebuild before its swap and verify that neither its
   content nor its completion records became visible.
1. Exercise artifact and remote imports in both orders. Different namespaces
   remain separate with the documented duplicate-history notice; exact-ID
   ownership conflicts leave existing rows unchanged. Matching native IDs
   alone must not merge two installations. Reject the receiver's current and
   aliased identities and retained artifact publication authority. Confirm
   attached sources cannot become local raw-sync or artifact publishers.
1. Distinguish capture-time deletions, later deletions in the saved archive, and
   receiver deletions. Verify the documented first-delivery behavior.
1. Inject corrupt chunks, wrong lengths, missing snapshot membership, cancelled
   copies, parse failures, and lease contention with prune. Check prior
   content, ownership, completion markers, and retry behavior. Track temporary
   disk and memory across bootstrap, parsing, and full rebuild.
1. Hold a real backup's exclusive lease while syncing the pinned snapshot. Local
   sync must finish and the attachment remain pending, without a corruption
   report. Release the lease and retry. Exercise a forced prune removing
   needed data during a read; verification must prevent publication, preserve
   pending work, and report the read failure. Repeated object reads within the
   pack-cache bound must reuse readers and parsed footers, while eviction and
   close release descriptors. Also start a backup during a source read, pause
   before parsing, and verify that the backup can acquire its lease once
   copying ends, before parsing is allowed to finish. Cover a backup lock
   timeout and retry, and changed pack placement between windows. During a
   full rebuild, complete one source, hold the backup lease before the next,
   and verify waiting progress. Release it and prove the same replacement
   resumes without rereading completed sources. Cancel a separate wait and
   verify prompt exit with the live database unchanged. A missing snapshot
   after reacquisition must fail rather than wait indefinitely.
1. Repeat the repository-only test on the preserved real archive. Report
   coverage by provider, unsupported originals, machine attribution,
   transcript fidelity, peak disk use, and first/unchanged sync time. Keep
   private captures and evidence outside the repository. Do not change the
   only recovery copy for this test.

The design reviews covered the temporary database decision, identities, leases
and scheduling. The rehearsal must pass before recommending this as the way to
browse a retired machine's history from another installation.
