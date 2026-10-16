# SSH Remote Sync Design

Status: implemented. This document is the design record;
[Remote Hosts](../../remote-access.md) is the user guide. Date: 2026-10-14.

AgentsView will sync a remote host over plain SSH without the user installing or
running agentsview there. The user adds a `[[remote_hosts]]` entry with
`transport = "ssh"`. On the first sync the local agentsview places a
version-matched agentsview binary in the remote user's cache directory. Each
sync then runs that binary for one short operation at a time over SSH. The
remote process reads its own configuration, answers, and exits. It never runs a
daemon, opens a database, or writes AgentsView state.

The data path reuses the HTTP remote sync pipeline: target resolution,
manifests, delta archives, SQLite snapshots, the local mirror, the change
journal, and the import. Only the transport differs.

## Why the earlier SSH transport was removed

Commit `a60347278` (#2038) removed the first SSH transport after `29e7de157`
(#1177) deprecated it. The new design avoids its failure classes:

| Earlier problem                                                                                                                                                                                                                                                                    | This design                                                                                                                                                       |
| ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| A generated POSIX shell script (about 760 lines) re-implemented provider discovery. It drifted from the Go resolver repeatedly: missed env vars, dropped migrated providers, never read the remote `config.toml`, and needed hand-written per-provider emitters for curated files. | The remote runs the same Go `remotesync.ResolveTargets` the HTTP daemon runs, against the remote's own `$HOME`, env, and `config.toml`.                           |
| Remote `tar` and shell differences: fish login shells, bsdtar exit codes, GNU tar "file changed" exits, `tar -T` backslash decoding, Windows command-length limits.                                                                                                                | The remote writes the archive with Go `archive/tar`. The only shell code is a small fixed probe and install script, wrapped in `sh -c`.                           |
| Every run copied the full tree, uncompressed.                                                                                                                                                                                                                                      | Manifest plus delta transfer through the existing mirror; zstd on every response.                                                                                 |
| Hermes `state.db` snapshots needed remote Python 3 and were skipped without it.                                                                                                                                                                                                    | The remote binary takes SQLite online-backup snapshots in Go, as HTTP does.                                                                                       |
| SSH was implicit: an omitted `transport` meant SSH, and an unconfigured `sync --host` quietly ran an SSH sync.                                                                                                                                                                     | SSH runs only for configured entries with `transport = "ssh"`. An omitted `transport` still means HTTP, and `sync --host NAME` still requires a configured entry. |

## Configuration

```toml
[[remote_hosts]]
host = "devbox"                # archive namespace
transport = "ssh"              # required for SSH
destination = "user@devbox.example.com"  # optional; defaults to host
interval = "15m"               # optional periodic sync
```

- `host` names the machine in the archive. Sessions are stored with
  `machine = host`, IDs `host~<id>`, and paths `host:<remote path>`, exactly
  as for HTTP hosts.
- `destination` is the argument passed to `ssh`. It defaults to `host`. It may
  be a `~/.ssh/config` alias or `user@hostname`.
- There are no port, user, identity, or option fields. Users configure those in
  `~/.ssh/config`, which also covers `ProxyJump` and `ControlMaster`.

`config.ValidateRemoteHosts` changes:

- Accept `transport = "ssh"` as a second `RemoteTransport` value.
- SSH entries reject `url` and `token`.
- HTTP entries, including entries with an omitted `transport`, reject
  `destination`.
- Reject a `destination` that is empty after trimming or starts with `-`.
- Host-name uniqueness applies across both transports.

## Remote command

The remote side is two hidden subcommands of the normal agentsview binary.

### `agentsview remote-sync probe`

Prints one JSON line, `{"version": "...", "protocol_version": N}`, and exits 0.
The local side uses it to decide whether a remote binary is usable. It loads no
configuration and touches no files.

### `agentsview remote-sync serve --op <op>`

Runs one operation and exits. `<op>` is `targets`, `manifest`, or `archive`.

| Op         | stdin (JSON)                                         | stdout (zstd stream)                               |
| ---------- | ---------------------------------------------------- | -------------------------------------------------- |
| `targets`  | `{"protocol_version": N}`                            | `TargetSet` JSON                                   |
| `manifest` | `{"protocol_version": N, "targets": TargetSet}`      | `Manifest` JSON                                    |
| `archive`  | `{"protocol_version": N, "request": ArchiveRequest}` | PAX tar from `WriteArchive` or `WriteArchiveFiles` |

Each operation follows the matching HTTP handler in
`internal/server/huma_routes_remote_sync.go`:

- It checks `protocol_version` against `remotesync.ProtocolVersion`.
- It resolves allowed targets fresh with `ResolveTargets`, then intersects the
  request with `SelectAllowedTargets` and, for deltas, `SelectAllowedFiles`.
- It refuses sanitized agents for `manifest`, as the HTTP route does.

On failure the command writes one JSON line to stderr,
`{"code": "...", "message": "..."}`, and exits with status 2. The codes carry
the HTTP status meanings:

| Code                    | HTTP equivalent | Local handling                                           |
| ----------------------- | --------------- | -------------------------------------------------------- |
| `incompatible_protocol` | 426             | Fail the host sync with a version message                |
| `forbidden`             | 403             | Fail the host sync                                       |
| `unsupported`           | 501             | Same fallbacks as HTTP (for example legacy full archive) |
| `internal`              | 500             | Fail the host sync with the message                      |

The shared handler logic moves out of the HTTP route file into `remotesync` so
both transports call the same functions. The HTTP routes keep their current
status codes and headers.

### Statelessness

`serve` and `probe` must leave no AgentsView state on the remote:

- Load configuration with `config.LoadReadOnly()`. It writes no config
  migrations, secrets, or installation identity.
- Do not open or create `sessions.db` or the data directory. The one exception
  is a read-only check of an existing `sessions.db`: like the HTTP remote-sync
  routes, `serve` refuses with `forbidden` when that database is archive-only,
  so a seeded or restored archive never publishes its sources.
- Do not start a daemon, watchers, pollers, telemetry, or update checks. The
  command path must bypass any CLI start-up hook that does these.
- Write SQLite snapshots to an `os.MkdirTemp` directory and remove it before
  exit, including on error and on interruption (see below).
- Exit when the operation ends.

The only lasting files are the cached binaries described below.

#### Interruption

By default a Go program that writes to a broken stdout pipe dies from `SIGPIPE`
without running deferred functions
([os/signal: SIGPIPE](https://pkg.go.dev/os/signal#hdr-SIGPIPE)). A probe with
the repository toolchain confirmed it: the process exited with status 141 and
its deferred cleanup did not run. The existing snapshot writer
(`writeSQLiteStateDBSnapshot` in `internal/remotesync/archive.go`) removes its
temp directory with `defer`, so a dropped connection in the middle of an archive
would leave the snapshot behind.

`serve` therefore installs its signal handling before any work:

- Call `signal.Notify` for `SIGPIPE`, with a channel nobody reads. A write to a
  closed stdout then returns `EPIPE`. The error unwinds through the archive
  writer, its deferred cleanup runs, and `serve` exits with status 2. The same
  probe confirmed this: with `Notify` installed, the write returned "broken
  pipe" and the deferred cleanup ran.

- Use `signal.NotifyContext` for `SIGHUP`, `SIGINT`, and `SIGTERM`. A signal
  only cancels the context; it does not interrupt I/O already in progress
  ([os/signal: NotifyContext](https://pkg.go.dev/os/signal#NotifyContext)).
  The next two bullets make cancellation reach every place where `serve` can
  block.

- **Archive generation observes the context.** Today it does not. Each file body
  is copied with plain `io.Copy` (`writeArchiveEntry` in
  `internal/remotesync/archive.go`). The SQLite snapshot
  (`internal/remotesync/sqlite_snapshot.go`) opens its connections with
  `context.Background()` and copies the whole database in one `Step(-1)` call.
  The change:

    - Wrap the archive's output writer in a context-checking writer. Every tar
      header and file body, including snapshot bodies, passes through it, so a
      cancelled context fails the next write without threading the context
      through each helper.
    - Pass the caller's context to the snapshot's `Conn` and `ExecContext` calls.
      Copy in batches of `Step(1024)` pages, checking the context between
      batches.
    - Hold one read transaction on the source connection for the whole copy:
      `BEGIN`, then `SELECT count(*) FROM sqlite_master`, and `ROLLBACK` on
      every exit path. SQLite restarts an online backup whenever another
      connection writes the source between steps, so without it a database the
      agent writes steadily is never copied. Backup steps reuse the open
      transaction. In WAL mode the copy reads that pinned snapshot while writers
      keep committing; in rollback-journal mode writers wait on the shared lock,
      as they did with `Step(-1)`. This works with the read-only (`mode=ro`)
      source connection.
    - Stop with an error after 1000 consecutive steps in which
      `backup.Remaining()` does not shrink. With the pinned snapshot a healthy
      copy shrinks it on every step, so the bound only ends an unexpected
      restart or `BUSY`/`LOCKED` loop.

    The HTTP archive route uses the same functions, so a cancelled HTTP request
    also stops snapshot work early.

- **A stalled stdout is unblocked.** When the local reader stops reading but
  keeps the pipe open, a stdout write blocks in the kernel, and no context
  check runs. On Linux and macOS, `serve` sets file descriptor 1 to
  non-blocking with `syscall.SetNonblock` and reopens it with `os.NewFile`,
  which returns a pollable file. When the context is cancelled, a goroutine
  closes that file. Closing a pollable file makes a pending write return
  `os.ErrClosed` ([os: File.Close](https://pkg.go.dev/os#File.Close)). The
  error unwinds through the archive writer, and the deferred cleanup runs. A
  probe with the repository toolchain confirmed both halves. With normal
  blocking stdout, `Close` did not release a stalled write. With the pollable
  file, the write returned "file already closed" 1 s after `Close`.

- **The request read stops on cancellation.** A collector that stalls
  mid-request keeps stdin open, and a blocked read sees no context. `serve`
  decodes the request in a goroutine and stops waiting when the context ends;
  the abandoned read ends with the process.

- `SIGKILL` cannot be handled. A killed `serve` can leave one
  `agentsview-sqlite-snapshot-*` directory in the remote temp directory. This
  is listed under Limits.

## Provisioning

Provisioning runs at the start of every SSH host sync. It is automatic; the
`transport = "ssh"` entry is the opt-in.

### Binary identity

Each cached binary lives in `$HOME/.cache/agentsview/bin/<id>/agentsview`. The
`<id>` must change whenever the bytes that would run change, so a stale binary
is never reused:

| Local build                                                   | `<id>`                                                | Upload source            |
| ------------------------------------------------------------- | ----------------------------------------------------- | ------------------------ |
| Release version: matches `^v?[0-9]+\.[0-9]+\.[0-9]+$`         | `v<ver>`                                              | Verified release tarball |
| Release version with no published release (download 404)      | `<ver>-<first 16 hex of SHA-256 of local executable>` | The local executable     |
| Any other version, including `dev` and `git describe --dirty` | `<ver>-<first 16 hex of SHA-256 of local executable>` | The local executable     |

- Release versions are immutable tags, so the version alone identifies the
  official binary.
- Ordinary builds default to `version = "dev"` (`cmd/agentsview/main.go`), and
  repeated dirty builds share one `git describe --tags --always --dirty`
  string (`Makefile`). The content hash separates them.
- The local side hashes its executable once per process and reuses the result
  while the file's size and mtime are unchanged.

### Probe

One SSH call runs a fixed POSIX script under `sh -c`. The script prints
`uname -s`, `uname -m`, and the result of `remote-sync probe` for these
candidates:

1. `$HOME/.cache/agentsview/bin/v<ver>/agentsview`, when the local version is a
   release version.
1. `$HOME/.cache/agentsview/bin/<ver>-<hash>/agentsview`.
1. `agentsview` found on `PATH`, only when its version is v0.23.0 or later.

Releases before v0.23.0 pass unknown arguments to the server, so
`agentsview remote-sync probe` would start a server that never exits. Every
release prints its version and exits for `agentsview version`, so the script
asks that first and matches the version with `case` glob patterns. A version
that is not a release number, such as `dev` or the bare commit hash that a clone
without tags reports, is skipped, because its age is unknown. Every pattern
requires a dot, which a hex hash never contains.

For each cached candidate whose probe succeeds, the script also touches
`$HOME/.cache/agentsview/bin/<id>/.last-used`.

The local side chooses, in order:

1. A cached binary at the local identity.
1. The `PATH` binary when its `protocol_version` equals the local one. A user's
   own install wins over an upload, including for development builds.
1. An upload.

The chosen path is remembered for the rest of that sync run only.

### Upload

1. Map `uname` output to a release platform: `Linux`/`Darwin` and
   `x86_64`/`amd64`/`aarch64`/`arm64`. Other platforms fail with a message
   that names the detected platform and says to install agentsview on the
   remote.
1. Fetch `agentsview_<ver>_<os>_<arch>.tar.gz` and `SHA256SUMS` from
   `https://github.com/kenn-io/agentsview/releases/download/v<ver>/`. Verify
   the SHA-256 against `SHA256SUMS`. Cache the verified tarball under
   `<data_dir>/ssh-binaries/<ver>/`. A checksum mismatch deletes the download
   and fails the host sync. Fetches that share a cache directory in one
   process run one at a time, so one download serves concurrent hosts and no
   fetch opens a file while another replaces it, which Windows refuses.
1. Stream the tarball over one SSH call to a fixed install script. The script
   extracts it into a fresh `.tmp-*` directory under
   `$HOME/.cache/agentsview/bin/`, checks that `agentsview` exists and that
   `remote-sync probe` succeeds, then renames the file to `<id>/agentsview`
   and touches `<id>/.last-used`.
1. Two collectors can install the same identity at once. Both renames succeed
   and the second replaces the first. That is harmless: one identity always
   names the same bytes, and a running process keeps its open file. Renaming
   the file, not the directory, avoids `mv` moving one directory inside an
   existing one.
1. After a successful install, apply the retention rule below.

### Retention

Several collectors can sync the same remote account, for example a laptop and a
desktop running different agentsview versions. One collector's install must not
delete a binary another collector selected and will run on its next SSH call.
The local mirror lock (`AcquireMirrorLock`) cannot prevent this: it guards local
mirror state, not files on the remote.

Retention is therefore based on idle time, not on the newest version:

- The install script removes an `<id>` directory only when its `.last-used` file
  is older than 30 days. It never removes the identity it just installed.
- It removes `.tmp-*` directories older than 1 day, which covers interrupted
  uploads without touching an upload still in progress.
- An active collector touches `.last-used` on every probe, at the start of each
  sync, so its binary is never idle long enough to be removed.
- Retention is best effort. The new binary is already in place, so a removal
  that fails, for example on a directory another install removed first, does
  not fail the install.
- A probe refreshes `.last-used` as part of selecting a cached identity, and a
  refresh that fails makes the identity a miss. A refreshed identity is safe
  from retention for 30 days.
- Retention can still remove an identity between its idle check and another
  collector's refresh. The serve script tests that its binary exists before
  `exec` and exits 69 (`EX_UNAVAILABLE`) when it does not; the shell's own
  status for a missing program is 126 on macOS `/bin/sh` and 127 elsewhere,
  and 126 also means a program that exists but cannot run. On exit 69 the SSH
  source provisions again once per sync and retries the step. A binary that
  exists but cannot run is reported, not uploaded again. The archive step
  waits for the stream's first byte before returning, so a vanished binary
  fails there, before any transfer, and is retried the same way. No remote
  lock is used: a POSIX shell has no atomic way to reclaim a lock left by a
  holder that died without risking removal of a newer lock.

A collector that syncs less than once every 30 days uploads again on its next
sync.

The tarball keeps the release's gzip compression. The remote must decompress it
with tools it already has; `tar xzf` is available on every supported platform,
while macOS does not ship `zstd`.

### Builds without a matching release

A development build has no release to download. A release version can also
return 404 when it was built from a tag before the release was published. In
both cases, if the remote platform equals the local
`runtime.GOOS`/`runtime.GOARCH`, the local side uploads its own executable
(`os.Executable()`) through the same install script, packed as a gzip tar
stream, under its content-hash identity. Otherwise the host sync fails with a
message that says to install a matching agentsview on the remote or to sync from
a release build.

## Local transport

### Source interface

`remotesync` gains an interface for the three fetches that HTTP sync performs
today:

```go
type remoteSource interface {
	Targets(ctx context.Context) (TargetSet, error)
	Manifest(ctx context.Context, targets TargetSet) (Manifest, error)
	Archive(ctx context.Context, req ArchiveRequest) (io.ReadCloser, error)
}
```

The HTTP client becomes one implementation and the SSH client the other. The
prepare flow in `http_prepare.go` (mirror lock, `SplitFileScoped`, manifest,
`MirrorDiff`, journal, deletions, delta or full archive, `PreparePending`) and
the import in `import.go` and `delta_import.go` run unchanged against the
interface. `downloadArchive` takes an `io.Reader` instead of an `*http.Response`
and keeps spooling to a temp file before extraction. Unsupported-manifest and
delta-retry fallbacks key off typed errors instead of HTTP status codes. An SSH
delta is retried as a full archive only when the remote reports `forbidden`, the
refusal of a delta file outside its allowed roots.

### Running ssh

The SSH client executes the system `ssh` binary:

```
ssh -T -o BatchMode=yes -o ConnectTimeout=10 -o Compression=no \
  -o ServerAliveInterval=15 -o ServerAliveCountMax=4 -- <destination> <command>
```

- `BatchMode=yes` means sync never prompts. Keys must be usable without a
  prompt, through an agent or `~/.ssh/config`.
- `Compression=no` avoids compressing the zstd stream a second time when the
  user's ssh config enables compression.
- `ServerAliveInterval=15` and `ServerAliveCountMax=4` notice a dead peer within
  about a minute. OpenSSH sends no keepalives by default, and TCP keepalive
  alone takes about two hours on Linux while the sync holds the mirror lock.
- `<command>` is always `sh -c '<script>'` built from fixed text. The only
  interpolated values are the binary identity and the operation name, both
  validated against `^[0-9A-Za-z._+-]+$` before use. Scripts are one line and
  contain no `'`, no `!`, and no backslash, which csh, tcsh, or fish login
  shells treat specially even inside single quotes.
- Request JSON goes to stdin. stdout is decoded with zstd. stderr is captured,
  bounded to its last 32 KiB, and parsed for the error line, which comes last.
  A failure summary shows the last non-empty stderr line that names a cause,
  skipping the generic lines ssh prints when a session or a `ProxyJump` leg
  closes (`Connection closed by <host> port <port>`,
  `stdio forwarding failed`,
  `kex_exchange_identification: Connection closed by remote host`, and
  `Connection to <host> closed.`). Terminal escapes and control characters are
  removed.
- `ssh` runs with `LC_ALL=C` in its environment, so the system error text it
  prints is untranslated and can be classified.

### Offline hosts

The host loop in `cmd/agentsview/sync.go` skips a host when
`remotesync.IsHostUnavailable` returns true, and does not report it as a
failure. So "unavailable" must mean only "the network could not reach the host".
It must not cover mistakes the user has to fix.

Exit status 255 cannot carry that meaning. OpenSSH exits 255 for any error of
its own ([ssh(1) EXIT STATUS](https://man.openbsd.org/ssh.1#EXIT_STATUS)). On
OpenSSH, a bad `Port` setting, a DNS failure, a refused connection, and a
rejected public key all exited 255 in a local probe.

An SSH error is classified as offline only when both of these hold:

- `ssh` exited 255.
- A whole captured stderr line matches one of these OpenSSH forms, which
  correspond to the network errors `IsHostUnavailable` already accepts for
  HTTP:
    - `ssh: Could not resolve hostname <host>: <reason>` (DNS failure, as for
      `*net.DNSError`)
    - `ssh: connect to host <host> port <port>: Connection refused`
    - `ssh: connect to host <host> port <port>: Connection timed out`
    - `ssh: connect to host <host> port <port>: Operation timed out`
    - `ssh: connect to host <host> port <port>: No route to host`
    - `ssh: connect to host <host> port <port>: Network is unreachable`
    - `Connection timed out during banner exchange`

The matching line may come with other lines. These forms also cover a
`ProxyJump` or `ProxyCommand` jump host that the network cannot reach. In a real
OpenSSH probe, a refused jump-host connection exited 255 and printed
`ssh: connect to host <jump> port <port>: Connection refused` followed by
`Connection closed by UNKNOWN port 65535`. A jump host that is down is offline
in the same sense as a target that is down, so that sync is skipped.

Every other failure is reported as a host failure with the captured stderr. That
includes:

- `Timeout, server <host> not responding.` after the keepalive gives up. The
  session had started and may have moved data, so the user sees it.
- configuration errors, including a bad setting for the jump host
- `Permission denied` on the jump host or the target
- host key verification failures
- a target that the jump host cannot open a channel to, such as
  `channel 0: open failed: ...` and `stdio forwarding failed`

The last case is left as a failure for now because it has not been observed yet.
The SSH client returns a typed `*CommandError{ExitCode, Stderr, Offline}`, and
`IsHostUnavailable` reads `Offline` rather than matching text itself.

Provisioning failures after ssh reached the host, such as a release download
that cannot reach GitHub or a development build for another platform, are a
typed `*ProvisionError`. It is never offline, even when its cause is a network
error, and its summary keeps the cause and remedy.

### Entry points

- Direct mode: `runRemoteSyncTransportWithCleanup` in `cmd/agentsview/sync.go`
  dispatches `ssh` entries instead of rejecting them.
- Daemon: `POST /api/v1/sync/remotes` resolves SSH entries from the daemon's
  config by host name, as it does for HTTP.
- Periodic sync: `startRemoteHostSync` runs SSH entries with an `interval`.
- Full rebuilds: SSH hosts join the unified rebuild through the same
  `RebuildContributor` HTTP hosts use.

A daemon started by a service manager may not see the user's SSH agent. The
documentation says to configure `IdentityAgent` or a key without a passphrase
for daemon-driven SSH sync.

## Compression

- The remote compresses every `serve` response with zstd from
  `github.com/klauspost/compress/zstd`, which is already a direct dependency.
  Go runs on both ends, so no remote tool is needed.
- The implementation benchmarks zstd levels on representative session archives
  (JSONL transcripts and SQLite snapshots) and records input size, output
  size, and compress/decompress time in the pull request. The default is
  `zstd.SpeedDefault` unless the benchmark shows a better trade-off.
- The binary upload stays gzip, as explained under Upload.
- HTTP remote sync negotiates zstd for archives, with gzip as the fallback,
  through `Accept-Encoding`; its manifests stay gzip. The collector decodes
  both transports' zstd archives with the same bounded `NewZstdReader`.

## Limits

- Supported remotes are Linux and macOS on amd64 and arm64, matching the release
  CLI tarballs. Windows remotes are not supported.
- Release Linux binaries link against glibc. A remote with an older glibc fails
  the post-install probe; the error reports the probe output.
- Discovery sees the environment of a non-interactive SSH session. Variables set
  only in interactive shell start-up files are not visible.
- A shell start-up file that prints to stdout in non-interactive sessions
  corrupts the response stream, as it does for `scp` and `rsync`. When a
  response does not start with the zstd frame magic, sync fails with a message
  that names this cause.
- The first sync to a remote, and the first after each upgrade, uploads about 50
  MB. A development build uploads itself again after each rebuild.
- A `serve` process killed with `SIGKILL` can leave one
  `agentsview-sqlite-snapshot-*` directory in the remote temp directory.
- Offline detection depends on OpenSSH's English error text. Other SSH clients,
  or new OpenSSH wording, make an unreachable host count as a failure rather
  than be skipped. A failure is still reported, never hidden.
- A target that is down behind a reachable jump host is reported as a failure,
  not skipped.

## Testing

- **End-to-end without sshd.** Tests put a fake `ssh` executable first on
  `PATH`. It drops ssh options, takes the destination and command, and runs
  the command locally with `HOME` set to a fixture directory. This covers the
  probe, upload, cached-binary reuse, every `serve` operation, deltas,
  deletions, and the error codes.
- **Offline classification.** The fake `ssh` prints each OpenSSH line listed
  under Offline hosts and exits 255; the host is skipped. It also prints a bad
  `Port` setting error, `Permission denied (publickey).`, and a host key
  verification failure with exit 255; each is reported as a failure with its
  stderr. A multi-line case reproduces the jump-host output above,
  `ssh: connect to host <jump> port 22: Connection refused` then
  `Connection closed by UNKNOWN port 65535`; the host is skipped. A
  `channel 0: open failed: connect failed: Connection refused` case is a
  failure.
- **Statelessness.** Run `serve` for each operation against a fixture `HOME`
  that contains agent data only. Assert the tree is unchanged afterwards and
  no temp directories remain.
- **Interrupted archive.** Build the real binary and run `serve --op archive`
  against a fixture with a Hermes `state.db`, with `TMPDIR` set to a test
  directory. In each case, assert that the process exits with status 2, not by
  signal, within 5 s, and that no `agentsview-sqlite-snapshot-*` directory
  remains:
    - **Broken pipe.** Read part of stdout, then close the read end.
    - **Stalled reader.** Read part of stdout, then stop reading and keep the read
      end open, so the remote blocks in a stdout write. Send `SIGTERM`. Because
      the pipe stays open, only cancellation can end the process; this case
      proves cancellation, not broken-pipe handling.
- **Cancelled snapshot.** Call the snapshot function on a database large enough
  to need several `Step(1024)` batches, with a context cancelled after the
  first batch. Assert that it returns `context.Canceled` and removes its temp
  directory.
- **Binary identity and retention.** Two development builds that differ only in
  content get different identities and both upload. A cached identity is
  reused without upload. An install keeps another identity whose `.last-used`
  is recent and removes one older than 30 days. Two concurrent installs of one
  identity both end with a working binary.
- **Downloads.** An `httptest` server stands in for the release host. Cover a
  good download, a checksum mismatch, a missing `SHA256SUMS` entry, and a 404
  that triggers the development-build path. The release base URL is an
  unexported package variable that tests override.
- **Configuration.** Table-driven validation tests for both transports and every
  rejection above.
- **Archive identity.** The end-to-end SSH test asserts the identity HTTP hosts
  get: session IDs `host~<id>`, `machine = host`, and paths
  `host:<remote path>`. Both transports call the same shared handlers and
  importer, so this checks the only part that differs.

## Documentation changes

- `docs/remote-access.md`: an SSH Remote Sync section next to HTTP Remote Sync,
  covering setup, what lands on the remote, and the daemon key note.
- `docs/configuration.md`: the `transport = "ssh"` and `destination` fields.
- `docs/commands.md`: `sync --host` with SSH entries.
- `SECURITY.md`: what the binary upload trusts (HTTPS plus `SHA256SUMS`) and
  where the binary lives on the remote.
- `docs/changelog.md`: a user-facing entry.
