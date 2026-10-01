# macOS agent handoff

Run the Go watcher experiment from draft PR #2055. Preserve existing work and
use a separate checkout of `t3code/scan-first-watcher` from kenn-io/agentsview.
Read `AGENTS.md` and `docs/internal/watchprobe.md` first. Do not change the
running AgentsView binary, source sessions, archives, or services.

Build with Go 1.27, or use the supplied binary matching `uname -m`:

```sh
CGO_ENABLED=0 go build -trimpath -o watchprobe ./cmd/watchprobe
./watchprobe --files 50000 --passes 3 --duration 5m --scan-interval 5s --profiles
./watchprobe --files 50000 --passes 3 --cache-bytes 0 --max-watches 0
./watchprobe --files 50000 --passes 3 --name-pattern entropy
./watchprobe --files 1024 --passes 1 --cache-bytes 131072 --event-queue 1 --max-watches 1
```

Run the sustained workload while the user does ordinary heavy work. Keep the
50,000-file workload unchanged. Record architecture, macOS version, filesystem
class, broad hardware capacity, workload description, and any failures. Avoid
hostnames, user paths, personal names, and transcript contents in the report.

Launch the sustained command in the background if needed to obtain its PID.
Sample `vmmap -summary PID` and `footprint PID` several times when available.
Report physical footprint and dirty memory separately from RSS and Go heap.
Do not use sudo. Do not stop other processes. Leave the user's work running.

Return a Markdown report plus each run's `report.json` and `metrics.jsonl`.
Preserve CPU and heap profiles for follow-up. Compare cold, warm, and uncached
scan times, process CPU, watch allocation failures, queue drops/recovery,
post-GC heap, cache disk size, and native/unwatched latency. Include all failed
checks and telemetry errors. A zero event-overlap count is an observation, not
proof of fair scheduling. macOS kqueue results do not qualify FSEvents.
