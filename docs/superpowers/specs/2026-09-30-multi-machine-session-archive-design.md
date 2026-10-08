# Move a session archive without losing its origins

Status: approved for careful implementation on October 2 after external review.
This specifies the intended delivery for
[PR #2036](https://github.com/kenn-io/agentsview/pull/2036). The
[current implementation](../../internal/local-session-archive.md) distinguishes
implemented behavior from the remaining work; approval does not mean the
retirement acceptance criteria have passed.

## Outcome and limits

A person retiring a computer can keep its original agent files, their existing
AgentsView history and curation, and durable evidence of which machine each
session came from. They can move the archive to another computer, browse it,
extract the original files, and explicitly reparse supported sources without
access to the retired computer.

The first delivery is a controlled, offline move into one personal archive. It
must also collect Claude and Codex captures from three other machines. The
receiving computer may itself be one of those sources. Repeating an identical
capture must not duplicate a conversation. Artifact exchange is disabled in this
archive; integration with that transport is deferred if the preflight below
confirms that none of the four inputs uses it.

Each machine's browsable raw history stops at its first accepted capture. Later
changed captures are recoverable evidence, not browsable updates. Even a change
to Codex's shared `session_index.jsonl` can conflict every source that includes
it. The final retirement archive is assembled afresh. This slice does not
provide an ongoing collector for the three machines still in use.

Continuous capture, automatic source deletion, team access controls, arbitrary
cross-machine conversation merging, and cloud object placement are outside this
delivery. The future team service uses a separate archive per team. Its raw
retention, garbage collection and disaster rebuild remain under
[issue #1352](https://github.com/kenn-io/agentsview/issues/1352).

## What we know from the code

The reviewed source baseline was PR #2036 at
`07de734e1d9b629b16a766145294f6e99cc12ace`. The first spec commit is `30537403`.
The October 2 review also inspected main at
`7203ab4df7071d0dd72bb2d215c12ef5aa706d3d`. These are observations, not claims
that the proposed behavior already exists:

- `internal/db/raw_archive.go` accepts roots from only one device. The import
  descriptor supplies device, root and machine identities manually.
- There is no `archive capture` command. Top-level `capture` runs an agent
  execution for usage reporting; the private preservation script is not an
  AgentsView feature. The source capture command specified below is new work.
- Artifact import writes `origin~native-session-id`; local raw reparse writes
  the parser's native ID. A synthetic end-to-end test through both production
  paths reproduced two rows for one Claude conversation. That probe is not
  committed; commit it as the first regression test when artifact matching is
  implemented, not as evidence of existing coverage.
- `rawcheckpoint.ResolveConfiguredRoot` keeps a random root ID against a local
  path in the checkpoint database. Its receipts are also durable state; that
  database cannot simply be discarded while promising chain continuity.
- Local reparse already uses the normal sync engine, including staged large
  Codex parsing. It publishes a full replacement SQLite copy after the
  selected batch succeeds. Startup resync carries archived content forward; it
  does not open the raw vault to reparse it.
- The pinned Docbank commit `72b055a6bdca17cc5dbf280e953174b21fde2fc9` already
  has `Vault.CreateBackup`, `BackupOptions.Prepare`, `ExtraFiles`, and
  `BackupRepository.Restore`. Restore without the original vault was exercised
  in Docbank's own tests. The current AgentsView filesystem recovery format
  does not use these APIs.
- The pin and Kit main at `4d5638e33162f44976a24656a6c1f9ca26823ecd` buffer
  backup extras in memory and reject an extra over 4 GiB (`backup/extras.go`).
  Large raw-object support does not cover application `ExtraFiles`. Repository
  encryption is unimplemented (`backup/FORMAT.md`); backup passes
  `Encrypted: false` (`backup/create_shared.go`).
- Docbank #741 merged on September 30 as `4e9a5a0e`. The branch's pre-squash pin
  is not that merged revision; no fetched release tag contains the merge.

The implementation baseline includes main `597a06ba` without rewriting the
branch's history. Its first tagged dependency pair was Docbank v0.15.0 and Kit
v0.31.1, covering embedded backup, large recovery files and main's telemetry
API. Current dependency versions live in `go.mod`. Recovery and
retired-generation regressions must pass against each updated pair before
delivery. Restore also uses Docbank's managed compression policy, including for
large chunked originals; see
[recovery storage](../../internal/local-session-archive.md#recovery).

[Hosted raw sync](../../hosted-raw-sync.md) retains originals and authenticated
source generations. [Artifact folder sync](../../artifact-sync.md) exchanges
normalized sessions; it does not carry the original files or curation. Neither
is a replacement for a complete archive recovery point.

## Keep source identity separate from the computer serving the archive

Use the source's existing AgentsView installation ID as its stable identity when
available. Keep its human-readable machine name as an editable label. Record the
label at capture as historical evidence. A rename, a different mount point, or a
new archive host changes neither identity nor original attribution. If a source
never had an installation ID, generate one once in the capture descriptor and
carry that descriptor with every subsequent copy.

There are several existing identities with different jobs. Do not replace them
all with a new universal ID:

| Record                      | Meaning and rule                                                                                                                                                 |
| --------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Source installation ID      | Identifies the original installation, independent of its label.                                                                                                  |
| Artifact origin             | Identifies an artifact publisher. Bind its existing origin explicitly to the source installation; do not regenerate it or adopt it as the collector's publisher. |
| Hosted tenant and device ID | Identify an authenticated sender within a tenant. Store an explicit binding to source attribution; importing that binding does not grant upload authority.       |
| Configured root ID          | Identifies a provider root on that source. Preserve existing checkpoint IDs; a capture path is only its current location.                                        |
| Archive ID                  | Identifies the archive that owns SQLite state. A whole-archive restore preserves it; collecting another source into that archive does not replace it.            |

Store source records and these bindings in `sessions.db`, and export them in the
capture descriptor and complete backup. Namespace root records by source
installation and provider as well as root ID; two machines may have the same
root name. Existing hosted manifests and receipts are retained unchanged as
supplemental evidence; checkpoint identity metadata excludes credentials. This
delivery does not accept those foreign envelopes as local source heads. It
constructs local manifests from the closed native capture, under the receiving
archive ID and source installation ID, using the recorded root IDs. The explicit
bindings explain their relationship to the hosted tenant and device; local
receipts never stand in for hosted acceptance receipts.

The first offline capture may create roots when no raw-sync roots exist. Save
those IDs in its descriptor. A later raw-sync enrollment must explicitly bind
those roots to the uploader; it must not infer identity from a relocated path.
This delivery preserves non-secret checkpoint identity and receipt information
as supplemental evidence. It does not make checkpoints disposable or implement
resumed continuous capture. Do not copy token-bearing checkpoint settings into
the new capture.

Collection into an existing archive preserves that archive's owner and
publisher. The retirement workflow below instead restores the retiring
installation's seed as an unpublished recovery rehearsal. It preserves that
seed's installation ID, archive ID and recorded machine label; it never adopts
the receiving host's existing installation. At cutover it becomes the retired
installation's replacement, with one active writer. The receiving host's own
captured sessions remain a separate source.

Persist `archive_only` in the archive's SQLite state before its first startup,
and preserve it in resync and backup/restore. This is new work, not `--no-sync`.
Every startup path, including config-only background launches, checks it before
provider discovery or scheduling. In this mode reject ordinary provider sync,
artifact exchange/publication and hosted raw-sync jobs. Explicit archive import,
verified materialization and reparse remain available. Parser-version resync may
carry stored rows forward but cannot read receiving-host provider roots.

The mode stays on after cutover; this archive never scans the receiving host.
There is no flag or config override to turn it off in this slice. New local
ingestion uses a separate data directory with its own installation ID. Test
foreground start, background start and restart with source files present on the
receiving host; none may label those files as the retired installation. Reading
the stored flag is not sufficient test evidence: exercise the launch and sync
entry points with provider input available.

Preserve existing machine aliases. Reuse explicit machine-identity selection for
historical hostname rows whose owner is not recorded; never assign every row to
the seed's installation. List unresolved machine keys and affected session
counts in the migration report. Source binding must not call local ownership
adoption for a foreign installation.

## Session identity and the boundary with artifact exchange

Add a durable mapping from an origin-qualified parser session identity to the
existing SQLite session ID for raw reparse. Source attribution is needed in the
first slice even when artifact exchange is unused. Extending this resolver to
artifact import is separate integration work, subject to the preflight below.

For Claude and Codex, the key is
`(source installation, provider, parser Session.ID before transport qualification)`.
This includes the parser's Codex prefix and Claude fork suffix.
`SourceSessionID` is separate provenance and grouping metadata: multiple Claude
branches can share it. Keep the raw source identity plus parsed session ID as a
raw alias. Parent and subagent links use the parser session key too. Do not
derive identity by blindly stripping `~` from stored strings.

Preserve a session ID that already exists in the receiving archive. For a new
foreign session, use `source-installation-id~parser-session-id` and persist the
provider-qualified mapping. This is deterministic across a fresh assembly from
the same seed and captures. Refuse a collision with an existing unrelated row.
Do not rename the seed archive's sessions or message IDs to standardize their
spelling. Relationship rewrites use the same mapping, including when the parent
arrives later. Namespace collisions are errors, never overwrites.

Before collection, require reports from all four captured databases. Defer
artifact integration only if they have no `artifact_origin_id`, no
`artifact_imported_sessions`, and no transport-qualified `~` session IDs.
Otherwise stop before assembling the collector and review the required artifact
scope; do not silently disregard existing imports. Missing or unreadable
databases are unknown, not zero. Preserve an existing ordinary artifact vault
when present, even though exchange is disabled.

The remaining paragraphs in this section define the **later artifact integration
contract**, not tasks for the zero-artifact retirement slice. Keep the wire pair
`(origin, NativeSessionID)` as a transport alias. If artifact import arrives
first, registration of that publisher's source binding lets raw reparse attach
to its existing row. If raw import arrives first, the artifact alias resolves to
that row. The mapping, source links and content publication commit together.
Artifact wire validation and publisher authority remain unchanged. The collector
does not export foreign sessions as new conversations authored by itself.

A publisher without a known source binding remains explicitly unattributed.
Neither equal machine labels, equal paths nor equal message text establish the
binding. An operator may bind the publisher using the source descriptor. If two
existing rows would then collapse, report the conflict and preserve both;
automatic reconciliation of previously duplicated rows needs a separate design.

Raw retention and publication are separate. A later artifact update cannot
replace content selected from an accepted raw source merely because it arrived
last. For a raw-backed conversation, retain the artifact evidence and report a
different content revision; explicit raw reparse owns the displayed projection.
Artifact-only conversations keep their existing update behavior. A raw reparse
may update that projection through the normal sync engine, preserving curation
and reporting ambiguous message identity under the existing storage rules.

The same provider ID observed on two different source machines remains two
source observations. This slice does not silently merge their content or
curation. Hosted raw derivation already has separate source and logical-group
identities; retain that distinction. Local observation mappings must not change
the hosted group algorithm or claim to be a global cross-archive session ID.

## Collect closed captures, then reparse deliberately

Keep the current command roles: `archive import` retains originals,
`archive verify` checks retained content, `archive reparse` updates browsable
sessions, and `archive extract` recovers native files. Collection never deletes
the source. Reports distinguish retained, reparsable, supplemental, conflicting,
and failed sources instead of treating successful parsing as proof of backup.

Implement `agentsview archive capture DESTINATION` on each source machine,
supporting macOS and Linux. This is a new read-only source operation. It loads
configuration without migration or identity creation, reads
`{dataDir}/telemetry-install-id`, and captures selected Claude/Codex roots plus
the AgentsView database and assets. Repeated `--root PROVIDER=PATH` options name
explicit roots, including `files` for supplemental trees. No unbounded home
directory walk. The command writes only a new destination outside its inputs.

If an ordinary artifact vault exists, capture its complete local tree while its
owner is stopped and the capture holds its exclusive ownership lock. Refuse an
active vault or external store bindings; report the needed stopped-owner capture
instead of silently omitting that component. Do not open or migrate the original
vault with branch code. Absence is recorded explicitly. Assets referenced by the
captured application database must all be present in the completed capture.

Read existing raw-sync root IDs when available. For generated root IDs or a
missing installation ID, repeat captures take
`--identity-from PREVIOUS/capture.json`; absence must not silently mint new IDs
while claiming continuity. The descriptor records installation ID, display
label, original paths, root IDs, capture interval, tool versions, omissions and
the capture method for each database. Never collect credential values into it.

Use SQLite's online backup API for live SQLite databases, including WAL content;
record the resulting snapshot hash, not a claim of byte identity with a moving
main file. Copy regular transcript files with bounded memory, verify output
hashes, and fail incomplete if a file changes during capture. List symlinks and
special files as omissions without following them. Publish `capture.json` and
the complete inventory only after all selected regular files succeed; a failed
capture cannot be imported as complete.

Write a versioned descriptor, checksummed inventory, source bindings and
root-relative paths. Give each immutable capture its inventory digest as its
capture ID, including identity and root bindings in that digest. Reject
unsupported descriptor versions and incomplete inventories. Import consumes the
generated descriptor with `archive import --spec CAPTURE/capture.json`; the
versioned descriptor and inventory validation are new importer work. Record
`--writers-stopped` as the operator's attestation, not something inferred from a
quiet file. A rolling capture is useful evidence but is not the final retirement
cutoff, even if its individual SQLite snapshots are consistent.

Preserve every selected Claude/Codex original and companion file, including
files outside provider capture plans. Retain other providers' native trees,
historical databases as supplemental files even when they cannot be reparsed.
Exclude provider credential files, token-bearing settings and original runtime
config; write only the allowlisted recovery settings described below. Inventory
each omission and its reason. Sensitive text may still occur in transcripts and
databases; this is not transcript redaction. Original paths are provenance, not
paths the receiver is allowed to read.

Use the existing provider capture plans, canonical raw manifest validation,
content store and verified materializer. Keep objects before accepting a
manifest, and persist acceptance in SQLite before reporting custody. Keep the
current immutable-source conflict policy: changed bytes for an accepted source
are retained but do not automatically replace its accepted head. A repeated
identical import is a no-op; moving its input directory does not create roots.
Do not route offline files through the upload spool or inherit its 1 GiB budget.
New captures do not advance existing heads, including when only a shared Codex
index changed. No append advancement or shared-index fan-out fix is claimed.

Persist each capture's exact inventory even if source acceptance reports a
conflict. Add `archive extract --capture ID` to select that inventory and write
only its version of each path. Extraction without a selector is permitted only
when every retained path is unambiguous. Recovery of conflicting bytes must not
require accepting them as a source head or guessing a winner by import order.

Only Claude and Codex are initially eligible for raw reparse. Stream them
through the normal local sync engine, with its content policy, stable message
IDs, asset handling, export index and secret scanning. Keep the existing
scratch-database publication boundary. Never use the hosted parser's buffered
result as a shortcut for large local transcripts.

Select archived sources explicitly. Parser-version startup resync carries their
stored content, source mappings, curation and parse status forward unchanged. It
does not select the whole vault for reparse. The offline command owns both
SQLite and the raw vault and refuses to run while the daemon owns them. No new
background queue, lease system or worker-process vault handoff is needed here.

## Preserve curation without turning this into a general database merger

Choose one captured AgentsView database as the seed: the latest verified
snapshot of the installation being retired, taken with its assets and identity
records. Older snapshots remain supplemental evidence. A whole-archive restore
of this seed preserves session IDs, stable message IDs, stars, pins, names,
trash, export identity and every other persistent archive table. Do not seed
from whichever snapshot happens to have the largest file size.

Add `archive import --seed --spec CAPTURE/capture.json` for assembly into a new
data directory. It installs the verified seed database, assets, any closed
ordinary vault and installation identity, and persists `archive_only` before
allowing startup or normal import. It refuses an existing target; it is not a
database merge. Later `archive import --spec` calls add only source captures to
that archive. Both rehearsal and final assembly use this explicit seed step.

Import the other machines' raw captures into that isolated seed archive using
source bindings. Their captured databases and assets remain recoverable
supplemental evidence. Their stars, pins, names, insights and project edits are
**not** merged into the browsable seed archive. Report that boundary and actual
per-source counts; later pin migration must resolve the destination message
within its mapped session, not copy a source database's numeric message row ID.

Before collection, query each closed captured database with
`mode=ro&immutable=1`. Include these counts in both the capture and import
reports, bound to the captured database hash:

```sql
SELECT
  (SELECT count(*) FROM starred_sessions) AS stars,
  (SELECT count(*) FROM pinned_messages) AS pins,
  (SELECT count(*) FROM sessions
    WHERE coalesce(display_name, '') <> '') AS renamed,
  (SELECT count(*) FROM sessions WHERE deleted_at IS NOT NULL) AS trashed,
  (SELECT count(*) FROM excluded_sessions) AS deleted,
  (SELECT count(*) FROM pg_sync_state
    WHERE key = 'artifact_origin_id') AS has_origin,
  (SELECT count(*) FROM artifact_imported_sessions) AS artifact_imports,
  (SELECT count(*) FROM sessions WHERE instr(id, '~') > 0) AS qualified_ids;
```

Also report insights, manual project assignments and worktree rules using the
captured schema. An unsupported schema, missing table or missing database is
reported as unknown and blocks projection until inspected; never report zero on
query failure. No live database migration is part of preflight.

**Deletion decision:** honor source trash and permanent exclusions by default.
Retain the raw bytes and deletion evidence, but do not make those foreign
sessions browsable. Record their source-qualified parser IDs and deletion kind
in the capture and durable archive metadata. Apply this suppression before
publishing a reparse, including every branch emitted by a shared source file.
Preserve suppression through resync and restore, and count intentional skips
separately from parse errors. Do not promote foreign trash into active rows or
silently turn it into permanent deletion; its kind stays recoverable even though
importing a foreign trash view is deferred. Ambiguous ID attribution blocks that
source's projection. This small deletion filter is new work, not a general
curation merge or a claim that existing sync automatically honors foreign IDs.

The rehearsal is disposable and must receive no unique user curation. Synthetic
curation checks run in a separate copy. Keep real changes on the source archive
until its final capture. If someone curates the rehearsal, stop final assembly
and preserve that database; this slice cannot silently merge its edits back.

For the final cutoff, capture the stopped source again and assemble a **fresh**
archive from that final seed and the selected closed captures. Do not refresh
the rehearsal in place: changed inputs cannot advance its immutable heads. Reuse
source descriptors and deterministic foreign IDs; keep earlier captures and
recovery points independently recoverable. This retirement procedure assumes the
source's seed has not itself been used as the rehearsal collector. If it already
has conflicting accepted raw heads, report that blocker; do not clear its raw
ledger or silently promote a different generation.

## Use Docbank's backup format and keep mutable databases local

Replace the unreleased filesystem recovery format in PR #2036 with Docbank's
embedded portable backup API. Pin merged Docbank `4e9a5a0e` or a tested
descendant containing #741, rather than the pre-squash branch commit, and
validate the resolved Kit dependency; do not assume a merge fixes unrelated
dependency failures. Keep existing experimental recovery points and their saved
readers untouched until the replacement has passed a cold restore.

Before using that layout, require bounded streaming and chunked capture for
`BackupExtraFile`, including files over 4 GiB. Merged Kit #145 and Docbank #764
now provide this path using existing object recipes, with upstream coverage for
full verification, restore and prune. Exercise both an application database and
an ordinary-vault database. This is a named prerequisite, not functionality
supplied by the existing large-content change. Do not add gzip workarounds or a
second AgentsView backup format to bypass it. Keep it as a prerequisite for
retirement backup even if the seed alone fits. Measure the SQLite snapshot size,
backup peak memory and scratch usage after trial collection of the other
captures.

The first backup remains a stopped-owner operation. Acquire AgentsView's writer
lock, keep both embedded vaults exclusive, and prevent application mutations
until completion. Use `Vault.CreateBackup` on the raw vault. Its `Prepare`
callback creates the consistent application SQLite snapshot. Enumerate that
snapshot, identity and source descriptors, `{dataDir}/assets`, recovery
settings, and the closed ordinary artifact vault as `ExtraFiles` under
`application/`. The ordinary artifact vault uses only its local store in this
delivery; reject external store bindings rather than make an incomplete
filesystem copy.

Docbank's freeze does not freeze another database or vault. Keep the ordinary
artifact vault closed and immutable for the entire backup under the application
lock. Record exact component paths in one application inventory extra; Docbank's
manifest owns their sizes and hashes. No second pack, compression or checksum
format is needed. **Omit credentials:** neither capture nor backup copies the
original `config.toml`, provider authentication files, raw-sync tokens or
credential-bearing connection settings. Recovery settings are an explicit
allowlist: content/image retention policy and the original display label;
installation identity and `archive_only` are preserved separately. No endpoints,
tokens, cursor secrets or unrecognized config fields pass through these
settings. Keep `AllowPlaintextSecrets` false; encrypted-repository support is
not available to solve this in the current API. Report exclusions rather than
claiming a complete configuration backup. Existing preserved captures are left
unchanged; do not quietly reimport their old credential files into the new
capture format.

Backup records the repository ID, exact snapshot ID, application reader build
and snapshot minimum reader version in its recovery report. Chunked extras
require Kit reader version 6. Once such a snapshot exists, older readers cannot
use the repository at all, including its older snapshots. Restore and
backup-repository verification require `--snapshot ID`, pass that exact ID
throughout the operation, and never resolve a changing `latest` snapshot on a
shared repository. Live `archive verify` still verifies the open archive and
does not select a backup snapshot. Docbank #764 exposes the minimum reader
version. Do not record a second manifest digest: Kit validates manifest content
against its snapshot ID.

Restore opens only the backup repository and uses `BackupRepository.Restore` in
a new staging directory. It then assembles the application layout from the
restored raw vault and `application/` extras, verifies SQLite integrity, all
inventory objects, source heads and required assets, and publishes to a new
destination only after the complete check succeeds. A matching vault ID alone
does not prove that a restored copy contains the latest accepted manifests.
Failure leaves the existing destination unchanged and reports staging cleanup.

Restore writes a fresh runtime config with new local authentication, persistent
archive-only state, no remote destinations and loopback access. Historical
provider paths are provenance, not active configuration. The source's
credentials are unnecessary for recovery; restoring must never initiate its old
uploads.

The live SQLite databases and vault metadata stay on the archive host's local
disk. Initially a NAS or external drive holds closed Docbank backup repositories
and captures. It is not a shared writable AgentsView data directory. This avoids
depending on network-filesystem WAL behavior; see
[SQLite's WAL constraints](https://sqlite.org/wal.html). Moving primary object
storage to a NAS or cloud backend is separate work and must retain this same
source identity and topology-independent recovery contract.

## Relationship to work already in progress

[PR #1741](https://github.com/kenn-io/agentsview/pull/1741) adds finite
resumable raw backfill;
[PR #1751](https://github.com/kenn-io/agentsview/pull/1751) adds retained-raw
migration parity and extracts shared source/group identity logic. They are
coordination points, not assumed merged prerequisites. Review their current
heads when implementing the source bindings. Reuse their identity semantics
without copying the PostgreSQL acceptance, leasing and publication subsystem
into SQLite.

Offline import and hosted raw upload are inputs to retained raw history. They
can have different receivers and authentication while preserving source
attribution. Artifact exchange stays a normalized view of that history. This
slice records provenance for later bindings between those paths; it does not
promise that an offline receipt is a hosted acceptance receipt or that a local
collector is already a hosted raw-sync endpoint.

## Acceptance evidence before retiring the source computer

Use a fresh isolated deployment on the receiving computer and protected copies
from four real source machines. Keep the existing service, binary, databases,
vaults, credentials and provider roots untouched. Record the exact build,
descriptor and inventory hashes, source cutoffs, input counts and commands in a
private runbook. The following are completion criteria, not tests already run:

1. Run the new capture command on both macOS and Linux; read the source's actual
   installation ID, reuse root IDs on a second capture, include a live SQLite
   WAL in a consistent snapshot, and report symlink and credential omissions.
   Failed or changing inputs must not publish a complete capture. Run
   preflight on all four closed databases before collection; record counts and
   unknowns.
1. Import all four eligible sources. Browse and filter their sessions by
   original machine, including sessions created on the receiving host. Rename
   a label and relocate a capture; stable identities, accepted heads and
   counts remain unchanged. Repeat import after restart with the same result.
1. In the first slice, artifact exchange must be rejected in archive-only mode.
   Raw reparse preserves Claude forks and subagents as distinct parser
   sessions. When artifact matching is taken up, first commit the existing
   duplicate-row reproduction, then cover both ingestion orders and
   unknown/conflicting origins. Those later tests must yield one conversation
   per parser session, with existing IDs and curation preserved.
1. Start the collector in the foreground, through the config-only background
   path, and after a restart with receiving-host provider files available.
   Also try explicit ordinary sync and artifact exchange. The persisted
   archive-only mode must prevent discovery and publication after rehearsal
   and cutover alike.
1. Compare the seed database's session IDs, message IDs, stored content,
   curation, trash and assets before and after collection, backup, restore and
   full resync. Check the declared limitation on other machines' curation in
   the report. Include nonzero foreign trash and permanent exclusions: raw
   bytes remain extractable but deleted sessions stay out of the browsable
   archive after reparse, resync and restore. Preserve unsupported provider
   data.
1. Back up, make the source trees and first vault unavailable, then cold-restore
   on the receiver from the repository and an explicit snapshot ID alone, even
   if a newer snapshot exists. Extract and hash every captured file against
   its selected capture inventory, including conflicting versions and a file
   over 4 GiB. Include a backup application extra over 4 GiB; large raw
   content alone does not exercise that contract. Reparse selected Claude and
   Codex sessions, a large Codex transcript and a fork with its parent. Prove
   the parser ran; carried-forward SQLite rows alone are not reparse proof.
   Compare the allowlisted recovery settings and confirm omitted credentials
   do not reappear in the restored runtime configuration.
1. Interrupt import and reparse; retry without duplicate acceptance or partial
   publication. A missing blob, corrupt backup extra or identity conflict must
   not damage the previous browsable archive. Startup after a parser
   data-version change must carry archived-only sessions forward without
   scanning the vault.
1. Establish the final stopped-writer cutoff for the retiring computer,
   including work since the first capture. Verify two independent copies on
   different physical storage, at least one off that computer, and a cold
   restore from the final recovery point. Both copies must survive erasing the
   source computer. Assemble this final archive afresh as described above,
   including a session that changed after rehearsal; verify its final accepted
   bytes can be reparsed. Record exactly which sessions and files the cutoff
   covers, remaining exclusions, and the recovery command, repository ID,
   snapshot ID and reader version. Demonstrate that a changed shared Codex
   index is reported as a conflict in the rehearsal rather than claiming
   active-machine refresh support.

The old computer is not ready to erase merely because import succeeds or a
backup verifies. The final cutoff, complete extraction comparison, preserved
curation, stable attribution and independent-copy restore are the decision
evidence. Deletion remains an explicit user action. Long-term preservation also
requires retaining readable formats and periodically checking the copies; this
delivery cannot promise permanent storage without that maintenance.
