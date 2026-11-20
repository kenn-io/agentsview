---
title: Artifact Folder Sync
description: Exchange normalized sessions between AgentsView archives through a trusted folder
---

You can exchange normalized AgentsView sessions between machines through any
trusted folder that both machines can access, such as a mounted NAS or an
external drive.

!!! warning "Early workflow"

    This is an initial manual transport, not a complete synchronization product. It
    has no watcher, schedule, hosted peer, or object-storage mode, and it does not
    exchange raw provider files or mutable curation. Use it only for controlled
    experiments between archives whose folder storage and operators you trust.

```bash
agentsview source sync --target /path/to/shared-folder
```

Artifact folder sync is explicit and opt-in. If you run `agentsview source sync`
without `--target`, AgentsView does not create the local artifact repository,
read an exchange folder, or change ordinary provider sync behavior.

!!! warning

    The target holds normalized session messages and related metadata. It can
    include sensitive prompts, responses, tool activity, and usage data. Use
    only a folder whose storage and access controls you trust.

## First use

Before you start, make sure the target path either does not exist yet or points
to an empty directory. On first use, AgentsView creates the folder and writes an
`.agentsview-artifacts.json` namespace marker with a randomly generated
namespace identity to bind local continuation state to that exact target. Later
runs refuse an unmarked nonempty directory instead of adopting unrelated files.

Run the sync command once on each participating machine:

```bash
# Machine A
agentsview source sync --target /Volumes/team-agentsview

# Machine B, after the same folder is available there
agentsview source sync --target /mnt/team-agentsview
```

The folder path can differ between machines. On each run, the archive pulls
verified immutable objects already in the folder, publishes its own objects, and
imports supported peer checkpoints into its local SQLite archive. Publication
also appends an immutable change-journal entry. Each machine keeps a
target-bound journal cursor, so repeating the command processes new entries
instead of rescanning and rehashing the folder's full history. Changed sessions
publish a new revision.

The command enforces object-count and decoded-byte budgets for both inbound and
outbound transport work. Because continuation cursors are durable, later runs
resume after the last accepted object instead of restarting from the first
object. If the summary reports that artifact work remains, run the same command
again:

```text
Artifact work remains; run the sync command again.
```

To request the existing full local resync along with a full artifact export,
pass `--full`:

```bash
agentsview source sync --full --target /path/to/shared-folder
```

If a writable local daemon owns SQLite, the CLI asks that daemon to run the
exchange after normal session sync. Otherwise, direct mode runs the same
operation while holding the local write-owner lock.

## What the folder contains

The exchange folder holds normalized, versioned AgentsView artifacts. It does
not store copies of provider-owned JSONL files and is not a raw-source backup.
Artifact sync does not modify or delete the JSONL files that your sessions were
parsed from.

This initial folder transport intentionally leaves out:

- continuous watching or automatic schedules;
- HTTP peers, hosted services, or object-storage transports;
- user curation and other mutable metadata;
- archival and restoration of raw provider sources; or
- provider-file eviction or deletion.

Those boundaries keep ordinary AgentsView behavior unchanged and keep the folder
protocol focused on repeatable normalized-session exchange.

## Machine identity

Each local archive keeps a durable artifact origin in `sessions.db`. Do not copy
an entire AgentsView data directory or `sessions.db` to set up a second active
machine, because copying it also copies publication authority. Let each machine
create and keep its own data directory, then exchange sessions through the
shared folder.

Copying `config.toml` alone does not copy the artifact origin. If you restore a
database from backup, keep only one active writer for that restored origin.

## Operational notes

- The exchange folder is a transport, not the local system of record. Each
  machine keeps imported sessions in its own SQLite archive.
- Objects are content-addressed and immutable. Conflicting content under an
  existing identity fails closed.
- Journal entries are immutable and sequence numbered. AgentsView advances a
  local cursor only after it reads, verifies, and accepts the referenced object,
  so interruption and retry are safe.
- AgentsView quarantines invalid complete objects so one corrupt peer artifact
  does not permanently block unrelated origins.
- Cooperating AgentsView processes serialize folder operations. Do not edit the
  folder contents manually while an exchange is running.
- Keep the target outside the AgentsView data directory and every configured
  provider root. AgentsView rejects overlapping paths.
