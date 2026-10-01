# Mac measurement handoff

For Windows, use the [PowerShell handoff](2026-10-01-source-watcher-windows-measurements.md)
with the same Python script and full workload.

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
full measurement while your normal heavy applications are running. Use the
same 50,000-file and 50,000/250,000-record workloads as Linux, on the Mac you normally use for builds and agents. A reduced smoke run checks setup; it is not the design benchmark. Do not
close applications, stop services, or wait for an artificially idle machine.
Repeat the full run three times during representative work and retain all three
reports. An optional idle baseline is useful for comparison, but does not
replace the loaded measurements. Use an existing scratch parent on the same filesystem as
the source collection when possible. The script creates and removes only its
own temporary child directory. Do not clean up other processes or directories.
Do not install Python if it is missing; report the missing prerequisite.

Return the three mac-source-watch-busy JSON reports and mac-source-watch-context.md. Inspect both
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
python3 measure-source-watch.py > measurement-report/mac-source-watch-busy-1.json
python3 measure-source-watch.py > measurement-report/mac-source-watch-busy-2.json
python3 measure-source-watch.py > measurement-report/mac-source-watch-busy-3.json
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

Return the three `mac-source-watch-busy-*.json` reports and a short
`mac-source-watch-context.md` with:

- Mac architecture, macOS version, memory capacity, and storage class such as
  internal SSD or external HDD. Avoid identifying hardware details.
- Whether sources and scratch are on APFS, case-sensitive APFS, a network mount,
  or another filesystem; whether they share a volume. Inspect locally and
  return only those generic properties.
- The concurrent workload in generic terms, such as compiling, containers,
  browser use, or active agent sessions. Record whether normal interaction was
  noticeably affected by the measurement, and whether the workload changed
  between runs. Do not attribute a slowdown to the watcher, which is not running.
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

The report records logical CPU count and start/end 1-, 5-, and 15-minute load
averages where available; Windows reports `null` for those fields. Load average includes runnable and some blocked work and is not CPU
utilization or a complete account of memory and disk contention. Supply the
workload context alongside it. Do not dismiss slow loaded results as noise or
reduce file cardinality to make them pass. Carry them into implementation
qualification. A slow Python probe alone does not prove the future Go watcher
fails; qualification must exercise the actual scanner under comparable load.

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
as the best persistence mechanism. Use the platform evidence below before
selecting the production representation and budget in the
[design](../specs/2026-10-01-central-source-watcher-design.md) and
[implementation plan](2026-10-01-central-source-watcher.md).


## Returned Mac results

The returned `mac-source-watch-handoff.md` report identifies script revision
`5e4618b0eeeaf2dc099afb6611caf9e9d32c9eae`. It reports a successful smoke run
and three full default runs, each exiting zero with empty stderr. Only the
aggregate Markdown report was supplied; the raw JSON reports were not supplied
for independent validation. Results below are reported measurements, with sums
and ranges calculated from its tables.

The host was an arm64 Mac with 18 logical CPUs, 128 GiB RAM, internal SSD, and
APFS. Source and scratch storage shared a volume. Python was 3.14.7 and SQLite
3.53.1. Normal desktop applications, coding agents, a VM, and development
services remained running. A build began before the third run. Start/end
one-minute load averages ranged from 5.61 to 6.27. This is a short concurrent
workload observation on a high-capacity Mac, not evidence of sustained contention
or qualification on a smaller machine.

The report notes elevated filesystem event and indexing service CPU after
synthetic file creation. Its attribution to the experiment is inferred, not
measured. Interactive responsiveness was not assessed. Cache and source timings
alone therefore do not account for the experiment's total host impact.

| Reported measurement | Range across three full runs |
| --- | --- |
| Full-run elapsed time | 11-12 s; about 35 s total |
| Default source candidate files / traversed directories | 19,922 / 1,010; stable across all passes |
| Source enumeration errors, symlinks, truncation | None reported |
| Sum of each root's subsequent-pass wall-time median | 94.7-107.9 ms |
| Synthetic file creation, 50,000 files / 196 directories | 2.262-2.425 s |
| Synthetic warm file-stat median | 101.9-112.0 ms |
| Synthetic warm listings-only median | 21.1-22.2 ms |
| Synthetic warm enumeration and stat median | 119.7-124.9 ms |

The sum of root medians is not the median of a timed all-root pass. The candidate
inventory is broad and is not parser coverage. Its relative filenames total
1,679,272 bytes, while basenames total 1,359,882 bytes. Directory-prefix removal
alone accounts for about 19% of those filename bytes. The larger storage gains
in the synthetic layout comparison also remove duplicated root and parent
strings and redundant indexes. Do not extrapolate a 75% saving to every real
filename distribution.

Reported database byte counts matched the Linux prototype at both scales in
all three runs. Directory-ID rows used 5.62 MiB for 50,000 files and 28.04 MiB
for 250,000. Prefix-coded pages used 3.95 MiB and 19.70 MiB. Every layout reportedly
round-tripped every signature; deletion and refill left file sizes unchanged.
Journal values were samples, not verified peak bounds.

| Layout | Files | Listing fill | Delete and refill | Reopen and full read |
| --- | --- | --- | --- | --- |
| Directory IDs and basenames | 50,000 | 86-87 ms | 82-88 ms | 27 ms |
| Prefix-coded pages | 50,000 | 136-166 ms | 123-125 ms | 20 ms |
| Directory IDs and basenames | 250,000 | 460-492 ms | 471-499 ms | 134-140 ms |
| Prefix-coded pages | 250,000 | 692-758 ms | 688-735 ms | 98-105 ms |

The evidence confirms why the production design must avoid duplicated full
paths. It does not select prefix-coded pages automatically: at 50,000 files
they save about 1.66 MiB over compact rows, while compact rows fill faster and
already use shared directory prefixes. Prefer compact rows as the first
candidate to qualify; adopt page coding only if final-schema measurements
justify its codec and page-replacement costs. Keep only the selected format.

Cheap warm metadata passes do not establish a need for persistent caching.
Compare actual uncached startup, durable reopen, selective changes, and eviction
with equivalent provider work before deciding. These runs do not qualify native
FSEvents behavior, production Go memory, sustained backlog, restart savings,
interactive impact, cold storage, or the complete storage envelope. Windows
results remain outstanding.
