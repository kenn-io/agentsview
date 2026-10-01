# Local session archives

Status: implemented on this branch; not yet released. User instructions and
command limits live in [the CLI reference](../commands.md#agentsview-archive).

## Storage and reuse

The offline commands keep original files in a dedicated embedded Docbank vault
under `raw-archive/artifacts`. `sessions.db` owns original device/root bindings,
file inventories, immutable source acceptance, current heads, and parse status.
Those records survive database rebuilds. Moving the capture changes its import
path, not those identities.

The importer hashes and streams every regular file directly into the vault. It
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
| Docbank              | Content retention and exclusive vault ownership. Extraction and automatic garbage collection are not enabled.                       |

The local path shares source discovery and matching with `rawderive` but does
not use its buffered hosted parse results. `PathRewriter` works independently of
session-ID prefixes, so source materialization does not rename sessions.
Original machine labels and source paths are restored. Ambient filesystem
project discovery is disabled.

## Reparse publication and ownership

The CLI holds the existing offline writer lock. It refuses to run while the
daemon owns the archive; there is no daemon/worker vault handoff in this
version. Normal startup resync never opens this raw vault.

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
sessions whose original sources are absent. The existing empty-discovery abort
remains in force. Accepted archived sources are not automatically reparsed on a
parser-version bump.

## Recovery

Stopped-owner filesystem backup includes SQLite, raw and normalized artifact
vaults, assets, config, and installation identity. It keeps the writer lock
until copying and read-back verification finish. Restore requires a new
directory, checks file hashes and SQLite integrity, and verifies every accepted
manifest and inventoried object. A matching vault ID or a bounded blob sample is
not sufficient. Restore upgrades an older database through the normal
preserved-provider rebuild with every live provider disabled. Native extraction
provides access to supplemental files too.

Docbank [PR #741](https://github.com/kenn-io/docbank/pull/741) and merged Kit
[PR #132](https://github.com/kenn-io/kit/pull/132) support large supplemental
objects without the old 4 GiB admission limit. The AgentsView dependency pins
that implementation. Existing standalone recovery repositories and their
binaries need not be upgraded in place.

## Deferred work

Continuous capture first needs durable root/head authority outside the
path-keyed upload checkpoint, bounded staging, and a solution for shared Codex
index changes that would otherwise multiply generations. The immutable importer
does not claim to solve that workload.

Team delivery uses a separate archive per team. Cross-device identity/curation
merging, project access controls for raw containers, object-storage placement,
online backup, retention, and disaster rebuild remain later work under
[the hosted roadmap](https://github.com/kenn-io/agentsview/issues/1352).
