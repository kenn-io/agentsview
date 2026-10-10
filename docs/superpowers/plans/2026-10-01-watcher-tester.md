# Source watcher diagnostic tester

Build an isolated executable, `cmd/watchprobe`, to evaluate the scanner proposal
before connecting it to AgentsView. This authorization covers the tester, not
the twelve-task production watcher cutover.

Use synthetic files only. Create a new private artifact directory, never open
an archive, and emit aggregate telemetry without source paths or machine names.
Keep metadata pages at 256 records, event queues bounded, and cache storage
capped. Reuse fsnotify, the existing pure-Go SQLite driver, and gopsutil for
portable local telemetry. No network exporter or collector is required; JSONL,
Go CPU/heap profiles, and optional runtime traces are local diagnostic hooks.

1. Implement fresh platform signatures and compact SQLite page operations.
   Preserve volume and all identity bits. Test selective writes, failed
   acknowledgement, restart, admission refusal, and open-writer visibility.
2. Implement bounded traversal, physical event routing, and synthetic scenarios.
   Report native delivery separately from injected loss/retry scenarios. Test
   page bounds, no-op writes, scoped work, and forced content verification.
3. Add the command, resource samples, latency histograms, profiles, reports,
   Linux runs, and cross-platform builds. Test aggregate report contracts and
   cleanup/cancellation. Document identical full workloads and sustained runs.

Linux is the first exercised backend. Darwin and Windows signature/resource hooks were build-checked and then
exercised on remote hosts. The [returned results](../../internal/watchprobe-platform-results.md)
record native loss and timing limits. Production backend qualification remains
separate work. Darwin fsnotify uses kqueue, not the production FSEvents bridge.
The tester is not a parser, archive, or final production coordinator. Its cold
and warm timings compare equivalent metadata work, not application startup.
