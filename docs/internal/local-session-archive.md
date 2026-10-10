# Local session archives

Status: implemented on this branch; not yet released. User instructions and
command limits live in [the CLI reference](../commands.md#agentsview-archive).

The approved
[multi-machine archive design](../superpowers/specs/2026-09-30-multi-machine-session-archive-design.md)
defines source attribution, artifact/raw identity matching and recovery.
Portable capture, seed assembly, multi-machine source identity mapping, recovery
and persisted archive-only mode are implemented here.

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

## Storage and reuse

The offline commands keep original files in a dedicated embedded Docbank vault
under `raw-archive/artifacts`. `sessions.db` owns original device/root bindings,
file inventories, immutable source acceptance, and parse status.
Those records survive database rebuilds. Moving the capture changes its import
path, not those identities.

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
| Docbank              | Content retention, exclusive vault ownership and capture extraction. Automatic garbage collection is not enabled.                    |

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

## Deferred work

Continuous capture first needs durable root/head authority outside the
path-keyed upload checkpoint, bounded staging, and a solution for shared Codex
index changes that would otherwise multiply generations. The immutable importer
does not claim to solve that workload.

Team delivery uses a separate archive per team. Cross-device identity/curation
merging, project access controls for raw containers, object-storage placement,
online backup, retention, and disaster rebuild remain later work under
[the hosted roadmap](https://github.com/kenn-io/agentsview/issues/1352).
