# SSH remote sync

Status: implemented. [Remote Access](../remote-access.md#ssh-remote-sync) is
the user guide; this document records why the transport is built the way it
is. The implementation lives in `internal/sshremote`, `internal/remotesource`,
and the transport-neutral parts of `internal/remotesync`.

## Problem

HTTP remote sync requires a running, authenticated agentsview daemon on the
remote machine. SSH remote sync instead collects from any host reachable with
`ssh`, with no agentsview install and no running daemon required there. A
`[[remote_hosts]]` entry with `transport = "ssh"` opts a host in; an omitted
`transport` still means HTTP.

## Why the earlier SSH transport was removed

Commit `a60347278` removed an earlier SSH transport after it had already been
deprecated. This design exists to avoid that transport's failure classes:

| Earlier problem | This design |
| --- | --- |
| A generated POSIX shell script (~760 lines) re-implemented provider discovery and drifted from the Go resolver: missed env vars, dropped migrated providers, never read the remote `config.toml`. | The remote runs the same Go `remotesync.ResolveTargets` the HTTP daemon runs, against the remote's own `$HOME`, environment, and `config.toml`. |
| Remote `tar` and shell differences (fish login shells, bsdtar exit codes, GNU tar "file changed" exits, command-length limits). | The remote writes archives with Go `archive/tar`. The only shell code is a small, fixed probe/install script run through `sh -c`. |
| Every run copied the full tree, uncompressed. | Manifest-plus-delta transfer through the same mirror HTTP sync uses; zstd on every response. |
| Hermes `state.db` snapshots needed remote Python and were skipped without it. | The remote binary takes SQLite online-backup snapshots in Go, as HTTP sync does. |
| SSH was implicit: an omitted `transport` meant SSH, and an unconfigured `sync --host` could quietly run an SSH sync. | SSH runs only for entries with `transport = "ssh"`. An omitted `transport` still means HTTP, and `sync --host NAME` still requires a configured entry. |

## Architecture

The HTTP remote-sync pipeline (mirror, manifest diff, delta archives, change
journal, import) does not know which transport fed it. `internal/remotesync`
exposes a `Source` interface with `Targets`, `Manifest`, and `Archive`
methods; `HTTPSource` and `internal/sshremote.Source` are its two
implementations, and `internal/remotesource.For` picks one for a configured
host. Manifest building, archive authorization (`ServeManifest`,
`PlanArchive`), and SQLite snapshot generation live once in `remotesync` and
are called both by the HTTP server routes and by the SSH remote command, so
the two transports cannot drift the way discovery drifted in the removed
design.

The remote side is two hidden subcommands of the normal agentsview binary,
`agentsview remote-sync probe` and `agentsview remote-sync serve --op <op>`
(`targets`, `manifest`, or `archive`), run over SSH for one operation at a
time. Each `serve` process loads configuration with `config.LoadReadOnly()`,
resolves its own allowed targets fresh, authorizes the request against them,
streams a zstd-compressed response, and exits; it never opens or creates
`sessions.db` (one exception below), never creates the data directory, and
never starts a daemon, watcher, or telemetry.

`serve` refuses with a `forbidden` error when an existing local database is
archive-only, the same check the HTTP remote-sync routes make, so a seeded or
restored archive never publishes its sources over SSH either.

### Wire protocol and error codes

Requests are one JSON line on stdin; responses are a zstd stream on stdout. A
failure is one JSON line on stderr, `{"code": "...", "message": "..."}`, and
exit status 2. The codes carry the same meaning as the HTTP remote-sync
status codes:

| Code | HTTP equivalent | Meaning |
| --- | --- | --- |
| `incompatible_protocol` | 426 | Collector and remote speak different `remotesync.ProtocolVersion`s |
| `forbidden` | 403 | Requested target or delta file is outside what the remote resolves itself |
| `unsupported` | 501 | Manifest unavailable, e.g. for a sanitized file-scoped agent (Windsurf) |
| `internal` | 500 | Any other failure |

The SSH client wraps a failure in `*CommandError{ExitCode, Stderr, Offline,
Remote}`. `Remote.Code == forbidden` marks a refusal the collector may retry
as a full archive, mirroring the HTTP client's `*StatusError` retry.

### Statelessness and interruption

A Go program that writes to a broken stdout pipe dies from `SIGPIPE` without
running deferred cleanup. `serve` installs `signal.Notify` for `SIGPIPE` so a
closed pipe instead returns `EPIPE` through the normal error path, letting
deferred temp-file cleanup run. `SIGHUP`, `SIGINT`, and `SIGTERM` cancel a
context via `signal.NotifyContext`, but a signal cannot interrupt a write
already blocked on a full pipe: on Linux and macOS, `serve` reopens file
descriptor 1 as non-blocking and pollable, and a goroutine closes it when the
context is cancelled, which fails a stalled write with `os.ErrClosed`. The
request read runs in a goroutine so a collector that stalls mid-request does
not block process exit either. `SIGKILL` cannot be handled and can leave one
`agentsview-sqlite-snapshot-*` directory behind; this is an accepted limit,
not a correctness issue, since the local SQLite archive never depends on it.

Archive generation and the SQLite snapshot observe the same context: tar
writes go through a context-checking writer, and the snapshot's online backup
copies in batches of 1024 pages, checking the context between batches and
holding one source read transaction for the whole copy so concurrent writers
cannot restart it. A thousand consecutive steps that fail to shrink
`Remaining()` is treated as a stuck restart loop and aborted, bounding
worst-case re-copied bytes instead of looping forever.

## Provisioning

Provisioning runs at the start of every SSH host sync; it is automatic once a
host is configured with `transport = "ssh"`.

Each cached binary lives at `$HOME/.cache/agentsview/bin/<id>/agentsview` on
the remote. The identity must change whenever the bytes that would run
change:

- A release version (matches `^v?[0-9]+\.[0-9]+\.[0-9]+$`) gets identity
  `v<version>`, since release tags are immutable.
- Any other build, including a development build or a release version with no
  published release yet, gets `<version>-<first 16 hex of SHA-256 of the
  local executable>`. The content hash keeps repeated dirty builds, which
  share one version string, from reusing each other's binary.

One SSH call probes the remote: it reports `uname -s`/`uname -m`, tries each
candidate cached identity (touching its `.last-used` file on success), and
tries `agentsview` on the remote's `PATH` only when its reported version is
v0.23.0 or newer (earlier releases pass unknown arguments to the server
instead of exiting, so probing them would hang). The local side picks, in
order, a cached binary at the local identity, then a `PATH` binary whose
protocol version matches, then an upload — a user's own install always wins
over an upload.

An upload either streams a verified release tarball
(`agentsview_<version>_<os>_<arch>.tar.gz`, checked against the release's
`SHA256SUMS`, cached under `<data_dir>/ssh-binaries/<version>/`; a checksum
mismatch deletes the download and fails that host's sync) or, for a
development build whose platform matches the collector's own, the
collector's own running executable. A platform mismatch with no release to
fall back to fails the sync with a message to install agentsview on the
remote. Concurrent fetches that share a release cache directory serialize so
one download serves every waiter and no fetch opens a file another is
replacing (needed on Windows).

Retention is idle-time based, not version based, because more than one
collector (for example two machines) can sync the same remote account and
must not delete a binary another collector just selected: the install script
removes an identity only when its `.last-used` is older than 30 days, never
the identity it just installed, and removes `.tmp-*` upload directories older
than one day. An active collector refreshes `.last-used` on every probe, so
its own binary is never idle long enough to be reclaimed. If retention still
removes a binary between another collector's probe and its next use, the
serve script detects the missing executable and exits 69 (`EX_UNAVAILABLE`,
chosen because shells disagree on the exit status for a merely-missing
program); the local side reprovisions once per sync and retries that step.

## Local transport

`internal/sshremote.Runner` runs the system `ssh` binary:

```
ssh -T -o BatchMode=yes -o ConnectTimeout=10 -o Compression=no \
  -o ServerAliveInterval=15 -o ServerAliveCountMax=4 -- <destination> sh -c '<script>'
```

`BatchMode=yes` means a sync never prompts; a key must already work
non-interactively. `Compression=no` avoids compressing the zstd stream a
second time. `ServerAliveInterval`/`CountMax` notice a dead peer within about
a minute, since OpenSSH sends no keepalives by default and bare TCP keepalive
can take hours while a sync holds the mirror lock. Every remote command is a
single-line script with no `'`, newline, `!`, or backslash, because csh,
tcsh, and fish login shells treat one or more of those specially even inside
single quotes; interpolated values (binary identity, operation name) are
restricted to `^[0-9A-Za-z._+-]+$` before use. `ssh` runs with `LC_ALL=C` so
its error text is untranslated and classifiable.

### Offline classification

The host-sync loop must skip a host only when the network could not reach
it, never when the user has something to fix. OpenSSH exits 255 for any
error of its own, so exit status alone cannot carry that meaning. An error is
classified offline only when `ssh` exited 255 *and* a whole captured stderr
line matches one of a fixed set of OpenSSH network-failure messages (DNS
failure, connection refused/timed out, no route, network unreachable, or a
banner-exchange timeout) — the same set HTTP remote sync treats as
unavailable. These forms also cover a down `ProxyJump` host. Every other
255-exit failure (bad `ssh_config`, authentication failure, host key
verification failure, a target down behind a reachable jump host) is
reported, not skipped. A keepalive timeout after the connection was already
established (`Timeout, server ... not responding.`) is also reported rather
than skipped, since the sync had already started and may have moved data.
Generic SSH session-teardown lines (for example `Connection closed by ...`)
are recognized and skipped over when picking the line that names the actual
failure, so a multi-line jump-host teardown does not hide the real cause.

### Entry points

SSH hosts join the same dispatch paths as HTTP hosts: direct `sync --host`,
the daemon's `POST /api/v1/sync/remotes` route, periodic per-host `interval`
sync, and the unified full-rebuild path. `internal/remotesource.For` is the
single place that turns a configured `RemoteHost` into a `Source`, so none of
those call sites need their own transport branch. A daemon started by a
service manager may not see the invoking user's SSH agent; the fix is to set
`IdentityAgent` in `~/.ssh/config` or use a key without a passphrase.

## Compression

`serve` responses are always zstd (`github.com/klauspost/compress/zstd`,
already a direct dependency), at `zstd.SpeedDefault` chosen from a benchmark
over representative session archives. The binary upload tarball stays gzip,
since the remote must decompress it with tools it already has (`tar xzf`)
and macOS does not ship `zstd`. HTTP remote sync separately negotiates zstd
for archives, with gzip as a fallback, and keeps gzip for manifests; the
collector decodes both transports' zstd archives through the same bounded
`NewZstdReader`.

## Limits

- Remotes must run Linux or macOS on amd64 or arm64. Windows remotes are not
  supported.
- Release Linux binaries link against glibc; an older remote glibc fails the
  post-install probe, and the error surfaces that probe's output.
- Discovery only sees the environment of a non-interactive SSH session;
  variables set only in interactive shell start-up files are invisible.
- A shell start-up file that prints to stdout in a non-interactive session
  corrupts the response stream. The local side checks for the zstd frame
  magic bytes and fails with a message naming this cause instead of trying to
  decode shell banner text.
- The first sync to a remote, and the first after each upgrade, uploads
  roughly 50 MB; a development build re-uploads itself after every rebuild.
- A `serve` process killed with `SIGKILL` can leave one
  `agentsview-sqlite-snapshot-*` directory in the remote's temp directory.
- Offline detection depends on OpenSSH's English error text; a different SSH
  client or new OpenSSH wording makes an unreachable host a reported failure
  instead of a silently skipped one — never the reverse.
- A target that is down behind a reachable jump host is reported as a
  failure, not skipped, because that distinction has not been needed in
  practice yet.

## Testing approach

Nearly all behavior above is exercised without a real `sshd`: tests put a
fake `ssh` executable first on `PATH` that strips the ssh options, takes the
destination and script, and runs the script locally against a fixture
`HOME`. The real `remote-sync serve`/`probe` subcommands run through the test
binary itself (a `TestMain` hook), so the probe, upload, retention,
statelessness, interruption, and offline-classification behavior all run
against the actual implementation rather than a mock of it.
