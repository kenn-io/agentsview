# Source Stewardship and Artifact Decomposition Design

## Status

Approved design for long-term stewardship of provider-owned session sources and
for separating that work from the normalized artifact-sync project.

This design supersedes the artifact umbrella's planned use of the existing
single-file `raw_source` field for JSONL capture. Before implementation
planning, the ignored umbrella working document must be updated with the
old-to-new scope mapping in this design.

## Problem

AgentsView currently depends on provider-owned source files for full resync.
Keeping every historical JSONL file indefinitely can consume tens of gigabytes,
but removing those files loses both provider-native history and the source from
which AgentsView can rebuild its normalized archive.

The artifact-sync project solves a related but different problem. It exchanges
normalized session manifests, message segments, checkpoints, and curation
metadata between AgentsView instances. It should not become the sole archival
authority for exact provider bytes:

- normalized artifacts and provider sources have different lifecycles;
- one provider parse unit may produce several sessions, and one session may
  depend on several provider files;
- provider sources must remain recoverable while an external archive is
  unavailable;
- retaining every source revision forever would bloat both local and external
  stores; and
- deleting provider files by default would surprise users and break
  provider-native resume and history workflows.

The design therefore introduces source stewardship as a second ledger. It shares
proven storage and verification mechanisms with artifact sync, but not
publication authority, queues, or retention policy.

## Goals

- Capture exact provider source bytes before they become unavailable.
- Preserve enough parse-relevant layout to rebuild the normalized database
  without restoring files into provider-owned directories.
- Keep capture, watcher, and reconciliation work bounded by the changed source
  batch rather than total archive size.
- Avoid parsing or inserting the same source revision twice when it exists in
  both provider and archive storage.
- Support a mounted-folder archive first and a service-backed archive later.
- Make provider-file eviction explicit, opt-in, receipt-gated, reversible until
  final removal, and unavailable until archive-backed resync is proven.
- Bound historical storage with acknowledgement, grace periods, verification,
  and completeness-gated garbage collection.
- Keep the normalized artifact project moving independently.

## Non-goals

- Governing organization-wide authorization, tenancy, visibility, or retention
  policy for a service-managed archive.
- Making a mounted SMB or NFS share prove device-level power-loss durability.
- Restoring archived files into provider-owned directories in the first version.
- Stewarding live SQLite databases or arbitrary provider directory stores
  without provider-specific snapshot support.
- Making provider-native resume or history work after an opted-in source has
  been evicted.
- Widening or reinterpreting the frozen artifact `raw_source` field.
- Globally deduplicating source chunks across origins or unrelated sources in
  the first version.

## 1. Authority Model

### Two ledgers

AgentsView maintains two independent data planes:

1. The **derived artifact ledger** owns normalized session manifests, message
   segments, checkpoints, metadata events, and peer convergence.
1. The **source stewardship ledger** owns exact provider parse units, immutable
   source revisions, archive receipts, provider-presence state, and retention
   decisions.

Each data plane has a separate Docbank vault root and lifecycle. A derived
artifact vault never becomes the implicit durable copy of stewarded provider
bytes, and a stewardship vault never publishes normalized artifact authority.

The two planes may share origin identifiers, SHA-256 identities, bounded
streaming helpers, canonical JSON code, verification patterns, and fail-closed
probing. They do not share queues, heads, acknowledgement, or garbage-collection
roots.

### Durable copies

The local stewardship vault is the durable capture ledger. This closes the
offline-rewrite hole that would exist if provider files were copied only to an
external target: a rewrite or truncation observed while the target is
unavailable can still create a new revision without destroying the prior
recoverable bytes.

The designated external archive is the off-machine copy required before
provider-file eviction. A target receipt acknowledges one exact source revision.
It does not automatically authorize deletion of other revisions, prove that a
mounted NAS survived a power loss, or transfer to another archive namespace.

Provider files remain provider-owned. AgentsView does not remove them unless the
user explicitly enables and runs eviction for an eligible source.

### Recovery authority

The stewardship vault and `stewardship.db` are authoritative for captured source
state. Provenance links stored in the normalized AgentsView database are derived
projections. Full resync may rebuild those links from stewardship state, but it
must not rebuild, replace, or discard the stewardship ledger.

Crash reconciliation runs in both directions:

- a manifest committed to the vault but missing from `stewardship.db` is a
  recoverable committed revision and is indexed;
- a ledger row without its expected manifest is failed closed and repaired or
  reported;
- chunks written before manifest publication are uncommitted orphans and become
  janitor candidates after a grace period; and
- normalized provenance missing from the main database is regenerated from the
  stewardship ledger during resync.

## 2. Source Model and Capture

### Provider capability and parse units

Stewardship is an explicit provider capability and defaults to unsupported. The
initial scope is providers whose session sources can be captured as regular
files with append-prefix semantics.

A **parse unit** is the complete provider-declared set of files needed to
reparse one source. It may be a single JSONL file or a set containing a primary
file and sidecars. Its membership is transitively closed over everything the
parser reads:

- if two units read each other's files, they are evicted together; or
- the provider declares them non-evictable.

The source manifest preserves every member's exact provider-relative,
parse-relevant layout. Sanitized paths are a separate display or sharing overlay
and never replace the stored parse coordinate. Archive-backed reparse
materializes the complete unit under a safe AgentsView temporary root; it never
writes into a provider directory. Capture rejects a coordinate that cannot be
represented safely beneath that root instead of rewriting it into a different
parse layout.

Database-backed providers and unsupported multi-file directory stores remain
outside stewardship until they define a sound provider-specific snapshot
mechanism. They continue to sync from provider storage and are never eligible
for eviction.

### Source identity

Every source has an origin-scoped stable `source_id`.

- A provider-native durable identity is preferred when available.
- Otherwise the exact provider-relative parse coordinate supplies identity.
- At a known path, prefix-continuity failure creates a rewrite revision of the
  same source. Filename reuse is therefore one revision history. This may
  conflate logically distinct uses of a recycled path, but it loses no bytes.

A source revision records:

- source ID and monotonic local revision;
- provider and parse-unit format version;
- exact member layout;
- reconstructed size and SHA-256 identity for each member;
- ordered chunk hashes, sizes, and fixed offsets;
- capture time and stability evidence; and
- continuity with the preceding revision.

Session-to-source provenance is many-to-many. One source unit can produce
several normalized sessions, and one session can depend on several source
members.

### Docbank mapping and chunk liveness

Docbank v0.11 stores whole documents, not chunks. Stewardship builds a logical
chunk layer:

- each immutable chunk is a Docbank `Create` at a source-local content-addressed
  path such as `sources/<source_id>/chunks/<sha256>`;
- each source revision manifest is a versioned source node written with `Put`
  and an expected prior identity; and
- chunk boundaries use large fixed offsets, initially in the single-digit MiB
  range, to balance append reuse against catalog-row and filesystem-object
  overhead.

Fixed-offset boundaries mean an append reuses every completed chunk and replaces
only the former tail. A rewrite produces a new source revision and may reuse any
chunks with matching identities.

Docbank does not understand manifest-to-chunk references. Committed manifests
and retention state define liveness. `stewardship.db` maintains a rebuildable
reference index and janitor progress. A grace-period mark-and-sweep removes:

- chunks abandoned before manifest commit;
- chunks no longer reachable from retained manifests; and
- crash-abandoned temporary objects.

The janitor never treats an incomplete listing as evidence of deletion.

### Capture algorithm

Capture is debounce- and quiescence-gated, with provider-specific scheduling
capabilities. Work is limited to the changed parse-unit batch.

For each member:

1. Open the path without following a final symlink and require a regular file.
1. Record file identity and the opening size from the opened descriptor.
1. Stream exactly that opening byte range into fixed-offset chunks.
1. Compute the whole-member hash and prefix continuity while streaming.
1. Recheck the opened descriptor and the provider path after the read.
1. Accept growth on the same file identity as a valid prefix revision.
1. Reject shrink, replacement, or loss of the provider coordinate.

Prefix hashes, not modification times, distinguish append from rewrite. Growth
after the opening size is left for a later bounded capture; it does not make a
hot session permanently uncapturable.

After streaming the complete unit, AgentsView repeats identity and size
validation across every member so a sidecar changed while a later member was
being read cannot silently escape the stability evidence. Once every member
snapshot is valid:

1. Create missing immutable chunks.
1. Put the source revision manifest with the expected previous identity.
1. Record or reconcile the committed revision in `stewardship.db`.
1. Queue the revision for delivery to configured archive targets.

The manifest is never authoritative before every referenced chunk is durable in
the local vault. If the manifest commits but the SQLite update does not,
reconciliation recovers it as a committed revision.

### Parse-once resync inventory

A full resync first builds one source inventory. Provider files and archived
copies are alternate locations for one logical revision, not separate inputs:

1. A provider copy matching a captured revision is parsed once from that
   revision.
1. A provider copy with failed prefix continuity or additional bytes is
   captured, then its captured revision is parsed once.
1. An evicted provider copy is parsed from the local stewardship vault.
1. A locally reclaimed revision is fetched from its designated external archive.
1. A source never covered by stewardship is parsed only from the provider copy;
   eviction does not apply.

Ingestion work is keyed by source ID and revision identity. Archive
materialization stages one complete parse unit at a time under an
AgentsView-owned temporary root, preserving its exact parse-relative layout.

## 3. Archive Protocol and Receipts

### Namespace identity

A folder target is initialized with a marker containing an archive UUID and
format version. Every operation verifies the marker before reading or writing. A
missing, malformed, or unexpected marker fails closed.

Receipts bind to the namespace UUID, not its configured path. Pointing a target
configuration at another directory cannot inherit deletion authority.
Unrecognized directories are never initialized implicitly during a mutating
operation.

### Logical protocol

The archive protocol exposes logical stewardship objects, never a Docbank
catalog, pack file, or physical vault layout:

1. Publish missing immutable source-local chunks by declared hash and size.
1. Publish an immutable source-revision manifest after all referenced chunks
   have been accepted.
1. Verify the manifest and every chunk identity according to target
   capabilities.
1. Return a receipt bound to the namespace, origin, source ID, sequence,
   manifest hash, and reconstructed content identity.

Uploads are idempotent. Existing content at the same logical identity must
verify byte-for-byte; an identity mismatch is conflicting authority and fails
closed.

### Sequence-derived heads

Mounted SMB and NFS targets do not provide a trustworthy compare-and-swap
primitive. Folder archives therefore follow the existing artifact-checkpoint
pattern:

- `stewardship.db` reserves a monotonic sequence per origin-scoped source;
- revision manifests are immutable and named by sequence;
- the effective remote head is the highest-sequence verified manifest; and
- the same sequence with a different identity is quarantined as conflicting
  authority.

The protocol assumes one owning writer per origin-scoped source. On a folder
target, this invariant plus sequence-collision detection replaces an atomic
mutable-head operation. Service targets may additionally offer a real
conditional head.

### Receipt confidence

A mounted-folder receipt proves that the target accepted bytes which read back
with the expected identities. Immediate read-back may be served from the
client's cache, while SMB and NFS servers may weaken flush guarantees.
Accordingly, this receipt does not prove device-level power-loss durability.

The mitigation is retention:

- the local stewardship copy remains through a safety interval; and
- a later-run verification provides a colder, independent observation before the
  final local chunks become reclaimable.

A service target issues a durable receipt only after the server independently
recomputes manifest and chunk identities. Echoing client-supplied hashes is not
sufficient.

### Completeness and remote garbage collection

The first folder format uses a separate chunk pool per source. It deliberately
forgoes cross-source and cross-origin deduplication so one source's liveness
does not depend on another writer's manifests.

Remote reclamation requires a complete verified manifest range known from the
local receipt ledger. If any expected sequence is absent, unreadable, or
invalid, the sweep stops without deleting content. Once completeness is
established, retained manifests mark live chunks and a grace-period sweep can
remove unreachable chunks and abandoned temporary writes.

Service-managed archives own their remote retention. AgentsView never infers
remote deletion authority from a partial or failed listing.

### Designated eviction target

Several mirrors may receive source revisions, but exactly one target is
designated as eviction-authorizing. Only receipts from its current namespace
permit provider-file eviction or final local reclamation.

Re-designation is an explicit state transition. Eviction and reclamation stop
until retained revisions are uploaded to, or independently verified by, the new
namespace. Receipt rows are never copied across archive identities.

## 4. Eviction and Retention

### State machine

Provider-file eviction is disabled by default. For a provider-declared evictable
parse unit, it follows this recoverable state machine:

```text
captured
  -> archive accepted
  -> eviction eligible
  -> eviction recorded
  -> quarantined
  -> final revision accepted
  -> provider copy removed
  -> local-vault retention
  -> remotely reverified
  -> local chunks reclaimable
```

Before beginning, AgentsView verifies:

- the designated archive marker and exact latest-revision receipt;
- complete, unchanged parse-unit membership;
- provider-specific minimum quiescence, which may be measured in days;
- transitive parse-unit closure and independent evictability; and
- successful archive-backed reparse support for the revision.

### Watcher coordination

Before moving any file, AgentsView records the source's eviction transition in
`stewardship.db`. The disappearance classifier added with the eviction feature
consults that state:

- an expected eviction changes provider presence to `evicted`; and
- an ordinary disappearance retains the existing source-missing tombstone
  behavior.

This preserves normalized sessions after intentional eviction. The
classification hook does not ship with capture, folder archival, or
archive-backed resync; it belongs to the eviction slice where non-default
eviction state first exists.

Quarantine has two placement requirements:

- it is outside every watched provider root, preventing rediscovery and double
  ingestion; and
- it is on the same filesystem as the provider path, permitting an atomic
  rename.

AgentsView therefore uses per-volume quarantine roots. If no safe
AgentsView-owned location exists on that filesystem, eviction is declined
instead of falling back to copy-then-unlink.

### Rollback and finalization

Every pre-removal failure has a reverse transition:

- identity mismatch, archive failure, or other verification failure restores the
  parse unit to its original path;
- target re-designation during eviction restores it by default;
- if the provider recreated the original path, AgentsView does not overwrite it;
  it final-captures and archives the quarantined bytes, then removes only the
  quarantine copy; and
- startup reconciliation resumes or rolls back an interrupted transition from
  its durable state.

After quarantine, AgentsView waits for further writes through any provider-held
descriptors. If bytes changed, it captures and uploads a final revision. The
quarantine copy is removed only after that exact revision earns a receipt from
the designated target.

Eviction has an inherent product trade-off: removing a provider source may break
that provider's resume command or native session-history view. The operation
must warn clearly about this consequence. Normal provider behavior is unchanged
only for sources that remain unevicted, which is the default.

### Retention tiers

- Unacknowledged revisions and their chunks are never reclaimed.
- The latest captured revision remains in the local vault while its provider
  copy exists, protecting against offline rewrite.
- Superseded acknowledged revisions retain bounded safety history before
  becoming janitor-eligible.
- After provider eviction, the latest local copy remains through a safety
  interval.
- A later-run remote verification is required before the last local chunks
  become reclaimable.
- Small manifests, receipts, and provenance survive chunk reclamation so
  archive-backed resync can find remote content.

For never-evicted sources, the steady-state local vault contains approximately
one compressed live revision per source plus grace-period history. JSONL often
compresses substantially, but no fixed ratio is guaranteed. The bound is the
compressed live source set and configured safety history, not permanent
retention of every revision.

## 5. Project Boundaries and Delivery Order

### Database boundary

Stewardship uses a dedicated `stewardship.db` beside its Docbank vault. It is
machine-local archival coordination state and is explicitly outside the
SQLite/PostgreSQL Backend Parity requirement. PostgreSQL does not receive
stewardship queues, receipts, chunk references, eviction transitions, or janitor
state.

The normalized database may store derived session-to-source provenance for
queries and deduplication. Because the two databases cannot share a transaction,
authority points from the stewardship ledger into the normalized projection:

- `stewardship.db` plus committed vault manifests define captured revisions;
- normalized provenance is rebuilt during full resync; and
- reconciliation repairs missing or stale projections rather than treating the
  normalized database as archival authority.

### Shared code boundary

Shared primitives are reuse-by-import, not an upfront extraction project.
Stewardship should import genuinely identical helpers from their current
packages when dependency direction remains sound. A new shared package is
justified only after another consumer or concrete divergence demonstrates the
need.

### Derived artifact track

The remaining normalized artifact work stays independently useful:

1. **Normalized import:** durable inbound work, bounded decoding, checkpoint
   recovery, origin handling, and idempotent session replacement.
1. **Transport and metadata convergence:** checkpoint exchange, folder and
   service adapters, HLC metadata, and replay.
1. **Lifecycle and product integration:** artifact GC, maintenance, watch mode,
   server and CLI orchestration, PostgreSQL identity integration, and UI.

The existing artifact `raw_source` field remains unset by stewardship. It is a
single-file, per-session reference with one hash, one size, one relative path,
and a 1 GiB limit. It cannot represent a multi-file, many-to-many parse unit. If
stewarded source bytes ever travel on the artifact wire, that requires a
wire-version bump and a distinct source-set reference type rather than a
widening of `raw_source`.

### Stewardship track

1. **Local capture ledger:** provider capabilities, parse units, the dedicated
   vault and database, chunks, source manifests, bounded scheduling, and
   reconciliation. No external archive or deletion.
1. **Folder/NAS archive:** namespace markers, sequence-derived manifests,
   receipts, verification, reconciliation, and completeness-gated remote GC.
   No provider deletion.
1. **Archive-backed full resync:** the five-branch parse-once inventory,
   temporary exact-layout materialization, derived provenance, and recovery
   from local or external source bytes.
1. **Opt-in eviction:** watcher classification, per-volume quarantine, rollback,
   provider warnings, delayed reverification, and local reclamation.
1. **Service-backed archive:** the same logical protocol with independent
   server-side identity verification. Service authorization, visibility, and
   retention remain outside AgentsView.

Destructive behavior lands only after capture, external archival, and
archive-backed resync have each operated safely without it.

### Mapping from the existing umbrella

The artifact umbrella is an ignored working document whose current PR sequence
predates this design. Its scopes map as follows:

| Existing umbrella scope                               | Disposition under this design                                                              |
| ----------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| PR3 raw JSONL capture into artifact `raw_source`      | Superseded by stewardship local capture; `raw_source` stays unset                          |
| PR4 folder transport, normalized import, one-shot CLI | Split into normalized artifact import/transport and a separate stewardship folder protocol |
| PR5 metadata convergence                              | Remains in the derived artifact track                                                      |
| PR6 artifact GC and maintenance                       | Remains in the derived artifact track; stewardship GC has separate roots                   |
| PR7 artifact watch mode                               | Remains in derived artifact lifecycle integration                                          |
| PR8 HTTP transport                                    | Remains an artifact transport; stewardship service transport is a separate adapter         |
| PR9 S3 transport                                      | Remains an optional derived artifact transport unless separately designed for stewardship  |
| PR10 PostgreSQL identity unification                  | Remains derived artifact integration; no stewardship backend parity                        |
| PR11 peers UI                                         | Remains derived artifact product integration                                               |

This design does not assign replacement PR numbers. Before writing
implementation plans, update the umbrella deliberately so its locked sequence,
milestones, findings ledger, and branch naming agree with the new two-track
scope.

## Alternatives Considered

### One unified artifact ledger

Storing exact provider bytes as ordinary derived artifacts would reuse more of
the existing pipeline, but it couples unrelated retention and authority. It also
cannot represent multi-file parse units through the frozen `raw_source` field.
The design keeps separate ledgers and shares only proven primitives.

### External archive without a local stewardship vault

An external-only design has a durability hole. If a provider rewrites or
truncates a source while the archive is unreachable, the preceding revision has
no durable home. The local stewardship vault is therefore the capture ledger,
while the external target supplies the off-machine copy required for eviction.

### A live Docbank vault on a mounted share

Docbank has one authority per vault and holds an exclusive hierarchy lock.
Putting a live vault directly on a shared mount would expose its private
catalog, locking, and pack representation as a network protocol. AgentsView
instead publishes logical immutable objects to a marked folder namespace or uses
a service that owns its Docbank vault behind the archive protocol.

### Replicating Docbank pack files

Sealed packs are useful physical storage but are not a stable selective-source
archive protocol. Their catalog coherence and representation lifecycle remain
Docbank implementation details. Archive manifests and receipts refer only to
logical source and chunk identities.

### Direct deletion after upload

Immediate unlink after a warm-cache read-back overstates mounted-share
durability and races active provider descriptors. The approved eviction ladder
uses durable state, same-filesystem quarantine, final capture, local retention,
and later-run remote reverification.

## Failure Handling

The system distinguishes content conflicts from operational failures:

- identity mismatch, namespace mismatch, sequence collision, missing expected
  manifests, and incomplete liveness ranges fail closed;
- transient filesystem, network, context, or database failures leave work
  pending without issuing receipts or consuming revisions;
- malformed remote candidates may be quarantined only after their complete
  received identity is known;
- absence from an incomplete or failed listing is never interpreted as deletion;
  and
- no claim, receipt, eviction step, or garbage-collection cursor advances before
  the durable state it represents.

## Testing Strategy

Tests use real SQLite databases, Docbank vaults, and temporary folder archives
where observable filesystem behavior matters.

### Capture and reconciliation

- Exact fixed-offset boundary and append-tail reuse.
- Growth during capture succeeds as a prefix revision.
- Shrink, replacement, symlink, non-regular file, and prefix-continuity
  failures.
- Crash after chunk creation but before manifest commit.
- Crash after manifest commit but before `stewardship.db` update.
- Ledger rebuild from committed manifests and orphan-chunk grace.
- Small and production-scale source inventories demonstrate bounded per-event
  work and memory.

### Parse units and resync

- Single-file and declared sidecar sets preserve exact parse-relative layout.
- Cross-linked units are grouped or rejected as non-evictable.
- Provider and archive copies of one revision produce one ingestion.
- Each of the five inventory branches selects exactly one source.
- Archive-backed full resync rebuilds normalized provenance.
- Unsupported and database-backed providers remain provider-only.

### Archive protocol

- Namespace marker mismatch, removal, and target-path reassignment fail closed.
- Idempotent chunk and manifest publication.
- Same-sequence/different-identity collision.
- Interrupted upload and crash-abandoned temporary-file cleanup.
- Folder receipt followed by later-run reverification.
- Service receipt requires independently verified identities.
- Remote sweep refuses incomplete manifest ranges and preserves live chunks.
- Target re-designation requires receipts to be re-earned.

### Eviction and retention

- Watcher disappearance during eviction does not tombstone normalized sessions.
- Ordinary disappearance still creates the existing source-missing tombstone.
- Quarantine is outside watched roots and same-filesystem; unsupported volumes
  decline eviction.
- Every failure edge restores the source or safely final-captures quarantine.
- Provider path recreation is never overwritten.
- Writes through an open provider descriptor produce a final archived revision.
- Crash recovery at every durable eviction state.
- Local and remote GC retain unacknowledged, incomplete, grace-period, and
  insufficiently reverified content.
