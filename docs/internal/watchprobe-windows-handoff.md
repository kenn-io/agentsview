# Windows agent handoff

Run the Go watcher experiment from draft PR #2055 in a separate checkout of
`t3code/scan-first-watcher` from kenn-io/agentsview. Preserve existing work.
Read `AGENTS.md` and `docs/internal/watchprobe.md` first. Do not modify the
running application, source sessions, archives, or services.

Use the supplied amd64 executable, or build with Go 1.27 in PowerShell:

```powershell
$env:CGO_ENABLED = '0'
go build -trimpath -o watchprobe.exe ./cmd/watchprobe
.\watchprobe.exe --files 50000 --passes 3 --duration 5m --scan-interval 5s --profiles
.\watchprobe.exe --files 50000 --passes 3 --cache-bytes 0 --max-watches 0
.\watchprobe.exe --files 50000 --passes 3 --name-pattern entropy
.\watchprobe.exe --files 1024 --passes 1 --cache-bytes 131072 --event-queue 1 --max-watches 1
```

Run the sustained workload during ordinary heavy use. Keep the 50,000-file
workload unchanged. Record Windows version, architecture, filesystem class,
broad hardware capacity, and other workload categories. Record failures and
elapsed wall time. Do not disable antivirus or indexing. Do not request admin
access or stop other processes.

Return a Markdown report and each run's `report.json` and `metrics.jsonl`.
Keep CPU/heap profiles for follow-up. Compare process CPU and RSS, Go heap,
cold/warm/uncached scans, open-writer visibility, replacement identity, cache
size and refusals, native drops/recovery, and native/unwatched latency. Include
all failed checks and resource errors. Exclude hostnames, personal identities,
absolute user paths, and transcript contents. The executable measures native
handle signatures and fsnotify, not Windows change-journal integration.
