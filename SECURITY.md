# Security and Privacy Posture

## Status

Foundational document. Describes agentsview's current security and privacy
behavior and the assumptions that behavior rests on, much of which is already
enforced in code but has not been written down in one place. It is intentionally
descriptive rather than prescriptive — several questions are flagged as open and
will be resolved by future proposals rather than decided here.

If you are reporting a vulnerability, see [Reporting](#reporting) below.

## Audience and deployment model

agentsview is designed to run on a single-user workstation, alongside the agent
tools whose sessions it indexes. Single-user laptop or developer workstation is
the assumed deployment; other deployments (shared hosts, public exposure,
multi-machine sync) are possible but require the user to opt into additional
risk via documented flags and config.

## Threat model

### In scope

- A local, authenticated user reading their own session archive through the
  agentsview UI, CLI, or API.
- Network attackers who cannot already execute code as the local user. The
  default loopback bind, Host-header allowlist (DNS rebinding defense), CORS
  restrictions, and CSP / `X-Frame-Options: DENY` headers are aimed at this
  attacker.
- Parser crashes, excessive resource use, or active-content injection triggered
  by content inside supported session files. Session files often contain
  web/tool output that agentsview did not author, and defensive parsing of
  that content is a security-relevant concern.
- Inadvertent exposure of secrets that appear in transcripts. agentsview ships a
  best-effort secret detector and redacts findings in the UI and CLI by
  default (see [Secrets subsystem](#secrets-subsystem)).

### Explicitly out of scope (today)

- Hostile peers on the same OS account. agentsview's data dir inherits
  permissions from the user's umask; no defense is offered against another
  process running as the same user.
- Same-user arbitrary file write or code execution. If an attacker can drop
  files into the configured session directories as the local user, they are
  inside the trust base.
- Multi-user machines with hostile peer accounts.
- Side-channel attacks against the local SQLite database.
- Strong isolation between projects, agents, or time periods within a single
  agentsview instance. The database is a unified archive; any caller able to
  read it can read all of it.
- Strong guarantees about data destruction. Deletion removes rows but does not
  promise SQLite page, WAL, or filesystem-level erasure.

## Trust boundaries

| Boundary                       | Posture                                                             | Notes                                                                  |
| ------------------------------ | ------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| Session files → parser         | Treated as untrusted data                                           | Parsers should not exec/eval; bugs are security-relevant.              |
| Imports and new readers        | Treated as untrusted structured data                                | Same posture as parsers; imported archives may be remote-origin.       |
| HTTP remote sync               | User-configured endpoints authenticated with per-host bearer tokens | Pulled archives are parsed locally as untrusted data.                  |
| SSH remote sync                | System `ssh` client; release binary checked against `SHA256SUMS`    | `SHA256SUMS.sig` is not checked. Pulled archives are untrusted data.   |
| Parser → SQLite / FTS5         | Trusted                                                             | Indexed content is preserved verbatim.                                 |
| HTTP server → caller           | Loopback-trusted; bearer-gated for `/api/` when `--require-auth`    | Static assets are not gated.                                           |
| Browser → HTTP server          | Host-header allowlist + CORS + CSP + X-Frame-Options enforced       | DNS-rebinding, framing, and cross-origin defenses.                     |
| agentsview → PostgreSQL (push) | TLS required for non-loopback hosts                                 | Plaintext rejected unless `allow_insecure = true` is set explicitly.   |
| agentsview → ClickHouse (push) | TLS required for non-loopback hosts                                 | Plaintext or unverified TLS rejected unless `allow_insecure = true`.   |
| agentsview → PostHog telemetry | One-way outbound, opt-out                                           | Anonymous usage events. Disable with `AGENTSVIEW_TELEMETRY_ENABLED=0`. |
| agentsview → update endpoint   | One-way outbound, opt-out                                           | Disable with `--no-update-check`.                                      |
| agentsview → pricing catalogs  | One-way outbound, automatic                                         | Public JSON from GitHub and OpenRouter; no session data sent.          |
| agentsview → model endpoints   | Opt-in; sends transcript content                                    | Generated insights, recall extraction, and embeddings.                 |
| agentsview → hosted services   | Opt-in; user-supplied credentials                                   | Codex Cloud, Cursor Admin API, Kata, Docbank, GitHub, raw sync.        |

## Data at rest

- The local archive (SQLite + FTS5 index) stores indexed session data in
  plaintext. This includes assistant responses, user prompts, tool arguments,
  command output, file contents fetched by agents, and any secrets that may
  have been pasted into an agent session.
- File permissions follow the user's umask. agentsview does not chmod the data
  directory and does not encrypt at rest.
- Treat the agentsview data directory with the same care you would treat your
  shell history or your editor's swap files. It is at least as sensitive.
- Deletion semantics: the UI supports both a soft-delete (trash) and a
  permanent-delete path. Permanent delete removes rows from the primary tables
  and the FTS5 index; it does not promise erasure of SQLite pages, the WAL,
  on-disk backups, OS file caches, or any external copy that was made via
  `pg push` or HTTP remote sync.

## Data in transit

Session data stays on your machine unless you turn on a feature that sends it. A
few requests run automatically, and none of them carries session data. This
section lists every outbound channel: what it sends, when, and how to turn it
off.

### Local server

- **Local UI / API.** The HTTP server binds to `127.0.0.1` by default. When
  exposed beyond loopback, `--require-auth` should be enabled. Authentication
  is a bearer token applied to `/api/` routes only; static assets remain
  ungated. Browser-facing defenses (Host-header allowlist, CORS restrictions,
  CSP, `X-Frame-Options: DENY`) are always on. See the CLI reference for token
  configuration. The web UI talks only to the agentsview server it is pointed
  at. It links to outside sites but never fetches from them.

### Automatic requests

These run without any configuration. None of them sends session content,
prompts, project names, file paths, or hostnames. Each one reveals your IP
address to the host it contacts.

- **Anonymous telemetry.** Release builds send usage events to PostHog's US
  endpoint, `us.i.posthog.com`. The server sends `daemon_active` at most once
  per installation per UTC day while it runs, and `setup_finished` once per
  installation when the archive first shows a session. The web UI reports a
  fixed set of events through the server: app opens, visit-length buckets,
  screen and analytics page views, the agent type of a viewed session, and
  which kind of search, export, insight, error, or sync failure occurred. The
  browser never contacts PostHog, and the server drops any property value
  outside each event's fixed list. Every event carries the random installation
  ID stored in `telemetry-install-id` in the data directory, the app version,
  git commit, distribution channel, operating system, CPU architecture, and
  whole hours since installation. Person profiles and GeoIP lookup are turned
  off. Builds without a release version or the PostHog project key send
  nothing, and so does any process with `CI` set. To turn telemetry off, set
  `AGENTSVIEW_TELEMETRY_ENABLED=0`, `TELEMETRY_ENABLED=0`, or
  `DO_NOT_TRACK=1`, then restart the server. See
  [Anonymous Daemon Telemetry](docs/configuration.md#anonymous-daemon-telemetry)
  for each event and its allowed values.
- **Update check.** When the web UI loads in a browser, the server requests
  `github.com/kenn-io/agentsview/releases/latest` and reads the release tag
  from the redirect. It caches the answer for one hour, or 15 minutes for
  development builds. When a newer release exists, it also requests the size
  of that release's archive for your platform and the release's `SHA256SUMS`.
  `agentsview update` makes the same requests and then downloads the release.
  To turn the check off, use `--no-update-check`,
  `AGENTSVIEW_DISABLE_UPDATE_CHECK=1`, or `disable_update_check = true`. See
  [Disabling Update Checks](docs/configuration.md#disabling-update-checks).
- **Desktop updater.** The Tauri desktop build skips the server's update check.
  Instead, its own updater checks the GitHub release feed shortly after
  launch. Set `AGENTSVIEW_DESKTOP_AUTOUPDATE=0` to turn it off.
- **Pricing catalogs.** The server downloads public model-pricing data at
  startup and once a day after that. It fetches the LiteLLM catalog and the
  Pydantic GenAI Prices document from `raw.githubusercontent.com`, and the
  OpenRouter model list from `openrouter.ai`. Usage reports and replica pushes
  also refresh stale prices, at most once an hour. These are plain `GET`
  requests. Pass `--offline` to `usage` commands to use the embedded prices
  instead. The server has no setting that turns its refresh off. See
  [Pricing Source](docs/token-usage.md#pricing-source).

### Features you turn on

Each of these is off until you configure or run it.

- **PostgreSQL sync.** `agentsview replica pg push` exports the local archive to
  a user-supplied PostgreSQL instance. Non-loopback DSNs are rejected unless
  TLS is enabled (`sslmode=require` or stronger), or the user has explicitly
  set `allow_insecure = true` under `[pg]`. The destination database itself is
  treated as user-provisioned infrastructure; agentsview asserts nothing about
  its access controls.
- **ClickHouse sync.** `agentsview replica clickhouse push` exports the local
  archive to a ClickHouse database you configure. Non-loopback URLs must use
  TLS (`secure=true` for the native protocol, `https://` for HTTP) unless
  `allow_insecure = true`. `skip_verify=true` is also rejected on a
  non-loopback host without `allow_insecure`. See
  [ClickHouse sync](docs/clickhouse-sync.md).
- **DuckDB Quack.** The DuckDB mirror is a local file.
  `agentsview replica duckdb quack serve` exposes it to DuckDB clients that
  hold its token. It binds to loopback by default; binding beyond loopback
  requires `--allow-insecure`, because Quack speaks plain HTTP. On the client
  side, setting `[duckdb].url` or `AGENTSVIEW_DUCKDB_URL` makes
  `duckdb status` and `duckdb serve` send the token and SQL queries to that
  Quack endpoint. A non-loopback client host also requires `allow_insecure`.
  Remove the URL to stop these requests. See
  [Quack Remote Access](docs/duckdb.md#quack-remote-access).
- **HTTP remote sync.** agentsview pulls session archives from configured remote
  daemons using a per-host bearer token. Archive endpoints require that token
  even when the remote daemon's other API routes do not require
  authentication. Use a private network such as Tailscale. Pulled files are
  parsed locally as untrusted data and merged into the unified archive. See
  [HTTP Remote Sync](docs/remote-access.md#http-remote-sync).
- **SSH remote sync.** agentsview runs the system `ssh` client with your
  `~/.ssh/config`, in batch mode, so it never prompts and keys must work
  unattended. It installs one binary per version under the remote user's
  `~/.cache/agentsview/bin`. A release build downloads that version's tarball
  from GitHub over HTTPS and checks it against `SHA256SUMS` from the same
  release. `SHA256SUMS.sig` is not checked, so the check catches a damaged
  download, not a compromised release. A development build uploads its own
  executable instead. The remote command resolves its allowed session
  directories from its own read-only configuration, answers one request, and
  exits. It writes only temporary SQLite snapshots that it removes before it
  exits, and it creates no AgentsView state. Pulled files are parsed locally
  as untrusted data. See
  [SSH Remote Sync](docs/remote-access.md#ssh-remote-sync).
- **S3 session sources.** An `s3://` entry in an agent's `dirs` makes agentsview
  list and download session objects from that bucket, using standard AWS-style
  credentials from the environment. Downloaded files are parsed as untrusted
  data. See
  [S3-Compatible Session Sources](docs/configuration.md#s3-compatible-session-sources).
- **Hosted raw sync.** `agentsview source raw backfill` and
  `agentsview source raw watch` upload original agent session files to an
  operator-managed server, authenticated with a device credential the operator
  supplies. Nothing uploads unless you run one of these commands. See
  [Hosted Raw Sync](docs/hosted-raw-sync.md).
- **Codex Cloud.** With `[agents.codex-cloud] enabled = true`, agentsview reads
  the ChatGPT access token from each Codex home's `auth.json` and uses it to
  fetch your chats and plan usage from OpenAI (`chatgpt.com`). Endpoint
  overrides must stay on `chatgpt.com`, `openai.com`, or loopback, and neither
  client follows redirects, so the token goes to no other host.
  `live_tokens = true` also listens in on running chats, which can change a
  chat on OpenAI's side. Listing `codex-cloud` in `disabled_agents` stops it.
  See [Codex Cloud](docs/configuration.md#codex-cloud).
- **Cursor Admin usage.** `agentsview source cursor sync-usage` sends your
  `cursor_admin_api_key`, optional user filters, and a date range to
  `api.cursor.com`. It runs only when you invoke it. See
  [Cursor Admin Usage Events](docs/token-usage.md#cursor-admin-usage-events).
- **Claude.ai sync.** When you click **Sync**, your Claude.ai chats are fetched
  with a signed-in Claude.ai session. With Chrome, the extension you install
  with `agentsview source chrome setup` makes the requests using Chrome's
  session and passes the chats to agentsview over a local socket. The desktop
  build instead makes the requests from its own sign-in window, which keeps a
  separate Claude.ai session. The agentsview server never contacts Claude.ai,
  and credentials stay in Chrome or the sign-in window. See
  [Sync Claude.ai chats](docs/chat-import.md#sync-claudeai-chats).
- **Generated insights.** When `[insights]` sets `endpoint` and `model`,
  agentsview sends transcript-derived content to that OpenAI-compatible
  endpoint. Remote endpoints must use HTTPS unless `allow_http = true`. With
  neither set, it runs the selected agent CLI, which sends the same content to
  that agent's provider. Tool effectiveness reports include each tool call's
  input and output, secrets included. See [Recall](docs/recall.md).
- **Recall extraction.** When `[recall.extract]` is enabled, agentsview sends
  transcript content to the configured endpoint. Non-loopback endpoints must
  use HTTPS unless a server entry sets `allow_http = true`. Redirects are not
  followed. See [Automatic extraction](docs/recall.md#automatic-extraction).
- **Embeddings.** When `[vector]` is enabled, agentsview sends user and
  assistant message text to the embedding server while it indexes, and each
  semantic or hybrid search query when you search. The server is a local
  Ollama or Kit server, or an OpenAI-compatible endpoint you configure.
  Setting `[vector.embed] recall = true` also sends accepted Recall entry
  titles, bodies, and triggers automatically. Plain HTTP is accepted for
  loopback, private-network addresses, and host names; only a public IP
  address requires HTTPS. Setting up a local server can download model files,
  and agentsview asks first. See [Semantic Search](docs/semantic-search.md).
- **Kata.** With `[kata] enabled = true`, the instance that holds the aggregated
  archive files Friction Log patterns as Kata issues, by hand or hourly when
  `[friction.kata] auto_file = true`. The issue title and body describe the
  finding and how often it recurred. Secrets are masked and home directories
  shortened to `~` first. Plain HTTP works only for loopback hosts unless
  `allow_insecure = true`. See [Kata](docs/kata.md).
- **Docbank.** With `[integrations.docbank]` configured, agentsview sends the
  SHA-256 digests of transcript images to your Docbank server to find where it
  stores them. Only digests leave the machine. The URL must use HTTPS, or HTTP
  on loopback. See
  [Find transcript images in Docbank](docs/configuration.md#find-transcript-images-in-docbank).
- **Publish to Gist.** Publishing a session or insight uploads its HTML export
  to GitHub as a public or secret gist. The server uses the token saved in
  settings (`github_token`). For requests from localhost only, it falls back
  to `AGENTSVIEW_GITHUB_TOKEN`, then `gh auth token`. A secret gist is
  unlisted, but anyone with its link can read it. See
  [Publish to Gist](docs/usage.md#publish-to-gist).
- **GitHub pull-request counts.** When you ask for pull-request outcomes, with
  `agentsview report stats --include-github-outcomes` or from the web UI,
  agentsview runs `gh pr list` in each repository found in the selected
  sessions' working directories. Each run asks GitHub for your pull requests
  opened and merged in the date range. The CLI uses `AGENTSVIEW_GITHUB_TOKEN`,
  then `gh auth token`. Web UI requests use the token resolution described for
  Publish to Gist. Without a token, it skips the lookup. Leave pull-request
  outcomes off to avoid these requests. See
  [Git And PR Aggregation](docs/stats.md#git-and-pr-aggregation).
- **Imports.** Imported archives from other agentsview instances or third-party
  exports are treated as untrusted structured data, no different from session
  files written by local agents.

## Browser-facing hardening

The HTTP server applies the following defenses unconditionally:

- **Host-header allowlist.** Requests whose `Host` does not match the configured
  listen address (or a configured public URL/origin) are rejected, defending
  against DNS-rebinding attacks where an attacker domain resolves to
  `127.0.0.1`.
- **CORS restrictions.** Cross-origin API requests must come from an allowed
  origin or carry the bearer token; preflight handling is explicit.
- **Content-Security-Policy.** A policy pinning the server's exact origin for
  script/style/image/font/default-src is set on non-API responses.
  `connect-src` is intentionally widened to allow the remote-server feature in
  the SPA; this is a documented tradeoff.
- **X-Frame-Options: DENY.** Framing is disallowed on non-API responses.

## Secrets subsystem

agentsview includes best-effort detection of secrets that appear in session
content (API keys, tokens, credentials). Detected secrets are:

- recorded as findings in the database;
- redacted by default in CLI and UI views;
- masked by default in every export that returns message text: the HTML and
  Markdown session exports, Gist publishing, the raw `agentsview session export`
  source stream, and `agentsview export conversations message`. Each has one
  opt-out: a `reveal=true` query parameter that only localhost callers may use,
  or a `--reveal` flag on the local CLI. Session summary and reporting exports
  carry no message text;
- revealable only via localhost-gated paths or an explicit local CLI flag.

This is a defense-in-depth feature, not data-loss prevention. It does not catch
every secret pattern, does not retroactively remove secrets from the on-disk
archive, and does not make plaintext-at-rest storage safe. Treat the archive as
if it contains every secret that ever appeared in any indexed session.

## Contributing features that touch sensitive data

Contributors adding parsers, indexes, readers, or sync targets that expand the
data agentsview ingests, retains, or transmits should:

1. State explicitly which trust boundary the change crosses.
1. Make a deliberate opt-in vs opt-out decision and document it.
1. Add any new egress channel to the Data-in-transit section of this document,
   with a clear description of what is sent.
1. Treat any new structured input source (file format, RPC, import) as untrusted
   by default.

Configuration ergonomics (one boolean vs many granular toggles) is a maintainer
design call and out of scope for this document.

## Reporting

Report security issues privately, not in public GitHub issues.

1. Open the repository's
   [Security tab](https://github.com/kenn-io/agentsview/security) and choose
   **Report a vulnerability**, or go straight to the
   [private report form](https://github.com/kenn-io/agentsview/security/advisories/new).
   You need to be signed in to GitHub.
1. Describe the issue, the affected version, and the steps to reproduce it.

Only you and the maintainers can see the report. Maintainers reply in the report
thread.

## Open questions

These are intentionally not resolved in this document. Each is a candidate for
its own proposal.

1. **Encryption at rest.** Should the SQLite archive support optional at-rest
   encryption (e.g., SQLCipher)? At what UX cost?
1. **Auth model evolution.** Should the bearer-token model grow (per-client
   tokens, expiry, rotation), and how should tokens be stored on disk?
1. **Multi-user machine support.** Is agentsview ever meant to run on a shared
   host, and if so what are the minimum hardening steps?
1. **`allow_insecure` UX.** Should setting `[pg] allow_insecure = true` require
   an additional confirmation (e.g., a `--yes-really` flag) on first use,
   given that it disables the only protection against plaintext PG egress?
1. **Deletion guarantees.** Should "permanent delete" grow into a stronger
   erasure path (VACUUM, WAL checkpoint + truncate, mirror propagation to
   PG/HTTP targets), or should the docs simply make the current limits
   clearer?
1. **Secret detection scope.** Should the detector expand (more patterns,
   structured-secret types), and should there be a "scrub-on-import" pass?
   Exports that return message text now mask detected secrets by default.
