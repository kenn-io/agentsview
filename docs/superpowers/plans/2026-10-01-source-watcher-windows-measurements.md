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
