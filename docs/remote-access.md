---
title: Remote Access
description: Access AgentsView from other devices on your network
---

By default, AgentsView binds to `127.0.0.1`, so only your local machine can
reach it. To open the UI from other devices on your network, you need to listen
on a non-loopback address or put the server behind a proxy, and require a bearer
token for API access. In current releases, `require_auth` controls bearer-token
auth.

## Quick setup

For a one-off background server, enable auth in `~/.agentsview/config.toml`:

```toml
require_auth = true
```

Then start the server with a one-off non-loopback bind:

```bash
agentsview serve --background --host 0.0.0.0
```

To keep that `serve` run in the foreground, omit `--background`. Passing
`--host` explicitly also allows a one-off unauthenticated non-loopback bind,
though authenticated access is strongly recommended.

For a persistent, config-driven node that stays reachable across restarts and
auto-started daemons, set both values in `config.toml` instead of passing a flag
each time:

```toml
host = "0.0.0.0"
require_auth = true
```

Then manage the server with the canonical writable-daemon lifecycle:

```bash
agentsview daemon start
agentsview daemon status
# After configuration changes:
agentsview daemon restart
```

If you set a non-loopback host in `config.toml`, or in `AGENTSVIEW_HOST` under
a [container deployment](/docs/configuration/#container-deployment), you must
also enable `require_auth`. Otherwise, the server refuses to start instead of
persistently exposing an unauthenticated API. The `--host` flag remains
available for one-off unauthenticated binds.

When the server runs inside WSL, AgentsView advertises the WSL `eth0` address
instead of `127.0.0.1`, so the printed URL works from the Windows host and
nearby LAN clients. An explicit `--public-url` still takes precedence.

When you enable auth, AgentsView generates a token if needed and stores it in
`~/.agentsview/config.toml`. Open `http://<your-ip>:8080` from another device
and enter the configured token in the frontend, or send it in an `Authorization`
header.

!!! note

    Older configs may still use `remote_access = true`. AgentsView still reads
    that legacy key, but new setups should use `require_auth = true`.

## Authentication

When `require_auth` is enabled, every `/api/` request must include this header:

```
Authorization: Bearer <token>
```

AgentsView generates the token automatically the first time you enable auth and
stores it in `config.toml` as `auth_token`, so you do not need to mint one
manually. For supervised services or temporary overrides, set
`AGENTSVIEW_AUTH_TOKEN`, which takes precedence over the config file.

AgentsView enforces auth on API routes, not on static assets. A browser can load
the HTML shell, while API requests fail with `401` until you supply the correct
token.

When `require_auth` is disabled, normal local loopback use remains ungated.

## HTTP remote sync

A `[[remote_hosts]]` entry syncs a remote machine over either HTTP, described
here, or [SSH](#ssh-remote-sync). For an HTTP entry, provide the URL and bearer
token of a running AgentsView daemon:

```toml
[[remote_hosts]]
host = "devbox1"
transport = "http" # optional; default
url = "http://devbox1.tailnet.ts.net:8080"
token = "remote-token"
interval = "5m" # optional: sync periodically while the collector daemon runs
```

Treat `host` as the remote machine's stable, unique identity. AgentsView uses it
to namespace imported session IDs, the database skip cache, and the mirror
directory. Changing `host` creates a new namespace and can duplicate sessions
from the same machine, while reusing it for a different machine can reuse stale
cached state. If the same logical machine moves, changing only `url` is fine.

The daemon on the remote machine must bind to a non-loopback interface by
setting `host = "0.0.0.0"` with `require_auth = true` in its `config.toml`, or
every sync fails with a connection-refused error. See
[Remote Hosts](/docs/configuration/#remote-hosts) for the full remote-side
setup, including how to keep detached daemons alive.

Set `require_auth = true` on remote nodes when practical, or at minimum keep
their generated `auth_token` configured. The remote archive endpoints always
require `Authorization: Bearer <token>`, even when the rest of the daemon API is
unauthenticated. The `token` in the collector's `remote_hosts` entry must match
the remote daemon's `auth_token` and is required for HTTP remote sync. Do not
reuse the collector daemon's own `auth_token` for untrusted remote endpoints.

The HTTP transport is meant for private networks such as Tailscale or another
restricted overlay. Do not expose raw archive endpoints directly to the public
internet.

AgentsView summarizes HTTP remote sync failures without echoing
remote-controlled URLs or response bodies. Common summaries point to the
specific fix:

- A rejected token means the collector's per-host `token` does not match the
  remote daemon's `auth_token`.
- A missing endpoint means the remote host needs a newer AgentsView.
- Connection refusal usually means the daemon is not running, is still bound to
  loopback, or uses a different port than the URL.
- DNS and timeout messages point back to the configured `url`.

A bare `agentsview source sync` treats offline, unreachable, and timed-out
configured HTTP hosts as absent fleet members. It prints a skipped-host progress
line, continues with local and reachable remote sources, and exits with success
unless another error occurs. By contrast, `agentsview source sync --host X`
stays strict and reports an unavailable explicitly selected host as an error.

Fleet nodes launched with `agentsview daemon start` stay available until you
stop them.

Run `agentsview source sync` on the collector to sync local sessions and every
configured host. To have a running collector daemon sync a host periodically,
set `interval` on its `[[remote_hosts]]` entry; omit `interval` or set it to
`0s` for manual sync only. See
[`agentsview source sync`](/docs/commands/#agentsview-sync) for single-host
selection and failure behavior.

## SSH remote sync

SSH remote sync collects sessions from a machine you can reach with `ssh`. The
remote machine needs no AgentsView install and no running daemon. This feature
is available in 0.46.0 and later.

### Set up

Add an entry with `transport = "ssh"` to `config.toml`:

```toml
[[remote_hosts]]
host = "devbox"
transport = "ssh"
destination = "user@devbox.example.com" # optional; defaults to host
interval = "15m" # optional: sync periodically while the collector daemon runs
```

`host` names the machine in your archive, the same way as an HTTP host.
`destination` is the argument passed to `ssh`, which can be a `~/.ssh/config`
alias or `user@hostname`. Set the port, user, identity file, `ProxyJump`, and
`ControlMaster` in `~/.ssh/config`. The entry has no fields for them. See
[Remote Hosts](/docs/configuration/#remote-hosts) for the validation rules.

AgentsView runs `ssh` without a prompt (`BatchMode=yes`), so your key must work
without asking for a password or passphrase. Test it first:

```bash
ssh -o BatchMode=yes user@devbox.example.com true
```

Then run `agentsview source sync --host devbox`. A bare `agentsview source sync`,
the daemon's periodic `interval` sync, and full rebuilds also include SSH hosts.
Sessions get the same IDs, machine name, and paths as HTTP hosts, and mirrors,
manifests, and delta transfers work as described in
[Incremental sync](#incremental-sync).

A daemon started by a service manager may not see your SSH agent. For
daemon-driven SSH sync, set `IdentityAgent` in `~/.ssh/config` or use a key
without a passphrase.

### What lands on the remote

AgentsView installs one file per version on the remote account:

```text
~/.cache/agentsview/bin/<id>/agentsview
~/.cache/agentsview/bin/<id>/.last-used
```

For a release build, `<id>` is `v<version>`. For a development build, it is
`<version>-<16 hex characters of the executable's SHA-256>`, so two builds never
share a binary. Each sync updates `.last-used` for the binary it runs. An
install removes other binaries that have been idle for more than 30 days and
leftover `.tmp-*` upload directories older than one day. If one cannot be
removed, the install still succeeds. If another collector's cleanup removes a
binary right after a sync picked it, that sync installs it again and carries on.

Each sync runs a short `agentsview remote-sync serve` process per step: target
resolution, manifest, and archive. Each process reads its own configuration
read-only, writes only temporary SQLite snapshots that it removes before
exiting, and then ends. Nothing stays running on the remote. AgentsView
compresses responses with zstd and turns off ssh compression so the stream is
not compressed twice.

### Where the binary comes from

At the start of each sync, one `ssh` call reports the remote platform and looks
for a usable binary. AgentsView picks the first match:

1. The cached binary for this version under `~/.cache/agentsview/bin/`.
1. An `agentsview` on the remote's `PATH` that speaks the same remote-sync
   protocol version. Your own install wins over an upload. AgentsView first runs
   `agentsview version` and skips a `PATH` binary older than v0.23.0, or one
   whose version is not a release number, such as `dev` or a bare commit hash.
   Releases before v0.23.0 start a server instead of answering.
1. An upload, described next. Sync progress shows
   `Installing agentsview on <host>` while it runs.

A release build downloads `agentsview_<version>_<os>_<arch>.tar.gz` from the
GitHub release for its version and checks it against the release's `SHA256SUMS`.
It does not check `SHA256SUMS.sig`. AgentsView caches the verified tarball under
`<data_dir>/ssh-binaries/<version>/` and streams it to the remote, where it
unpacks, probes, and moves the binary into place. A checksum mismatch deletes
the download and fails that host's sync.

A development build has no release to download. If the remote's OS and
architecture match the collector's, AgentsView uploads its own executable.
Otherwise, the sync fails with a message to install agentsview on the remote or
sync from a release build.

### Offline hosts

A bare `agentsview source sync` skips an SSH host that the network cannot reach,
the same as an offline HTTP host. `agentsview source sync --host devbox` reports
it as an error. `ssh` must exit with status 255 and print one of these OpenSSH
messages:

- `Could not resolve hostname`
- `connect to host ... port ...: Connection refused`, `Connection timed out`,
  `Operation timed out`, `No route to host`, or `Network is unreachable`
- `Connection timed out during banner exchange`

These also cover a `ProxyJump` host that is down. AgentsView reports every other
failure with the last line `ssh` printed. That includes authentication failures,
host key verification failures, ssh configuration errors, and a target that is
down behind a reachable jump host.

While a sync runs, `ssh` checks the connection every 15 seconds. If the host
stops answering for about a minute, `ssh` exits with
`Timeout, server ... not responding.` Because the sync had already started,
AgentsView reports that as a failure instead of skipping the host.

### Limits

- Remotes must run Linux or macOS on amd64 or arm64. Windows remotes are not
  supported.
- Release Linux binaries link against glibc. On a remote with an older glibc,
  the check after install fails and the error shows its output.
- Discovery sees the environment of a non-interactive SSH session. Variables set
  only in interactive shell start-up files are not visible.
- A shell start-up file that prints to stdout in non-interactive sessions
  corrupts the response stream, as it does for `scp` and `rsync`. Sync then
  fails with an error that names this cause.
- The first sync to a remote, and the first after each upgrade, uploads about 50
  MB. A development build uploads itself again after each rebuild.
- A `serve` process killed with `SIGKILL` can leave one
  `agentsview-sqlite-snapshot-*` directory in the remote's temporary directory.
- Offline detection depends on OpenSSH's English error text. With another ssh
  client or new OpenSSH wording, AgentsView reports an unreachable host as a
  failure instead of skipping it.

## Incremental sync

Starting in 0.37.4, HTTP remote sync keeps a persistent mirror of each remote
machine's syncable source files under this path:

```text
<data_dir>/remote-mirrors/<sanitized-host>-<hash>/
```

SSH remote sync uses the same mirror and the same steps, asking a short-lived
process over `ssh` instead of a daemon.

For the default data directory, that parent is `~/.agentsview/remote-mirrors/`.
A hash follows the readable host component so names that sanitize to the same
directory name do not collide. A lock file next to each mirror serializes
concurrent syncs of that host from the same data directory.

The mirror adds an on-disk copy of the remote session sources to the collector
alongside the indexed database. Budget roughly the size of each remote host's
syncable source corpus for it.

### How a sync works

1. The collector asks the remote daemon for its resolved agent roots. The remote
   controls this allowlist, and the collector cannot name arbitrary roots.
1. The collector locks the host mirror and requests a gzip-compressed manifest
   of regular, non-symlink files. Each entry carries its absolute remote path,
   size, and modification time.
1. The remote sends manifest entries sorted by mirror path. As each entry
   arrives, the collector walks the mirror in the same order and compares file
   size and modification time at microsecond precision, so it holds neither the
   manifest nor the mirror listing in memory. It schedules missing or changed
   files for fetch and removes mirror files that disappeared from the manifest.
   Each list stays in memory up to 4 MiB of paths and spills to a temporary file
   next to the mirror past that. Before changing the mirror, the collector
   durably merges those paths into a bounded recovery journal next to the
   mirror. When more than 8,192 files change, or their paths add up to more than
   2 MiB, the journal records one full import instead, and that sync re-reads
   every session from the host.
1. The collector requests only missing or changed files in requests of at most 4
   MiB of paths each. When every manifest file needs fetching, it requests a
   full archive instead of sending the full file list. Archive transfers prefer
   Zstandard compression, while gzip remains available through HTTP encoding
   negotiation. Manifests use gzip.
1. For a changed file that the mirror already holds, the request also sends the
   mirror copy's length and a SHA-256 hash of its last 64 KiB. When the file is
   a JSON Lines `.jsonl` transcript, the remote copy is longer, and its bytes
   before that length end the same way, the remote sends only the bytes past it
   and the collector appends them to the mirror copy. Agent transcripts grow by
   appending, so a long session transfers only its new lines. Any other file,
   such as a SQLite database that rewrites earlier pages as it grows, and every
   file from a remote that predates this check, arrives whole. If an append
   fails partway, the mirror copy stays at its old length, so the next sync
   fetches the file again.
1. The collector extracts the archive into the mirror with remote modification
   times preserved. AgentsView classifies the pending journal paths and imports
   only their affected sources. If a provider's exact scope cannot be proven, it
   falls back to discovery under that provider's roots without widening other
   providers. AgentsView retires the journal only after session writes and the
   remote skip cache are durable.

An explicit full import reparses the complete mirror without forcing unchanged
bytes across the network. If a rejected delta request requires a full archive,
the collector still imports only the affected sources.

Sync logs report target resolution time and manifest time, which includes the
mirror comparison. They also report each archive's encoding, compressed byte
count, download time, and extraction time. Download time includes waiting for
the remote response. The existing import log reports planning, processing, and
cache persistence times.

For a configured full sync that includes local sources, mirror preparation
finishes before database work begins. Up to four remote hosts prepare at once
for this combined rebuild, and mirror locks keep the same acquisition order
across collectors to avoid deadlocks. The collector then ingests local sources
and every prepared remote mirror through the same batched temporary-database
path, suspending FTS maintenance during ingest and rebuilding it once before an
atomic swap. A preparation, parser, batch-write, or FTS failure leaves the
active archive unchanged.

Remote-only syncs, including
`agentsview source sync --host <configured-http-host> --full`, continue to
import into the active archive and do not use the combined rebuild path.

If no directory-scoped files changed, the collector skips the archive request
and, when no recovery work is pending, performs no import. Files that disappear
remotely are deleted from the mirror, though remote import is intentionally
non-destructive and sessions already stored in the AgentsView database remain
available.

If a process stops after mutating the mirror, the next sync replays the pending
journal. AgentsView arms cache invalidation once per observed remote version so
a same-mtime extraction cannot be hidden by stale cache state. A deterministic
parse failure is then retained in the existing error-skip cache, retried on
replay as a skip, and re-armed only when the remote path changes again. Recovery
work is bounded, and overflow collapses to one conservative full import.

A remote sync requested from a running daemon, such as `agentsview sync` while
`agentsview serve` runs, belongs to the daemon. If the requesting client
disconnects or is killed, the daemon stops sending it progress and finishes the
import. Only daemon shutdown cancels the run.

Windsurf is a special case. Its state database is sanitized into a curated
export for every transfer, so the raw tree cannot safely participate in the
manifest. AgentsView fetches that small export as a separate full archive on
each sync while the rest of the host remains eligible for delta transfer. The
Windsurf content in the mirror is the sanitized export, not a byte-for-byte copy
of the remote state database.

### Curated provider exports

Most providers export their configured directories as-is. Providers whose
configured root is a broad application-data directory export a curated file list
instead, so caches, extension state, and credential files stored near the
sessions never enter the manifest, the archive, or the mirror:

- **Cursor** exports only the transcript files its parser discovers under each
  project's `agent-transcripts/` directory.
- **VS Code Copilot** exports only chat session files under
  `workspaceStorage/<hash>/chatSessions/` and
  `globalStorage/{emptyWindowChatSessions,transferredChatSessions}/`, plus each
  exporting workspace's `workspace.json` for project attribution.
- **Zed** exports `threads/threads.db` as a consistent SQLite snapshot taken
  with the online backup API, so committed write-ahead-log data arrives as one
  standalone database without `-wal` or `-shm` sidecars. Hermes state databases
  use the same snapshot mechanism.
- **RooCode** and **Kilo Legacy** export only their discovered per-task session
  files, **Poolside** narrows its root to `trajectories/`, and **Windsurf**
  exports the sanitized copy described above.

Budget mirror disk for the curated session files only, not for the full
application-data directories those editors keep around them. When the last
session under a curated Cursor or VS Code Copilot root disappears, the root
stays advertised with an empty file list so the next sync empties the mirror
instead of failing.

AgentsView treats an unreadable snapshot database as missing: the sync
continues, the mirror drops its cached copy, and the file transfers again once
the remote application repairs it. Sessions already imported into the AgentsView
database remain available throughout. Any other read failure during target
resolution fails that host's sync without touching the mirror.

### When AgentsView downloads a full archive

A full HTTP transfer happens in these cases:

- the per-host mirror is new or was removed
- every manifest file is missing or changed
- a delta request is rejected after a manifest succeeded, in which case the
  collector retries once with a full archive

Windsurf's curated export is also fetched in full on every sync, independently
of the directory-scoped archive decision.

`--full` reparses every discovered remote session, while still using the
manifest comparison to decide which mirror bytes need transferring. It does not
delete the local database or turn remote sync into a destructive reconciliation.

### Compatibility and recovery

HTTP remote sync requires the collector and remote daemon to use the same
remote-sync protocol version. Every target, manifest, and archive request
carries that version, and successful responses echo it. A missing or mismatched
request version returns HTTP 426 before targets or archive data are exchanged,
so upgrade both hosts before syncing again after either side changes protocol.

An HTTP remote on Windows running an older AgentsView can fail a sync with a
message to upgrade AgentsView on the remote host. That older release lists its
files in a different order under the same protocol version.

The normal mirror comparison detects an interrupted extraction when the
resulting file size or modification time differs from the manifest, and the next
sync fetches that file again. It also repairs file-versus-directory conflicts
left by an interrupted extraction.

The comparison does not hash file content. A remote rewrite that preserves both
size and modification time, or local mirror corruption with the same metadata,
can therefore look unchanged. Because `--full` now separates reparsing from
mirror transfer, it does not repair same-stat corruption by itself.

The mirror is a disposable transfer cache. When no sync is running, you can
repair a host's mirror by deleting its mirror directory; the next compatible
sync bootstraps it again. Leave the adjacent `.lock` file in place. Removing
mirror files never removes imported sessions from the database.

### Transfer safety

The remote daemon recomputes its allowed sync targets for each request and
rejects paths outside them. Manifest walks omit symlinks and special files.
Delta requests must either match an allowed extra file exactly or use an
absolute path in the same POSIX, drive-letter, or UNC dialect as the allowed
root, and the daemon refuses symlinked roots or intermediate components. The
collector rejects any manifest whose entry is larger than 256 KiB, which is well
above the longest path any supported system allows. Mirror deletions and
type-conflict cleanup are separately confined to the per-host mirror root.

These checks limit what authenticated sync requests can read, but they do not
replace network isolation or bearer-token security. Keep the daemon on a private
network and protect its `auth_token` as described above.

## SSE endpoints

The live-update streams accept the same `Authorization: Bearer <token>` header
as every other API call. The bundled web UI always sends the header, including
for these streams.

For clients built on the browser's native `EventSource`, which cannot set
headers, the streams also accept the token as a `?token=` query parameter:

```text
http://<host>:8080/api/v1/events?token=<token>
http://<host>:8080/api/v1/sessions/<id>/watch?token=<token>
http://<host>:8080/api/v1/changes/stream?token=<token>
```

Because a token in the URL can end up in proxy access logs and browser history,
send the header whenever your client supports it. Managed Caddy mode strips the
`token` parameter from the request URLs it logs. If you run your own reverse
proxy, filter the parameter out of its access logs. Other API endpoints never
accept the query parameter.

## Public URL and trusted origins

The bind address, browser URL, and trusted origins serve different purposes:

| Setting | Purpose |
| --- | --- |
| `--host` / `--port` | Choose the backend HTTP listener (default `127.0.0.1:8080`). |
| `--proxy-bind-host` / `--public-port` | Choose the managed Caddy listener when `--proxy caddy` is enabled. |
| `--public-url` | Choose the browser URL and add it to the trusted origins. Managed Caddy also uses it as its site address. |
| `--public-origin` | Add trusted origins for request `Host` and `Origin` checks without selecting a browser URL. |

If you already operate a hostname or reverse proxy, pass the URL you intend to
open in the browser:

```bash
agentsview serve --public-url https://agents.example.com
```

This setting does not configure DNS, port forwarding, certificates, or an
external reverse proxy. The startup message labels it **browser URL**, separate
from the backend's listening address, and does not confirm that the URL is
reachable. Here, "public" means the client-facing address, which may be local
or private.

Opening that URL locally helps when you want the same HTTPS hostname and proxy
route as other clients. Direct backend access uses a different browser origin,
with separate browser storage. On a headless server, pass `--no-browser`. The
URL still contributes to origin validation and managed Caddy configuration.

Use a concrete hostname or IP in `--public-url`. Wildcard addresses such as
`0.0.0.0` and `::` select listening interfaces, so put them in `--host` or
`--proxy-bind-host` instead of the browser URL.

To add trusted origins without choosing a browser URL, use `--public-origin`.
It works on its own or alongside `--public-url`:

```bash
agentsview serve \
  --public-origin https://agents.example.com \
  --public-origin https://internal.example.com
```

You can also pass comma-separated values:

```bash
agentsview serve --public-origin https://a.example.com,https://b.example.com
```

To persist these settings in `config.toml`:

```toml
public_url = "https://agents.example.com"
public_origins = [
  "https://internal.example.com",
]
```

You do not need to repeat `public_url` in `public_origins`. Neither setting
enables bearer-token authentication; `--require-auth` controls that.

### Reverse proxy subpaths

If an external reverse proxy mounts AgentsView under a URL prefix, pass
`--base-path`. Keep `--public-url` origin-only; the server appends the mount
path to the browser URL and uses it for assets, API requests, live event
streams, and client-side navigation.

```bash
agentsview serve \
  --base-path /av \
  --public-url http://agents.example.com
```

The proxy must preserve `/av` in the upstream request. For nginx, omit the
trailing slash from `proxy_pass` so nginx does not strip the location prefix:

```nginx
location = /av {
    return 301 /av/;
}

location /av/ {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $http_host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_http_version 1.1;
    proxy_buffering off;
}
```

The same flag works with `serve --background`. Because the configuration-only
`daemon start` and `daemon restart` commands do not accept serve-specific flags,
start a subpath-mounted background server with `agentsview serve --background
--base-path /av`.

### Forwarded dev environments

AgentsView validates the request `Host` header before serving API requests. That
protects local loopback servers from DNS-rebinding attacks, and it means SSH
port forwards, reverse proxies, WSL2, Codespaces, Coder, and similar remote
development environments must use a trusted public URL or origin.

Static assets can still load in these environments because the HTML shell is
intentionally less strict than `/api/` routes. The dashboard may appear briefly
before `/api/v1/settings` or another API request fails with `403 Forbidden` if
the browser `Host` is the forwarded hostname rather than one of the local
allowed hosts.

Restart the server with the exact origin you open in the browser:

```bash
# exe.dev browser URL: https://<vm>.exe.xyz
agentsview serve --public-url https://<vm>.exe.xyz

# ssh -L 18080:127.0.0.1:8080 host
# Browser opens http://127.0.0.1:18080
agentsview serve --public-url http://127.0.0.1:18080
```

`--public-url` must match the browser origin exactly, including the scheme,
hostname, and non-default port. When the browser sends an untrusted `Host`,
current releases return `403 Forbidden` with a response body that explains the
rejected host, the allowed set, and the `--public-url` fix. The **Settings**
page shows the same actionable message instead of prompting for an auth token,
and the server writes a short breadcrumb to its debug log.

### Troubleshoot forwarded access

If the UI flashes and then the dashboard or **Settings** page fails, inspect the
`/api/v1/settings` request in browser devtools:

| Symptom or check                              | Meaning                                    | Fix                                                                          |
| --------------------------------------------- | ------------------------------------------ | ---------------------------------------------------------------------------- |
| `/api/v1/settings` returns `401 Unauthorized` | Bearer-token auth is enabled               | Use the token printed by the server or stored in `~/.agentsview/config.toml` |
| `/api/v1/settings` returns `403 Forbidden`    | The browser host or origin was rejected    | Restart with `agentsview serve --public-url <exact-browser-origin>`          |
| `/api/v1/settings` goes to an unexpected host | The frontend has a saved remote server URL | Clear `localStorage.getItem("agentsview-server-url")` for this site          |

To clear an unintended saved server URL, run this in the browser console and
reload:

```js
localStorage.removeItem("agentsview-server-url")
location.reload()
```

### Slow aggregates behind a proxy

Most API responses have a write deadline. When a request exceeds it, the server
returns `503` with a JSON body that has an `error` field set to
`request timed out` and a `detail` field, and affected dashboard panels show
"request timed out". The default deadline is 30 seconds, which is comfortable
for local archives but can be tight for large shared datasets.

Long-running routes are exempt from this deadline. They include session and
content search, session statistics, usage summaries, comparisons, and top
sessions, activity report sessions, Kata status, single-session sync, archive
maintenance, friction reviews, and conversation export. Cancelling a search
request stops its search. A reverse proxy may impose its own timeout.

To raise the deadline, pass `--write-timeout` with a Go duration. Both the local
SQLite server and the PostgreSQL read server accept this flag:

```bash
# Local SQLite server
agentsview serve --write-timeout 120s

# PostgreSQL-backed read server (the multi-tenant / large-dataset case)
agentsview replica pg serve --write-timeout 120s
```

Set `--write-timeout` to `0` to disable the deadline entirely. If aggregates are
slow enough to need a large timeout, that usually points at a database-side cost
worth investigating first. For a multi-tenant read role, confirm any row-level
security policy is set-based (`session_id IN (SELECT ...)`) rather than a
per-row function call, which the query planner cannot hoist into a single join.

## Managed Caddy mode

AgentsView can manage a [Caddy](https://caddyserver.com) reverse proxy for
TLS-terminated access. The AgentsView backend stays on loopback while Caddy
handles the public socket.

!!! warning "Enable auth first"

    Managed Caddy exposes a public endpoint. Set `require_auth = true` in
    `~/.agentsview/config.toml` before starting the server, or the proxy will
    front an unauthenticated API. See [Quick Setup](#quick-setup).

```bash
agentsview serve \
  --public-url https://agents.example.com \
  --proxy caddy \
  --tls-cert /path/to/cert.pem \
  --tls-key /path/to/key.pem
```

| Flag                | Default     | Description                                           |
| ------------------- | ----------- | ----------------------------------------------------- |
| `--proxy`           |             | Proxy mode — currently `caddy`                        |
| `--caddy-bin`       | `caddy`     | Path to the Caddy binary                              |
| `--proxy-bind-host` | `127.0.0.1` | Interface for Caddy to bind                           |
| `--public-port`     | URL port or `8443`      | Managed Caddy HTTP/HTTPS listener and URL port                      |
| `--tls-cert`        |             | TLS certificate file path                             |
| `--tls-key`         |             | TLS key file path                                     |
| `--allowed-subnet`  |             | Client CIDR allowlist (repeatable or comma-separated) |

Caddy must already be installed and on `PATH` unless you override it with
`--caddy-bin`.

### Listener and URL ports

`--public-port` applies only to managed Caddy. It controls Caddy's listening
port and the port in the resolved `--public-url`, without changing the backend
`--port`. It works with both HTTP and HTTPS based on the URL scheme. HTTPS
requires `--tls-cert` and `--tls-key`, and HTTP must omit them.

- If the URL has an explicit port, Caddy uses that port.
- If the URL omits a port, Caddy uses `--public-port`, or `8443` when the flag
  is unset. For example, `--proxy caddy --public-url https://agents.example.com`
  resolves to `https://agents.example.com:8443`, not port 443.
- If both the URL and `--public-port` specify ports, they must match. For HTTPS
  on port 443, pass `--public-port 443` or put `:443` in the URL.

Caddy binds to `127.0.0.1` by default, even when the URL contains a remote
hostname. To accept LAN connections, set `--proxy-bind-host` and an
`--allowed-subnet` as shown below. DNS and routing remain your responsibility.

With an external proxy, leave `--proxy` unset and put its browser-facing port
directly in `--public-url`. `--public-port` has no effect in that mode. For
example, if an existing proxy listens on 443 and forwards to backend port 8080:

```bash
agentsview serve --port 8080 --public-url https://agents.example.com --no-browser
```

### Subnet allowlists

When you bind Caddy to a non-loopback host, restrict access to specific
networks. Subnet allowlists complement bearer-token auth rather than replacing
it, so still set `require_auth = true` before binding Caddy off loopback.

```bash
agentsview serve \
  --proxy caddy \
  --proxy-bind-host 0.0.0.0 \
  --allowed-subnet 192.168.1.0/24 \
  --allowed-subnet 10.0.0.0/8
```

Requests from outside the allowed subnets receive `403`.

## Settings page

You can also configure remote access from the **Settings** page. In the
**Remote access** section, you can:

- toggle `require_auth`
- view the auto-generated auth token
- connect the frontend to another AgentsView server by URL and token

![Settings remote access](/docs/assets/generated/screenshots/settings-remote.png)

Auth changes take effect as soon as you flip the toggle.

## CLI flags reference

| Flag                | Default     | Description                                         |
| ------------------- | ----------- | --------------------------------------------------- |
| `--host`            | `127.0.0.1` | Interface to bind                                   |
| `--require-auth`    | `false`     | Require a bearer token for API requests             |
| `--public-url`      |             | Browser URL, also added to trusted origins             |
| `--public-origin`   |             | Trusted browser origin (repeatable/comma-separated) |
| `--write-timeout`   | `30s`       | API response deadline; long-running routes are exempt; `0` disables it |
| `--proxy`           |             | Managed proxy mode (`caddy`)                        |
| `--caddy-bin`       | `caddy`     | Caddy binary path                                   |
| `--proxy-bind-host` | `127.0.0.1` | Interface for managed proxy                         |
| `--public-port`     | URL port or `8443`      | Managed Caddy HTTP/HTTPS listener and URL port                     |
| `--tls-cert`        |             | TLS certificate path                                |
| `--tls-key`         |             | TLS key path                                        |
| `--allowed-subnet`  |             | Client CIDR allowlist (repeatable/comma-separated)  |

## Config file reference

Remote-access fields in `~/.agentsview/config.toml`:

```toml
require_auth = true
auth_token = "auto-generated-base64-token"
public_url = "https://agents.example.com"
public_origins = ["https://agents.example.com"]

[proxy]
mode = "caddy"
bin = "caddy"
bind_host = "127.0.0.1"
public_port = 8443
tls_cert = "/path/to/cert.pem"
tls_key = "/path/to/key.pem"
allowed_subnets = ["192.168.1.0/24"]
```

| Field                   | Description                                                                |
| ----------------------- | -------------------------------------------------------------------------- |
| `host`                  | Server bind interface; non-loopback values require `require_auth = true`   |
| `require_auth`          | Require bearer-token authentication for API access                         |
| `auth_token`            | Auto-generated 256-bit bearer token; overridden by `AGENTSVIEW_AUTH_TOKEN` |
| `public_url`            | Browser URL, trusted origin, and managed Caddy site address                                      |
| `public_origins`        | Additional trusted origins for request Host/Origin checks                                            |
| `proxy.mode`            | Managed proxy mode (`caddy`)                                               |
| `proxy.bin`             | Path to proxy binary                                                       |
| `proxy.bind_host`       | Interface for proxy to bind                                                |
| `proxy.public_port`     | Managed Caddy HTTP/HTTPS listener and URL port                                                    |
| `proxy.tls_cert`        | TLS certificate path                                                       |
| `proxy.tls_key`         | TLS key path                                                               |
| `proxy.allowed_subnets` | CIDR allowlist for proxy connections                                       |

For LAN access, you still need a non-loopback bind such as
`agentsview serve --host 0.0.0.0`. `require_auth` controls API auth and does not
change the bind address on its own.
