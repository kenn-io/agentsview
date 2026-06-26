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

When this command runs inside WSL, AgentsView advertises the WSL `eth0` address
instead of `127.0.0.1` so the printed URL is usable from the Windows host and
nearby LAN clients. An explicit `--public-url` still takes precedence.

When auth is enabled, AgentsView generates a token if needed and prints it on
startup:

```
Auth enabled. Token: <token>
```

Open `http://<your-ip>:8080` from another device and configure the token in the
frontend, or send it in an `Authorization` header.

!!! note

    Older configs may still use `remote_access = true`. AgentsView still reads that
    legacy key, but new setups should use `require_auth = true`.

## Authentication

When `require_auth` is enabled, all `/api/` requests must include:

```
Authorization: Bearer <token>
```

The token is stored in `config.toml` as `auth_token`. It is generated
automatically the first time auth is enabled, so you do not need to mint one
manually.

Auth is enforced on API routes, not on static assets. That means a browser can
load the HTML shell, but API requests fail with `401` until the correct token is
supplied.

When `require_auth` is disabled, normal local loopback use remains ungated.

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
    `~/.agentsview/config.toml` before starting the server, or the proxy will front
    an unauthenticated API. See [Quick Setup](#quick-setup).

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

| Field                   | Description                                        |
| ----------------------- | -------------------------------------------------- |
| `require_auth`          | Require bearer-token authentication for API access |
| `auth_token`            | Auto-generated 256-bit bearer token                |
| `public_url`            | Public URL for host/origin validation              |
| `public_origins`        | Additional trusted CORS origins                    |
| `proxy.mode`            | Managed proxy mode (`caddy`)                       |
| `proxy.bin`             | Path to proxy binary                               |
| `proxy.bind_host`       | Interface for proxy to bind                        |
| `proxy.public_port`     | External port for proxy                            |
| `proxy.tls_cert`        | TLS certificate path                               |
| `proxy.tls_key`         | TLS key path                                       |
| `proxy.allowed_subnets` | CIDR allowlist for proxy connections               |

For LAN access you still need a non-loopback bind such as
`agentsview serve --host 0.0.0.0`. `require_auth` controls API auth; it does not
change the bind address by itself.
