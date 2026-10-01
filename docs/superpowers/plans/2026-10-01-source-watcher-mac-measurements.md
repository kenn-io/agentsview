# Mac measurement handoff

Give the prompt below and `scripts/measure-source-watch.py` to an agent on your
Mac. The script needs Python 3.9 or newer and its standard library. It measures
source metadata and compares three disposable cache layouts. It does not run a
watcher or parse sessions. You can copy these two files without checking out or
building AgentsView.

## Prompt for the Mac agent

```text
Measure the scan-first watcher proposal on this Mac using the attached
measure-source-watch.py. Read this guide first. Do not implement the watcher.

You may enumerate provider source directories and read file metadata. Do not
read transcript contents, open live provider databases, inspect credentials,
write to the archive, restart daemons, replace installed binaries, use sudo,
or clear OS caches. Skip symlinks. Keep source paths and filenames private.
The script avoids static symlinks but is not a sandbox against concurrent path
replacement. Run it against stable roots you recognize.

Create a private report directory. Run the smoke measurement first, then the
full measurement. Use an existing scratch parent on the same filesystem as
the source collection when possible. The script creates and removes only its
own temporary child directory. Do not clean up other processes or directories.
Do not install Python if it is missing; report the missing prerequisite.

Return mac-source-watch.json and mac-source-watch-context.md. Inspect both
before returning them. Include only aggregate measurements and generic machine
characteristics. Omit usernames, hostnames, absolute paths, project names,
filenames, transcript text, volume names, serial numbers, and identifiers.
Report errors and truncation rather than calling a partial inventory complete.
Leave native FSEvents correctness and sustained watcher memory unmeasured.
```

## Commands

Run from the directory containing the script. In a repository checkout, use
`scripts/measure-source-watch.py` in place of `measure-source-watch.py`.

```bash
umask 077
mkdir -p measurement-report
python3 measure-source-watch.py --skip-live --synthetic-files 100 \
  --rows 100 600 --passes 2 > measurement-report/smoke.json
python3 measure-source-watch.py > measurement-report/mac-source-watch.json
```

The default inventory examines these conventional source directories relative
to the home directory. Configured custom locations and other providers require
explicit roots. Missing defaults appear as errors, not as empty collections.

| Provider | Relative directory |
| --- | --- |
| Claude | `.claude/projects` |
| Codex | `.codex/sessions` |
| Pi | `.pi/agent/sessions` |
| Cursor | `.cursor/projects` |
| OpenCode | `.local/share/opencode` |

Use repeated `--root` arguments to replace the defaults. Each is reported as
`root-1`, `root-2`, and so on. Keep their private mapping locally. Use
`--scratch-parent` to select an existing directory on the filesystem being
measured. The default system temporary directory might be on a different
volume. Neither argument appears in the JSON report.

The full workload creates 50,000 empty files and tests 50,000 and 250,000
metadata records per layout. Allow about 500 MiB of free scratch space, plus
filesystem overhead for the empty files. Temporary artifacts are removed when
the script completes normally or handles an exception. An interrupted or killed
process may leave its private child directory behind. Keep the JSON report.

The live walk stops after one million directory entries per root per pass.
`--max-entries` changes that limit. Candidate matching accepts `.jsonl`, `.json`,
`.txt`, `.db`, and `.db-wal`; it intentionally overcounts parser inputs. Errors,
symlinks, and truncation are explicit. Broad enumeration may be expensive in a
source tree containing unrelated generated files. Choose narrower explicit roots
when that better represents your configured providers.

## Return these files

Return `mac-source-watch.json` and a short `mac-source-watch-context.md` with:

- Mac architecture, macOS version, memory capacity, and storage class such as
  internal SSD or external HDD. Avoid identifying hardware details.
- Whether sources and scratch are on APFS, case-sensitive APFS, a network mount,
  or another filesystem; whether they share a volume. Inspect locally and
  return only those generic properties.
- Whether the machine was idle, busy, or actively recording sessions.
- Whether the conventional roots covered the collection, and any missing
  provider types. Describe custom roots generically.
- Any errors, truncated inventories, or workload changes. Do not copy raw error
  messages containing paths into the returned context.

Do not return source listings, environment dumps, raw disk reports, or caches.

## What the comparison answers

`full_paths` reproduces the wasteful baseline. Each row stores the root,
relative path, and parent path, with primary and parent indexes. `interned_dirs`
stores each directory once as a parent ID and basename. File rows use directory
IDs and basenames in a `WITHOUT ROWID` table, eliminating the extra path index.
`prefix_pages` stores directory IDs and sorted listing pages of at most 256
files. Within each page, a basename stores its common-prefix length with the
previous name and the remaining suffix. Both compact layouts include directory
metadata; all three preserve size, mtime, ctime, and inode for every file.

The script verifies signatures after deletion, refill, and reopening. It reports
main-file bytes, listing-fill time, whole-listing churn time, reopen plus full-read time,
and sampled journal bytes. The `build_s` timer excludes schema and directory-table
initialization. These are representation experiments. They omit
production generations, plan fingerprints, activity accounting, selective page
lookups, and directory-tree indexes. Do not treat their disk sizes as a complete
watcher estimate or their read times as a measured startup saving.

The synthetic tree has 256 files per directory and deliberately repetitive
rollout-style basenames. Real names may share less prefix. Long paths, sparse
one-file directories, nested trees, and changed pages need further qualification
with the eventual implementation. The journal sample includes a deliberately
large whole-cache delete transaction to expose a cost that insertion-only probes
miss. Sampling does not establish a universal peak or enforce the proposed cap.

The first live pass runs in a fresh process, but the kernel may already have
cached metadata. Subsequent passes are warm medians. Synthetic files were just
created, so their metadata measurements are warm. Recent mtimes are an activity
proxy, not change-event counts. The script does not measure FSEvents delivery,
overflow recovery, production Go CPU, retained heap, or multi-hour physical
memory. Those checks need an isolated watcher implementation later.

## Linux reference

A Linux/XFS run on 2026-10-01 with Python 3.14.7 and SQLite 3.53.1 produced:

| Layout | 50,000 files | 250,000 files | Bytes per file at 250,000 |
| --- | --- | --- | --- |
| Full paths and duplicated indexes | 16.10 MiB | 80.84 MiB | 339.1 |
| Directory IDs and basenames | 5.62 MiB | 28.04 MiB | 117.6 |
| Directory IDs and prefix-coded pages | 3.95 MiB | 19.70 MiB | 82.6 |

Every layout passed the full signature roundtrip check. Prefix-coded pages
reduced this baseline by about 75%. This establishes that repeated prefixes and
indexes materially inflated the earlier estimate. It does not establish SQLite
as the best persistence mechanism. Compare Mac results before selecting the
production representation and budget in the
[design](../specs/2026-10-01-central-source-watcher-design.md) and
[implementation plan](2026-10-01-central-source-watcher.md).
