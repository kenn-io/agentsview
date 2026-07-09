---
title: Remote Access
description: Access AgentsView from other devices on your network
---

AgentsView binds to `127.0.0.1` by default, so only your local machine can reach
it. To make the UI available from other devices, you need to do two things:

- listen on a non-loopback address or put the server behind a proxy
- require a bearer token for API access

In current releases, bearer-token auth is controlled by `require_auth`.

## Quick Setup

Enable auth in `~/.agentsview/config.toml`:

```toml
require_auth = true
```

Then start the server on a non-loopback interface:

```bash
agentsview serve --host 0.0.0.0
```

To make the bind persistent — so restarts, auto-started daemons, and reboots
keep the server reachable — set `host` in `config.toml` instead of passing
the flag each time:

```toml
host = "0.0.0.0"
```

A non-loopback `host` in `config.toml` requires `require_auth = true`; the
server refuses to start rather than persistently exposing an unauthenticated
API. The `--host` flag remains available for one-off unauthenticated binds.

When this command runs inside WSL, AgentsView advertises the WSL `eth0` address
instead of `127.0.0.1` so the printed URL is usable from the Windows host and
nearby LAN clients. An explicit `--public-url` still takes precedence.

When auth is enabled, AgentsView generates a token if needed and stores it in
`~/.agentsview/config.toml`. Open `http://<your-ip>:8080` from another device
and enter the configured token in the frontend, or send it in an
`Authorization` header.

!!! note

    Older configs may still use `remote_access = true`. AgentsView still reads
    that legacy key, but new setups should use `require_auth = true`.

## Authentication

When `require_auth` is enabled, all `/api/` requests must include:

```
Authorization: Bearer <token>
```

The token is stored in `config.toml` as `auth_token`. It is generated
automatically the first time auth is enabled, so you do not need to mint one
manually. For supervised services or temporary overrides, set
`AGENTSVIEW_AUTH_TOKEN`; it takes precedence over the config file.

Auth is enforced on API routes, not on static assets. That means a browser can
load the HTML shell, but API requests fail with `401` until the correct token is
supplied.

When `require_auth` is disabled, normal local loopback use remains ungated.

## HTTP Remote Sync

Configured `[[remote_hosts]]` entries can use `transport = "http"` when the
remote machine is already running an AgentsView daemon:

```toml
[[remote_hosts]]
host = "devbox1"
transport = "http"
url = "http://devbox1.tailnet.ts.net:8080"
token = "remote-token"
interval = "5m" # optional: sync periodically while the collector daemon runs
```

Treat `host` as the remote machine's stable, unique identity. AgentsView uses
it to namespace imported session IDs, the database skip cache, and the mirror
directory. Changing it creates a new namespace and can duplicate sessions from
the same machine; reusing it for a different machine can reuse stale cached
state. Changing only `url` is fine when the same logical machine moves.

The daemon on the remote machine must bind a non-loopback interface (set
`host = "0.0.0.0"` with `require_auth = true` in its config.toml) or every
sync fails with a connection-refused error. See
[Remote Hosts](/configuration/#remote-hosts) for the full remote-side setup,
including keeping detached daemons alive.

Use `require_auth = true` on remote nodes when practical, or at minimum keep
their generated `auth_token` configured. The remote archive endpoints always
require `Authorization: Bearer <token>`, even when the rest of the daemon API is
unauthenticated. The `token` in the collector's `remote_hosts` entry must match
the remote daemon's `auth_token` and is required for HTTP remote sync. Do not
reuse the collector daemon's own `auth_token` for untrusted remote endpoints.

The HTTP transport is intended for private networking such as Tailscale or an
equivalent restricted overlay. Do not expose raw archive endpoints directly to
the public internet.

HTTP remote sync failures are summarized without echoing remote-controlled URLs
or response bodies. Common summaries point at the specific fix: a rejected token
means the collector's per-host `token` does not match the remote daemon's
`auth_token`; a missing endpoint means the remote host needs a newer
AgentsView; connection refusal usually means the daemon is not running, is still
bound to loopback, or the URL port is wrong; DNS and timeout messages point back
to the configured `url`.

For always-available fleet nodes launched with `agentsview serve --background`,
set:

```toml
daemon_idle_timeout = "0s"
```

That setting controls detached background daemons only. Supervised daemons run
under systemd, launchd, Docker, or a foreground shell never create the idle
tracker and already stay alive until their supervisor stops them.

Run `agentsview sync` on the collector to sync local sessions and every
configured host. Set `interval` on a `[[remote_hosts]]` entry when a running
collector daemon should sync that host periodically; omit it or set it to `0s`
for manual sync only. See [`agentsview sync`](/commands/#agentsview-sync) for
single-host selection and failure behavior.

## Incremental Sync

Starting in 0.37.4, HTTP remote sync keeps a persistent mirror of each remote
machine's syncable source files under:

```text
<data_dir>/remote-mirrors/<sanitized-host>-<hash>/
```

For the default data directory, that parent is
`~/.agentsview/remote-mirrors/`. The readable host component is followed by a
hash so names that sanitize to the same directory name do not collide. A lock
file next to each mirror serializes concurrent syncs of that host from the same
data directory.

The mirror adds an on-disk copy of the remote session sources to the collector,
in addition to the indexed database. Budget roughly the size of each remote
host's syncable source corpus for it. Incremental transfer applies only to the
HTTP transport; SSH remote sync continues to copy a full session tree on each
run.

### How A Sync Works

1. The collector asks the remote daemon for its resolved agent roots. The
   remote controls this allowlist; the collector cannot name arbitrary roots.
2. The collector locks the host mirror and requests a gzip-compressed manifest
   of regular, non-symlink files. Each entry carries its absolute remote path,
   size, and modification time.
3. The collector walks the mirror and compares file size and modification time
   at microsecond precision. It schedules missing or changed files for fetch
   and removes mirror files that disappeared from the manifest.
4. When fewer than half of the manifest's files need fetching, the collector
   requests only that delta. At half or more, it requests a full archive instead
   of sending a large file list. The current collector advertises gzip support
   for both archive modes.
5. The archive is extracted into the mirror with remote modification times
   preserved. AgentsView then imports from the complete mirror, so parsers that
   read sibling files behave the same way they do against a full source tree.
   The separate database skip cache avoids unnecessary parsing of unchanged
   sessions during normal syncs.

If no directory-scoped files changed, the collector skips the archive request
and imports directly from the existing mirror. Files that disappear remotely
are deleted from the mirror, but remote import is intentionally non-destructive:
sessions already stored in the AgentsView database remain available.

Windsurf is a special case. Its state database is sanitized into a curated
export for every transfer, so the raw tree cannot safely participate in the
manifest. AgentsView fetches that small export as a separate full archive on
each sync while the rest of the host remains eligible for delta transfer. The
Windsurf content in the mirror is the sanitized export, not a byte-for-byte copy
of the remote state database.

### When AgentsView Downloads A Full Archive

A full HTTP transfer occurs in these cases:

- the per-host mirror is new or was removed
- `agentsview sync --full` is used
- at least half of the manifest files are missing or changed
- a collector data-version resync forces its configured remote syncs full
- a delta request is rejected after a manifest succeeded; the collector retries
  once with a full archive
- the remote daemon does not support manifests, in which case the collector
  uses the legacy full-transfer path on every sync

Windsurf's curated export is also fetched in full on every sync, independently
of the directory-scoped archive decision.

`--full` refreshes the mirror bytes and bypasses the persistent remote
path/mtime skip cache during import. It does not delete the local database or
turn remote sync into a destructive reconciliation.

### Compatibility And Recovery

A 0.37.4 collector works with older remote daemons. A missing manifest route —
including an old daemon's HTML app shell answering that route — makes the
collector report that incremental sync is unavailable and use the legacy
full-archive flow. That flow extracts to a temporary directory and does not
create or update the persistent mirror. Older collectors also continue to use
the full-archive endpoint on a 0.37.4 remote.

The normal mirror comparison detects interrupted extraction when the resulting
file size or modification time differs from the manifest, and the next sync
fetches that file again. It also repairs file-versus-directory conflicts left by
an interrupted extraction.

The comparison does not hash file content. A remote rewrite that preserves both
size and modification time, or local mirror corruption with the same metadata,
can therefore look unchanged. Run a full sync to refresh the bytes:

```bash
# Refresh local sessions and every configured remote.
agentsview sync --full

# Refresh one configured host through the local daemon.
agentsview sync --host devbox1 --full
```

The second form requires `devbox1` to match a configured `[[remote_hosts]]`
entry; otherwise `--host` is an ad hoc SSH sync.

The mirror is a disposable transfer cache. When no sync is running, deleting a
host's mirror directory is safe and makes the next compatible sync bootstrap it
again. Leave the adjacent `.lock` file in place. Removing mirror files never
removes imported sessions from the database.

### Transfer Safety

The remote daemon recomputes its allowed sync targets for each request and
rejects paths outside them. Manifest walks omit symlinks and special files;
delta requests must either match an allowed extra file exactly or use an
absolute path in the same POSIX, drive-letter, or UNC dialect as the allowed
root. Symlinked roots or intermediate components are refused. Mirror deletions
and type-conflict cleanup are separately confined to the per-host mirror root.

These checks limit what authenticated sync requests can read, but they do not
replace network isolation or bearer-token security. Keep the daemon on a
private network and protect its `auth_token` as described above.

## SSE Endpoints

The SSE endpoints also accept `?token=<token>` because browser `EventSource`
cannot set custom headers:

```text
http://<host>:8080/api/v1/events?token=<token>
http://<host>:8080/api/v1/sessions/<id>/watch?token=<token>
```

Use header-based auth for normal API calls whenever possible.

## Public URL And Trusted Origins

When you access AgentsView through a hostname or reverse proxy, tell the server
about the public URL:

```bash
agentsview serve --public-url https://agents.example.com
```

For additional trusted origins, use `--public-origin`:

```bash
agentsview serve \
  --public-origin https://agents.example.com \
  --public-origin https://internal.example.com
```

Flags can also be comma-separated:

```bash
agentsview serve --public-origin https://a.example.com,https://b.example.com
```

These can also be persisted:

```toml
public_url = "https://agents.example.com"
public_origins = [
  "https://agents.example.com",
  "https://internal.example.com",
]
```

### Forwarded Dev Environments

AgentsView validates the request `Host` header before serving API requests. That
protects local loopback servers from DNS-rebinding attacks, but it also means
SSH port forwards, reverse proxies, WSL2, Codespaces, Coder, and similar remote
development environments must use a trusted public URL or origin.

Static assets can still load in these environments because the HTML shell is
intentionally less strict than `/api/` routes. The dashboard may appear briefly,
then `/api/v1/settings` or another API request can fail with `403 Forbidden` if
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
rejected host, the allowed set, and the `--public-url` fix. The Settings page
shows the same actionable message instead of prompting for an auth token. A
short breadcrumb is also written to the server debug log.

### Troubleshooting Forwarded Access

If the UI flashes and then the dashboard or settings page fails, check the
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

## Managed Caddy Mode

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
| `--public-port`     | `8443`      | External port for the public URL                      |
| `--tls-cert`        |             | TLS certificate file path                             |
| `--tls-key`         |             | TLS key file path                                     |
| `--allowed-subnet`  |             | Client CIDR allowlist (repeatable or comma-separated) |

Caddy must already be installed and available on `PATH` unless you override it
with `--caddy-bin`.

### Subnet Allowlists

When using a non-loopback bind host for Caddy, restrict access to specific
networks. Subnet allowlists complement bearer-token auth rather than replacing
it — `require_auth = true` should still be set before binding Caddy off
loopback.

```bash
agentsview serve \
  --proxy caddy \
  --proxy-bind-host 0.0.0.0 \
  --allowed-subnet 192.168.1.0/24 \
  --allowed-subnet 10.0.0.0/8
```

Requests from outside the allowed subnets receive `403`.

## Settings Page

Remote access can also be configured from the Settings page. The **Remote
Access** section lets you:

- toggle `require_auth`
- view the auto-generated auth token
- connect the frontend to another AgentsView server by URL and token

![Settings remote access](/assets/generated/screenshots/settings-remote.png)

Changes that affect bind or auth behavior may require a server restart.

## CLI Flags Reference

| Flag                | Default     | Description                                         |
| ------------------- | ----------- | --------------------------------------------------- |
| `--host`            | `127.0.0.1` | Interface to bind                                   |
| `--public-url`      |             | Public URL for hostname or proxy access             |
| `--public-origin`   |             | Trusted browser origin (repeatable/comma-separated) |
| `--proxy`           |             | Managed proxy mode (`caddy`)                        |
| `--caddy-bin`       | `caddy`     | Caddy binary path                                   |
| `--proxy-bind-host` | `127.0.0.1` | Interface for managed proxy                         |
| `--public-port`     | `8443`      | External port for managed proxy                     |
| `--tls-cert`        |             | TLS certificate path                                |
| `--tls-key`         |             | TLS key path                                        |
| `--allowed-subnet`  |             | Client CIDR allowlist (repeatable/comma-separated)  |

## Config File Reference

Remote-access-related fields in `~/.agentsview/config.toml`:

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
| `require_auth`          | Require bearer-token authentication for API access                         |
| `auth_token`            | Auto-generated 256-bit bearer token; overridden by `AGENTSVIEW_AUTH_TOKEN` |
| `public_url`            | Public URL for host/origin validation                                      |
| `public_origins`        | Additional trusted CORS origins                                            |
| `proxy.mode`            | Managed proxy mode (`caddy`)                                               |
| `proxy.bin`             | Path to proxy binary                                                       |
| `proxy.bind_host`       | Interface for proxy to bind                                                |
| `proxy.public_port`     | External port for proxy                                                    |
| `proxy.tls_cert`        | TLS certificate path                                                       |
| `proxy.tls_key`         | TLS key path                                                               |
| `proxy.allowed_subnets` | CIDR allowlist for proxy connections                                       |

For LAN access you still need a non-loopback bind such as
`agentsview serve --host 0.0.0.0`. `require_auth` controls API auth; it does not
change the bind address by itself.
