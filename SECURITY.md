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
  web/tool output that agentsview did not author, and defensive parsing of that
  content is a security-relevant concern.
- Inadvertent exposure of secrets that appear in transcripts. agentsview ships a
  best-effort secret detector and redacts findings in the UI and CLI by default
  (see [Secrets subsystem](#secrets-subsystem)).

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

| Boundary                          | Posture                                                          | Notes                                                                |
| --------------------------------- | ---------------------------------------------------------------- | -------------------------------------------------------------------- |
| Session files → parser            | Treated as untrusted data                                        | Parsers should not exec/eval; bugs are security-relevant.            |
| Imports and new readers           | Treated as untrusted structured data                             | Same posture as parsers; imported archives may be remote-origin.     |
| SSH remote sync                   | Treated as user-provisioned and user-authenticated               | Pulled archives are parsed locally as untrusted data.                |
| Parser → SQLite / FTS5            | Trusted                                                          | Indexed content is preserved verbatim.                               |
| HTTP server → caller              | Loopback-trusted; bearer-gated for `/api/` when `--require-auth` | Static assets are not gated.                                         |
| Browser → HTTP server             | Host-header allowlist + CORS + CSP + X-Frame-Options enforced    | DNS-rebinding, framing, and cross-origin defenses.                   |
| agentsview → PostgreSQL (pg push) | TLS required for non-loopback hosts                              | Plaintext rejected unless `allow_insecure = true` is set explicitly. |
| agentsview → update endpoint      | One-way outbound, opt-out                                        | Disable with `--no-update-check`.                                    |
| agentsview → LiteLLM pricing      | One-way outbound, on-demand                                      | Public JSON fetched from GitHub raw; no session data sent.           |

## Data at rest

- The local archive (SQLite + FTS5 index) stores indexed session data in
  plaintext. This includes assistant responses, user prompts, tool arguments,
  command output, file contents fetched by agents, and any secrets that may have
  been pasted into an agent session.
- File permissions follow the user's umask. agentsview does not chmod the data
  directory and does not encrypt at rest.
- Treat the agentsview data directory with the same care you would treat your
  shell history or your editor's swap files. It is at least as sensitive.
- Deletion semantics: the UI supports both a soft-delete (trash) and a
  permanent-delete path. Permanent delete removes rows from the primary tables
  and the FTS5 index; it does not promise erasure of SQLite pages, the WAL,
  on-disk backups, OS file caches, or any external copy that was made via
  `pg push` or SSH sync.

## Data in transit

agentsview makes several distinct outbound connections. Each is listed
explicitly because "data stays on your machine" is the default but is not a
complete description of the system once optional features are in use.

- **Local UI / API.** The HTTP server binds to `127.0.0.1` by default. When
  exposed beyond loopback, `--require-auth` should be enabled. Authentication is
  a bearer token applied to `/api/` routes only; static assets remain ungated.
  Browser-facing defenses (Host-header allowlist, CORS restrictions, CSP,
  `X-Frame-Options: DENY`) are always on. See the CLI reference for token
  configuration.
- **PostgreSQL sync.** `agentsview pg push` exports the local archive to a
  user-supplied PostgreSQL instance. Non-loopback DSNs are rejected unless TLS
  is enabled (`sslmode=require` or stronger), or the user has explicitly set
  `allow_insecure = true` under `[pg]`. The destination database itself is
  treated as user-provisioned infrastructure; agentsview asserts nothing about
  its access controls.
- **SSH remote sync.** agentsview can pull session archives from another machine
  over SSH. Authentication is whatever the user's SSH configuration provides.
  Pulled files are parsed locally as untrusted data and merged into the unified
  archive.
- **Imports.** Imported archives from other agentsview instances or third-party
  exports are treated as untrusted structured data, no different from session
  files written by local agents.
- **LiteLLM pricing.** Pricing metadata is fetched on demand from
  `raw.githubusercontent.com/BerriAI/litellm/...` to compute token costs. No
  session data is sent.
- **Update check.** On startup agentsview makes one outbound HTTPS request to
  check for new releases. No session data is sent; the request is opt-out via
  `--no-update-check`.

## Browser-facing hardening

The HTTP server applies the following defenses unconditionally:

- **Host-header allowlist.** Requests whose `Host` does not match the configured
  listen address (or a configured public URL/origin) are rejected, defending
  against DNS-rebinding attacks where an attacker domain resolves to
  `127.0.0.1`.
- **CORS restrictions.** Cross-origin API requests must come from an allowed
  origin or carry the bearer token; preflight handling is explicit.
- **Content-Security-Policy.** A policy pinning the server's exact origin for
  script/style/image/font/default-src is set on non-API responses. `connect-src`
  is intentionally widened to allow the remote-server feature in the SPA; this
  is a documented tradeoff.
- **X-Frame-Options: DENY.** Framing is disallowed on non-API responses.

## Secrets subsystem

agentsview includes best-effort detection of secrets that appear in session
content (API keys, tokens, credentials). Detected secrets are:

- recorded as findings in the database;
- redacted by default in CLI and UI views;
- revealable only via localhost-gated paths.

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

Security issues should be reported privately, not via public GitHub issues.

\[TODO: maintainer to enable GitHub private vulnerability reporting on the
repository and/or provide a private contact (email, PGP key). Until that contact
is published, please open a non-detailed GitHub issue requesting a private
channel and a maintainer will follow up out-of-band; do not include exploit
details in the public issue.\]

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
   an additional confirmation (e.g., a `--yes-really` flag) on first use, given
   that it disables the only protection against plaintext PG egress?
1. **Deletion guarantees.** Should "permanent delete" grow into a stronger
   erasure path (VACUUM, WAL checkpoint + truncate, mirror propagation to PG/SSH
   targets), or should the docs simply make the current limits clearer?
1. **Secret detection scope.** Should the detector expand (more patterns,
   structured-secret types), should redacted-by-default extend to exports, and
   should there be a "scrub-on-import" pass?
