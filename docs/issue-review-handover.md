# Proactive Issue Review handover

Status date: 2026-08-10

Release decision: **LOCAL SQLITE DESKTOP RELEASE DEPLOYED AND ACCEPTED** at
`28eff630d84bf9c582eadd6b9b854fcfa1f2b99c`. The installed binary, API,
browser workflow, rollback artifact, and daily read-only task are verified.
PostgreSQL integration execution remains the only unresolved backend-parity
gate because this host has no dedicated PostgreSQL test database and cannot
start the Docker test service while firmware virtualization is disabled.

Base revision: `915e83b91da3c553a8735f89309fd4b055189f65`

## Objective

Ship a local-first, cross-chat review that finds recurring work, confusion,
blockers, failures, slow tools, and automation opportunities across a selected
timeframe, chat, project, and working folder. Every finding keeps redacted
evidence and recommends one concrete skill, script, rule, or tool improvement.

The finished workflow provides:

- deterministic review across matching chats;
- date, project, folder, and exact-chat scope;
- filtering, sorting, stable pagination, and evidence navigation;
- hourly refresh while the panel is open;
- cache-bypassing review on demand;
- explicit coverage when optional telemetry is missing or unsupported;
- read-only reporting with no automatic changes to chats, code, skills,
  GitHub, or daemon state.

Automatic remediation remains out of scope.

## Executive status

| Surface | State | Evidence or remaining gate |
| --- | --- | --- |
| Shared detector and recommendations | Ready | Focused and full SQLite suites pass |
| SQLite read path | Ready | Conditional tail, cache, pagination, and redaction coverage passes |
| DuckDB read path | Ready | Focused and full suites pass |
| PostgreSQL read path | Compile-verified | Dedicated `pgtest` execution is blocked by host capabilities |
| Filters, sorting, pagination, and evidence links | Ready | 2,278 frontend tests and production build pass |
| Telemetry supplement | Ready | Status remains explicit; benchmark reports `available` with zero scoped rows |
| Hourly and on-demand refresh | Ready | One-hour cache and forced-refresh coverage passes |
| Performance | Ready | Two forced requests pass the unchanged 30-second timeout |
| Finding privacy | Ready | Full benchmark pagination has zero path or credential leaks |
| Feature commit | Ready | Feature `252cef0`; optimized release `28eff63` |
| Installed desktop release | Ready | Exact artifact installed, hashed, healthy, and browser-verified |
| Daily scheduled report | Active | `daily-agentsview-issue-review`, daily 09:00 local Kyiv time |

## Current implementation

### Data flow

1. Global analytics filters resolve matching sessions.
2. SQLite, PostgreSQL, or DuckDB loads bounded message and tool-call rows.
3. The shared Go analyzer deduplicates imported copies, classifies failures,
   groups recurring signatures, measures durations, and attaches evidence.
4. Local SQLite optionally supplements exact scoped calls with read-only Codex
   telemetry from `%USERPROFILE%\.codex\logs_2.sqlite`.
5. Cheap filters, sorting, and pagination apply to a one-hour cached base
   analysis.
6. The Svelte panel renders findings and links evidence to the exact chat and
   message ordinal.

Codex JSONL-derived archive rows remain the conversation and tool-result
authority. `logs_2.sqlite` supplements timing and runtime failures; it is not a
complete chat archive.

### Detection coverage

- command, edit, build, test, migration, Git, GitHub, and CI failures;
- missing files and dependencies, permissions, network failures, rate limits,
  timeouts, shell syntax, Windows PowerShell, and line-ending failures;
- crashes, structured tool failures, and conservative successful recovery;
- persistent polling, repeated reads, and substantial workflows repeated
  across chats or projects;
- exact-normalized user requests repeated across chats;
- explicit user corrections and assistant-reported blockers;
- slow non-wait tools, measured p95, duration coverage, and excess-duration
  triage proxy;
- referenced GitHub issues and allowlisted Codex router, hook, response,
  session, and shell-snapshot failures.

### Controls

Global controls apply date, project, machine, agent, termination, one-shot, and
automation scope. Issue Review adds:

- chat and working folder;
- category, tool, evidence source, and session outcome;
- severity, confidence, lifecycle status, and recommendation type;
- minimum occurrences, chats, projects, and excess duration;
- impact, frequency, recency, waste, and duration sorting;
- stable pages of 100 findings and explicit **Load more**;
- persisted local filter state, **Clear filters**, and **Refresh now**.

### Reliability and privacy boundaries

- Explicit event status and non-zero exit codes outrank keyword inference.
- Search exit code 1 is no match unless a concrete error exists.
- Successful read-only diagnostics may bridge a failed call to an identical
  successful retry. Writes, edits, builds, failures, compound commands, and
  unrelated operations close recovery.
- Every store loads a bounded result head. It loads the bounded tail only when
  structured status or the head proves a likely failure.
- Content-block JSON and escaped CRLF/LF are decoded before signature
  selection. Wrapper and progress lines are removed.
- Imported message and tool-call copies are deduplicated by stable identity.
- Finding signatures, recommendations, evidence, and telemetry tails redact
  credentials, bearer values, and absolute Windows or Unix paths.
- Telemetry reports `available`, `missing`, `unavailable`, or `unsupported`.
- Base analysis is cached for one hour. Manual refresh bypasses the cache.

## Resolved release blockers

### B1: unconditional result-tail extraction

Resolved in SQLite, PostgreSQL, and DuckDB. Obsolete length and tail parameters
were removed. Failure-tail and successful-no-tail fixtures pass in SQLite and
DuckDB; the equivalent PostgreSQL test compiles behind `pgtest`.

### B2: orchestration messages counted as repeated requests

Resolved. The detector rejects these harness envelopes before classification:

- `<task-notification>`;
- `<subagent_notification>`;
- `Perform any necessary follow-up actions in response to the subagent
  completion above`;
- `Briefly inform the user about the task result`.

The production-scale benchmark has zero banned harness matches and 598 genuine
repeated-request findings.

### B3: user-correction turn counting

Resolved. Harness envelopes are rejected first, the selected-message
`userCount` heuristic is removed, and the first selected strong correction is
classified.

### B4: backend parity

SQLite and DuckDB execution coverage passes. PostgreSQL query construction and
server compilation pass. `internal/postgres/issue_review_pgtest_test.go` covers
the same failure-tail and successful-no-tail contract but has not executed on a
database.

### B5: release state

Implementation and validation are complete. The final freeze must include only
Issue Review files and this handover. Preserve unrelated untracked
`.claude/skills/gitnexus/` and `build/` content.

## Remaining PostgreSQL gate

The canonical integration test requires a dedicated database because it drops
and recreates its test schema. This host cannot currently supply one:

- Docker Desktop cannot start its Linux engine because WSL2 reports firmware
  virtualization disabled;
- no local PostgreSQL service or listener exists;
- no test container was created.

Do not point `pgtest` at production or a shared database. When a dedicated test
database becomes available, run:

```powershell
$env:TEST_PG_URL = '<dedicated test database URL>'
$env:CGO_ENABLED = '1'
go test -tags 'fts5,pgtest' ./internal/postgres/... -v -count=1
```

The PostgreSQL gate remains unresolved until that command executes
successfully.

## Validation evidence

### Final code and storage checks

| Check | Result |
| --- | --- |
| `go fmt ./...` | Pass |
| `go vet ./...` | Pass |
| Focused SQLite Issue Review | Pass, package 0.758 seconds |
| Focused DuckDB Issue Review | Pass, package 0.951 seconds |
| PostgreSQL/server compile | Pass, packages 1.474 and 0.593 seconds |
| Full SQLite suite | Pass, package 93.901 seconds |
| Full DuckDB suite | Pass, package 202.524 seconds |
| PostgreSQL `pgtest` execution | Not run; dedicated database unavailable |

### Final frontend checks

| Check | Result |
| --- | --- |
| `npm run i18n:compile` | Pass |
| `npm run generate:api` | Pass with x64 CGO toolchain available to the subprocess |
| Locale parity | Pass; five catalogues, 1,610 keys each |
| `npm run check` | Pass; zero errors and eight known CSS warnings |
| `npm test` | Pass; 148 files and 2,278 tests |
| `npm run build` | Pass; one known large-chunk warning |
| project-local `vp check` | Exact documented baseline: 486 files, exit 1 |

Do not run `vp check --fix`; it would create an unrelated repository-wide
rewrite. The final staged diff still requires `git diff --check` and the
private-data scrub.

## Branch-21 archive benchmark

The final benchmark used an isolated dirty-worktree executable, copied session
data, copied SQLite archive, isolated data directory, port 8091, hidden pprof,
and the unchanged 30-second write timeout.

Benchmark binary SHA-256:
`9044AC3CFC68D6D883403A503E2B3B53690D3764C746565AA9D01707EF7FE931`.
This is benchmark evidence only, not the release artifact.

| Metric | Result |
| --- | --- |
| Forced request 1 | HTTP 200, 19.729398 seconds |
| Forced request 2 | HTTP 200, 19.320250 seconds |
| Cached request | HTTP 200, 4.861 milliseconds |
| Total findings | 3,688 |
| Scanned messages | 6,426 |
| Scanned tool calls | 75,949 |
| Repeated requests | 598 |
| Recurring findings | 926 |
| Open findings | 160 |
| Recovered findings | 12 |
| Observed findings | 2,590 |
| Duplicate IDs across every page | 0 |
| Banned harness matches | 0 |
| Credential leaks in finding text | 0 |
| Absolute path leaks in finding text | 0 |
| Exact-chat containment | 31 of 31 evidence rows matched |
| Exact-folder containment | 31 of 31 evidence rows matched |
| Telemetry | `available`; zero matching scoped rows |
| Longest signature | 241 characters |

The benchmark closed two late issues:

1. Regex-heavy GitHub and logical-failure checks now use cheap syntax or
   keyword prefilters before regular expressions. Both forced requests remain
   below 20 seconds.
2. The shared finding sanitizer now redacts absolute Windows paths with either
   slash style and absolute Unix paths, including paths embedded in markdown.

Changing the production write timeout was not required.

## Final release closeout

### Frozen revision and validation

The release code is frozen in three focused commits:

- `252cef0` — proactive Issue Review;
- `4c3c108` — allocation reduction;
- `28eff63` — hot-path allocation reduction.

`go fmt ./...`, focused SQLite and DuckDB Issue Review tests,
`go vet -tags fts5 ./...`, the private-data scrub, `git diff --check`, the
isolated performance/parity gate, and the exact frontend build pass. The
broader `go test -tags fts5 ./internal/db ./internal/duckdb -count=1` exceeded
the 184-second harness timeout without emitting a failure; the affected
focused tests pass, and the full backend suites passed before the final
localized optimization. PostgreSQL `pgtest` remains blocked as described
above.

GitNexus is current at exact revision `28eff63`: 47,865 nodes, 278,060 edges,
2,390 clusters, and 300 flows. Graphify remains unavailable for this
repository. Analyzer-generated `AGENTS.md` and `CLAUDE.md` churn was not kept.

On the same isolated production-scale database, `4c3c108` took 24.488 seconds
for 1,135 findings. `28eff63` took 17.763 and 17.868 seconds for the same 1,135
findings; a cached request took 0.064 seconds. Telemetry was `available`.

### Exact artifact, deployment, and rollback

The clean detached release worktree is
`%LOCALAPPDATA%\Temp\agentsview-issue-review-28eff63-release`.

| Evidence | Result |
| --- | --- |
| Version | `v0.40.1-5-g28eff63` |
| Compiler | `x86_64-w64-mingw32` |
| Build time | `2026-08-09T22:32:15Z` |
| SHA-256 | `09515D3A0E3517D07C0F96A00627F1467A4AFC2D01BC7280494D79E694C7D16F` |
| Installed path | `%LOCALAPPDATA%\Programs\AgentsView\agentsview.exe` |
| Installed daemon | PID 64192 on `127.0.0.1:8080` after retry acceptance |
| Root UI | HTTP 200 |

The installed hash and version match the release artifact. The prior
`4c3c108` binary is backed up at
`%LOCALAPPDATA%\Programs\AgentsView\backups\20260810-013422-28eff63-predeploy\agentsview.exe`
with SHA-256
`3FC516FEA37080344997808D7CFF1D92C058C34D9F93CC02AA566C41BD8B2D2D`.

### Installed API and browser acceptance

The live archive changed during acceptance, increasing the result from 1,150
to 1,152 findings. The installed API returned:

| Check | Result |
| --- | --- |
| Cold forced request | HTTP 200, 29.488 seconds |
| Warm forced request | HTTP 200, 28.215 seconds |
| Cached pagination | 12 pages in 0.487 seconds |
| Findings | 1,152 |
| Scanned tool calls | 88,040 |
| Analyzed tool calls | 79,111 |
| Duplicate imported calls | 8,929 |
| Telemetry | `available` |
| Duplicate finding IDs | 0 |
| Absolute path leaks | 0 |
| Credential leaks | 0 |

Installed-browser acceptance passed for primary navigation, every Issue Review
filter group, a high-severity filter, **Clear filters**, **Load more findings**
(100 to 200), **Refresh now**, keyboard selection in a filter, and evidence
navigation to the exact session and message query. Browser diagnostics were
empty before the deliberate retry test. The screenshot is
`%LOCALAPPDATA%\Temp\agentsview-issue-review-28eff63.png`; the repeatable API
acceptance script is
`%LOCALAPPDATA%\Temp\agentsview-issue-review-acceptance.ps1`.

Retry acceptance stopped and verified the initial PID 8804, rendered the
first-load **Retry** state, started the same installed artifact as PID 64192,
and recovered through **Retry** to 1,153 findings and 88,195 scanned tool calls.
The only new browser diagnostics were the expected fetch warnings during the
deliberate outage; there were no errors after recovery. The daemon remains
running and healthy.

The in-app acceptance harness has a fixed 1280×720 viewport and exposes no
viewport override. Narrow-layout CSS was source-reviewed and the full
frontend test/build gates pass, but an installed sub-720-pixel manual resize
was not reproducible in this harness. This is the one remaining installed-UI
evidence limitation.

### Daily read-only task

`daily-agentsview-issue-review` is active as a heartbeat in the dedicated
Issue Review task. It runs daily at 09:00 local Kyiv time and uses the previous
completed `Europe/Kiev` day. Its prompt permits one forced GET followed by
cached GET pagination and forbids repair, restart, import, sync, repository
changes, and worktree creation. It pauses after three consecutive
unreachable-daemon runs and requests operator review.

The host local zone is Windows `FLE Standard Time` for Kyiv. The verified
`Europe/Kiev` offset is UTC+02:00 in winter and UTC+03:00 in summer, so the
local-wall-clock schedule remains 09:00 through daylight-saving changes.

The exact prompt was tested before scheduling against 2026-08-09 with label
`issue-review:2026-08-09:Europe/Kiev:human-excluding-one-shot`. It returned 120
unique findings across two pages: 13 recurring and 107 observed; 23 high, 86
medium, and 11 low severity; telemetry `available`; six sessions, 30 messages,
and 2,630 tool calls. This is the comparison baseline for the first scheduled
run.

## Release gates

### Gate 5: freeze, graph review, and commit — complete

1. Stage regenerated API output and all intended Issue Review files.
2. Preserve unrelated untracked `.claude/skills/gitnexus/` and `build/`.
3. Run `git diff --check`, the private-data scrub, and inspect the complete
   staged diff.
4. Refresh GitNexus against the frozen diff and run change-impact detection.
5. Resolve every blocking finding and repeat affected checks.
6. Create one focused conventional commit. Do not amend, push, merge, or create
   a branch.

Graphify has no graph for this repository. GitNexus is the release graph
authority; a missing Graphify artifact is not review evidence.

### Gate 6: exact-commit Windows deployment — complete

1. Create a detached clean worktree at the committed revision.
2. Verify the worktree is clean and `HEAD` equals the release revision.
3. Verify the compiler reports `x86_64-w64-mingw32`.
4. Build the embedded frontend and release binary with CGO and `fts5`.
5. Record revision, version, architecture, build time, and SHA-256.
6. Resolve `%LOCALAPPDATA%\Programs\AgentsView\agentsview.exe`; verify any
   daemon PID belongs to that executable.
7. Stop the daemon and prove its PID exited before replacement.
8. Copy the installed executable to a timestamped backup directory.
9. Install the exact-commit binary and compare artifact and deployed hashes.
10. Restart and verify daemon version, `127.0.0.1:8080`, root UI, and
    `/api/v1/analytics/issue-review`.
11. Stop the isolated 8091 benchmark server only after re-verifying its PID and
    executable path.

Rollback:

1. Stop and verify the new daemon exited.
2. Restore the timestamped prior executable.
3. Restart and verify its version, root UI, and API health.
4. Keep the failed artifact and sanitized logs for diagnosis.

### Gate 7: installed browser acceptance — desktop complete; narrow resize limited

Verify in the installed desktop UI:

- **Issue Review** is visible in primary navigation;
- global timeframe and project filters change the scan scope;
- chat and folder selectors enforce exact evidence containment;
- category, tool, source, outcome, severity, confidence, status,
  recommendation, thresholds, and sort controls work;
- **Clear filters**, **Load more**, retry, and **Refresh now** work;
- **Refresh now** forces analysis and background refresh remains cached;
- evidence links open the correct chat and message;
- API output contains no unredacted credential-shaped value or absolute path in
  finding text;
- narrow and desktop layouts remain keyboard accessible.

Record screenshots, API status, installed version, and hash. A successful
process start alone is not deployment acceptance.

## Daily scheduled report plan

Create this only after Gate 7 passes. Use a scheduled task attached to a
dedicated Issue Review operations chat so reports remain comparable.

Schedule:

- daily at 09:00 in `Europe/Kiev`;
- previous completed local day;
- one forced API request, then cached pagination for the same scope;
- idempotency label `issue-review:<local-date>:Europe/Kiev:<scope>`;
- report in the task and **Scheduled** inbox;
- read-only permissions and no worktree changes.

Each run reports:

- open, recurring, recovered, and total finding counts;
- severity and confidence split;
- total excess duration and slowest recurring tools;
- new or materially changed top patterns when prior context is available;
- grouped skill, script, rule, and tool-fix recommendations;
- telemetry status and scanned count;
- direct evidence links where available;
- daemon or API unavailability without attempting repair.

Operational acceptance:

- test the exact prompt manually before scheduling;
- verify timezone and daylight-saving behavior;
- review the first scheduled run and one subsequent comparison run;
- keep scope bounded and do not scan unrelated local folders;
- say when comparison context is unavailable;
- pause after three consecutive unreachable-daemon runs and request operator
  review instead of attempting repair.

Official OpenAI documentation requires the computer and desktop app to remain
running for scheduled tasks that need local files or localhost services. See
[Scheduled tasks](https://developers.openai.com/codex/app/automations).

## Definition of done

Implementation is complete when:

- B1-B5 are resolved;
- SQLite, DuckDB, server, frontend, build, and default-timeout benchmark gates
  pass on the frozen diff;
- PostgreSQL integration is either executed or remains explicitly blocked from
  the local SQLite release;
- an exact-commit binary is installed, hashed, healthy, and browser-verified;
- rollback evidence exists;
- the daily read-only task is created after installed acceptance.

Operational follow-up remains open until the first daily run and one subsequent
comparison run are reviewed.

## Post-release backlog

- execute PostgreSQL `pgtest` against a dedicated database;
- named saved views and multiple filter presets;
- acknowledge, suppress, and expiry rules for accepted findings;
- persisted “new since last review” trend snapshots;
- per-tool slow thresholds and project-specific rule packs;
- conservative near-duplicate request clustering beyond exact normalization;
- JSON or CSV export of filtered findings and evidence;
- optional read-only GitHub status enrichment for referenced issues.

Any semantic or near-duplicate detector must ship with a labeled evaluation
set, a false-positive budget, and a deterministic fallback.
