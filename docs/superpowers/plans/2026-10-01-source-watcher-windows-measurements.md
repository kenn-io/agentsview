# Windows measurement handoff

Use `scripts/measure-source-watch.py` from draft PR #2055. The same script and
full workload are used on Linux and macOS, so the report compares equivalent
inventories and cache representations. No AgentsView build is needed. Read the
[measurement explanation and limitations](2026-10-01-source-watcher-mac-measurements.md#what-the-comparison-answers)
before interpreting the results.

## Prompt for the Windows agent

```text
Run the source metadata and disposable-cache experiments from:
https://github.com/kenn-io/agentsview/pull/2055

Read the Windows measurement handoff. Download scripts/measure-source-watch.py
from the PR's current head into a new directory under the current user's local
profile. Record the commit hash in the context report. No checkout or build is
needed. Use an existing Python 3.9+ installation. Do not install dependencies,
change PowerShell execution policy, or request administrator privileges.
If Python is missing, report that prerequisite.

Run the smoke measurement, then three full measurements while ordinary heavy
applications such as builds, containers, browsers, and agents are running.
Use the same 50,000 synthetic files and 50,000/250,000 cache records as Linux
and Mac. Do not reduce cardinality to obtain faster results. Do not stop
applications, suspend services, change antivirus exclusions, clear caches,
or manufacture an idle machine. Use an existing scratch parent on the source
filesystem where possible and allow about 500 MiB plus filesystem overhead.

Live source access is limited to directory listings and file metadata. Do not
read transcripts, open provider databases, inspect credentials, modify live
archives, replace binaries, or restart daemons. The script skips symlinks and
Windows reparse points, including junctions. It is not a sandbox against
concurrent path replacement. Use stable recognized roots.

Return windows-source-watch-busy-1.json, windows-source-watch-busy-2.json,
windows-source-watch-busy-3.json, and windows-source-watch-context.md.
Inspect outgoing files. Include only aggregates and generic machine facts.
Exclude usernames, hostnames, absolute paths, filenames, project names,
transcripts, volume labels, serial numbers, and device identifiers.

Report missing roots, skipped reparse points, errors, and truncated inventories.
Do not label a partial inventory complete. Native Windows watcher correctness,
Go memory retention, and production restart savings remain unmeasured.
```

## PowerShell commands

Run from the directory containing the downloaded script. With an existing
Python launcher, use these commands. If the machine has `python` rather than
`py`, substitute `python` for `py -3` after checking its version.

```powershell
py -3 --version
New-Item -ItemType Directory -Path measurement-report -ErrorAction Stop
py -3 measure-source-watch.py --skip-live --synthetic-files 100 --rows 100 600 --passes 2 |
    Out-File -Encoding utf8 measurement-report/smoke.json
if ($LASTEXITCODE -ne 0) { throw 'Smoke measurement failed' }

foreach ($run in 1..3) {
    py -3 measure-source-watch.py |
        Out-File -Encoding utf8 "measurement-report/windows-source-watch-busy-$run.json"
    if ($LASTEXITCODE -ne 0) { throw 'Full measurement failed; retain the failed report locally' }
}
```

The script emits ASCII JSON; Windows PowerShell 5.1 may add a UTF-8 BOM to
redirected reports. Consumers can read them with `utf-8-sig`. No script execution
policy change is needed because PowerShell invokes Python directly.

Defaults cover home-relative `.claude/projects`, `.codex/sessions`,
`.pi/agent/sessions`, `.cursor/projects`, and `.local/share/opencode`. These are
conventional locations, not proof of the installed provider configuration.
Replace defaults with repeated `--root` arguments for known custom locations.
Do not dump configuration or environment variables to locate or report them.
Root paths never appear in JSON; keep their private mapping locally.

Use `--scratch-parent` with an existing directory to select the target
filesystem. The script creates and removes only its own temporary child.
The system temporary directory may be on a different volume. If a root or
ancestor is a junction or another reparse point, the script skips it. A directly
addressed physical root can be measured separately if known; do not bypass
skipping by following arbitrary links. Cloud placeholders may also be skipped,
so record whether sources are under cloud synchronization generically.

Keep artifacts in the current user's local profile with its existing access
controls. POSIX `umask` is not a Windows ACL boundary; the script does not
modify ACLs. Do not place reports or synthetic caches in shared folders.

## Context to return

Report Windows version, architecture, RAM capacity, generic storage class,
source/scratch filesystem types, whether they share a volume, and concurrent
workload categories. Mention network, cloud-synchronized, or encrypted storage
if relevant, without identifiers. Describe workload changes and any noticeable
impact on normal interaction. Do not attribute an observed slowdown to the
watcher, which this script does not run.

Windows has no `os.getloadavg`; the corresponding report fields are `null`.
Logical CPU count is still recorded. Supply workload context rather than
inventing a load-average equivalent. If Task Manager shows substantial CPU,
memory, or disk pressure, summarize it in prose; do not return screenshots
containing process, account, or machine names. Do not change protection settings
to improve numbers. File creation and scanning costs should reflect ordinary
host operation.

Errors and truncation are part of the result. Return them with their scope
rather than resizing the workload or treating them as successful qualification.


## Returned Windows results

The received portable report contains the smoke run, three full baseline runs,
four supplemental experiment results, raw aggregate JSON, and experiment source.
All eight JSON blocks parsed. The pinned baseline script at
`5e4618b0eeeaf2dc099afb6611caf9e9d32c9eae` has SHA-256
`015a8042a62a1764ba83f49b1ec1d575c0b31c13e460f661e1d7210486a888b2`,
which matches the repository blob. Aggregate arithmetic and canary assertions
were checked against the supplied JSON; experiments were not rerun here.

The host was Windows 11, x64, 24 logical CPUs, about 128 GiB RAM, and a local
SSD using NTFS. Source and scratch shared a volume. Normal browser, agent, and
editor/runtime processes remained present. Snapshots did not establish
sustained contention; responsiveness was not assessed. The baseline used Python
3.14.7 and SQLite 3.53.1. Its smoke and three full runs reportedly exited zero
with empty stderr, retaining the full cardinality. Full runs took 87.31-90.05 s.

Live coverage was small: only Codex existed at the default roots, with 33
candidates in 17 directories. Four missing roots reported errors. No truncation
or skipped links were reported in retained first/last passes. These are not
large live-provider coverage results; synthetic inventories carry the scale
comparison.

| Baseline measurement | Range across three runs |
| --- | --- |
| Create 50,000 files in 196 directories | 14.952-16.570 s |
| Warm individual path-stat median | 3.624-3.670 s |
| Warm listing-only median | 83.7-94.0 ms |
| Warm enumeration plus stat median | 112.8-119.8 ms |

Every baseline representation round-tripped all records, and database sizes
matched Linux and Mac. Compact directory-ID rows used 5.62/28.04 MiB at
50,000/250,000 files; prefix pages used 3.95/19.70 MiB. The large difference
between individual checks and enumeration is an API-semantics distinction,
not evidence that equivalent fresh observations are cheap on Windows.

### Freshness and identity constraints

The supplemental native probe compared full closed-file signatures with
independent individual handle queries. Bulk enumeration included actual change
time and 128-bit file IDs, with zero mismatches on 50,000 closed files. Python
DirEntry metadata omitted IDs in this sample, and its ctime was creation time,
not Windows change time. A production signature must retain volume identity,
all 128 file-ID bits, and an explicit flag when identity/change time is unknown.
Prototype disk sizes do not include this final signature representation.

Three open-writer append canaries are decisive counterexamples to using listing
metadata as a fresh known-file observation. Native bulk and Python enumeration
both returned size four; individual handle and path queries returned size five.
The parent directory signature did not change. Enumeration after an individual
query returned five, so query order can hide the stale-listing behavior. All
closed-writer listings matched the handle oracle. Fast listing metadata may
support discovery; it must not suppress fresh checks for known candidates.

Two of three rapid same-size overwrite canaries retained the compared write
and change timestamps even after close. The experiment establishes unchanged
signatures, not their cause. Explicit native dirty paths must still reach
owning content verification, and lost-history recovery must retain scoped
content verification. Stat-only absence of change is not content proof.
Native notification delivery was not measured.

### Scope, bounded workers, and cache writes

| Supplemental operation | Median wall time | Qualification limit |
| --- | --- | --- |
| Full individual native signatures, 50,000 files | 5.650 s | Scratch-only Python/Win32 |
| Same checks scoped to 768 files in three directories | 87.6 ms | Models scope, not actual coordinator routing |
| Closed-file bulk metadata, 8 KiB / 64 KiB buffers | 125 / 128 ms | Stale open-writer entries remain possible |
| One stat worker, 50,000 signatures | 6.009 s; 5.734 CPU s | Separate worker experiment |
| Two stat workers | 4.448 s; 8.250 CPU s | 26.0% less wall time, 43.9% more CPU time |
| Four stat workers | 4.320 s; 9.094 CPU s | Little further wall-time benefit |

Workers submitted at most one 256-record page and preserved every signature.
These Python/ctypes results justify qualifying a small pool only if serial Go
checks fail latency requirements. They do not justify enabling one by default
or trading more CPU for a nominal benchmark win. Physical routing, selective
work, cancellation, and fair progress between units come first. A background
coverage pass must not delay unrelated pending event work until all files have
been checked; qualify service between bounded pages with the real coordinator.

Selective 256-record cache reads had medians of 0.278-0.451 ms for compact rows
and 0.323-0.435 ms for prefix pages. One-file update medians were 4.513-5.001 ms
and 4.918-5.783 ms respectively. Row storage changed one record; page storage
re-encoded 256. These small samples do not establish a strong throughput ranking.
Both sampled 4,616 journal bytes, not a peak bound. Ordinary acknowledged file
changes should update only existing signatures or affected pages; reserve full
listing generations for membership or completeness changes.

For equal-length high-entropy synthetic names at 250,000 files, compact rows
used 29,396,992 bytes and prefix pages 28,663,808 bytes. The latter saved only
2.49%, compared with about 30% for repetitive rollout-style names. This supports
compact rows as the first format to qualify and reinforces the need to measure
actual name distributions before adding a codec.

### Remaining evidence limits

The metadata/cache optimization suite emitted complete JSON and checks, but its
launcher did not retain the final exit status after an earlier interrupted
launch. Its status is unrecorded, not a verified successful exit. The separate
open-writer, worker, and scoped-signature probes reportedly exited zero with
empty stderr. Their source and raw JSON were inspected, not executed locally.

All probes are synthetic, short, and on high-capacity hardware. Their oracles
retain full inventories, so they do not qualify bounded production memory.
Native event delivery, overflow recovery, sustained contention, Go latency and
CPU, interactive impact, final schema/caps, and actual restart gains remain
unmeasured. Unchanged post-delete size and journal samples do not establish
the total storage envelope. The design gates remain implementation work.
