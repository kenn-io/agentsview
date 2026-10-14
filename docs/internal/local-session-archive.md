# Local session archives

Status: implemented on this branch; not yet released. User instructions and
command limits live in [the CLI reference](../commands.md#agentsview-archive).

Portable capture, seed assembly, multi-machine source identity mapping, recovery
and persisted archive-only mode are implemented here. This delivery was reviewed
against [PR #2036](https://github.com/kenn-io/agentsview/pull/2036); approval of
that review does not mean every
[retirement acceptance criterion](#pending-verification-before-retiring-a-computer)
has been exercised end to end.

## Outcome and limits

A person retiring a computer can keep its original agent files, their existing
AgentsView history and curation, and durable evidence of which machine each
session came from. They can move the archive to another computer, browse it,
extract the original files, and explicitly reparse supported sources without
access to the retired computer.

This delivery is a controlled, offline move into one personal archive. It
collects Claude and Codex captures; the receiving computer may itself be one of
the sources. Repeating an identical capture does not duplicate a conversation.
Artifact exchange is refused in an archive-only destination.

Each source's browsable raw history stops at its first accepted capture. Later
changes to the same source are recoverable evidence, not browsable updates — a
change to Codex's shared `session_index.jsonl` can conflict every source that
includes it. The retirement archive is assembled afresh from a final capture;
this delivery does not provide an ongoing collector for machines still in use.

Continuous capture, automatic source deletion, team access controls, arbitrary
cross-machine conversation merging, and cloud object placement are outside this
delivery. A future team service would use a separate archive per team; its raw
retention, garbage collection and disaster rebuild remain tracked under
[issue #1352](https://github.com/kenn-io/agentsview/issues/1352).

## Source identity model

Several existing identities have different jobs. None of them is replaced by a
new universal ID:

| Record                      | Meaning and rule                                                                                                                                                        |
| --------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Source installation ID      | Identifies the original installation, independent of its display label. Reused from `telemetry-install-id` when available, or generated once in the capture descriptor. |
| Artifact origin             | Identifies an artifact publisher. Bound explicitly to the source installation; never regenerated or adopted as the collector's own publisher.                           |
| Hosted tenant and device ID | Identify an authenticated sender within a tenant. An explicit binding records the relationship; importing it does not grant upload authority.                           |
| Configured root ID          | Identifies a provider root on a source. Existing checkpoint IDs are preserved; a capture path is only that root's current location.                                     |
| Archive ID                  | Identifies the archive that owns SQLite state. A whole-archive restore preserves it; collecting another source into that archive does not replace it.                   |

Root records are namespaced by source installation and provider as well as root
ID, since two machines may use the same root name. A session from a foreign
installation uses `installation~parser-id` identity and keeps its original
machine attribution; namespace collisions are refused rather than silently
merged. Collection into an existing archive preserves that archive's own owner
and publisher — the retiring installation's captured database is never adopted
as the receiving host's identity, and the receiving host's own sessions remain a
separate source.

## Source capture

`archive capture` writes an immutable package with source identity, root
bindings, a checksummed inventory and a closed application database. It reuses
the raw capturer's online SQLite backup without its upload spool budget, and
reads only allowlisted identity/receipt fields from raw-sync checkpoints.
Configuration loading and source database reads never migrate the source.
Identity reuse validates the previous descriptor without reading its retained
files. An explicit binding preserves the application root's original path when
its source data directory moves; the current path remains capture provenance.

A digest binds the inventory to its source and root identities. Import checks
all captured files and rechecks each file against that inventory while storing
it. The descriptor and exact inventory are retained as supplemental evidence.
Curation counts come from the closed database, not its live predecessor. The
regular importer refuses unknown preflight, incomplete or unattributed deletion
evidence, and artifact history. Accepted source deletions suppress matching
sessions during reparse. Ordinary-vault capture remains blocked on upstream
stopped-owner custody support.

## Seed assembly

`archive import --seed` builds a new data directory from the capture's complete
application database, assets and installation identity. It verifies each copied
stream against the inventory before opening the destination database. The source
capture remains read-only. Seed trash and exclusions are allowed because their
complete database state is preserved; unknown preflight and artifact history
remain refusals.

Assembly sets archive-only mode, retains raw originals through the existing
importer, and uses recovery verification before the final no-replace rename. An
older parser generation rebuilds from stored rows with live providers disabled.
No source is automatically reparsed. Existing raw archive state in the seed is
refused rather than cleared or detached from its vault.

The seed preserves archive and session identities and machine aliases. A
conflicting recorded installation owner is an error. Unresolved machine keys and
counts are reported without assigning them to the seed installation. Runtime
configuration comes from the same allowlist and fresh local authentication as
restore. Sessions from other installations use `installation~parser-id`
identities and retain their original machine attribution. Their trash and
permanent-delete records prevent raw files from bringing those sessions back.
Other foreign curation remains in the captured database; it is not merged into
browsable state. Empty and `local` machine sentinels denote the seed owner.
Reparse recognizes them only for that owner's sources, with the same provider
and path checks as other existing sessions.

Keep real changes to stars, pins, names and other curation on the source archive
until its final capture. The rehearsal archive is disposable and must receive no
unique user curation; run synthetic curation checks in a separate copy. If the
rehearsal contains unique edits, stop final assembly and preserve that database.
Seed import cannot merge those edits into the final archive.

## Storage and reuse

The offline commands keep original files in a dedicated embedded Docbank vault
under `raw-archive/artifacts`. `sessions.db` owns original device/root bindings,
file inventories, immutable source acceptance, and parse status. Those records
survive database rebuilds. Moving the capture changes its import path, not those
identities.

The importer requires a validated capture. After whole-package verification, it
streams each file into the vault using the inventory's hash and size. The vault
checks the stored stream against both before custody is recorded. The importer
then uses Claude/Codex capture plans and the hosted raw manifest format to
accept complete sources. Objects and manifests are stored before SQLite
acceptance. Retries reuse acceptance. Conflicting imports retain evidence but do
not reparent the source. Unsupported trees and files remain supplemental
inventory. Raw manifest limits remain 1 MiB metadata, 4,096 entries, 16,384
object references, and 16 GiB per logical source file; larger retained files are
supplemental.

| Existing subsystem   | Reuse and boundary                                                                                                                  |
| -------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| Hosted raw sync      | Canonical manifests, provider plans, object adapter, verified materializer. PostgreSQL leases and server publication remain hosted. |
| Local sync           | Normal SQLite content policy, conversation identities, signals, secret scanning, and bounded large-Codex parsing.                   |
| Artifact folder sync | Existing normalized session exchange stays separate from the original-file vault.                                                   |
| Docbank              | Content retention, exclusive vault ownership and capture extraction. Automatic garbage collection is not enabled.                   |

The local path shares source discovery and matching with `rawderive` but does
not use its buffered hosted parse results. `PathRewriter` works independently of
session-ID prefixes, so source materialization does not rename sessions.
Original machine labels and source paths are restored. Ambient filesystem
project discovery is disabled.

## Reparse publication and ownership

The CLI holds the existing offline writer lock. It refuses to run while the
daemon owns the archive; there is no daemon/worker vault handoff in this
version. Normal startup resync never opens this raw vault. Archive commands
require the persisted archive-only marker before opening raw storage. An
ordinary database is refused, not silently converted. Seed import and restore
create the dedicated archive.

Each explicit reparse batch creates one consistent full SQLite copy alongside
the working database. The normal sync engine writes selected materialized
sources into that copy. Identity checks reject a session already attributed to a
different source. Processing status and curation are installed with the copy
through the existing resync database swap only after the batch succeeds. Failure
discards the copy and leaves custody and browsable content unchanged.
Content-addressed offloaded images may be added before publication, as in normal
sync. They do not replace existing assets.

This deliberately costs a full SQLite copy per batch. It avoids a second
publication engine and whole-session transfers between databases. Materializing
one source at a time bounds source scratch usage; the database copy and WAL need
additional disk space. Explicit batches can be retried after interruption.

Full resync carries durable raw records with other archive state and copies
stored sessions. For an archive-only database, every live provider is disabled
during the rebuild, even when its original paths exist on the receiving host.
Accepted archived sources are not automatically reparsed on a parser-version
bump.

## Recovery

The stopped archive writer keeps SQLite and the embedded raw vault under
exclusive ownership while Docbank creates a portable backup. Its preparation
callback takes a consistent SQLite snapshot, including WAL content. Application
extras contain that snapshot, assets, installation identity and an allowlist of
recovery settings. An application inventory names these components; Docbank owns
their hashes, sizes and chunk recipes. Backup reports the repository ID, exact
snapshot ID, application build and minimum reader version.

Backup currently refuses an existing ordinary artifact vault. Docbank has no
public API to hold that vault's ownership while leaving it closed and
unmigrated. It must not be silently omitted from a recovery point. This
limitation blocks complete recovery for archives that use that vault.

Restore selects an exact snapshot, assembles a new data directory in private
staging, checks SQLite integrity, and verifies every accepted manifest,
inventoried raw object and referenced message/tool image before publishing.
Missing or corrupt referenced images also prevent backup. A matching vault ID or
bounded blob sample is insufficient. Restore upgrades an older database through
the normal preserved-provider rebuild with every live provider disabled. Native
extraction provides access to supplemental files too.

Docbank applies its managed compression policy when restoring individual raw
objects, including large originals reconstructed from chunks. The source vault's
write settings do not control restore compression. Existing backups need no
conversion. Packed objects retain their packing; SQLite databases and
application extras are restored as ordinary files. The restored archive can
therefore differ in size from both the working archive and the backup. Restore
needs temporary space for raw and compressed candidates as well as the staged
database.

The original runtime config is omitted. Only content/image retention policy and
the original display label return; local authentication is regenerated and the
host binds to loopback. Previously retained raw content is not redacted and may
contain credentials.

Restore permanently marks the staged database as archive-only before publishing
it. Every subsequent foreground or background start skips provider watchers,
automatic sync and filesystem project discovery. Manual source sync, transcript
uploads, conversation imports, source transfer, artifact exchange and mirror
publication are refused. Read commands, curation, archive import, extraction,
backup and explicit archive reparse remain available. The flag lives in SQLite
and survives database rebuilds and backups; there is no flag or config setting
to turn it off. Use a separate data directory with a fresh installation identity
to collect new local sessions.

Merged Docbank [#741](https://github.com/kenn-io/docbank/pull/741) and
[#764](https://github.com/kenn-io/docbank/pull/764), with Kit
[#132](https://github.com/kenn-io/kit/pull/132) and
[#145](https://github.com/kenn-io/kit/pull/145), support large raw objects and
application extras without the old 4 GiB limit. Extras over 64 MiB require Kit
reader version 6. Once such a snapshot exists, older readers cannot use the
repository, including its older snapshots. Existing experimental filesystem
recovery points require their saved readers and are left untouched.

## Pending verification before retiring a computer

These are completion requirements, not a record of checks already run. Use a
fresh isolated deployment on the receiving computer and protected copies from
four real source machines. Keep existing services, binaries, databases, vaults,
credentials and provider roots untouched. Record the exact build, descriptor and
inventory hashes, source cutoffs, input counts and commands in a private
runbook.

1. Capture on both macOS and Linux. Read the source's actual installation ID,
   reuse root IDs on a second capture, include live SQLite WAL content in a
   consistent snapshot, and report symlink and credential omissions. Failed or
   changing inputs must not publish a complete capture. Run preflight on all
   four closed databases before collection; record counts and unknowns.
1. Import all four eligible sources. Browse and filter sessions by original
   machine, including sessions created on the receiving host. Rename a label,
   relocate a capture, and repeat import after restart. Stable identities,
   accepted heads and counts must remain unchanged.
1. Start the collector in the foreground, through the config-only background
   path, and after restart with receiving-host provider files available. Try
   ordinary sync and artifact exchange explicitly. Persisted archive-only mode
   must prevent discovery and publication after rehearsal and final assembly.
1. Compare the seed database's session IDs, message IDs, stored content,
   curation, trash and assets before and after collection, backup, restore and
   full resync. Record the limit on other machines' curation. Include nonzero
   foreign trash and permanent exclusions: raw bytes must remain extractable,
   while deleted sessions stay out of browsable history after reparse, resync
   and restore. Preserve unsupported provider data.
1. Back up, make the source trees and original vault unavailable, then restore
   on the receiver using only the repository and an explicit snapshot ID, even
   if a newer snapshot exists. Extract and hash every captured file against
   its selected capture inventory, including conflicting versions and a file
   over 4 GiB. Include a backup application extra over 4 GiB; large raw
   content alone does not exercise that contract.
1. Reparse selected Claude and Codex sessions, a large Codex transcript, and a
   fork with its parent. Prove the parser ran; carried-forward SQLite rows
   alone are insufficient. Claude forks and subagents must remain distinct
   parser sessions. Compare the allowlisted recovery settings and confirm
   omitted credentials do not reappear in the restored runtime configuration.
1. Interrupt import and reparse, then retry without duplicate acceptance or
   partial publication. A missing blob, corrupt backup extra or identity
   conflict must leave the previous browsable archive intact. Startup after a
   parser data-version change must carry archived-only sessions forward
   without scanning the vault.
1. Stop source writers and establish the final capture cutoff, including work
   since the rehearsal. Assemble the final archive afresh under the
   [seed assembly rules](#seed-assembly), including a session changed after
   rehearsal, and verify its final accepted bytes can be reparsed. Verify two
   independent copies on different physical storage, at least one off the
   retiring computer, and a cold restore from the final recovery point. Both
   copies must survive erasing that computer. Record covered sessions and
   files, exclusions, recovery command, repository ID, snapshot ID and reader
   version. Confirm that a changed shared Codex index produces a conflict
   during rehearsal; it must not be mistaken for ongoing source refresh
   support.

Successful import or backup verification alone does not establish readiness to
erase the source computer. Require the final cutoff, complete extraction
comparison, preserved curation, stable attribution and independent-copy restore.
Deletion remains an explicit user action. Keep readable formats and periodically
check both copies; this delivery does not promise permanent storage without that
maintenance.

## Relationship to other raw-history work

[Hosted raw sync](../hosted-raw-sync.md) retains originals and authenticated
source generations on a server. [Artifact folder sync](../artifact-sync.md)
exchanges normalized sessions without the original files or curation. Neither is
a substitute for a complete offline archive recovery point, and this delivery
does not make an offline capture or import equivalent to a hosted acceptance
receipt.

[PR #1741](https://github.com/kenn-io/agentsview/pull/1741) and
[PR #1751](https://github.com/kenn-io/agentsview/pull/1751) extract shared raw
source/group identity logic for hosted raw sync. This delivery reuses that
identity reasoning for local source bindings without adopting the PostgreSQL
acceptance, leasing and publication subsystem into SQLite. Adding artifact
exchange on top of this identity mapping — matching a wire alias to a
raw-imported row regardless of which side arrives first — is deferred
integration work, not part of this delivery.

## Deferred work

Continuous capture first needs durable root/head authority outside the
path-keyed upload checkpoint, bounded staging, and a solution for shared Codex
index changes that would otherwise multiply generations. The immutable importer
does not claim to solve that workload.

Team delivery uses a separate archive per team. Cross-device identity/curation
merging, project access controls for raw containers, object-storage placement,
online backup, retention, and disaster rebuild remain later work under
[the hosted roadmap](https://github.com/kenn-io/agentsview/issues/1352).
