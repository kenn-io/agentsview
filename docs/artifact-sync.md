---
title: Artifact Folder Sync
description: Exchange normalized sessions between AgentsView archives through a trusted folder
---

Artifact folder sync exchanges normalized AgentsView sessions between machines
through a folder that both machines can access. The folder can be a mounted NAS,
an external drive, or another trusted filesystem location.

```bash
agentsview sync --target /path/to/shared-folder
```

This feature is explicit and opt-in. Running `agentsview sync` without
`--target` does not create the local artifact repository, read an exchange
folder, or change ordinary provider sync behavior.

!!! warning

    The target contains normalized session messages and related metadata. It can
    contain sensitive prompts, responses, tool activity, and usage data. Use only a
    folder whose storage and access controls you trust.

## First use

The target must either not exist or be an empty directory. On first use,
AgentsView creates it and adds an `.agentsview-artifacts.json` namespace marker.
Later runs refuse an unmarked nonempty directory rather than adopting unrelated
files.

Run the command once on each participating machine:

```bash
# Machine A
agentsview sync --target /Volumes/team-agentsview

# Machine B, after the same folder is available there
agentsview sync --target /mnt/team-agentsview
```

The path can differ between machines. Each archive pulls verified immutable
objects already in the folder, publishes its own objects, and imports supported
peer checkpoints into its local SQLite archive. Repeating the command is safe:
already accepted content is verified and skipped, while changed sessions publish
a new revision.

The command performs a bounded amount of work so concurrent provider writes
cannot keep it running forever. If the summary says that artifact work remains,
run the same command again:

```text
Artifact work remains; run the sync command again.
```

`--full` requests the existing full local resync and a full artifact export:

```bash
agentsview sync --full --target /path/to/shared-folder
```

When a writable local daemon owns SQLite, the CLI asks that daemon to perform
the exchange after normal session sync. Otherwise, direct mode performs the same
operation while holding the local write-owner lock.

## What the folder contains

The folder contains normalized, versioned AgentsView artifacts. It does not
contain copies of provider-owned JSONL files and is not a raw-source backup.
Artifact sync does not modify or delete the JSONL files from which sessions were
parsed.

This initial folder transport intentionally does not include:

- continuous watching or automatic schedules;
- HTTP peers, hosted services, or object-storage transports;
- user curation and other mutable metadata;
- archival and restoration of raw provider sources; or
- provider-file eviction or deletion.

Those boundaries keep ordinary AgentsView behavior unchanged and keep the folder
protocol focused on repeatable normalized-session exchange.

## Machine identity

Each local archive keeps a durable artifact origin in `sessions.db`. Do not copy
an entire AgentsView data directory or `sessions.db` to create a second active
machine: doing so also copies publication authority. Let each machine create and
retain its own data directory, then use the shared folder to exchange sessions.

Copying `config.toml` alone does not copy the artifact origin. If a database is
restored from backup, keep only one active writer for that restored origin.

## Operational notes

- The exchange folder is a transport, not the local system of record. Each
  machine retains imported sessions in its own SQLite archive.
- Objects are content-addressed and immutable. Conflicting content under an
  existing identity fails closed.
- Invalid complete objects are quarantined so one corrupt peer artifact does not
  permanently block unrelated origins.
- Folder operations are serialized between cooperating AgentsView processes. Do
  not edit the folder contents manually while an exchange is running.
- Keep the target outside the AgentsView data directory and every configured
  provider root. Overlapping paths are rejected.
